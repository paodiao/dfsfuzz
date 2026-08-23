# HMDFS 内核 Bug：merge_view 遍历丢失 peer 内容（get_next_hmdfs_file_info 错误返回 + readdir 跨调用 pos 错乱）

## 元信息

| 项目 | 内容 |
| --- | --- |
| 发现日期 | 2026-08-23 |
| 复现环境 | 3 节点 QEMU（192.168.0.59/.60/.61）、Linux 6.6.0-dirty、Monarch (syzkaller) fuzzer + hmdfs agent 隧道（`update_socket_param` 移交 fd 模式） |
| Bug 标题 | merge_view 聚合视图丢失"最后注册 peer"的全部内容 |
| 位置 | `hmdfs/file_merge.c:289-304`（`get_next_hmdfs_file_info`）；`hmdfs/file_merge.c:323-383`（`hmdfs_iterate_merge`） |
| 性质 | 链表遍历语义错误 + readdir 跨调用位置（pos）状态不一致，导致 merge 目录遍历提前终止、遗漏设备 |
| 影响 | 节点间 merge_view 内容不一致（"三方互缺"），分布式一致性测试（CSAN 文件数校验）失败：exec 0/2=400、exec 1=1501 |

## 摘要

HMDFS 的 merge_view（合并视图）在遍历各设备（本地 + 各 peer）目录内容时，存在两个叠加的内核缺陷：

1. **`get_next_hmdfs_file_info()` 在"未找到匹配 device_id"时错误返回链表首节点**（应为 NULL）。`list_for_each_entry_safe` 遍历完链表后，safe 指针 `n` 恰好等于 `next(head)`（链表首节点），原实现仅检查 `fi_result != fi_head`，误把首节点当作有效返回值。
2. **`hmdfs_iterate_merge()` 的跨调用位置状态损坏**：设备目录遍历结束（EOF）后 `ctx->pos` 被置为 `LLONG_MAX`，而函数入口的结束检查是 `ctx->pos == -1`（`0xFFFF...`），二者不一致；`LLONG_MAX` 经位域解码得到 `device_id = ULONG_MAX`，随后触发缺陷 1 的错误返回，遍历序列错乱（首设备被重复遍历、最后一个设备从未遍历）。

实际表现为：merge_view 聚合后**缺失"最后注册的 peer"的全部专属内容**（例如节点 2 缺 edd5a2a9 的 8 个专属根目录），且与访问时机、连接状态、peer 在线与否**均无关**——peer 已注册、连接已建立、`device_view` 直访正常，但 merge_view 中永久缺失该 peer。

## 复现现象

### 现象 1：merge_view 内容缺失（三方互缺）

3 节点完全重启、挂载、agent 全部连接建立后（等待 2-5 分钟），各节点 `ls merge_view/`：

| 节点 | 结果 | 缺失 |
| --- | --- | --- |
| 节点 0（74cb） | 12 项 = 74cb 的 4 根 + edd5a2a9 的 8 根 | ab44c6ad 的 6 个专属根 |
| 节点 1（edd5a2a9） | 12 项（同上） | ab44c6ad 的 6 个专属根 |
| 节点 2（ab44c6ad） | 10 项 = 74cb 的 4 根 + 本地 6 根 | edd5a2a9 的 8 个专属根 |

规律：**每个节点恰好缺失"自己最后注册的 peer"（device_id 最大的）的全部内容**。全量应为 18 项（4+6+8）。

排除项：
- `device_view/<cid>` 直访正常（能看到缺失 peer 的内容）→ 连接层/agent 无问题
- 等待 2-5 分钟（peer 全部 online）后访问仍缺 → 与访问时机无关
- `echo 2 > /proc/sys/vm/drop_caches` 后仍缺 → 与 dentry 缓存无关（mount root 常驻，comrade 列表不重建）

### 现象 2：分布式一致性测试（CSAN）文件数不一致

