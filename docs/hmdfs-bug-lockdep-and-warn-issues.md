# HMDFS 内核锁/断言问题记录（lockdep 与 DEBUG_ATOMIC_SLEEP 暴露）

## 概述

syzkaller 测试内核全量开启 `CONFIG_PROVE_LOCKING`（lockdep）与 `CONFIG_DEBUG_ATOMIC_SLEEP`，暴露了 HMDFS 内核代码中若干锁使用规范与防御断言问题。这些问题在华为生产内核（上述选项默认关闭）上不产生任何输出，且多数实际行为安全——因此官方代码"能跑通"。但 fuzz 环境下，任何 WARNING 都会被 syzkaller 判定为 crash 并重启 VM，**阻塞 fuzz 正常运行**，必须处理。

本文档与 [`hmdfs-bug-held-lock-freed-connection-put.md`](./hmdfs-bug-held-lock-freed-connection-put.md)（connection_put UAF）并列，记录 4 个问题的根因、性质与处理状态。

**复现环境**：3 节点 QEMU（192.168.0.5/.6/.7）、Linux 6.6.0-dirty、openEuler 官方挂载配置（`local_dst=挂载点`，见附录）。

处理策略见下方"备选方案"章节与各问题"处理预案"。

---

## 备选方案：禁用内核检测选项（对比路线，未采用）

**内容**：编译内核时关闭 `CONFIG_PROVE_LOCKING`（lockdep）+ `CONFIG_DEBUG_ATOMIC_SLEEP`，消除 ①②③ 的 WARNING——① 递归锁、③ 环形依赖由 lockdep 检测；② 由 DEBUG_ATOMIC_SLEEP 检测。

**关键结论（经实验验证）**：

- **④ 无法被该方案消除**：`WARN_ON(ret == -ETIME)`（hmdfs_client.c:345）是裸 `WARN_ON` 断言，与 lockdep/DEBUG_ATOMIC_SLEEP 无关——关选项后依旧触发 → 即使走禁用路线，④ 仍需代码处理（降级为 `hmdfs_err` 日志）
- **③ 真死锁风险失去检测**：锁序环（sb_writers ↔ i_mutex_dir）是真实死锁风险，关 lockdep 后不再报警；若实际发生将无声卡死，必须保留 `CONFIG_DETECT_HUNG_TASK` 兜底
- **fuzz 检测能力受损**：丢失全部锁序/原子违规检测——这正是 fuzz 的核心价值之一，会漏掉一大类 bug
- **注意**：syzkaller 检测到内核 WARNING 输出即判 crash 重启 VM（与 panic_on_warn 无关），因此关闭选项是唯一能阻止"WARNING → 重启"循环的配置手段；但代价如上

**结论**：该路线仅作为"快速验证 fuzz 全链路"的临时手段。本项目最终采用**代码修复路线**——问题 ①② 修代码 + **保留全部检测选项**（lockdep/KASAN/DEBUG_ATOMIC_SLEEP 全开），③④ 作为真实 bug 保留给 fuzz 触发。

---

## 问题 ①：`lookup_merge_root` 的 kern_path 重入（recursive locking）

**状态：✅ 已修复**（`hmdfs/inode_merge.c`，待编译验证）

### 现象

`ls /mnt/hmdfs/100/non_account/merge_view/` 或 `touch merge_view/xxx` 即触发：

```
WARNING: possible recursive locking detected
touch/9530 is trying to acquire lock:
ffff888020a783a8 (&type->i_mutex_dir_key#7){.+.+}-{4:4}, at: walk_component+0x190/0x250
but task is already holding lock:
ffff888020a783a8 (&type->i_mutex_dir_key#7){.+.+}-{4:4}, at: walk_component+0x190/0x250
#0: walk_component+0x190  (VFS lookup 持有)
#1: hmdfs_lookup_merge+0x8fe  (kern_path 内部再次获取同一把锁)
```

### 根因

- `lookup_merge_root`（inode_merge.c:640-673）同步调用 `hmdfs_get_path_in_sb` → `kern_path(real_dst + "/device_view")`
- 挂载配置 `local_dst=挂载点` → `real_dst = /mnt/hmdfs/100/non_account`（hmdfs 挂载点自身）
- `kern_path` 遍历绝对路径时**走进 hmdfs 自身** → `walk_component` 对 merge_view 与 device_view **共享的同一目录 inode** 再次 `down_read(i_mutex_dir)`——而 VFS 外层 lookup 已持有该读锁 → 同任务递归获取同一 rwsem 读锁

