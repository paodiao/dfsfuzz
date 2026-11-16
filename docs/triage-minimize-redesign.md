# Triage / Minimize 重设计

> 状态：Go 侧已实现，待运行验证；2026-11-15 补充 GlusterFS v1 profile/gate 说明
> 日期：2026-11-08
> 范围：coverage/DAG triage、WorkTriage、minimize、dedup、corpus 合并
> 相关：`docs/executor-file-tree-restore.md`、`docs/glusterfs-adaptation-plan.md` §10.5

## 0. 摘要

- minimize 注释停用。
- coverage triage 保留 Monarch 的 signal gate，但只执行一次，同时完成 signal 验证和 Cover 收集。
- DAG 反馈是 seed 级全局反馈，不按节点归属。
- DAG-only 跳过 triage，直接本地入 corpus、以 `Call="dag"` 发给 manager、然后 `WorkSmash`。
- DAG 仍发 manager；`NewInput()` 的 genuine gate 只对 DAG 放行，`saturatedCalls["dag"]` 保留默认阈值、不豁免。
- DAG-only 不额外跑 coverage-only，空 Cover 接受。
- DAG+coverage 时固定先发 DAG RPC，再发 coverage per-target RPC；同一 hash 的 per-call 统计归到 `"dag"`。
- DAG direct 先用初始 `info.Extra.Signal` 添加本地 corpus；coverage 后到只合并 signal。空 signal 时接受 priority=1。
- `WorkSmash` 由第一次 `addInputToCorpus` 返回 `added=true` 的路径触发，每个 seed 最多一个。
- `minimizeCorpus()` 正常最小化后，未入选的 DAG 按 hash 采样保留 50%，入选的 DAG 保留。
- `cltTick` / `freqCov` 保留设计，不删除；只有 client WorkTriage 每个 seed 最多发送一次。
- client 内部的所有新 call/extra 合并为一个 target list；server fallback 内部的所有 extra 合并为另一个 target list；两边不合并。
- coverage 多 target 采用 per-target RPC，每个有效 target 单独发给 manager。
- corpus 去重由 `addInputToCorpus` 负责，hash 已存在时仍合并 signal。
- `triageInput` 的 early dedup 和 minimize 后的 stage 2 dedup 都删除；它们是我们后加的，不是 Monarch 原版。
- `data` / `sig` 的一次性序列化和 hash 计算保留，只删除 dedup 检查。

## 1. WorkTriage 的原始语义

- `WorkTriage` 是 per-target coverage 验证任务，不是 seed 执行任务。
- 一个 seed 可以有多个 WorkTriage，对应不同 call/extra。
- `triageInput()` 才负责执行 seed、验证 signal、收集 Cover、加入 corpus。
- 原版 triage：
  - 3 次 signalRuns
  - `prog.Minimize`
  - 3 次 coverage 收集
- 原版没有 `triageDag`，DAG 是后加的。
- 原版没有 `triageInput` 开头的 program-level hash dedup。

原版代码位置：

- `Monarch-master-back/src/syz-fuzzer/proc.go:186-324`
- `Monarch-master-back/src/syz-fuzzer/workqueue.go:38-54`
- `Monarch-master-back/src/pkg/ipc/ipcconfig/ipcconfig.go:48-55`

## 2. flag 事实

- `FlagSignal`：config 层，控制是否收集 signal。
- `FlagCollectCover`：exec 层，控制是否输出原始 `Cover`。
- `FlagDedupCover`：只在已收集 Cover 时去重，不代表要收集。
- 默认 `execOpts` 只有 `FlagDedupCover`，通常有 signal、没有完整 Cover。
- `execOptsCover` 才加 `FlagCollectCover`。
- `execOptsLight` 来自 `execOptsCover`，triage 阶段有 Cover。
- `execOptsLightMinimize` 来自 `execOptsNoCollide`，只靠 signal。

关键代码：

- `src/pkg/ipc/ipcconfig/ipcconfig.go:48-55`
- `src/pkg/ipc/ipc.go:61-68`
- `src/executor/executor.cc:1850-1851`
- `src/executor/executor.cc:2050-2056`

executor 行为：

```c
* signal_count_pos = nsig;
if (!flag_collect_cover)
    return;
```

所以默认 `execOpts` 下通常是：

```text
Signal: 有
Cover:  无
```

## 3. 改造前代码偏差

- `triageInput` 开头的 program-level hash early dedup 是后续 growth tuning 加的，不是 Monarch 原版。
- 加入 commit：
  - `43d1c5e`（2026-09-08）：program-level triage dedup
  - `8ee0ef2`（2026-10-09）：minimize 后 stage 2 dedup
- 它会吞掉同一 seed 不同 call 的 coverage WorkTriage。
- 它也会和 DAG 先入 corpus 冲突。
- `triageDag` 当前只跳过 signal 验证和 minimize，没有跳过最后的 3 次 coverage 循环。
- 当前 DAG 分支仍然是：
  `enqueueDagTriage -> triageInput -> 3 次 coverage -> add corpus -> WorkSmash`