executor 每轮在 `merge_view` 目录下遍历统计（`src/executor/executor.cc:3141-3159`，`write_dir_info("/mnt/hmdfs/100/non_account/merge_view")`），各节点 executor 看到的 merge_view 内容不同 → 统计结果不一致（exec 0/2=400、exec 1=1501），触发 saveCsanBug。

### 诊断日志摘录（节点 2，Linux 6.6.0-dirty 诊断打印）

```
lookup_merge_root() merge_lookup_root: peers_in_node_list=2 work_count_before=0
lookup_merge_root() merge_lookup_root: schedule peer devid=1 cid=74cb462516cfffba
lookup_merge_root() merge_lookup_root: schedule peer devid=2 cid=edd5a2a95088dcc6
merge_lookup_work_func() merge_lookup_work: devid=0 name=device_view/local OK linked
merge_lookup_work_func() merge_lookup_work: devid=1 name=device_view/74cb... OK linked
merge_lookup_work_func() merge_lookup_work: devid=2 name=device_view/edd5a2a9... OK linked
lookup_merge_root() merge_lookup_root: done work_count=0 ret=0
do_dir_open_merge() dir_open_merge: dentry=merge_view total_comrades=3
do_dir_open_merge() dir_open_merge: devid=0 opened
do_dir_open_merge() dir_open_merge: devid=1 opened
do_dir_open_merge() dir_open_merge: devid=2 opened
hmdfs_iterate_merge() iterate_merge: entry pos=0 decoded_devid=0
hmdfs_iterate_merge() iterate_merge: list devid=0
hmdfs_iterate_merge() iterate_merge: list devid=1
hmdfs_iterate_merge() iterate_merge: list devid=2
hmdfs_iterate_merge() iterate_merge: traverse devid=0
get_next_hmdfs_file_info() get_next: in_devid=-1 out_devid=0     ← 入参 -1（ULONG_MAX 截断），错误返回首节点
hmdfs_iterate_merge() iterate_merge: entry pos=9223372036854775807 decoded_devid=18446744073709551615
hmdfs_iterate_merge() iterate_merge: list devid=0
hmdfs_iterate_merge() iterate_merge: list devid=1
hmdfs_iterate_merge() iterate_merge: list devid=2
hmdfs_iterate_merge() iterate_merge: traverse devid=0            ← 首设备被重复遍历
get_next_hmdfs_file_info() get_next: in_devid=0 out_devid=1
hmdfs_iterate_merge() iterate_merge: traverse devid=1
                                                                    ← devid=2 从未遍历！
```

## 根因分析

### 代码位置

| 文件 | 行号 | 内容 |
| --- | --- | --- |
| `hmdfs/file_merge.c` | :289-304 | `get_next_hmdfs_file_info()`（缺陷 1） |
| `hmdfs/file_merge.c` | :306-321 | `get_hmdfs_file_info()`（按 device_id 精确查找） |
| `hmdfs/file_merge.c` | :323-383 | `hmdfs_iterate_merge()`（merge 目录 readdir 主流程，缺陷 2） |
| `hmdfs/file_merge.c` | :342 | `if (ctx->pos == -1) return 0;`（EOF 检查，与缺陷 2 相关） |
| `hmdfs/file_merge.c` | :209-287 | `hmdfs_actor_merge()`（去重/输出 actor，:249-250 目录重复跳过） |
| `hmdfs/inode_merge.c` | :606-675 | `lookup_merge_root()`（merge_view 根 lookup，comrade 建立） |
| `hmdfs/file_remote.c` | :885-893 | `hmdfs_set_pos()`（device_id/group/offset 位域编码） |
| `hmdfs/hmdfs_dentryfile.h` | :31-33 | `POS_BIT_NUM=64`、`DEV_ID_BIT_NUM=16`、`GROUP_ID_BIT_NUM=39` |

### 缺陷 1：`get_next_hmdfs_file_info` 无匹配时错误返回链表首节点

