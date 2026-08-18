# HMDFS 内核 Bug：WARNING: held lock freed in refcount_dec_and_mutex_lock（connection_put UAF）

## 元信息

| 项目 | 内容 |
| --- | --- |
| 发现日期 | 2026-08-17 |
| 复现环境 | 3 节点 QEMU（192.168.0.5/.6/.7）、Linux 6.6.0-dirty、Monarch (syzkaller) fuzzer |
| Bug 标题 | `WARNING: held lock freed in refcount_dec_and_mutex_lock` |
| 位置 | `hmdfs/comm/connection.c:778`（`connection_release` 内 `kfree(conn)`） |
| 性质 | 内核引用计数/锁生命周期误用导致的 use-after-free（UAF） |

## 摘要

HMDFS 内核 comm 层的 `connection_put()` 误用 `kref_put_mutex()`：当连接引用计数归零时，内核会在持有 `conn->ref_lock` 的情况下调用释放回调 `connection_release()`，而该回调在末尾 `kfree(conn)` 释放了**锁对象本身所在的内存**。释放之后 `kref_put_mutex` 还会对已释放的内存执行 `mutex_unlock()`，构成双重释放级别的 UAF，可破坏 slab 内存并导致 VM 卡死、连接失联、fuzzer 反馈归零等一系列连锁故障。

## 复现现象

连接断开时（`tcp_read_head_from_socket() recv error 0`），接收线程退出路径触发（简版日志摘录）：

```
[18.445491][ T9606]  hmdfs: tcp_read_head_from_socket() tcp recv error 0
[18.447183][ T9606] WARNING: held lock freed!
[18.447800][ T9606] dfs_rcv1_1_5/9606 is freeing memory ffff88810a698800-ffff88810a6989ff, with a lock still held there!
[18.448239][ T9606] ffff88810a698880 (&tcp_conn->ref_lock){+.+.}-{4:4}, at: refcount_dec_and_mutex_lock+0x51/0xd0
[18.450687][ T9606] stack backtrace:
[18.451797][ T9606]  dump_stack_lvl+0xd9/0x1b0
[18.452113][ T9606]  debug_check_no_locks_freed+0x130/0x170
[18.452616][ T9606]  slab_free_freelist_hook.constprop.0+0xdd/0x180
[18.452811][ T9606]  ? connection_put+0x3a/0x50
[18.453014][ T9606]  __kmem_cache_free+0xa2/0x2b0
[18.453203][ T9606]  connection_put+0x3a/0x50
[18.453404][ T9606]  tcp_recv_thread+0x1c9/0xe90
[18.454251][ T9606]  kthread+0x120/0x160
[18.454820][ T9606]  ret_from_fork_asm+0x1b/0x30
```

syz-manager 控制台反复报告：

```
vm-0: crash: WARNING: held lock freed in refcount_dec_and_mutex_lock
vm-0: crash: lost connection to test machine
```

两个崩溃交替出现，说明 UAF 引发的内核异常直接导致 VM 失联。

## 根因分析

### 代码位置

| 文件 | 行号 | 内容 |
| --- | --- | --- |
| `hmdfs/comm/connection.h` | :54-57 | `struct connection` **内嵌** `struct mutex ref_lock` |
| `hmdfs/comm/connection.c` | :809-814 | `connection_put()` 调用 `kref_put_mutex(&conn->ref_cnt, connection_release, &conn->ref_lock)` |
| `hmdfs/comm/connection.c` | :744-779 | `connection_release()` 末尾 `kfree(tcp); kfree(conn);` |
| `hmdfs/comm/transport.c` | :642 | `tcp_recv_thread` 退出前 `connection_put(tcp->connect)`（触发点） |
| `hmdfs/comm/transport.c` | :980-982 | 接收线程命名 `dfs_rcv%u_%llu_%d`（与崩溃线程名吻合） |

### kref_put_mutex 语义（关键）