- `DagSignal` 在 `executeRaw` 中挂到第一个 client info 上，只是实现细节，不代表 DAG 属于该节点。

当前代码位置：

- early dedup：`src/syz-fuzzer/proc.go:486-507`
- signal 循环：`src/syz-fuzzer/proc.go:515-541`
- minimize：`src/syz-fuzzer/proc.go:548-577`
- coverage 循环：`src/syz-fuzzer/proc.go:615-644`
- DAG 入队：`src/syz-fuzzer/proc.go:1365-1374`
- DAG signal 挂载：`src/syz-fuzzer/proc.go:1572-1585`

## 4. 已确定方向

### 4.1 minimize

- 删除 `prog.Minimize` 整段：target list 改造后旧的单 target 字段已不存在，旧实现由 git history 保留。
- 删除 `minimizeAttempts`。
- 删除 `execOptsLightMinimize` 的字段、初始化和赋值：
  - `src/syz-fuzzer/proc.go:48`
  - `src/syz-fuzzer/proc.go:82-83`
  - `src/syz-fuzzer/proc.go:339`
- 保留 `ProgMinimized` 条件和 target list 中的对应判断。
- 保留 `totalLen == 0` 兜底。

### 4.2 coverage-only

- 保留 `corpusSignalDiff` 预过滤；但如果 seed hash 已经在 corpusHashes 中（例如 DAG direct 或另一侧先加入），跳过一次 global diff，直接用 target raw signal 继续验证，避免把后到的 server/coverage target 误判为已覆盖。
- 只执行一次 `executeRaw(execOptsLight, StatTriage)`。
- 同一次执行完成：
  - `reexecutionSuccess`
  - target signal 交集
  - server/client signal 合并
  - CliCover/SrvCover 收集
- 不再 3+3 次。
- client 内部多个 call/extra 合并成一个 target list，一次验证全部。
- server fallback 内部多个 extra 合并成另一个 target list；server fallback 的触发条件不变。

### 4.3 DAG

- DAG 是全局时间线分析结果，seed 级，无节点归属。
- 当前 DAG 反馈只挂在第一个非 nil 的 client info 上：`infos[ServNum:]` 找到后立即 `break`，所以一次执行最多产生一个 DAG feedback。
- 因此不需要按 client 聚合成多个 DAG `WorkTriage` 或 RPC；DAG 的 `subNum` 就是第一个非 nil client。
- DAG 新颖性不需要 coverage signal 再验证。
- DAG-only 跳过 `WorkTriage`，直接：
  `sendInputToManager(Call="dag") -> addInputToCorpus -> WorkSmash`
- DAG-only 不额外执行 coverage-only triage：
  - 初始执行有 `info.Extra.Signal` 就随本地 corpus 添加和 RPC 携带；
  - Cover 为空可以接受；
  - manager 侧 `"dag"` 统计的 cover 可能为空，这是预期行为。
- DAG+coverage：
  - 固定先发 DAG 的 `Call="dag"` RPC；
  - 再处理 coverage 的 target list，每个有效 target 发一个对应 call 的 RPC；
  - DAG direct 必须在 `execute()` 内同步发送，不能延后成一个 `WorkTriage`；
  - 本地 corpus 由 DAG direct 用初始执行的 `info.Extra.Signal` 先 `addInputToCorpus`；
  - coverage WorkTriage 后到，只合并 signal/cover，不重复新增 corpus；
  - `WorkSmash` 由第一次 `addInputToCorpus` 返回 `added=true` 的路径触发，每个 seed 最多一个。
- DAG+coverage 的本地 corpus 细节：
  - 初始 `info.Extra.Signal` 非空时正常计算 corpus priority；
  - 初始 signal 为空时，接受 priority=1；
  - coverage 后续合并新 signal 时不重新计算 priority，这是原版 `addInputToCorpus` 行为。
- DAG+coverage 的已知代价：
  - manager 对相同 `Prog` hash 保留第一次成功输入的 `Call`；
  - DAG 先进入后，这个 hash 的 `Call` 是 `"dag"`；
  - 后续 coverage RPC 仍会合并 signal/cover，并分别经过自己的 `saturatedCalls[target call]`；
  - 但 `collectSyscallInfo()` 的 per-call 统计中，这个 seed 只归到 `"dag"`，不会同时归到 coverage target 的 syscall。
- DAG direct 的 `MaxDagCorpus`：
  - 在 direct 入口按一次 execution 检查一次；
  - 检查必须在发送 DAG RPC、本地 add 和入队 `WorkSmash` 之前；
  - 超过上限则不发 DAG RPC、不加本地 corpus、不 smash。
