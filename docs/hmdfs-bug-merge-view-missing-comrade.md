# HMDFS 内核 Bug：merge_view readdir 跨调用状态（ctx->pos 位域）机制缺陷与 fi_head 状态机重构

## 元信息

| 项目 | 内容 |
| --- | --- |
| 发现日期 | 2026-08-23 |
| 复现环境 | 3 节点 QEMU（192.168.0.59/.60/.61）、Linux 6.6.0-dirty、Monarch (syzkaller) fuzzer + hmdfs agent 隧道（`update_socket_param` 移交 fd 模式） |
| Bug 标题 | merge_view 聚合视图遍历丢失设备内容（readdir 跨调用状态损坏） |
| 位置 | `hmdfs/file_merge.c:332-427`（`hmdfs_iterate_merge`）；`hmdfs/file_merge.c:289-307`（`get_next_hmdfs_file_info`）；`hmdfs/hmdfs.h:259-274`（`struct hmdfs_file_info`） |
| 性质 | 目录遍历迭代器的跨调用状态（`ctx->pos` 位域编码）被设备层 f_pos 语义污染、且位域容量不足，叠加"设备完成/缓冲满"判定语义错乱，导致设备遍历遗漏与分批续读失效 |
| 影响 | 节点间 merge_view 内容不一致（"三方互缺"）；大目录（1100 文件）分批 readdir 丢剩余条目；分布式一致性测试（CSAN 文件数校验）失败：exec 0/2=400、exec 1=1501 |

## 摘要

HMDFS 的 merge_view（合并视图）readdir 通过 `hmdfs_iterate_merge` 遍历各设备（本地 + 各 peer），依赖 `ctx->pos` 的位域编码（`hmdfs_set_pos`，`POS_BIT_NUM=64/DEV_ID_BIT_NUM=16/GROUP_ID_BIT_NUM=39/OFFSET_BIT_NUM=8`）实现跨调用续读。该机制存在三类缺陷：

1. **`get_next_hmdfs_file_info()` 无匹配时错误返回链表首节点**（`list_for_each_entry_safe` 遍历完 `n = next(head)` 误判），破坏遍历序列；
2. **`ctx->pos` 被设备 f_pos 污染**：设备遍历完/中断后设备 f_pos（`LLONG_MAX` 等）被直接赋给 `ctx->pos`，下次调用解码出 `ULONG_MAX`，设备定位错乱；且 **`OFFSET_BIT_NUM` 仅 8 位**，本地设备（ext4 透传）的目录偏移超过 255 即溢出，设备内续读位置无法经位域无损承载；
3. **"设备完成/缓冲满"判定语义错乱**：`ctx_merge.result`（最后一次 emit 的返回值）为 `true` 恰是"设备正常遍历完"，原代码却当作"缓冲满"提前停止（每次调用只遍历 1 个设备 → VFS 位置无进展判定 EOF → 后续设备永不遍历）；而远程设备 `err=1`（filldir 满）又曾被误当"完成"推进/EOF（大目录分批丢剩余）。

最终以 **`fi_head`（file->private_data，open 期间存活）显式状态机**重构迭代器：`cur_dev`（当前设备）+ `dev_pos`（设备内续读位置，原样保存/恢复，不经位域）+ `seq`（VFS 进度计数），彻底放弃从 `ctx->pos` 解码恢复进度；`ctx->pos` 仅作 VFS 进度指示。

## 复现现象

### 现象 1：merge_view 内容缺失（三方互缺）

3 节点完全重启、挂载、agent 全部连接建立后（等待 2-5 分钟），各节点 `ls merge_view/`：

| 节点 | 结果 | 缺失 |
| --- | --- | --- |
| 节点 0（74cb） | 12 项 = 74cb 的 4 根 + edd5a2a9 的 8 根 | ab44c6ad 的 6 个专属根 |
| 节点 1（edd5a2a9） | 12 项（同上） | ab44c6ad 的 6 个专属根 |
| 节点 2（ab44c6ad） | 10 项 = 74cb 的 4 根 + 本地 6 根 | edd5a2a9 的 8 个专属根 |

