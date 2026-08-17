# HMDFS 内核锁/断言问题记录（lockdep 与 DEBUG_ATOMIC_SLEEP 暴露）

## 概述

syzkaller 测试内核全量开启 `CONFIG_PROVE_LOCKING`（lockdep）与 `CONFIG_DEBUG_ATOMIC_SLEEP`，暴露了 HMDFS 内核代码中若干锁使用规范与防御断言问题。这些问题在华为生产内核（上述选项默认关闭）上不产生任何输出，且多数实际行为安全——因此官方代码"能跑通"。但 fuzz 环境下，任何 WARNING 都会被 syzkaller 判定为 crash 并重启 VM，**阻塞 fuzz 正常运行**，必须处理。

本文档与 [`hmdfs-bug-held-lock-freed-connection-put.md`](./hmdfs-bug-held-lock-freed-connection-put.md)（connection_put UAF）并列，记录 4 个问题的根因、性质与处理状态。

**复现环境**：3 节点 QEMU（192.168.0.5/.6/.7）、Linux 6.6.0-dirty、openEuler 官方挂载配置（`local_dst=挂载点`，见附录）。

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

**状态：⏳ 保留实测**（**真实死锁风险**，留给 fuzz 触发）

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

### 性质

**真实死锁风险**（lockdep 静态依赖检测证实）：kworker 持目录读锁等待 sb_writers，同时 openat 持 sb_writers 等待目录写锁时死锁。需特定并发时序（rebuild 触发 truncate 且 openat 同目录创建）才实际发生，概率低。生产内核不检测 lockdep，可能偶发未复现。

### 处理预案

- **保留**：真实 bug，留给 fuzz 触发（lockdep WARNING 本身即可作为发现报告）
- 若实测**高频阻塞 fuzz**：方案 A——在 cfg 加 syzkaller `suppressions` 正则净化报告（不 reproduce、不污染 bug 列表；注意 suppressions 不影响 VM 重启）；方案 B——代码修复：`iterate_dir` 前 `mnt_want_write(file->f_path.mnt)` 使锁序与 openat 同向（mnt_want_write 可重入）

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
| `hmdfs/inode_merge.c` | :588-638 | `lookup_merge_root`（① 已异步化） |
| `hmdfs/hmdfs_merge_view.h` | :167-190 | `has_merge_lookup_work`/`is_merge_lookup_end`（② 已无锁化） |
| `hmdfs/hmdfs_server.c` | :1487-1544 | `hmdfs_server_rebuild_dents`（③ 锁序环位置） |
| `hmdfs/hmdfs_client.c` | :341-346 | `WARN_ON(ret == -ETIME)`（④ 位置） |
| `hmdfs/comm/socket_adapter.c` | :480-493 | 请求超时 → -ETIME |
| `hmdfs/main.c` | :689 | F_WRITEPAGE 超时 = TIMEOUT_COMMON = 4s |