- DAG 统计语义：
  - `dagPairCount`：新 DAG pair bit 数，在 novelty 判定时更新，不受 `MaxDagCorpus` 影响。
  - `dagCorpusCount`：统计“通过 `MaxDagCorpus` 检查、进入 DAG corpus 路径”的 event 数，也就是当前代码中 DAG triage 实际发生的次数。
    - direct 路径在通过 cap 检查后、发送 RPC 和 add 之前增加一次。
    - 即使后续 `addInputToCorpus` 因 hash 已存在返回 false，这次 event 仍然计入 `dagCorpusCount`。
  - `dagCorpusEntries`：只在 `addInputToCorpus` 实际新增后增加，用于 `MaxDagCorpus` 的累计上限。
  - `dagEnqueues` 是当前 `execute` 的诊断计数；direct 后不再有 DAG `WorkTriage` enqueue，改名为 `dagDirect`，不参与 cap。
  - `dagSchedCount`、`dagHbPairCount`、`dagCcPairCount`、depth 统计不受 DAG direct 影响。
- manager 侧 `"dag"` 是独立统计组，`saturatedCalls["dag"]` 与真实 syscall、`".extra"` 分开，便于后续单独调整。
- `saturatedCalls["dag"]` 当前保留默认阈值，不豁免；DAG-only 空 Cover 可能使其更早饱和，后续实验需要时再单独调整。
- 单 host 单 fuzzer 下 manager 不再承担跨 fuzzer 分发，但仍用于 corpus 持久化、统计、最小化和重启恢复，所以 DAG 仍然发送到 manager。
- manager 侧最终处理：
  - `NewInput()` 只对 `Call == "dag"` 跳过 genuine gate，`checkProgram`、`newInput` 和 cover 合并不变。
  - `saturatedCalls` 不做 DAG 例外，`"dag"` 仍按原逻辑饱和。
  - `minimizeCorpus()` 正常执行 `signal.Minimize()`；入选的 DAG 保留，未入选的 DAG 按 hash 采样保留 50%，之后 `corpusDB` 按新 corpus 删除。

### 4.4 dedup

这里有三个不同的东西，不能混为一谈：

1. **`triageInput` 开头的 program-level early dedup**
   - 我们加的，commit `43d1c5e`。
   - 删除 dedup 检查；保留同一代码块里的 `data` / `dataForHash` / `sig` 生成。
   - 当前代码：`src/syz-fuzzer/proc.go:501-507`。

2. **`prog.Minimize` 后的 stage 2 dedup**
   - 我们加的，commit `8ee0ef2`。
   - 它依赖 minimize 改写程序后重新序列化，minimize 停用后整个 stage 2 删除。
   - 包括 `preSig`、重新生成 `data`/`dataForHash`、重新计算 `sig`、dedup 检查。
   - 当前代码：`src/syz-fuzzer/proc.go:595-610`。
   - 末尾的 `preSig` 更新也删除：`src/syz-fuzzer/proc.go:666-672`。

3. **Monarch 原版的 `addInputToCorpus` hash 去重**
   - 原版代码：`Monarch-master-bak/src/syz-fuzzer/fuzzer.go:664-686`。
   - 当前代码：`src/syz-fuzzer/fuzzer.go:812-837`。
   - 保留；这是最终 corpus 去重和 signal 合并入口。

`addInputToCorpus` 现有行为：

```go
if _, ok := fuzzer.corpusHashes[sig]; !ok {
    fuzzer.corpus = append(fuzzer.corpus, ps)
    fuzzer.corpusHashes[sig] = struct{}{}
    added = true
}
if !cliSign.Empty() || !srvSign.Empty() {
    fuzzer.corpusCliSignal.Merge(cliSign)
    fuzzer.corpusSrvSignal.Merge(srvSign)
}
```

manager 对相同 hash 也会合并 `CliSignal` / `SrvSignal` / `CliCover` / `SrvCover`。

### 4.5 WorkTriage target list

`WorkTriage` 从“一个 target 一个 item”改成“同一 side 的 target list”：

```go
type TriageTarget struct {
    call   int
    subNum int
    info   ipc.CallInfo
}

type WorkTriage struct {
    ps           []*prog.Prog
    targets      []TriageTarget
    flags        ProgTypes
    triageClient bool
}
```

`triageClient` 属于整个 `WorkTriage`，同一个 target list 里的 target 必须全部来自同一侧。

`execute()` 收集 target：

- `clientTargets`：所有 client 的新 call/extra；
- `serverTargets`：仅在原 server fallback 条件成立时，收集所有 server 的 `info.Extra`；
- `clientTargets` 非空时，enqueue 一个 `WorkTriage{triageClient: true}`；
- `serverTargets` 非空时，enqueue 一个 `WorkTriage{triageClient: false}`；
- 两边绝不放进同一个 target list；
- 如果两边都非空，则有两个 `WorkTriage`，分别执行；
- target 在同一侧内按 `(subNum, call)` 去重和排序。