规律：**每个节点恰好缺失"自己最后注册的 peer"（device_id 最大的）的全部内容**。全量应为 18 项（4+6+8）。排除项：`device_view/<cid>` 直访正常（连接层无问题）；等待 2-5 分钟（peer 全部 online）仍缺（与访问时机无关）；`drop_caches` 后仍缺（mount root 常驻，comrade 列表不重建）。

### 现象 2：分布式一致性测试（CSAN）文件数不一致

executor 每轮在 `merge_view` 目录下遍历统计（`src/executor/executor.cc:3141-3159`，`write_dir_info("/mnt/hmdfs/100/non_account/merge_view")`），各节点 executor 看到的 merge_view 内容不同 → 统计不一致（exec 0/2=400、exec 1=1501），触发 saveCsanBug。

### 现象 3：大目录分批 readdir 丢剩余条目（修复演进中暴露）

大目录（`Eris_qfehribmzl_93443936692.d`，1100 文件，位于 edd5a2a9，见附录"大目录结构说明"）在节点 2（远程单设备 merge 目录，comrade 仅 edd5a2a9）上 `ls | wc -l` 仅得 **585**——1100 文件分批（> getdents64 缓冲 ~32KB）时，第一批后的续读失效，剩余 515 丢失。

### 诊断日志摘录（节点 2，Linux 6.6.0-dirty 诊断打印）

**merge_view 根（修复推进后）**：

```
lookup_merge_root() merge_lookup_root: peers_in_node_list=2 work_count_before=0
merge_lookup_work_func() merge_lookup_work: devid=0/1/2 ... OK linked   （3 comrade 全建）
do_dir_open_merge() dir_open_merge: dentry=merge_view total_comrades=3
do_dir_open_merge() dir_open_merge: devid=0/1/2 opened
iterate_merge: traverse devid=0 → get_next: in=0 out=1
iterate_merge: entry pos=-9223231299366420480 decoded_devid=1  → traverse devid=1 → get_next: in=1 out=2
iterate_merge: entry pos=-9223090561878065152 decoded_devid=2  → traverse devid=2 → get_next: in=2 out=-1
（EOF：pos=-1 → 函数入口直接返回）
```

**大目录（585 现象，修复前 err 分支缺陷期）**：

```
do_dir_open_merge() dir_open_merge: dentry=Eris_qfehribmzl... total_comrades=1
do_dir_open_merge() dir_open_merge: devid=2 opened
iterate_merge: entry pos=0 decoded_devid=0
iterate_merge: list devid=2
iterate_merge: traverse devid=2
get_next: in=2 out=-1          ← 单设备；第一批 585 项后 filldir 满 → err=1 → 被当"完成"推进 → -1 EOF → 丢 515
```

## 根因分析

### 代码位置

| 文件 | 行号 | 内容 |
| --- | --- | --- |
| `hmdfs/file_merge.c` | :289-307 | `get_next_hmdfs_file_info()`（缺陷 1） |
| `hmdfs/file_merge.c` | :332-427 | `hmdfs_iterate_merge()`（缺陷 2/3；最终状态机重构处） |
| `hmdfs/file_merge.c` | :209-287 | `hmdfs_actor_merge()`（去重树 + 输出 actor，:276 记录 result） |
| `hmdfs/file_merge.c` | :429-489 | `do_dir_open_merge()`（fi 列表建立，open 期间固定） |
| `hmdfs/hmdfs.h` | :259-274 | `struct hmdfs_file_info`（状态机字段） |
| `hmdfs/file_remote.c` | :885-893 | `hmdfs_set_pos()`（位域编码） |
| `hmdfs/file_remote.c` | :900-1006 | 远程设备 readdir（`iterate_result` 语义、:960/:966 设备内位置更新） |
| `hmdfs/file_local.c` | :231-250 | 本地设备 readdir（ext4 透传） |
| `hmdfs/hmdfs_dentryfile.h` | :31-35 | 位域定义（`OFFSET_BIT_NUM=8`、`OFFSET_BIT_MASK=0xFF`） |

### 原设计：ctx->pos 位域承载跨调用续读