Linux 内核 `kref_put_mutex()` 的标准行为（openEuler 6.6 与上游一致，include/linux/kref.h）：

```c
static inline int kref_put_mutex(struct kref *kref,
				 void (*release)(struct kref *kref),
				 struct mutex *lock)
{
	if (refcount_dec_and_mutex_lock(&kref->refcount, lock)) {
		release(kref);        // ← 持锁调用 release 回调
		return 1;
	}
	return 0;
}
```

即：refcount 减到 0 时**先获取 `ref_lock`** → 调用 `release` 回调 → **`kref_put_mutex` 本身不解锁**——**解锁由 release 回调负责**（回调返回后内核不再触碰锁）。

### 缺陷定位

`connection_release()`（connection.c:744-779）末尾：

```c
	kfree(tcp);   // :777
	kfree(conn);  // :778  ← 释放了内嵌 ref_lock 的 connection 对象
```

在 `ref_lock` 仍被当前线程持有的情况下 `kfree(conn)`：

1. lockdep 的 `debug_check_no_locks_freed` 在 `__kmem_cache_free` 时检测到"释放的内存中仍持有锁" → 触发 WARNING（崩溃栈中 `connection_put+0x3a` 正是 `refcount_dec_and_mutex_lock` 获取锁的代码点）；
2. 锁对象（`conn->ref_lock`）随内存一起被释放且**从未解锁**——锁的状态永远无法正确归还，`connection` 对象释放路径存在锁生命周期缺陷。

> 注：`kref_put_mutex` 在 release 回调返回后**不会**再对锁执行任何操作（openEuler 6.6 与上游一致，见上），因此本 bug 的表现形式为"持锁释放 + 锁泄漏"的 WARNING，**不包含**"已释放内存上再次 unlock"的二次 UAF 步骤。

### 对照组（排除其他可能）

`peer_put()`（connection.c:816-821）同样使用 `kref_put_mutex`，但锁是 `&peer->sbi->connections.node_lock`——**位于 sbi 对象上，不在被释放的 peer 对象内**，因此 peer 释放从不触发 held lock freed。只有 connection 把锁内嵌在自身结构里，恰好是出问题的那一个，这排除了"环境/配置因素"的解释。

## 触发链路

```
连接断开（对端关闭/网络异常）
  → tcp_recv_thread 的 tcp_receive_from_sock 返回 -ESHUTDOWN（transport.c:625-628）
  → recv 线程 break 退出循环（transport.c:633）
  → 退出前 connection_put(tcp->connect)（transport.c:642）
  → 这是建立线程时 connection_get（transport.c:977）配对的最后一个引用
  → kref_put_mutex：持 ref_lock 调 connection_release
  → kfree(conn) 释放含锁内存 → held lock freed WARNING + UAF
```

## 为什么 100% 是内核 Bug（而非 agent/用户态问题）

1. **lockdep 是内核运行时自证**：检测逻辑在 `__kmem_cache_free` 内执行，证明"释放时锁被持有"这一事实；用户态进程在物理上不可能持有内核 mutex。
2. **线程名吻合**：崩溃线程 `dfs_rcv1_1_5` 与 transport.c:980-982 `kthread_create(..., "dfs_rcv%u_%llu_%d", ...)` 命名完全一致，是内核接收线程，不是任何用户态线程。
3. **agent 只是扳机**：agent 的连接检查/断开行为会触发连接释放，从而让该 bug 暴露得更频繁；但即使 agent 行为完全正确，分布式系统必然存在连接断开，一旦发生释放路径，bug 必然触发。agent 侧的修复（禁用 connect_checker + GET_SESSION stale 重连）只消除了"人为频繁误断"这条触发链，未改变内核 bug 本身。

## 影响分析