`triageInput()` 的处理顺序：

```text
1. 序列化 seed 并计算一次 hash；只读取 corpusHashes 判断 seed 是否已存在，不提前返回
2. 每个 target 用原始 info 做 signal 预过滤；seed 已存在时跳过 global `corpusSignalDiff`，使用 target raw signal
3. 只执行一次 executeRaw(execOptsLight, StatTriage)
4. 每个 target 做 reexecutionSuccess + signal 交集
5. 失败的 target 丢弃；全部失败则返回
6. 每个有效 target 单独发 RPC
7. 当前 side 所有有效 target 的 signal 合并后，addInputToCorpus 一次
8. 如果 corpus 实际新增，入队一个 WorkSmash
```

其他约束：

- 不再执行 3 次 signal + `prog.Minimize` + 3 次 coverage；
- client 和 server 各自一次执行，互不合并；
- `WorkSmash` 每个 seed 最多一个，`call = -1`，`subNum` 取该 side 第一个有效 target；
- 纯 DAG 不走 target list，仍走独立的 DAG 路径；
- `ProgMinimized` / stash 判断保留，但 minimize 本身停用。

`cltTick` 的当前事实：

```go
freqCov := int32(10)
// 每 3 分钟置 0
// 收到 cltTick 时 +1
```

但 `freqCov` 目前没有任何读取点，所以 `cltTick` 是遗留信号，没有实际 fuzzing 影响。target list 流程中约定：

- 只有 client WorkTriage 发送 `cltTick`；
- 至少有一个有效 target 且 `target.call != -1` 时，每个 seed 最多发送一次；
- server WorkTriage 不发送 `cltTick`；
- 不为同一个 seed 的多个 target 重复发送。

当前决策：**保留 `cltTick` / `freqCov`，不删除，也不改成新机制**。`freqCov` 仍然没有读取点，所以它不产生实际行为影响；保留只是为了减少这次改动的范围。后续如果确认无用，再单独清理。

### 4.6 triageInput 删除清单（改造前）

以下为改造前代码位置，当前实现已完成 target list 改造。

#### 删除

- `src/syz-fuzzer/proc.go` 中的 `prog.Minimize` 整段；target list 改造后旧字段已移除，旧实现由 git history 保留。
- `src/syz-fuzzer/proc.go:501-507`
  - early dedup 的 `corpusHashes` 检查和早退。
  - 该代码块前面的 `data` / `dataForHash` / `sig` 生成必须保留。
- `src/syz-fuzzer/proc.go:595-610`
  - stage 2 的 `preSig`、重新序列化、重新计算 `sig` 和 dedup。
- `src/syz-fuzzer/proc.go:666-672`
  - `preSig` 对应的 `corpusHashes` 更新。
- `src/syz-fuzzer/proc.go:48`、`:82-83`、`:339`
  - `execOptsLightMinimize` 字段、初始化和赋值。
- `src/syz-fuzzer/proc.go:509-512`
  - `signalRuns`、`minimizeAttempts` 常量。
- `src/syz-fuzzer/proc.go:514`
  - `notexecuted`。
- `src/syz-fuzzer/proc.go:515-541`
  - 旧的 3 次 signal 循环，由一次执行 + target list 替换。
- `src/syz-fuzzer/proc.go:615-644`
  - 旧的 3 次 coverage 循环，由一次执行 + per-target 结果替换。

#### 保留

- `data` / `dataForHash` / `sig` 的一次性生成。
- `totalLen == 0` 兜底。
- `addInputToCorpus` 的最终 hash 去重和 signal 合并。
- `WorkSmash` 入队。
- `cltTick`，但按 client seed 最多发送一次。

## 5. call 字段的下游用途

### 5.1 RPCInput.Call

- `call != -1`：syscall 名。
- `call == -1`：`".extra"`。
- DAG：当前代码只设置了 `logCallName = "dag"`，实际 `RPCInput.Call` 仍为 `".extra"`；新设计决定改为 `RPCInput.Call = "dag"`。
- 实现时建议在 `rpctype` 定义 `DagCall = "dag"`，fuzzer 和 manager 共用，避免字符串漂移。
- manager 用它：
  - `mgr.saturatedCalls[inp.Call]` 做 per-call 饱和判断
  - `collectSyscallInfo()` 按 call 分组 corpus
  - `a.Call == "failure"` 合并 `corpusFailSignal`
  - UI / 日志 / 按 call 查看 corpus

相关代码：

- `src/pkg/rpctype/rpctype.go:14-27`
- `src/syz-manager/manager.go:1481-1487`
- `src/syz-manager/manager.go:1537-1553`
- `src/syz-manager/rpc.go:309-407`

### 5.2 WorkSmash.call/subNum