```c
loff_t hmdfs_set_pos(unsigned long dev_id, unsigned long group_id, unsigned long offset)
{
    pos = (dev_id << 47) + (group_id << 8) + offset;
    if (dev_id) pos |= (1 << 63);          /* 远程标记 */
}
/* 解码（原 file_merge.c:339-340） */
device_id = (pos << 1) >> 48;
```

意图：`ctx->pos` 同时携带"设备号 + 设备内位置"，readdir 分批（filldir 满）时下次调用从编码位置续读。**该机制在实现层面有三处破坏**：

### 缺陷 1：`get_next_hmdfs_file_info` 无匹配时返回链表首节点

`list_for_each_entry_safe(pos, n, head, member)` 遍历完（未 `break`）时 `pos` 回落为链表头、`n = next(head) = 链表首节点`。原实现仅检查 `fi_result != fi_head`，把首节点误当有效"下一个"返回（应为 NULL）。导致解码无效（ULONG_MAX）时错误回到首设备，遍历序列错乱。

### 缺陷 2：`ctx->pos` 被设备 f_pos 污染 + 位域容量不足

**f_pos 污染**：原实现在每次设备遍历后执行：

```c
err = iterate_dir(lower_file_iter, &ctx_merge.ctx);
file->f_pos = lower_file_iter->f_pos;      /* 设备 f_pos 直接赋给 hmdfs f_pos */
ctx->pos = file->f_pos;                    /* ctx->pos 被污染 */
```

本地设备遍历**完成**后设备 f_pos = `LLONG_MAX`（EOF 标记，日志实锤）→ `ctx->pos = LLONG_MAX` → 下次调用解码 `(LLONG_MAX << 1) >> 48`（算术右移符号扩展）→ **ULONG_MAX**（日志 `decoded=18446744073709551615`）→ 设备定位失效，依赖缺陷 1 的错误返回"苟延"。

**位域容量不足**：`OFFSET_BIT_NUM=8`（`OFFSET_BIT_MASK=0xFF`）——本地设备（ext4 透传）的目录偏移（大目录可达数 KB）**超过 255 即溢出**；远程设备内部位置是 `set_pos(dev,i,j)` 编码（与 hmdfs 层编码同形但语义是"设备内组/项"）。**设备内续读位置无法经 `ctx->pos` 位域无损承载**——这是"从头 + 去重树续读"（而非真续读）出现的根因，也是最终选择 `fi_head` 直接保存原值的原因。

### 缺陷 3："设备完成/缓冲满"判定语义错乱

设备 readdir 返回三个独立信号：

| 信号 | 含义 | 本地（ext4 透传） | 远程 |
| --- | --- | --- | --- |
| `err` | 设备 readdir 返回值 | 恒 0 | `iterate_result`：0=组遍历完；1=filldir 满（file_remote.c:964-966） |
| `ctx_merge.result` | **最后一次 actor 返回值**（:276） | 完成=true；满=false | 同左 |
| `ctx_merge.ctx.pos` | 设备内部遍历位置 | ext4 偏移 | `set_pos(dev,i,j)`（:960/:966） |

两个误判：

1. **`result=true`（=设备完成）被原代码当"缓冲满"提前停**（原 :391 `if (ctx_merge.result) goto done;`，注释语义与实现相反）→ **每次调用只遍历 1 个设备** → done 后 `ctx->pos` 停在设备 f_pos（LLONG_MAX）→ 与调用进入时相同 → **VFS `vfs_getdents` 的 `ctx->pos == last_pos` 检查判定位置无进展 → 提前 EOF** → 后续设备（如 dev 2）**永不遍历**（merge_view 根缺 dev 2 的直接机制）；
2. **`err>0`（=filldir 满）曾被当"完成"推进/EOF**（修复 4 期）→ 大目录第一批 585 项后 `err=1` → 单设备 `get_next=NULL` → `ctx->pos=-1` → EOF → **剩余 515 永久丢失**。

### 设计 vs 实现对照表

