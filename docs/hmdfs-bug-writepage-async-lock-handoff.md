# HMDFS 内核 Bug：wpage_sem 跨 task 异步锁移交导致的三类 lockdep 崩溃

## 元信息

| 项目 | 内容 |
|---|---|
| 发现时间 | 2026-08-30 ~ 09-01（36h hmdfsfuzz 运行） |
| Crash bucket | `crashes/32f14526`（MAX_LOCK_DEPTH）、`crashes/8de54a80`（workqueue leaked lock）、`crashes/8cbb3ef1`（bad unlock balance） |
| 源码位置 | `hmdfs/file_remote.c`（hmdfs_writepage_remote）、`hmdfs/hmdfs_client.c`（writepage_cb/done/err）、`hmdfs/client_writeback.c`（writeback_inode_handler） |
| 状态 | **根因确认，决策：不修，记录** |
| 严重性 | 中高（死锁/锁状态破坏风险；lockdep 报告为主，非内存安全） |

## 概述

hmdfs 客户端远端写回路径（writepage）采用**异步锁移交**设计：`hmdfs_writepage_remote` 在成功发出远端写请求后**不释放 `wpage_sem` 读锁**，而是把锁持有状态记录进 `param->rsem_held`，由 RPC 响应上下文的完成回调释放。该设计在语义上合法（rwsem reader 无 owner，允许跨 task 释放），但与 **lockdep 的 per-task held-locks 跟踪模型不兼容**，在 fuzzer 的 fault injection（网络延迟/断连）与批量脏页写回放大下，触发三类不同的 lockdep 崩溃。

## 锁的生命周期（源码还原）

```c
// file_remote.c — hmdfs_writepage_remote（写回一个脏页）
static int hmdfs_writepage_remote(struct page *page, struct writeback_control *wbc)
{
        bool rsem_held = false;
        bool sync = wbc->sync_mode == WB_SYNC_ALL;

        if (!allow_cur_thread_wpage(info, &rsem_held, sync))
                goto out_unlock;               // sync=false: down_read_trylock(&wpage_sem)
        ...
        param->rsem_held = rsem_held;          // ★ 锁"移交"给 param
        ret = hmdfs_remote_do_writepage(info->conn, param);   // 异步 RPC 发出
        if (likely(!ret))
                return 0;                      // ★ 成功路径：不释放，task 带锁返回
out_free:
        kfree(param);
out_endwb:
        end_page_writeback(page);
        if (rsem_held)
                up_read(&info->wpage_sem);     // 仅失败路径就地释放
        ...
}
```

```c
// hmdfs_client.c — RPC 响应上下文（另一个 task）
void hmdfs_client_writepage_done(struct hmdfs_inode_info *info,
                                 struct hmdfs_writepage_context *ctx)
{
        bool unlock = ctx->rsem_held;
        SetPageUptodate(page);
        end_page_writeback(page);
        if (unlock)
                up_read(&info->wpage_sem);     // ★ task B 释放 task A 获取的锁
        unlock_page(page);
}
```

设计意图：`wpage_sem`（per-inode，`main.c:318` 初始化）用于让 fsync/release 路径的 `down_write`（file_remote.c:456、:592）等待所有在途远端写页排空，实现 close-to-open 一致性。写回侧以**读锁**标记"该 inode 有在途写页"，完成回调释放。

## 三个 Crash 的机制

### 1. BUG: MAX_LOCK_DEPTH too low!（crashes/32f14526）

- 现场：workqueue `dfs_ino_wb1` → `hmdfs_writeback_inode_handler` 批量处理该 inode 的**全部脏页**（38 个，实测锁图），逐页调用 `hmdfs_writepage_remote`。
- 每页 `down_read_trylock(&wpage_sem)` 都成功（读锁并行，38 次 reader 计数叠加），38 个 param 全部在途（远端响应未回——fuzzer 网络延迟/fault injection 放大）。
- **lockdep 对同一 task 未释放的重复获取逐层计数**：38 层 wpage_sem + RPC 发送路径的网络栈 8 层（rcu_read_lock、slock-AF_INET、rcu_read_lock_bh、qdisc_tx_busylock 等，见锁图 #40-#47）+ 其他 2 层 = 48 层，`MAX_LOCK_DEPTH(48)` 爆。
- 锁图证据：#2-#39 全部是**同一地址** `ffff888112eb6a40 (&gi->wpage_sem)`，获取点均为 `hmdfs_writepage_remote+0x25e/0xcd0`。

### 2. BUG: workqueue leaked lock or atomic in wb_workfn（crashes/8de54a80）