- UAF 直接破坏 slab 内存（`mutex_unlock` 写入已释放对象），可能损坏其他并发分配的对象
- VM 内核异常 → `lost connection to test machine`、`executor not serving`、执行极慢
- fuzzer 覆盖率/信号/DAG 反馈全部归零（executor 卡死的次生结果）
- 在 fuzz 场景下表现为"崩溃-失联"交替循环，无法积累有效反馈

## 修复方案（内核侧补丁）

修改 `hmdfs/comm/connection.c`。两种等价方案，**本项目采用方案 A（改动最小）**：

### 方案 A（已实施，commit `903c33a`）：release 回调内解锁后再释放

`connection_release` 末尾在 `kfree(conn)` 前解锁（kref_put_mutex 的标准配套——解锁由 release 负责）：

```c
	kfree(tcp);
	/* kref_put_mutex 持锁进入 release 且自身不解锁：先解锁再释放 conn */
	mutex_unlock(&conn->ref_lock);
	kfree(conn);
}
```

要点：
- 锁在释放前解锁 → `debug_check_no_locks_freed` 不再报 held lock freed
- release 返回后 `kref_put_mutex` 不再触碰 conn（openEuler 实现无 release 后 unlock）→ 无二次访问
- `connection_release` 中其余清理（sockfd_put、kmem_cache_destroy、list_del 等）保持不变

### 方案 B（备选）：`connection_put` 改为手动持锁释放

```c
void connection_put(struct connection *conn)
{
	struct mutex *lock = &conn->ref_lock;

	mutex_lock(lock);
	if (kref_put(&conn->ref_cnt, connection_release)) {
		mutex_unlock(lock);
		kfree(conn);
		return;
	}
	mutex_unlock(lock);
}
```

配套：`connection_release` 删除 `kfree(conn)`（移入 `connection_put` 解锁后）。

要点：
- `kfree(conn)` 移到 `ref_lock` 解锁之后，彻底消除"持锁释放"
- 语义上 `ref_lock` 仅保护引用计数判定，不再保护 release 清理
- `hmdfs_peer_release` 无需修改（其锁 `node_lock` 不在被释放对象内）

## 验证方法

1. 将补丁应用到内核源码的 `hmdfs/comm/connection.c`（用户内核版本可能行号不同，代码一致）
2. 重新编译内核并部署到 3 个节点 VM
3. 重跑 fuzzer 验证：
   - 不再出现 `WARNING: held lock freed in refcount_dec_and_mutex_lock`
   - 连接断开/重连时 VM 不再卡死/失联
   - executor 响应恢复，cliCover / DAG 反馈开始增长

## 附录

### 相关文件速查

| 文件 | 行号 | 说明 |
| --- | --- | --- |
| `hmdfs/comm/connection.h` | :54-57 | `struct connection`（内嵌 `ref_cnt` / `ref_lock`） |
| `hmdfs/comm/connection.c` | :744-779 | `connection_release()`（缺陷点 :778） |
| `hmdfs/comm/connection.c` | :809-814 | `connection_put()`（误用 kref_put_mutex） |
| `hmdfs/comm/connection.c` | :816-821 | `peer_put()`（对照组，无此问题） |
| `hmdfs/comm/transport.c` | :593-644 | `tcp_recv_thread()`（触发点 :642） |
| `hmdfs/comm/transport.c` | :977-982 | `connection_get` + 线程命名 |

### 与 agent 修复的关联

本项目同时在用户态侧修复了 agent 的连接检查逻辑（`hmdfs_agent.c`，已提交）：

- **禁用 `connect_checker_thread_func`**：不再对已移交内核的 fd 做 TCP_INFO 轮询误判离线、不再主动发 `CMD_OFF_LINE`
- **`HandleAllNotify`/`handle_hmdfs_notify` 的 NOTIFY_GET_SESSION 分支**：验证 fd 真实存活才 skip，stale fd 清理后重连，保证断线恢复路径通畅

用户态修复消除"人为误杀"触发链，但本内核 bug 是根因，仍需上述内核补丁根治。