| 设计意图 | 实现缺陷 | 后果 |
| --- | --- | --- |
| ctx->pos 编码跨调用续读 | 设备 f_pos（LLONG_MAX）污染 ctx->pos（原 :396-397） | 解码 ULONG_MAX → 设备定位错乱 |
| ctx->pos 携带设备内位置 | OFFSET_BIT_NUM=8 容量不足；远程/本地语义不一 | 真续读不可行 → 只能从头+去重（259 改名重复风险） |
| result=false=满、true=完成 | 原代码把 true 当"满"提前停 | 每调用 1 设备 → VFS pos 无进展 EOF → 缺 dev 2 |
| err>0=满（远程） | 修复 4 期被当"完成"推进/EOF | 大目录分批丢剩余（585/1100） |

## 触发链路

```
挂载 + agent 连接建立（所有 peer 注册、online，device_view 直访正常）
  → 访问 merge_view 根 / 大目录（用户 ls / executor write_dir_info）
  → lookup_merge_root / lookup_merge_normal：comrade 建立（work 全 OK）
  → do_dir_open_merge：comrade 全部打开（fi 列表建立，open 期间固定）
  → hmdfs_iterate_merge 跨调用遍历：
      · 设备遍历完 → result=true → （原代码）提前停 → ctx->pos=LLONG_MAX
        → 下次调用解码 ULONG_MAX → 遍历错乱 → 后续设备缺失（三方互缺）
      · 分批（filldir 满）→ 设备内位置无可靠载体 → 从头+去重续读
        （259 改名重复风险）或 err>0 被当完成 → EOF → 大目录丢剩余
```

## 为什么 100% 是内核 Bug（排除用户态/连接层）

1. **连接层完全正常**：`merge_lookup_work` 全 OK、comrade 全 opened、`device_view` 直访正常、连接日志无 broken/reject；
2. **与访问时机无关**：等待 2-5 分钟（peer 全 online）后首访仍缺；`drop_caches` 后仍缺；
3. **与 agent 无关**：agent 连接全部成功，无旧版"双向连接竞争拒绝"问题；
4. **内核日志自证**：`get_next: in=-1 out=0`（错误返回）、`decoded=18446744073709551615`（ULONG_MAX）、`traverse` 序列缺 dev 2、大目录 585 后 EOF——全部是 `file_merge.c` 迭代器内部行为。

## 影响分析

- merge_view 聚合视图缺失"最后注册 peer"全部内容 → 各节点视图不一致（"三方互缺"）→ 分布式一致性校验（文件数/目录结构比对）失败；
- 大目录（1100 文件）分批 readdir 丢剩余 → 内容不可见/统计不准；
- CSAN 测试（exec 0/2=400、exec 1=1501）被误报为文件系统一致性 bug。

## 修复方案（内核侧补丁）

### 修复 1：`get_next_hmdfs_file_info` 无匹配时返回 NULL（保留）

```c
	mutex_unlock(&fi_head->comrade_list_lock);

	/* 遍历完链表未找到匹配（list_for_each_entry_safe 结束后 fi_iter
	 * 回落为链表头、fi_result == next(head) == 链表首节点）：
	 * 必须返回 NULL，否则首节点被误当作"下一个"返回，破坏遍历序列。 */
	if (&fi_iter->comrade_list == &fi_head->comrade_list)
		return NULL;
	return fi_result != fi_head ? fi_result : NULL;
```

### 最终修复：fi_head 显式状态机重构 `hmdfs_iterate_merge`

**动机**：`ctx->pos` 位域既被设备 f_pos 污染（缺陷 2），又无法承载设备内续读位置（8 位 offset 溢出），且"完成/满"判定被多次误用（缺陷 3）——跨调用状态需要一个**不受位域约束、open 期间存活**的载体，即 `fi_head`（file->private_data）。

**hmdfs.h（`struct hmdfs_file_info` +3 字段）**：

```c
struct hmdfs_file_info {
	union {
		struct { struct rb_root root; struct mutex comrade_list_lock; };
		struct { struct file *lower_file; int device_id; };
	};
	struct list_head comrade_list;
	/* merge readdir 跨调用遍历状态（fi_head 持有，open 期间存活） */
	int   cur_dev;    /* 当前遍历设备（get 用；取不到时回退链表第一个） */
	loff_t dev_pos;   /* 当前设备内续读位置（原样保存 ctx_merge.ctx.pos） */
	loff_t seq;       /* VFS 进度计数（缓冲满时 ctx->pos = set_pos(dev,0,seq)） */
};
```