```c
struct hmdfs_file_info *
get_next_hmdfs_file_info(struct hmdfs_file_info *fi_head, int device_id)
{
	struct hmdfs_file_info *fi_iter = NULL;
	struct hmdfs_file_info *fi_result = NULL;

	mutex_lock(&fi_head->comrade_list_lock);
	list_for_each_entry_safe(fi_iter, fi_result, &(fi_head->comrade_list),
				  comrade_list) {
		if (fi_iter->device_id == device_id)
			break;
	}
	mutex_unlock(&fi_head->comrade_list_lock);

	return fi_result != fi_head ? fi_result : NULL;
}
```

`list_for_each_entry_safe(pos, n, head, member)` 的语义：遍历结束后（未 `break`），`pos` 回落为链表头 `head`，而 `n` 是"当前节点的 next"，即 `n = next(head) = 链表首节点`。

因此：
- **匹配成功**（`break`）：`fi_result = 匹配节点的 next`，返回正确；
- **匹配失败**（遍历完）：`fi_result = next(head) = 链表首节点`，**不等于 `fi_head`，通过检查，错误返回首节点**（应为 NULL）。

### 缺陷 2：`hmdfs_iterate_merge` 跨调用位置状态不一致

`hmdfs_iterate_merge` 的跨调用续读机制依赖 `ctx->pos` 携带"当前遍历到哪个设备 + 设备内位置"，由 `hmdfs_set_pos(dev_id, group_id, offset)` 位域编码（`hmdfs_dentryfile.h:31-33`）：

```c
loff_t hmdfs_set_pos(unsigned long dev_id, unsigned long group_id,
		     unsigned long offset)
{
	loff_t pos;
	pos = ((loff_t)dev_id << (POS_BIT_NUM - 1 - DEV_ID_BIT_NUM)) +
	      ((loff_t)group_id << OFFSET_BIT_NUM) + offset;
	if (dev_id)
		pos |= ((loff_t)1 << (POS_BIT_NUM - 1));
	return pos;
}
```

解码（`file_merge.c:330-331`）：

```c
unsigned long device_id = (unsigned long)((ctx->pos) << 1 >>
			  (POS_BIT_NUM - DEV_ID_BIT_NUM));
```

问题链（诊断日志实锤）：

1. 某设备遍历结束后，其 `f_pos` 被置为 `LLONG_MAX`（`0x7FFF...`，EOF 标记）；`hmdfs_iterate_merge` 在 `iterate_dir` 之后执行 `file->f_pos = lower_file_iter->f_pos; ctx->pos = file->f_pos;`（:362-363），`ctx->pos` 因而变成 `LLONG_MAX`；
2. 函数入口的 EOF 检查是 `ctx->pos == -1`（:342，即 `0xFFFF...`）——**与设备层 EOF 标记 `LLONG_MAX` 不一致**，检查永不触发；
3. 下一次调用对 `LLONG_MAX` 解码：`(LLONG_MAX << 1) >> 48` → 算术右移符号扩展 → `0xFFFF...` → `device_id = ULONG_MAX`（日志：`decoded_devid=18446744073709551615`）；
4. `get_hmdfs_file_info(ULONG_MAX)` 返回 NULL → 调用 `get_next_hmdfs_file_info(ULONG_MAX)` → **触发缺陷 1**，错误返回首节点（日志：`in_devid=-1 out_devid=0`）→ 首设备被重复遍历（`traverse devid=0` 第二次）；
5. 重复遍历首设备（去重树跳过、无新输出）后经 `get_next(0)` 回到正常序列（`in=0 out=1` → `traverse devid=1`），但**调用在 devid=1 之后提前结束，devid=2 从未被遍历** → 缺失最后注册 peer 的全部内容。

## 触发链路