- 仅在 `comparisonTracingEnabled && call != -1` 时用于 `executeHintSeed`。
- 不影响 smash 的主体变异。
- `call == -1` 时跳过 hint seed。

`comparisonTracingEnabled` 的来源：

```go
r.CheckResult.Features[host.FeatureComparisons].Enabled
```

它不是普通配置默认项，依赖目标内核是否启用 `CONFIG_KCOV_ENABLE_COMPARISONS`，默认不保证开启。

多 target 聚合后的最小适配：

```text
WorkSmash.call = -1
WorkSmash.subNum = 第一个有效 target 的 subNum（或 0）
```

这样 comparison tracing 关闭时完全无影响；开启时跳过 hint seed，不额外执行。未来若需要 hint，可以再携带多个 target，或选择第一个有效 target。

相关代码：

- `src/syz-fuzzer/proc.go:687-689`
- `src/syz-fuzzer/proc.go:726-770`

### 5.3 多 target 聚合后的 RPCInput.Call

`RPCInput.Call` 不是执行字段，而是 manager 侧的 corpus 分类和准入字段。一次 triage 验证多个 target 后，没有唯一的 call 能代表整个 seed。

可选方案：

1. 取第一个有效 target 的 call。
   - 改动最小，只有一个 RPC。
   - manager 会按这个 call 的饱和状态决定是否接收整个 seed，统计会偏斜。

2. 使用 `"multi"` / `"seed"` 作为 call。
   - 不会误用具体 call 的饱和判断。
   - 会绕过 manager 的 per-call 饱和控制，所有聚合输入归到一个统计组。

3. 一次执行后，每个有效 target 发一个 RPC。
   - 同一个 `Prog`，每个 RPC 携带该 target 的 `Call` 和对应 signal/cover。
   - manager 对相同 hash 的多个 RPC 会合并 signal/cover，但每个 RPC 仍按自己的 `Call` 做饱和判断和统计。
   - 最贴近原版 per-target 语义，代价是每个 seed 多几个 RPC。

决定采用方案 3：

```text
一次 triage 执行
-> 得到所有 valid target
-> 对每个 valid target 分别 sendInputToManager
   Call = target call
   Signal/Cover = target 对应结果
-> 本地 corpus 只 addInputToCorpus 一次，合并 aggregate signal
```

DAG 单独使用：

```text
Call = "dag"
```

DAG 和 coverage 同时存在时：

- DAG 发一个 `"dag"` RPC；
- coverage 每个有效 target 发一个对应 call 的 RPC；
- manager 按相同 `Prog` hash 合并。

这样不需要给 manager 增加 `"multi"` 特殊逻辑，也保留原版 `RPCInput.Call` 的含义。

### 5.4 DAG 发送 manager 的注意点

- `NewInput()` 的 genuine gate 只对 `Call == "dag"` 放行：DAG RPC 即使没有新的 `CliSignal` / `SrvSignal`，也不会被早退；`checkProgram()` 仍然执行。
- DAG 的 `genuine` 为 false 时，后面的全局 signal 合并和跨 fuzzer 分发仍会跳过；单 fuzzer 下可以接受。
- manager 合并相同 `Prog` hash 时保留旧 `Call`，不会因为后来的 DAG RPC 改成 `"dag"`。如果希望 DAG 统计不被 coverage 抢走，DAG RPC 应尽量先发。
- `collectSyscallInfo()` 会把 `"dag"` 当成一个非 syscall 分组，UI 里 syscall ID 为空，这是预期行为。
- `saturatedCalls` 不豁免 DAG；`saturatedCalls["dag"]` 仍可阻止后续 DAG 输入，后续需要时再单独调整阈值或加例外。

### 5.5 DAG 在 minimizeCorpus 的部分保留

不修改 `signal.Minimize()` 本身：

```text
1. 所有 corpus 输入（包括 DAG）正常进入 signal.Minimize
2. 被 signal.Minimize 选中的输入保留
3. 对未被选中的 DAG，按 hash 采样保留 50%
4. 更新 mgr.corpus
5. 更新 mgr.lastMinCorpus
6. 继续饱和检查和 corpusDB 删除
```

采样条件：

```go
for sig, inp := range mgr.corpus {
    if inp.Call != "dag" {
        continue
    }
    if _, kept := newCorpus[sig]; kept {
        continue
    }
    if sig[0] < '8' {
        newCorpus[sig] = inp
    }
}
```

`sig` 是 SHA1 的十六进制字符串，首字符在 `0-7` 时保留，约为 50%。这样：

- 有 signal 且被正常最小化选中的 DAG 不丢；
- 未被选中的 DAG 每轮只保留约一半；
- 不会永久保留全部 DAG；
- 非 DAG 输入仍完全走原来的 `signal.Minimize()`；
- `corpusDB.Delete` 在采样后执行，保留的 DAG 不删，淘汰的 DAG 正常删除。

### 5.6 per-target RPC 发送