**file_merge.c（`hmdfs_iterate_merge` 重写，:332-427）**：

```c
int hmdfs_iterate_merge(struct file *file, struct dir_context *ctx)
{
	int err = 0;
	struct hmdfs_file_info *fi_head = hmdfs_f(file);
	struct hmdfs_file_info *fi_iter = NULL;
	struct file *lower_file_iter = NULL;
	loff_t start_pos = ctx->pos;
	int device_id = -1;
	struct hmdfs_iterate_callback_merge ctx_merge = {
		.ctx.actor = hmdfs_actor_merge,
		.caller = ctx,
		.root = &fi_head->root,
		.dev_id = 0
	};

	/* pos = -1 indicates that all devices have been traversed
	 * or an error has occurred.
	 */
	if (ctx->pos == -1)
		return 0;

	/*
	 * 设备定位：从 fi_head 状态恢复，不再解析 ctx->pos 位域——
	 * 原实现把设备 f_pos（遍历完为 LLONG_MAX 等 EOF 标记）直接赋给
	 * ctx->pos，下次调用解码出 ULONG_MAX 导致遍历错乱。
	 */
	fi_iter = get_hmdfs_file_info(fi_head, fi_head->cur_dev);
	if (!fi_iter) {
		mutex_lock(&fi_head->comrade_list_lock);
		if (!list_empty(&fi_head->comrade_list))
			fi_iter = list_first_entry(&fi_head->comrade_list,
						   struct hmdfs_file_info,
						   comrade_list);
		mutex_unlock(&fi_head->comrade_list_lock);
	}
	if (!fi_iter)
		return 0;
	fi_head->cur_dev = fi_iter->device_id;

	while (fi_iter) {
		ctx_merge.dev_id = fi_iter->device_id;
		device_id = ctx_merge.dev_id;
		lower_file_iter = fi_iter->lower_file;
		ctx_merge.ctx.pos = fi_head->dev_pos;   /* 恢复设备内续读位置 */
		err = iterate_dir(lower_file_iter, &ctx_merge.ctx);
		fi_head->dev_pos = ctx_merge.ctx.pos;   /* 保存设备内位置（原样） */

		if (err < 0)
			goto done;                          /* 真实错误 */

		if (err > 0 || !ctx_merge.result) {
			/* 缓冲满（远程 iterate_result=1 或最后一次 emit=false）：
			 * 设备未遍历完，本次调用结束，下次从 fi_head->dev_pos
			 * 续读。ctx->pos 仅作 VFS 进度指示（seq 单调递增），
			 * 不再被解析。 */
			err = 0;                        /* 正数返回值规范化为 0 */
			ctx->pos = hmdfs_set_pos(device_id, 0, ++fi_head->seq);
			file->f_pos = ctx->pos;
			goto done;
		}

		/* 设备遍历完成（最后一次 emit 成功）：推进到下一个设备，
		 * 同一调用内继续遍历。 */
		fi_head->dev_pos = 0;
		fi_iter = get_next_hmdfs_file_info(fi_head, device_id);
		if (!fi_iter) {
			ctx->pos = -1;                      /* EOF */
			file->f_pos = ctx->pos;
			fi_head->cur_dev = -1;
			goto done;
		}
		fi_head->cur_dev = fi_iter->device_id;
		ctx->pos = hmdfs_set_pos(fi_iter->device_id, 0, 0);
		file->f_pos = ctx->pos;
	}
	ctx->pos = -1;
	file->f_pos = ctx->pos;
done:
	trace_hmdfs_iterate_merge(file->f_path.dentry, start_pos, ctx->pos, err);
	return err;
}
```

**设计要点**：