```
挂载 + agent 连接建立（所有 peer 注册、online，device_view 直访正常）
  → 首次访问 merge_view 根（用户 ls / executor write_dir_info）
  → lookup_merge_root：对 local + 各 peer 发起 merge_lookup_async（全部 OK，3 个 comrade 建立）
  → do_dir_open_merge：3 个 comrade 全部打开成功（fi 列表 [0,1,2]）
  → hmdfs_iterate_merge 第 1 次调用：遍历 devid=0（本地）后提前退出，ctx->pos=LLONG_MAX
  → 第 2 次调用：解码 device_id=ULONG_MAX → get_next 错误返回首节点（缺陷 1）
  → 遍历序列错乱（0 重复、2 缺失）→ merge_view 缺最后注册 peer 的全部内容
```

## 为什么 100% 是内核 Bug（排除用户态/连接层）

1. **连接层完全正常**：`merge_lookup_work` 三个 work 全部 `OK linked`；`dir_open_merge` 三个 comrade 全部 `opened`；`device_view/<cid>` 直访正常（能看到缺失 peer 的完整内容）；连接建立日志无任何 broken/reject。
2. **与访问时机无关**：等待 2-5 分钟（peer 全部 online）后首访仍缺；`drop_caches` 后仍缺。
3. **与 agent 无关**：本次环境 agent 全部连接成功（`tcp_update_socket` state 正常、握手完成、online 回调正常），无旧版"双向连接竞争拒绝"问题。
4. **内核日志自证**：`get_next: in=-1 out=0`（入参 ULONG_MAX 截断为 -1，返回首节点）、`decoded_devid=18446744073709551615`、`traverse` 序列缺 devid=2——全部是内核 `file_merge.c` 内部遍历逻辑的行为。

## 影响分析

- merge_view 聚合视图缺失"最后注册 peer"的全部内容 → 各节点视图不一致（"三方互缺"）→ 分布式文件系统一致性校验（文件数、目录结构比对）失败；
- CSAN 测试（`exec 0/2=400`、`exec 1=1501`）被误报为文件系统一致性 bug；
- 缺陷 2（EOF 标记与结束检查不一致）在任意"设备遍历完 + 提前退出"的路径下都可能损坏跨调用状态，影响所有 merge 目录（含大目录）的 readdir 分批行为。

## 修复方案（内核侧补丁）

### 修复 1：`get_next_hmdfs_file_info` 无匹配时返回 NULL

```c
struct hmdfs_file_info *
get_next_hmdfs_file_info(struct hmdfs_file_info *fi_head, int device_id)
{
	struct hmdfs_file_info *fi_iter = NULL;
	struct hmdfs_file_info *fi_result = NULL;

	mutex_lock(&fi_head->comrade_list_lock);
	list_for_each_entry_safe(fi_iter, fi_result, &(fi_head->comrade_list),
				  comrade_list) {
		if (fi_iter->device_id == device_id)
			break;
	}
	mutex_unlock(&fi_head->comrade_list_lock);

	/* 遍历完链表未找到匹配：fi_iter 回落为链表头（list_for_each_entry_safe
	 * 遍历结束语义），此时 fi_result == next(head) == 链表首节点，
	 * 原实现误判为有效返回值。显式检查链表头。 */
	if (&fi_iter->comrade_list == &fi_head->comrade_list)
		return NULL;
	return fi_result != fi_head ? fi_result : NULL;
}
```

要点：匹配成功（`break`）时 `fi_iter` 是有效节点、`fi_result` 是其 next；遍历完（未匹配）时 `fi_iter` 回落为链表头，`&fi_iter->comrade_list == &fi_head->comrade_list` 成立 → 返回 NULL。匹配到链表尾节点时 `fi_result == fi_head` → 返回 NULL（原有语义不变）。

### 修复 2：`hmdfs_iterate_merge` 解码出的 device_id 无效时从头遍历