每个有效 target 发送一个 `RPCInput`：

```go
RPCInput{
    Call:      target syscall name 或 ".extra",
    Prog:      data,
    CliSignal: 该 target 的 client signal,
    SrvSignal: 该 target 的 server signal,
    CliCover:  该 target 的 client cover,
    SrvCover:  该 target 的 server cover,
    Timings:   prog.ProgTimings(item.ps),
}
```

规则：

- 同一 seed 的所有 target RPC 共享 `Prog` 和 `Timings`；
- `Call` 必须是该 target 自己的 syscall，不能统一用第一个 target；
- 不为每条 RPC 复制 aggregate signal/cover，避免 manager 的 per-call 统计被重复放大；
- manager 对相同 `Prog` hash 的多个 RPC 会合并为一个 corpus 项，但每个 RPC 仍分别经过自己的 `Call` 饱和判断；
- 只有通过 reexecution 和 signal 交集的 target 才发 RPC；
- 本地 `addInputToCorpus` 使用所有有效 target 的 aggregate signal，只调用一次；
- `WorkSmash` 使用 `call = -1`，避免 comparison tracing 下针对单个 target 执行 hint seed；
- DAG 单独使用 `Call="dag"`，不和 coverage target 混在同一个 RPC 里；
- DAG+coverage 同时存在时，DAG RPC 必须先于 coverage RPC 发出。

## 6. 待讨论

当前没有阻塞设计的待讨论项。

已确定：

- DAG-only 不接受额外 coverage-only 执行，空 Cover 可以接受。
- `saturatedCalls["dag"]` 先保留默认阈值，不豁免；后续实验需要时再调整。

## 7. 实现级细节

### 7.1 target list 构造

```go
type TriageTarget struct {
    call   int
    subNum int
    info   ipc.CallInfo
}

type WorkTriage struct {
    ps           []*prog.Prog
    targets      []TriageTarget
    flags        ProgTypes
    triageClient bool
}
```

在 `execute()` 中收集，不在 `executeRaw()` 中：

- `clientTargets`：
  - client 的每个新 call；
  - client 的 extra；
  - `subNum = client idx`；
  - `triageClient = true`。
- `serverTargets`：
  - 原 server fallback 条件成立时才收集；
  - 只收集 server 的 extra；
  - `subNum = server idx`；
  - `triageClient = false`。
- 构造 target 时复制 `info.Signal`，并设置 `info.Cover = nil`。
- 同一 side 内按 `(subNum, call)` 去重和排序。
- `enqueueCallTriage()` 改成返回 `TriageTarget`，不再自己 enqueue。
- `clientTargets` 非空时 enqueue 一个 client `WorkTriage`。
- `serverTargets` 非空时 enqueue 一个 server `WorkTriage`。
- 两边不混用；两边都非空时有两个 `WorkTriage`。
- DAG direct 在 enqueue 这些 `WorkTriage` 之前执行。
- 纯 DAG 不再构造 `WorkTriage`；`triageDag`、`enqueueDagTriage()` 删除。

### 7.2 `triageInput()` 的 per-target 结果

当前代码：`src/syz-fuzzer/proc.go:462-690`。

处理顺序：

1. `data` / `dataForHash` / `sig` 只计算一次；读取 `corpusHashes` 判断 seed 是否已存在，但不提前返回。

2. 对每个 target 做 initial signal 预过滤：

   ```go
   inputSignal := signal.FromRaw(target.info.Signal,
       signalPrio(item.ps[target.subNum], &target.info, target.call))
   pending := inputSignal
   if !alreadyInCorpus {
       pending = proc.fuzzer.corpusSignalDiff(inputSignal)
   }
   ```

   `alreadyInCorpus` 为 true 时，DAG direct 或另一侧可能已经先把 signal 合入 corpus；这里不能再用 global diff，否则 server/coverage target 会被误丢。`pending` 为空的 target 丢弃；全部为空则返回。

3. 只执行一次：

   ```go
   infos, _, _, err := proc.executeRaw(proc.execOptsLight, item.ps, StatTriage)
   ```

   执行失败直接返回。

4. 对每个 target 独立验证：

   - `reexecutionSuccess(infos[target.subNum], &target.info, target.call)` 失败则丢弃；
   - 用 `getSignalAndCover(...)` 计算这一轮的 target signal；
   - `pending = pending.Intersection(thisSignal)`；
   - `pending` 为空且 `flags&ProgMinimized == 0` 时丢弃该 target；
   - 如果没有有效 target，直接返回。

5. 对每个有效 target 构造 RPC 结果，保持当前 per-target 的 client/server 侧语义：

   - `triageClient == true`：
     - target 的 `CliSignal` / `CliCover` 来自该 client 的 call/extra；
     - `SrvSignal` / `SrvCover` 来自所有 server 的 `Extra`。
   - `triageClient == false`：
     - target 的 `SrvSignal` / `SrvCover` 来自该 server 的 `Extra`；
     - `CliSignal` / `CliCover` 来自所有 client 的 calls。