### 性质

**rwsem 读锁递归获取是合法的**（reader count 递增，配对 up_read 即可，不死锁）。lockdep 的 "possible recursive locking" 是保守告警（无法证明读锁递归安全）。生产内核 `CONFIG_PROVE_LOCKING=n` 不检测 → 官方环境无感。

### 改动点（已实施）

`lookup_merge_root` **异步化**，与兄弟函数 `lookup_merge_normal`（inode_merge.c:480-545，华为已有模式）同构：

- 移除同步 `hmdfs_get_path_in_sb`/`kern_path(real_dst/device_view)` 调用
- 改为持 `mdi->work_lock` + `sbi->connections.node_lock` 提交 `merge_lookup_async`（"device_view/local" + 各 peer 的 "device_view/<cid>"）
- `wait_event(mdi->wait_queue, is_merge_lookup_end(mdi))` 等待 workqueue 完成
- `kern_path` 移入 workqueue 上下文（`merge_lookup_comrade`），**不持有 VFS lookup 锁** → 无重入
- 同步路径使用的 `do_lookup_merge_root` 删除；`lock_root_inode_shared`/`restore_root_inode_sem` 保留（cloud 路径仍用）

---

## 问题 ②：wait_event condition 内 mutex_lock（DEBUG_ATOMIC_SLEEP）

**状态：✅ 已修复**（`hmdfs/hmdfs_merge_view.h`，待编译验证）

### 现象

同一 lookup 路径触发：

```
do not call blocking ops when !TASK_RUNNING; state=2 set at prepare_to_wait_event+0x54
WARNING: CPU: 1 PID: 9530 at kernel/sched/core.c:10110 __might_sleep
__mutex_lock → hmdfs_lookup_merge+0xa22 → lookup_open
```

### 根因

`is_merge_lookup_end`（hmdfs_merge_view.h:178-187）与 `has_merge_lookup_work`（:167-176）作为 `wait_event` 的 condition 被求值时，**内部调用 `mutex_lock(&mdi->work_lock)`**——此时任务处于 `TASK_UNINTERRUPTIBLE`（prepare_to_wait_event 设置），在非 RUNNING 状态执行阻塞操作，违反 `DEBUG_ATOMIC_SLEEP` 规范。共 8 处 wait_event 受影响（inode_merge.c:531/1002/1036/1136/1185/1266/1272、file_merge.c:395、dentry.c:274）。

### 性质

违反规范但**实际行为正确**：`mutex_lock` 自身管理任务调度状态（slowpath 重新设置状态再 schedule），即使从 state=2 调用也能正确睡眠/唤醒。生产内核 `CONFIG_DEBUG_ATOMIC_SLEEP=n` 不检测。

### 改动点（已实施）

两个 condition 函数**无锁化**：

- `has_merge_lookup_work`：`READ_ONCE(mdi->work_count) != 0`
- `is_merge_lookup_end`：`READ_ONCE(mdi->work_count) == 0 || list_empty(&mdi->comrade_list)`

安全性：`work_count` 的 ++/-- 均在 `work_lock` 保护下（int 原子读）；`list_empty` 只读 head 指针；wait_event 先 prepare 再查 condition → wakeup 不丢失；瞬时不一致只导致多等一轮。

---

## 问题 ③：`hmdfs_server_rebuild_dents` 锁序环（circular locking）

**状态：✅ 已修复（最终方案，`hmdfs/hmdfs_dentryfile.c`，待编译验证）**

### 现象

```
WARNING: possible circular locking dependency detected
kworker/u6:2/68 (dfs_req1_2) is trying to acquire lock:
sb_writers#4, at: vfs_truncate → create_dentry → hmdfs_filldir_real → iterate_dir
but task is already holding lock:
&type->i_mutex_dir_key#3, at: iterate_dir
```

### 根因

服务端 readdir 重建路径（workqueue dfs_req1_* 上下文）：

```
hmdfs_server_readdir → hmdfs_server_rebuild_dents → iterate_dir（持 i_mutex_dir 读锁）
  → hmdfs_filldir_real → create_dentry → cache_file_truncate → vfs_truncate → mnt_want_write(sb_writers)
```