1. **设备定位不再依赖 ctx->pos 位域解码**：`get_hmdfs_file_info(fi_head->cur_dev)`，取不到回退链表第一个（覆盖单设备 peer merge 目录——大目录只在 peer 有、链表无 dev 0——首访场景）；
2. **真设备内续读**：`dev_pos` 原样保存/恢复 `ctx_merge.ctx.pos`（本地=ext4 偏移、远程=`set_pos` 编码，均无损）——根治位域容量问题，**不再"从头 + 去重"**（消除 259 改名重复风险）；
3. **"完成/满"语义统一**：`err<0`=真实错误；`err>0 || !result`=缓冲满（本次结束，下次续读）；`result=true`=设备完成（同调用内推进下一个设备，`get_next` 依赖修复 1 的 NULL 语义）；
4. **ctx->pos 纯 VFS 进度指示**：满=`set_pos(dev,0,seq++)`（单调，保证 `pos != last_pos` 不提前 EOF）、推进=`set_pos(next,0,0)`、EOF=`-1`；**`file->f_pos` 三处显式同步**（VFS 下次调用用 `file->f_pos` 初始化 ctx->pos）；
5. **`err>0` 规范化 0**（iterate_shared 惯例返回 0/负，避免未来 VFS 对正数敏感）。

**收敛性论证**：满 → `seq` 单调 → VFS 位置前进 → 用户态再调 → `cur_dev`+`dev_pos` 续读；EOF（-1）→ 入口直接返回 0 → `pos == last_pos` → 终止。无死循环、无重复输出（每设备只遍历一次，去重树仅兜底"最后条目恰好满"的边界）。

### 修复演进记录（尝试与教训，供论文/提交参考）

| 阶段 | 方案 | 结果 | 放弃/替代原因 |
| --- | --- | --- | --- |
| 修复 2 | 解码无效时"从头遍历"（去重树兜底） | 部分生效 | 依赖重遍历：条件边角（单设备无 dev 0 首访）、"从头"对文件触发 259 改名重复风险——被状态机替代 |
| 修复 3 | `while` 正常退出置 `ctx->pos=-1`（EOF） | 弃用 | `while` 正常退出仅发生在"缓冲满"路径（`result=false`）——置 -1 切断大目录续读（1501→~1201 预测） |
| 修复 4 | 设备完成（result=true / err>0）推进到下一设备 | 部分生效 | 对 merge_view 根有效（18 项 ✓）；但 `err>0`（filldir 满）被当"完成"推进/EOF → 大目录 585 丢 515——被状态机替代 |

## 验证方法

1. 应用修复（hmdfs.h + file_merge.c），重新编译 hmdfs 内核模块，部署到 3 个节点 VM；
2. 挂载 + 启动 agent，等待连接全部建立（peer online）；
3. `ls merge_view/` → 预期 18 项全输出（含 edd5a2a9 的 8 个专属根、hisrrykxiv 大目录根）；
4. 大目录完整路径 `ls | wc -l` → 预期 1100（分批续读无重复无丢失；节点 2 远程单设备场景为关键验证）；
5. `dmesg` 中 `traverse` 序列应为 `0 → 1 → 2`（merge_view 根，同调用内连续推进）；大目录 `cur_dev=2` 保持、分多批续读；
6. 重跑 CSAN 分布式一致性测试 → 各 executor 文件数统计一致（三方互缺消失）。

## 验证结果（截至 2026-08-24）

- **merge_view 根 18 项全输出 ✓**（修复 4 期验证：`traverse 0→1→2` 跨调用推进、EOF 干净）；
- **大目录 585/1100**（修复 4 期暴露：`err>0` 被当完成 → EOF 丢 515）；
- **状态机重写后：待部署验证**（预期 1100 无重复无丢失——验证通过后回填本段）。

## 遗留问题（后续调查项）

1. **同一 fd 的并发 readdir 不加锁（已知限制）**：`cur_dev`/`dev_pos`/`seq` 与去重树 `rb_root` 均无锁。POSIX 对同一 fd 并发 readdir 行为未定义；`iterate_shared` 语义由 FS 自担并发或接受未定义；64 位平台字段读写原子（无撕裂），真正风险（`rb_root` 并发 insert）无法用短临界区解决（`iterate_dir` 远程调用期间不可持锁）。如追求形式完整，可选方案：fi_head 加 `state_lock`（mutex），仅在"状态快照/更新"短临界区持锁（约 10 行），不跨 `iterate_dir`——收益有限，测试（单线程）不触发。
2. **设备内位置语义未统一**：本地（ext4 偏移）与远程（`set_pos` 编码）的 `ctx_merge.ctx.pos` 语义不同，状态机原样保存规避了编码问题，但若未来需要统一（如持久化/校验）建议给设备 readdir 定义统一的位置语义。