- 内核 writeback 线程（`wb_workfn`，balance_dirty_pages → writepage 路径，WB_SYNC_NONE）同样进入 `hmdfs_writepage_remote`。
- 锁移交后 task 返回时**持有未释放的读锁**，触发 workqueue 的 lockdep exit 检查（work 函数返回时不得持锁）。
- 报告：`BUG: workqueue leaked lock or atomic: kworker/u6:1/0x00000000/67, last function: wb_workfn`。

### 3. WARNING: bad unlock balance in hmdfs_client_writepage_done（crashes/8cbb3ef1）

- done 回调在 **RPC 响应上下文（task B）** 执行 `up_read`，释放的是 **task A（writeback 线程）down 的锁**。
- lockdep 的 held-locks 跟踪是 per-task 的：task B 的锁栈上找不到该锁 → 误报 "bad unlock balance"。
- 栈证据：`hmdfs_client_writepage_done+0x5f/0x70 ← hmdfs_writepage_cb+0x45e/0x640`。

## 本质

**rwsem 语义上 reader 锁允许跨 task 释放（无 owner 概念），但 lockdep 无法跟踪跨 task 的锁移交**。三个报告分别是同一设计缺陷在三个 lockdep 检查维度上的表现：重复获取计数（depth）、work 退出持锁（leak）、跨 task 释放（balance）。内核功能本身（页写回、fsync 排空）行为正确，lockdep 观测层崩溃。

## 触发放大器

1. **fuzzer fault injection**：网络延迟（syz_net_delay_add）与断连（net_down）拉长在途 param 的存活窗口，使"同 task 未释放的重复获取"层数轻易突破 48。
2. **批量写回无上限**：`hmdfs_writeback_inode_handler` 的逐页循环对在途 param 数量没有限制，脏页数即并发锁数。
3. **大批量 Write 链 + fsync** 的种子（fileops 增厚后更常见）制造大脏页集合。

## 详细分析补充与修正

以下为本轮深度分析（源码审查 + report 数据核实）对初版结论的修正与补充。

### 1. MAX_LOCK_DEPTH 现场的层数修正：46 → 38

实测锁图（report0）中 `&gi->wpage_sem` 的重复层为 **#2-#39 共 38 层**（同一地址 `ffff888112eb6a40`，获取点均为 `hmdfs_writepage_remote+0x25e/0xcd0`），加网络发送栈 8 层（#40-#47：rcu_read_lock、slock-AF_INET、rcu_read_lock_bh、qdisc_tx_busylock）与其他 2 层（wq_completion、work_completion）= 48 爆。

### 2. bad unlock balance 的锁名级实锤（8cbb3ef1）

balance 报告全文关键段：

```
kworker/0:4/5156 is trying to release lock (&gi->wpage_sem) at:
[<ffffffff819e0f9f>] hmdfs_client_writepage_done+0x5f/0x70
but there are no more locks to release!
2 locks held by kworker/0:4/5156:
 #0: ((wq_completion)dfs_async1_1){....}-{0:0}, at: process_one_work+0x285/0x8a0
```

- 释放方线程为 **dfs_async1_1**（hmdfs 自建的 RPC 异步响应处理队列）——即 cb（`hmdfs_client_writepage_done`）执行上下文；
- 该线程仅持有自己的 wq_completion 锁，`wpage_sem` 的 reader 计数在写回线程（task A）侧——lockdep per-task 跟踪无法配对，误报 "bad unlock balance"；
- **机制从推断升级为锁名级实锤**。

### 3. workqueue leaked lock 的锁身份为机制推断（8de54a80）

leak 报告仅给出 `kworker/u6:1/0x00000000/67, last function: wb_workfn`，**无锁名无栈**。`wpage_sem` 持锁返回为唯一合理解释（writepage_remote 成功路径锁移交），但报告本身不直接指认——标注为**机制推断**（置信度高：`aops.writepage = hmdfs_writepage_remote` 直挂 VFS writeback，见下条源码定位）。

### 4. 源码定位补充