锁序：`i_mutex_dir → sb_writers`；而 openat 路径为 `sb_writers → i_mutex_dir`——**锁序相反 → 环形依赖**。

### 实测结论（18:40 运行）

**高频触发，阻塞 fuzz**：syz-manager 每 ~30 秒报 `vm-0: crash: possible deadlock in vfs_truncate`（18:40:48/18:41:28/18:41:57），fuzz 程序首轮 readdir 重建即触发，`executed 0`——**不修 fuzz 无法运行**。原"低概率"假设不成立。

### 修复演进（勿再走回头路）

1. **第一轮（已回退）**：`rebuild_dents` 在 `iterate_dir` 前 `mnt_want_write`——环消除但引入**同线程递归获取 sb_writers** → lockdep `possible recursive locking`（实测 #3 内核 ~30s/次仍崩）。
2. **第二轮（已回退）**：`hmdfs_fill_super` 对 `SB_FREEZE_WRITE` 层 `lockdep_set_novalidate_class`——**无效**：`lockdep_init_map_type` 拒绝覆盖已在 `alloc_super`（`percpu_init_rwsem`）初始化过的 key（只打印 "key is not as annotated" 警告）。实测 #4 内核 recursive 仍每 ~30s 触发。
3. **最终方案（当前）**：**根因修复**——`cache_file_truncate` 不再用 `vfs_truncate()`（路径级 API，强制 `mnt_want_write`/sb_writers），改用 **`do_truncate()`**（`ftruncate(2)` 语义）：

```c
long cache_file_truncate(struct hmdfs_sb_info *sbi, struct file *filp,
			 loff_t length)
{
	const struct cred *old_cred = hmdfs_override_creds(sbi->system_cred);
	long ret = do_truncate(file_mnt_idmap(filp), filp->f_path.dentry,
				length, 0, filp);

	hmdfs_revert_creds(old_cred);
	return ret;
}
```

- `dentry_file` 由调用者 **O_RDWR 打开**（`create_local_dentry_file_cache`）→ 写权限已在 open 时检查 → 与 `ftruncate(2)` 一致**不需要** sb_writers（openEuler 签名：`do_truncate(struct mnt_idmap *, struct dentry *, loff_t, unsigned int, struct file *)`，fs/open.c:40）
- 修改后 iterate 路径**完全不获取 sb_writers**：`iterate_dir（目录 i_rwsem 读）→ do_truncate → notify_change → inode_lock（文件 i_rwsem 写）`——不同 inode 动态 key、与 openat 同向 → **circular 与 recursive 同时从根源消失，无需任何 lockdep 豁免**
- 外层 `mnt_want_write` 补丁（hmdfs_server.c）与 novalidate 补丁（main.c）**全部回退**（git 参考：`44e776e` 与 `903c33a`/`f3e2088`/`5b60d3e`）

### 残余风险（记录，未处理）

- filldir 的 **DT_LNK 分支**（`hmdfs_lookup_symlink` → `hmdfs_open_link` → `filp_open(O_RDWR)`）仍在 iterate 内取 sb_writers——理论上存在同款环，但 fuzz 目录无 symlink、从未实测触发；若触发，将 `hmdfs_open_link` 改 `O_RDONLY`（读 symlink 内容不需写权限）
- freeze 语义差异（ftruncate 语义，测试环境无 freeze，无影响）

---

## 问题 ④：`WARN_ON(ret == -ETIME)`（断网写回超时）

**状态：⏳ 保留实测**（**"半死连接"行为缺陷**，留给 fuzz 触发）

### 现象

fuzz 程序 `syz_failure_net_down`（真实执行 iptables DROP）期间有同步写回时触发：

```
hmdfs_writepage_cb → redo 写回仍失败 → WARN_ON(ret == -ETIME)  (hmdfs_client.c:345)
```

### 根因

断网故障注入链路：

```
iptables DROP（静默丢包，TCP 连接不快速断开——无 RST/FIN）
  → 写回请求 wait_event_interruptible_timeout 超时（F_WRITEPAGE=4s，main.c:689）
  → ret = -ETIME（socket_adapter.c:493）
  → hmdfs_client_redo_writepage 重试（同步写 WB_SYNC_ALL 时）
  → 仍超时 → WARN_ON(ret == -ETIME)
```