## 附录

### 相关文件速查

| 文件 | 行号 | 说明 |
| --- | --- | --- |
| `hmdfs/file_merge.c` | :289-307 | `get_next_hmdfs_file_info`（修复 1） |
| `hmdfs/file_merge.c` | :332-427 | `hmdfs_iterate_merge`（状态机重写） |
| `hmdfs/file_merge.c` | :209-287 | `hmdfs_actor_merge`（去重树、冲突改名 :259-266） |
| `hmdfs/file_merge.c` | :429-489 | `do_dir_open_merge`（fi 列表建立） |
| `hmdfs/hmdfs.h` | :259-274 | `struct hmdfs_file_info`（cur_dev/dev_pos/seq） |
| `hmdfs/file_remote.c` | :885-893 | `hmdfs_set_pos`（位域编码） |
| `hmdfs/file_remote.c` | :900-1006 | 远程设备 readdir（iterate_result/设备内位置） |
| `hmdfs/file_local.c` | :231-250 | 本地设备 readdir（ext4 透传） |
| `hmdfs/hmdfs_dentryfile.h` | :31-35 | 位域定义（OFFSET_BIT_NUM=8） |
| `src/executor/executor.cc` | :3141-3159 | executor 遍历 merge_view（触发者） |

### 大目录结构说明（测试环境）

- `initialdir/large_dir.info` 显示大目录路径为 **6 层嵌套**：`Eris_hisrrykxiv_451072199116.d/Eris_pifhmayyvd_504478729483.d/Eris_kpajhwdhpa_566550034554.d/Eris_quiwhjbffo_119403881206.d/Eris_bhuojnglng_234302675950.d/Eris_qfehribmzl_93443936692.d`（1100 文件，位于节点 1/edd5a2a9）；
- **中间 5 层是 `num_dirs`（--num-dirs 50）生成的普通目录**（全部在 `initialdir/<node_id>.dir` 中），由 `generate_test_files.py` 的 `force_deep` 逻辑（15% 目录强制从深度 ≥4 池选父，保证 5+ 层深度 bucket 覆盖）形成——**不是 tmpdir**；
- `initialdir/<node_id>.tmpdir` 恒为空（0 行）：`.tmpdir` 对应 `intermediate_dirs`（`create_intermediate_dirs` 收集"创建目标时自动产生的中间父目录"），但 `generate_dir_name`/`generate_file_name` 均为单层生成、父链总是已存在 → 收集恒空——**死逻辑**；
- 中间目录"未填充"（无文件）是随机性：`num_files=50` 用 `random.choice(current_dirs)` 随机放文件，深层目录可能未被选中——非刻意留空（`empty_dirs` 才是刻意不填充）。

### 诊断代码（临时，验证后应移除）

定位过程在 `hmdfs/file_merge.c` 与 `hmdfs/inode_merge.c` 中加入了诊断打印（`merge_lookup_root`/`merge_lookup_work`/`dir_open_merge`/`iterate_merge`/`get_next` 各环节），已随调试提交。状态机重写验证通过后，应删除全部诊断打印，仅保留功能性修复。

### 与测试环境的关系

- 节点编号：节点 0=74cb、节点 1=edd5a2a9、节点 2=ab44c6ad；agent 启动顺序 0→1→2，device_id 按 UPDATE_SOCKET 到达顺序分配（后注册的 peer device_id 更大）；
- "缺失最后注册 peer"的规律与连接注册顺序强相关：节点 2 的缺失对象（edd5a2a9）由其内核日志直接确认（`Got a newly allocated peer: device_id = 2` 后缺它）；**节点 0/1 的缺失对象（ab44c6ad）为按 agent 启动顺序（0→1→2）推断**（其 `Got a newly allocated peer` 日志不打印 cid，无法直接确认 dev1/dev2 对应关系）——但无论 dev2 具体是谁，"缺失最后注册者"的模式成立；且**与连接是否成功无关**（连接全部成功仍缺）。