6. 每个有效 target 发送一个 RPC：

   ```go
   RPCInput{
       Call:      target syscall 名，call == -1 时为 ".extra",
       Prog:      data,
       CliSignal: targetCliSignal.Serialize(),
       SrvSignal: targetSrvSignal.Serialize(),
       CliCover:  targetCliCover.Serialize(),
       SrvCover:  targetSrvCover.Serialize(),
       Timings:   prog.ProgTimings(item.ps),
   }
   ```

7. 所有有效 target 的 `CliSignal` / `SrvSignal` 合并后，本地 `addInputToCorpus` 一次。

8. `cltTick`：

   - 只有 client `WorkTriage` 发送；
   - 至少一个有效 target 且 `target.call != -1` 时，每个 seed 最多一次。

9. 入队 `WorkSmash`：

   - 只在 `addInputToCorpus` 返回 `added=true` 时；
   - `call = -1`；
   - `subNum` 取第一个有效 target；
   - `flags&ProgSmashed == 0`。

不执行：3 次 signal、`prog.Minimize`、3 次 coverage、stage 2 dedup。

### 7.3 DAG direct

DAG 处理在 `execute()`；`DagSignal` 在 `executeRaw()` 中挂到第一个非 nil client info 上（改造前位置：`src/syz-fuzzer/proc.go:1271-1310` 和 `:1572-1585`）。

位置：

```text
execute()
  -> executeRaw() 返回 infos
  -> 收集 client/server coverage targets
  -> DAG direct（必须在 enqueue coverage WorkTriage 之前）
  -> enqueue coverage WorkTriage
```

DAG direct 条件：

- `newBits := checkNewDagSignal(info.DagSignal)`；
- `newBits.Len() > 0`；
- 通过 `MaxDagCorpus` 检查。

DAG direct 的初始 signal/cover：

- 第一个非 nil client 的 `info.Extra`；
- 所有 server 的 `info.Extra`；
- 使用 `getSignalAndCover(..., -1)`；
- Cover 可以为空。

处理顺序：

```text
MaxDagCorpus 检查
-> dagCorpusCount++
-> sendInputToManager(Call="dag")
-> addInputToCorpus
-> added 时 dagCorpusEntries++
-> added 时 enqueue WorkSmash
```

细节：

- DAG RPC 先于 coverage RPC。
- DAG 本地 corpus 先用初始 `info.Extra.Signal` 添加。
- 初始 signal 为空时接受 `priority=1`。
- coverage `WorkTriage` 后到只合并 signal/cover，不重复新增 corpus。
- `WorkSmash` 由第一次 `added=true` 的路径触发。
- `dagCorpusCount` 在通过 cap 后、RPC/add 前增加；hash 已存在导致 add 失败也计数。
- `dagCorpusEntries` 只在 `addInputToCorpus` 成功后增加。
- `dagPairCount`、`dagSchedCount`、`dagHbPairCount`、`dagCcPairCount`、depth 统计保持原逻辑。
- `dagEnqueues` 改名为 `dagDirect`，只用于诊断，不参与 cap。

### 7.4 Manager 侧

- 在 `rpctype` 定义 `DagCall = "dag"`。
- `RPCServer.NewInput()`：
  - `isDag := a.Call == rpctype.DagCall`；
  - genuine gate 对 `isDag` 放行；
  - `checkProgram()`、`newInput()`、cover 合并不变。
- `Manager.newInput()`：
  - `saturatedCalls` 不豁免 DAG；
  - 相同 `Prog` hash 保留旧 `Call`。
- `Manager.minimizeCorpus()`：
  - 正常执行 `signal.Minimize()`；
  - 未入选的 DAG 按 `sig[0] < '8'` 保留约 50%；
  - 补回后再更新 `mgr.corpus`、`mgr.lastMinCorpus`；
  - 之后执行饱和检查和 `corpusDB.Delete`。

### 7.5 代码清理

按 §4.6 执行：

- 删除 `prog.Minimize` 整段；
- 删除 early dedup 检查，但保留 `data` / `dataForHash` / `sig` 生成；
- 删除 stage 2 dedup、`preSig` 和末尾 `preSig` 更新；
- 删除 `execOptsLightMinimize`、`signalRuns`、`minimizeAttempts`、`notexecuted`；
- 删除旧的 3 次 signal / 3 次 coverage 循环；
- 保留 `addInputToCorpus`、`totalLen` 兜底、`WorkSmash`、`cltTick`。

### 7.6 GlusterFS v1 profile/gate

本文的 triage/minimize 算法本身是 DFS 无关的，GlusterFS 只补 profile/gate：

- `EnableDagFb`、DAG build、coverage board 初始化不能只对
  `DFSName == "hmdfs"` 开启；GlusterFS 同样启用。