### 性质

**华为代码未预料的"半死连接"行为缺陷**：华为的假设是"断网 → TCP 连接快速断开 → 节点 offline → 写回转 stash（离线暂存）"；而 iptables DROP 制造的是**连接保持 ESTABLISHED 但请求永远无响应**的状态——写回既不成功也不转 stash，反复超时。`-ETIME` 本身是代码设计内的合法错误（`hmdfs_client_writepage_err` 完整处理），`WARN_ON` 是防御性过度断言。

### 处理预案

- **保留实测频率**：真实行为缺陷，与 ③ 同为 fuzz 目标
- 若实测**高频阻塞 fuzz**：将 `WARN_ON(ret == -ETIME)` 降级为 `hmdfs_err`（dmesg 保留 -ETIME 事件信息，不再触发 crash 判定），并把"半死连接"缺陷记录为独立发现

---

## 问题 ⑤：merge_view lookup 竞态/UAF（ENOENT + merge_lookup mutex WARN）

**状态：✅ 已修复**（`hmdfs/inode_merge.c` + `hmdfs/hmdfs_merge_view.h`，待编译验证）

### 现象（2026-08-17 20:36 运行，两个独立表现）

```
A. SYZFAIL: executor 0: opendir /mnt/hmdfs/100/non_account/merge_view
   failed No such file or directory (errno 2)          ← merge_view 根 lookup 失败
B. DEBUG_LOCKS_WARN_ON(__owner_task(owner) != get_current())
   WARNING: CPU: 1 PID: 5153 at kernel/locking/mutex.c:918
   __mutex_unlock_slowpath ... merge_lookup_work_func+0x1ef
```

### 根因（hmdfs 原生缺陷，被 lookup_merge_root 异步化放大）

`merge_lookup_async`（inode_merge.c）两处设计缺陷：

1. **`++work_count` 在 `schedule_work` 之后**：work 可能在另一 CPU 立即运行并 `--work_count`（work_lock 内），先于调用者的 `++` → 计数提前归零/变负 → `is_merge_lookup_end`（`work_count==0 || list_empty`）立即为真 → `wait_event` 提前返回 → comrade_list 尚未 link → `lookup_merge_root` 返回 **-ENOENT** → merge_view 根目录创建失败 → 所有 merge_view 操作 ENOENT → SYZFAIL（现象 A）。
2. **work 无 dentry/mdi 引用保护**：work 只持 `&mdi->wait_queue` 指针；mdi 挂于 `dentry->d_fsdata`，dentry 释放时 `d_release_merge` 直接 `kmem_cache_free(mdi)`（dentry.c:338）。fuzz 高频 create/rmdir/unlink + 断网 → dentry 快速消亡 → work 仍运行在已释放 mdi 上 → `work_lock` 状态错乱 → `mutex_unlock` owner 不符 → WARN + 崩溃（现象 B）。

### 改动点（已实施）

- `merge_lookup_async`：**`++work_count` 移到 `schedule_work` 之前**（++/-- 均在 work_lock 下互斥，配对成立）→ wait_event 不再提前返回 → ENOENT 消失
- `merge_lookup_work` 增加 `struct dentry *dentry` 字段；async 内 `dget(dentry)`，work 结束 `dput(dentry)` → **mdi 生命周期与 work 绑定** → UAF 消除
- `merge_lookup_work_func`：**`mutex_unlock(work_lock)` 移到 `wake_up_all` 之前** → 唤醒者重入（新 lookup/create）时锁已释放，减少竞争窗口
- 4 个调用点（lookup_merge_normal ×2、lookup_merge_root ×2）传目标 dentry

---

## 问题 ⑥：`held lock freed`（connection_put 持锁释放 conn）

**状态：✅ 已修复**（`hmdfs/comm/connection.c`，待编译验证）

### 现象（2026-08-17 20:38 运行）

```
WARNING: held lock freed!
dfs_rcv1_1_5/9599 is freeing memory ffff88801d2f4400-ffff88801d2f45ff,
with a lock still held there!
&tcp_conn->ref_lock at refcount_dec_and_mutex_lock+0x51
connection_put → __kmem_cache_free → tcp_recv_thread+0x1c9
```