```c
	fi_iter = get_hmdfs_file_info(fi_head, device_id);
	if (!fi_iter)
		fi_iter = get_next_hmdfs_file_info(fi_head, device_id);
	if (!fi_iter) {
		/* get/get_next 都未命中：解码出的 device_id 在 comrade 链表中
		 * 不存在（如单设备 merge 目录——大目录只在 peer 上有，链表无
		 * local/dev 0——首访 pos=0 解码 device_id=0 找不到；
		 * 或 EOF 标记 LLONG_MAX 被错解为 ULONG_MAX）。
		 * 从头开始遍历，已输出的条目由去重树
		 * （fi_head->root / insert_filename）跳过，不会重复输出。 */
		mutex_lock(&fi_head->comrade_list_lock);
		if (!list_empty(&fi_head->comrade_list))
			fi_iter = list_first_entry(&fi_head->comrade_list,
						   struct hmdfs_file_info,
						   comrade_list);
		mutex_unlock(&fi_head->comrade_list_lock);
	}
```

要点：
- 插入位置：位于 `if (!fi_iter) { ... }` 块（:346-352，内含 `get_next` 与 `ctx_merge.ctx.pos = hmdfs_set_pos(...)` 设置）**之后**——此时 `ctx_merge.ctx.pos` 保持初始化值 0，被遍历设备从第 0 项开始（"从头"）；
- **条件为 `!fi_iter`（不含 `device_id != 0` 限制）**：最初版本带 `device_id != 0`（假设"device_id=0 且都未命中 = 链表空"），但**单设备 peer merge 目录**（大目录只在 edd5a2a9 有，comrade 链表只有 dev 2、无 dev 0）在**首访 `pos=0` 解码出 `device_id=0`** 时同样未命中——带条件会跳过"从头"导致空输出（实测 `ls 大目录 | wc -l` = 0）。放宽后：链表空时 `list_empty` 兜底返回 NULL（无死循环，VFS 位置检查终止）；merge_view 根（链表含 dev 0）`get(0)` 命中不触发，无影响；
- 取链表第一个节点（不依赖 device_id == 0 存在与否，比 `get(0)` 更健壮）；
- 去重树兜底：`hmdfs_actor_merge` 对已输出条目返回旧类型（:249-250 目录 `goto done` 跳过），重复遍历无副作用；
- 行为与原代码"缺陷 1 错误返回首节点"等效（原代码正是靠错误返回实现"从头"），修复后显式化，不再依赖 bug。

### 修复 3（曾尝试，已放弃）：`while` 正常退出时置 EOF 标记

**尝试方案**：在 `while` 循环正常退出（`get_next` 返回 NULL）后执行 `ctx->pos = -1`，使下次调用命中函数入口的 `ctx->pos == -1` 检查而直接返回。

**放弃原因（实测后推演确认）**：`while` 正常退出只发生在 `ctx_merge.result == false` 的路径上——即"设备遍历因缓冲满而中断"（最后一次 emit 返回 false）。此时**设备尚未遍历完**（大目录 1100 项 > getdents64 缓冲 ~32KB，必然分批），置 `-1` 会**切断续读、永久丢失剩余条目**（大目录统计将从 1501 掉到 ~1201）。而 merge_view 根场景（每设备 6/4/8 项，缓冲不满）的 `while` 正常退出**永不发生**（设备完成后 `result=true` 走修复 4 的提前 done 分支）——因此该方案"要么不触发、要么有害"，予以放弃。merge_view 根的 EOF 由修复 4 覆盖（`get_next` 无下一个设备时置 `-1`）。

### 修复 4：设备遍历完成（提前退出）后推进到下一个设备