| 环节 | 位置 | 关键事实 |
|---|---|---|
| 自建 inode 写回队列 | `client_writeback.c:40` `hmdfs_writeback_inode_handler` | `while (!list_empty)` 循环内 `write_inode_now(inode, 0)`（WB_SYNC_NONE）**一次写完全部脏页，无在途上限**——38 页即单 inode 38 个脏页（大文件 write 链） |
| 内核 writeback 入口 | `file_remote.c:881` `aops.writepage = hmdfs_writepage_remote` | VFS writeback（`wb_workfn` → balance_dirty_pages）直接进入，WB_SYNC_NONE 分支 `down_read_trylock`——**wb_workfn 持锁返回的 leak 路径闭合** |
| 异步分发 | `hmdfs_client.c:393-397`（`req.private = param`、`hmdfs_send_async_request`） | 响应到达后由 dfs_async 线程执行 `hmdfs_writepage_cb` → done/err 的 `up_read`——跨 task 释放的代码路径 |
| sync 分支豁免 | `file_remote.c:699-706`（`allow_cur_thread_wpage`） | WB_SYNC_ALL 时不持锁（`rsem_held=false` 直接放行）——sync 路径的 `filemap_write_and_wait` 由 fsync 处的 `down_write`（file_remote.c:456/:592）互斥，深度可控 |

### 5. 新增附加风险：redo/迟到响应导致 done 双跑（潜在 UAF + 二次 up_read）

`hmdfs_writepage_cb`（hmdfs_client.c:323-358）内存在两条 ctx 复用路径：

```c
if (hmdfs_client_redo_writepage(peer->sbi, ctx, ret)) {   // cb 内重写判定
        ret = hmdfs_remote_do_writepage(peer, ctx);        // ctx 复用，重新发送
        if (!ret)
                goto cleanup_req;                          // 只 kfree(req->data)，ctx 存活
}
hmdfs_client_writepage_err(peer, info, ctx, ret);          // 重发失败才 err（up_read + 后续 kfree）
```

时序竞争：cb 内 redo 重发成功后 ctx 仍在途；若**首个响应的迟到副本**（或超时重发后旧请求的响应）在 ctx 被 kfree 之前再次到达分发器，会**二次进入 cb**——第一次已 done（`up_read` + `kfree(ctx)`），第二次访问已释放的 ctx → **use-after-free**，且若再次走 done 分支则**第二次 `up_read`**（reader 计数失衡，即另一形态的 unlock balance）。该风险与已归档的 `hmdfs-bug-held-lock-freed-connection-put.md`（connection_put UAF）可能共享根因域（响应/重试时序），列为后续观察项。MAX_LOCK_DEPTH 现场的 38 层 depth 爆仍是主观测结论，本条为代码审查发现的附加风险，未在本批 crash 中直接捕获。

### 6. 时序重建不可行

report0/printk 输出未带时间戳（printk 时间戳未启用），无法从日志重建 38 页 down_read 的时间跨度与在途时长。深度/锁图/调用点证据已足以支撑根因结论。

## 修复选项（决策：不修，仅记录）

| 方案 | 改动 | 效果 | 评估 |
|---|---|---|---|
| 1. 批量上限 | `hmdfs_writeback_inode_handler` 循环内在途 param 达阈值（如 8）时等待排空 | 消 depth 爆；leak 缓解 | 治标；锁移交设计仍在 |
| 2. 根治 | 放弃跨 task 锁移交：writepage_remote 成功路径立即 `up_read`；fsync 互斥改原子在途计数（`atomic64 pending_wpages`，fsync 等 count==0） | 三报全消；rwsem 只做短临界区 | ~40 行（file_remote.c 3 处 + client.c 2 处 + fsync 2 处 + inode_info 计数器）；需验证 fsync close-to-open 语义等价 |
| 3. lockdep 标注 | 移交/释放处手工 `lockdep` 注释配平 | 掩盖 | hacky，不推荐 |

**不修的理由**：
1. 内核功能语义正确（lockdep 观测层问题），非必现数据破坏路径；
2. 方案 2 需内核构建环境验证 close-to-open 一致性等价，改动面触及一致性核心语义，与当前"用 fuzzer 收集一致性 bug"的目标冲突（修锁设计会改变一致性表现，混淆归档）；
3. 触发依赖 fuzzer 的 fault injection 放大，生产语义下 depth 爆需要单 inode 38+ 在途写页——真实负载中窗口更小。

**后续观察项**：若部署后该类 crash 频率显著上升（影响 fuzz 有效时间），优先按方案 1 止血。

## 关联文档

- `hmdfs-bug-lockdep-and-warn-issues.md`：同 writepage 路径的另一组问题（redo 失败 WARN_ON(-ETIME)、F_WRITEPAGE 4s 超时、断连转 stash），根因不同（超时/重试语义 vs 锁移交）。
- `hmdfs-bug-fpdzkkvcoj-collect-enoent.md`：写回/一致性异常的下游表现之一（收集阶段 ENOENT）。
