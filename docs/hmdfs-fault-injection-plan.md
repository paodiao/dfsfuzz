# HMDFS 故障注入设计基线（下大阶段）

> 状态：设计基线——待实施（当前阶段 bug 修复完成后启动）
> 来源：新突变方向评估——候选 2/4 合并为"故障注入"统一框架（2026-08 讨论）
> 关联：S11（net down 语义分析）、L21（空 prog/executor 约束）

## 1. delay 类型（推送慢——net down 覆盖不了的语义）

**动机**（与 net down 的语义差异）：
- net down/up = 连接级断开（iptables DROP）——阻断期消息**丢弃**/连接重置——且**无法控制阻断时长**（窗口 = prog 调用序列位置——粗粒度）
- delay = 消息**仍送达仅时序改变**（"推送慢"）——覆盖 stash 写回窗口的**推送竞态**（读验证与慢推送并发——未推送完时读端可见性）——net down 覆盖不了

**可行性（已核实）**：
- `syz_net_delay_add(cmd ptr[in, filename])` / `syz_net_delay_del()`（net_partition.txt:4-5）——**已定义但从未使用**
- 机制：tc netem（`tc qdisc add dev <iface> root netem delay Xms`）——**内核无需修改**——命令注入模式与 net_down（iptables）同模式
- 参数：delay 毫秒（50-500ms 量级——与写回窗口的时序竞态）

**实施点（下阶段）**：
- 生成器：stash 种子写后注入 add、恢复后 del（目标节点集合——复用 mutateStashTargetNodes 模式）
- 突变器：delay 并入故障窗口类
- checker（syscalls.py）：补 net_delay_add/del 模拟（当前只有 down/up）
- **待确认**：tc netem 需要接口名（net_down 用 IP 过滤不需要）——实施时从执行环境脚本确认

## 2. crash 类型（节点崩溃恢复）

**动机**：stash 线**唯一未覆盖的故障类型**（生成器只有 net down/up——HasNetFail；HasCrashFail 的 crash 注入仅用于动态分组/非 hmdfs 分支）——崩溃→恢复后 stash 暂存数据一致性（写节点本地暂存——崩溃后暂存保留/推送——读验证可见性）

**现状（已核实）**：`syz_failure_down/up`（fuzzer.go:422-425——DownId/UpId）——hmdfs stash 线未使用

**实施点（下阶段）**：
- stash 生成/突变的 crash 窗口变体（对齐 net 窗口结构——down 插写调用间、up 窗口末）
- 组合故障（delay+crash、down+crash 多故障序列）

## 附：四维框架（下阶段设计参考）

类型（net down/up / delay / crash / droppush-timeout）× 位置（failPos 窗口 / delay 窗口 / 崩溃点）× 目标（节点集合）× 参数（delay 毫秒等）