**背景**：`ctx_merge.result` 记录的是**最后一次 actor（emit）的返回值**（file_merge.c:276）——设备正常遍历完（最后条目 emit 成功）时 `result=true`，原代码 391 行 `if (ctx_merge.result) goto done;` 将其当作"缓冲满"提前停止。**结果：每次调用只处理 1 个设备就提前 done**，且 done 后 `ctx->pos` 停在设备 f_pos（EOF 标记 `LLONG_MAX`）——与调用进入时相同 → **VFS 的 `ctx->pos == last_pos` 检查判定位置无进展 → readdir 提前 EOF** → 后续设备永不遍历（devid=2 缺失的直接原因）。

```c
		if (err) {
			/* 远程设备 readdir 返回 iterate_result（>0 表示输出中止/
			 * 本次调用完成，<0 为真实错误）：推进到下一个设备 */
			if (err < 0)
				goto done;
			fi_iter = get_next_hmdfs_file_info(fi_head, device_id);
			if (fi_iter) {
				file->f_pos = hmdfs_set_pos(fi_iter->device_id, 0, 0);
				ctx->pos = file->f_pos;
			} else {
				ctx->pos = -1;
			}
			goto done;
		}
		if (ctx_merge.result) {
			/* 当前设备已遍历完成（最后一次 emit 成功）：推进到
			 * 下一个设备，避免 ctx->pos 停在设备 f_pos（EOF 标记）
			 * 导致 VFS 判定位置无进展而提前结束 readdir。 */
			fi_iter = get_next_hmdfs_file_info(fi_head, device_id);
			if (fi_iter) {
				file->f_pos = hmdfs_set_pos(fi_iter->device_id, 0, 0);
				ctx->pos = file->f_pos;
			} else {
				ctx->pos = -1;
			}
			goto done;
		}
```

**修复后调用序列**（merge_view 根，每次调用推进一个设备）：

```
调用1: fi0(6项) → 推进 fi1 → ctx->pos = set_pos(1,0,0)（≠进入时 → VFS 继续）
调用2: fi1(4项) → 推进 fi2 → ctx->pos = set_pos(2,0,0)
调用3: fi2(8项) → get_next(2)=NULL → ctx->pos = -1
调用4: ctx->pos == -1 → 函数入口直接返回 0 → EOF
总输出 18 项 ✓
```

**大目录（单设备）"缓冲满"路径**（`result=false`）：不触发本分支，走 `get_next` 推进（单设备返回 NULL）→ `while` 正常退出 → 无 EOF 标记（修复 3 已放弃）→ `ctx->pos` 停在设备 f_pos → 下次调用从头续读（去重树跳过已输出条目）→ 剩余输出——恢复修复前的分批续读行为，大目录统计保持准确。

### 收敛性论证（无死循环、无重复输出）

- 每次 `hmdfs_iterate_merge` 调用从有效 device 开始遍历；设备完成后由修复 4 推进到下一个设备（`ctx->pos` 编码为 `set_pos(next, 0, 0)`，位置有进展）；
- 已输出条目由去重树跳过（目录 `:249-250`）——正常推进路径每设备只遍历一次，不产生重复；
- 全部设备遍历完 → `ctx->pos = -1` → 下次调用 `:342` 直接返回 0；
- VFS 侧兜底：`vfs_getdents` 对每次 `iterate_dir` 调用检查 `ctx->pos == last_pos`（位置无进展即视为 EOF，`fs/readdir.c`）——即使出现位置未编码等异常，VFS 也必然终止（这也是当前代码在遍历错乱后 ls 仍能正常结束的机制），保证无死循环。

## 验证方法

1. 应用上述修复（修复 1/2/4），重新编译 hmdfs 内核模块，部署到 3 个节点 VM；
2. 挂载 + 启动 agent，等待连接全部建立（peer online）；
3. `ls merge_view/` → 预期 18 项全输出（含 edd5a2a9 的 8 个专属根、hisrrykxiv 大目录根）；
4. `dmesg` 中 `traverse` 序列应为 `0 → 1 → 2`（跨 2-3 次调用，每次推进一个设备）；
5. 重跑 CSAN 分布式一致性测试 → 各 executor 文件数统计一致（三方互缺消失）；
6. 重点确认大目录统计不受影响（exec 1 仍为 1501——验证"缓冲满"路径的续读未被子修复破坏）。