- DAG 只消费 client events（`infos[ServNum:]`），过滤 server slots。
- `directDagTriage` 的 `subNum` 是绝对 executor index；第一个非 nil client
  就是 DAG feedback 的 owner。
- server programs 为空，`serverTargets` 自然为空；不能把 server slot 当成
  client target。
- `MaxDagCorpus`、`DagCall`、`saturatedCalls["dag"]` 保持全局语义，不做
  per-DFS 特判。
- `minimizeCorpus` 的 DAG 保留策略不改。
- `WorkTriage` target list 仍然按同一侧聚合；client/server 不混在一个 list 里。
- minimize 已全局停用，不再为 GlusterFS 增加 client-view minimize 分支。

实现状态（2026-11-17）：GlusterFS 已接入完整 DAG feedback（board learning +
DAG corpus/triage），不是只喂 coverage board。corpus 侧仍由
`addInputToCorpus` hash 去重、`MaxDagCorpus` 硬 gate、`minimizeCorpus`
修剪、`saturatedCalls["dag"]` 和固定 `smashIterations` 约束；
`MaxDagCorpus` 默认 0（unlimited），长跑按规模配置。

## 8. 验证

- client 的多个 call/extra 只生成一个 client `WorkTriage`，只执行一次 triage。
- server fallback 的多个 extra 只生成一个 server `WorkTriage`，只执行一次 triage。
- client 和 server 的 target 不混在同一个 `WorkTriage`；两边同时触发时执行两次。
- server fallback 的触发条件仍是原版 `!clientHasNew` / failure 条件。
- 部分 target 通过时只合并通过的 signal/cover，并且只为通过的 target 发 RPC。
- seed 已经在 corpusHashes 中时，target 用 raw signal 继续验证，不会被 global `corpusSignalDiff` 误丢。
- 每个 target 的 `RPCInput.Call`、signal、cover 对应该 target，不使用 aggregate。
- 不含 DAG 的相同 `Prog` 多 target RPC 在 manager 侧合并为一个 corpus 项，per-call 统计正常。
- DAG+coverage 时 DAG RPC 先于 coverage RPC 到达 manager。
- DAG direct 先用初始 `info.Extra.Signal` 添加本地 corpus；空 signal 时 priority=1，后续 coverage 只合并 signal，不更新 priority。
- `WorkSmash` 只由第一次 `addInputToCorpus` 返回 `added=true` 的路径触发，每个 seed 最多一个。
- DAG+coverage 同 hash 时，manager corpus 的 `Call` 是 `"dag"`；coverage RPC 合并 signal/cover 并按 target call 检查饱和，但 per-call 统计已知归到 `"dag"`。
- `cltTick` 按保留设计，只由 client `WorkTriage` 发送，且对同一个 seed 最多一次。
- DAG 反馈只挂在第一个非 nil client info 上，一次执行最多一个 DAG RPC。
- DAG direct 在 `execute()` 中、coverage WorkTriage enqueue 之前执行。
- DAG direct 的 `MaxDagCorpus` 在入口检查一次，且在发送 RPC、本地 add、`WorkSmash` 之前。
- `dagCorpusCount` 在通过 cap 检查后增加一次；hash 已存在导致 add 失败时仍然计数。
- `dagCorpusEntries` 只在 `addInputToCorpus` 成功后增加。
- `dagEnqueues` 改名为 `dagDirect`，只用于诊断，不参与 cap。
- target 入队时复制 `info.Signal` 并设置 `info.Cover = nil`。
- DAG-only 零重执行，且 manager 中出现独立的 `"dag"` corpus 统计。
- DAG-only 空 Cover 可进入 manager，不额外执行 coverage-only triage。
- DAG RPC 无新 signal 时仍进入 manager，不被 genuine gate 丢弃。
- `saturatedCalls["dag"]` 仍能阻止 DAG 输入，没有被 DAG 特殊放行；默认阈值先保留。
- `minimizeCorpus()` 后，入选的 DAG 保留，未入选的 DAG 约保留 50%，非 DAG 最小化结果不变。
- 同 hash 的 DAG/coverage 输入，manager 最终保留的 `Call` 与发送顺序符合预期。
- DAG+coverage 的 corpus/smash/cover 合并行为。
- early dedup 和 stage 2 dedup 已删除，seed hash 只计算一次并用于 RPC 和 `addInputToCorpus`。
- `addInputToCorpus` 仍负责最终 corpus 去重和 signal 合并。
- `prog.Minimize` 代码只保留为注释，triage 不再执行 minimize。
- `Timings` 来自最后一次实际执行。
- `addInputToCorpus` 的 corpus 去重和 signal 合并正常。
- `GOOS=linux go build ./syz-fuzzer`
- `GOOS=linux go test -c ./prog`
- `GOOS=linux go vet`