### 根因

`connection_put`（connection.c:820）用 `kref_put_mutex(&conn->ref_cnt, connection_release, &conn->ref_lock)`——**持 ref_lock 调用 release** 且 `kref_put_mutex` 自身**从不解锁**（release 负责解锁）。而 `connection_release`（:744-786）直接 `kfree(conn)` → **释放包含仍被持有 mutex（ref_lock）的内存** → `debug_check_no_locks_freed` WARN。锁本身随内存释放**永久泄漏**。

触发路径：`tcp_recv_thread` 退出（transport.c:642）——连接 RST 后（`tcp recv error -104`；由 `syz_failure_net_down` iptables DROP / agent 重连制造）。此前 fuzz 每 ~30s 崩于 ③ 掩盖了此路径。

### 改动点（已实施）

`connection_release` 在 `kfree(conn)` 前 **`mutex_unlock(&conn->ref_lock)`**（kref_put_mutex 标准配套）。已验证安全性：`conn->close`(=tcp_stop_connect) 为空函数、list 操作用独立锁 `node->conn_impl_list_lock`、`kfree(tcp)` 独立分配。

---

## 附录：挂载配置分析

### `local_dst` 语义（代码证据）

挂载时 `hmdfs_update_dst`（main.c:801-829）派生：

- `real_dst = local_dst`（挂载选项原值）
- `local_dst = real_dst + "/device_view/local/"`

用途：`real_dst` 是 device_view 目录树的物理根（`kern_path(real_dst/device_view)` 定位、`hmdfs_root_mkdir` 创建设备目录）；派生 `local_dst` 是本设备 local 视图文件的真实存储路径（`filp_open(local_dst + path)` 等 10+ 处）。

### openEuler 官方示例（本项目采用）

```
mount -t hmdfs -o merge,local_dst="/mnt/hmdfs/100/non_account" \
  "/data/service/el2/100/non_account" "/mnt/hmdfs/100/non_account"
```

`local_dst=挂载点` 是 openEuler 文档的既定用法，**本项目不改配置**。此配置使 `kern_path(real_dst/device_view)` 重入 hmdfs 自身（问题 ① 的触发条件），但这是内核代码需适配的既定用法，而非配置错误。

### 为何 `local_dst=SOURCE_DIR` 不可行

实验验证：改为 `local_dst=/data/service/el2/100/non_account` 后，`kern_path(real_dst/device_view)` 报 `-ENOENT`（`hmdfs_get_path_in_sb() can't get .../device_view -2`）——device_view 实体目录不存在于该下层路径（华为语义中由系统初始化创建），lookup 直接失败。

---

## 相关文件速查

| 文件 | 行号 | 说明 |
| --- | --- | --- |
| `hmdfs/inode_merge.c` | :606-654 | `lookup_merge_root`（① 已异步化） |
| `hmdfs/hmdfs_merge_view.h` | :167-190 | `has_merge_lookup_work`/`is_merge_lookup_end`（② 已无锁化） |
| `hmdfs/hmdfs_server.c` | :1487-1540 | `hmdfs_server_rebuild_dents`（③ 已回退裸 iterate_dir） |
| `hmdfs/hmdfs_dentryfile.c` | :1597-1614 | `cache_file_truncate`（③ 最终修复：do_truncate） |
| `hmdfs/hmdfs_dentryfile.c` | :902-906 | ③ 调用点（传 filp） |
| `hmdfs/inode_merge.c` | :444-481 | `merge_lookup_async`（⑤ ++/schedule 顺序 + dget） |
| `hmdfs/inode_merge.c` | :395-442 | `merge_lookup_work_func`（⑤ wake 后置 + dput） |
| `hmdfs/hmdfs_merge_view.h` | :24-39 | `struct merge_lookup_work`（⑤ 加 dentry 字段） |
| `hmdfs/comm/connection.c` | :744-790 | `connection_release`（⑥ unlock ref_lock） |
| `hmdfs/hmdfs_client.c` | :341-346 | `WARN_ON(ret == -ETIME)`（④ 位置） |
| `hmdfs/comm/socket_adapter.c` | :480-493 | 请求超时 → -ETIME |
| `hmdfs/main.c` | :689 | F_WRITEPAGE 超时 = TIMEOUT_COMMON = 4s |