## 遗留问题（后续调查项）

1. **"设备遍历后提前退出"的确切触发点**：已基本定位——`ctx_merge.result` 记录最后一次 emit 的返回值，设备正常遍历完（最后条目 emit 成功）时 `result=true`，原代码 391 行将其误当"缓冲满"提前停止（修复 4 已处理）；"缓冲满"（`result=false`，真实分批）路径仍依赖"从头续读"（去重树兜底），无设备内续读机制，值得后续统一。
2. **跨调用位置（pos）状态机脆弱**：设备层 EOF 标记（`LLONG_MAX`）与 merge 层结束检查（`-1`）不一致、提前退出时 `ctx->pos` 未按位域编码（直接赋值设备 f_pos）——建议后续统一 EOF 语义并完善编码（设备内续读），从根本上消除状态损坏。
3. **文件冲突改名行为**：`hmdfs_actor_merge` 对重复文件条目（`:259-266`）会改名后输出（`CONFLICTING_FILE_SUFFIX`）。在"从头重遍历"路径（大目录分批续读）下同名文件可能被误判为冲突而改名重复输出（当前实测大目录统计 1501 未观察到重复，但存在理论风险），需在统一续读机制后复查。

## 附录

### 相关文件速查

| 文件 | 行号 | 说明 |
| --- | --- | --- |
| `hmdfs/file_merge.c` | :289-304 | `get_next_hmdfs_file_info`（缺陷 1） |
| `hmdfs/file_merge.c` | :323-383 | `hmdfs_iterate_merge`（缺陷 2，修复 2/3） |
| `hmdfs/file_merge.c` | :342 | `ctx->pos == -1` EOF 检查 |
| `hmdfs/file_merge.c` | :209-287 | `hmdfs_actor_merge`（去重树、冲突改名） |
| `hmdfs/file_merge.c` | :382-428 | `do_dir_open_merge`（comrade → lower_file） |
| `hmdfs/inode_merge.c` | :606-675 | `lookup_merge_root`（comrade 建立） |
| `hmdfs/inode_merge.c` | :345-374 | `merge_lookup_comrade`（vfs_path_lookup） |
| `hmdfs/file_remote.c` | :885-893 | `hmdfs_set_pos`（位域编码） |
| `hmdfs/hmdfs_dentryfile.h` | :31-33 | 位域定义（POS=64/DEV=16/GROUP=39） |
| `src/executor/executor.cc` | :3141-3159 | executor 遍历 merge_view（触发者） |

### 诊断代码（临时，定位后应移除）

本次定位过程在 `hmdfs/file_merge.c` 与 `hmdfs/inode_merge.c` 中加入了诊断打印（`merge_lookup_root`/`merge_lookup_work`/`dir_open_merge`/`iterate_merge`/`get_next` 各环节），已随调试提交。修复方案落地并验证通过后，应删除全部诊断打印，仅保留功能性修复。

### 与测试环境的关系

- 节点编号：节点 0=74cb、节点 1=edd5a2a9、节点 2=ab44c6ad；agent 启动顺序 0→1→2，device_id 按 UPDATE_SOCKET 到达顺序分配（后注册的 peer device_id 更大）；
- "缺失最后注册 peer"的规律与连接注册顺序强相关：节点 2 的缺失对象（edd5a2a9）由其内核日志直接确认（`Got a newly allocated peer: device_id = 2` 后缺它）；**节点 0/1 的缺失对象（ab44c6ad）为按 agent 启动顺序（0→1→2）推断**（其 `Got a newly allocated peer` 日志不打印 cid，无法直接确认 dev1/dev2 对应关系）——但无论 dev2 具体是谁，"缺失最后注册者"的模式成立；且**与连接是否成功无关**（连接全部成功仍缺）。
