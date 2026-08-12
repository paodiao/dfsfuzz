# DAG 反馈系统：已知小问题记录

> 范围：操作 DAG 反馈系统（`src/prog/dag.go`、`src/executor/*`、`src/syz-fuzzer/*`）review 时发现的小问题。
> 以下问题均评估为**不影响正确性或影响可忽略**，当前决定不修；每项给出原因与将来的修复方案，避免遗忘。

## 1. 非 x86 平台 `tsc_ns_to_global` 的时间域兜底不一致

**位置**：`src/executor/executor.cc` 校准块 `#else` 分支

**现象**：eBPF 事件时间戳来自 `bpf_ktime_get_ns()`（内核 monotonic 时钟，纳秒），而调用窗口时间戳是 `rdtsc()`（原始 TSC 周期数）。两者是同一物理时间的不同线性刻度，必须做仿射变换才能比较。x86 系通过 `calibrate_tsc()`（sleep 50ms 采样两点求比例）统一到全局 TSC 域；但 `#else`（非 x86）分支的 `tsc_ns_to_global` 直接返回输入值不做转换。

**影响**：非 x86 上事件时间与调用窗口不在同一时间域 → 事件→调用匹配（`matchEventToCall`）会全部失败、跨 VM 时间比较错误。hmdfs 目标实际仅 x86_64 部署，该分支不会被触发。

**不修理由**：原文件 `rdtsc()` 本身就只有 `__i386__`/`__x86_64__` 两个实现，非 x86 无从校准；补其他架构（如 ARM 用 `clock_gettime`）无硬件验证条件。

**将来修复**：ARM 等架构部署时，把 `th->stime/etime`（executor.cc 调用计时处）改用 `clock_gettime(CLOCK_MONOTONIC)` 并同步改 `tsc_ns_to_global` 为恒等或对应换算。

---

## 2. schedule bit 恰好为 0 时被跳过统计

**位置**：`src/syz-fuzzer/fuzzer.go` `checkNewDagSchedule`（`bit == 0` 直接返回 0）

**现象**：schedule 位是全部 pair 哈希有序再哈希后截断的 uint32，理论上可能恰为 0。实现用 `bit != 0` 作为"有 schedule"的守卫，为 0 时不计入 `dag schedule signal` 统计。

**影响**：概率 2⁻³²（约 43 亿分之一）；且该执行的新 pair 位仍正常驱动 corpus 反馈（schedule 位本来就不驱动 corpus——见附录的论证），只丢 1 点统计。

**不修理由**：无实际影响。

**将来修复**：若需绝对精确，可将 schedule 位映射为 `0x80000000 | (hash & 0x7fffffff)` 消除零值。

---

## 3. `dagCorpusCount` 统计口径与门限（部分已修复）

**位置**：`src/syz-fuzzer/proc.go` triageInput（`dagCorpusCount` 统计 +1）+ `dagCorpusEntries`（门限）+ pollLoop（Swap 清零）

**现状（2026-08 修正）**：
1. **统计口径**（`dagCorpusCount`）：在 `triageInput` 完成时 +1（**非 enqueue 时**——本文档原描述已过时）——仍为近似（不查 hash 去重——重复 DAG triage 也 +1）——pollLoop 每 10s Swap 清零（增量统计）。
2. **门限**（`dagCorpusEntries`）：新增——`addInputToCorpus` 返回 bool（hash 去重成功）后、仅 `triageDag && added` 时 +1——**不被 pollLoop 清零**——`MaxDagCorpus` 门限基于真实去重条目数（原"将来修复"已实现）。

**遗留**：统计口径（triageInput 完成数）与真实 corpus 条目有微小偏差（重复 triage）——仅统计影响，不修。

---

## 4. `determinePathRel` 的 cross-product 近似（ordered 参数未使用）

**位置**：`src/prog/dag.go` `determinePathRel`

**现象**：每个顶点有两个候选路径——`Path`（操作完成后的 post 路径）和 `prePath`（操作开始前的 pre 路径，仅 rename 时两者不同）。函数把 `[A.post, A.pre] × [B.post, B.pre]` 做笛卡尔积，任一组合满足 SAME_PATH / PARENT_CHILD / SAME_PARENT 即返回该关系。但**严格语义应按配对方向选时刻**：HB 有序对 A→B 应只比较 `A.post × B.pre`；并发对（窗口重叠、无先后）才应全组合比较。`ordered` 参数在函数体内未被使用。

**伪关系示例**：A: `creat X`，B: `rename Y→X`，有序 A→B。严格应比 `A.post(X) vs B.pre(Y)` → 无关系；但全组合里 `A.post(X) == B.post(X)` → 误判 SAME_PATH。实际是两个不同 inode 先后占据同一路径位置。

**影响边界**：仅当配对中至少一方是 rename（pre ≠ post）时全组合才有别于单组合；SAME_INODE 独立按 ino 判断不受影响；配对方向由 HB/并发时间序决定、不由 PathRel 决定，故伪关系不会造成方向性错误。"先后占据同一路径"在路径占用语义下也有一定意义。

**不修理由**：影响面窄、收益边际，且修改后需要重新验证配对行为。

**将来修复**：`ordered == true` 时只比较 `A.Path × prePath(B)`；并发对保留全组合。

---

## 5. `newDagSignal` / `newDagSchedSignal` 集合未被消费

**位置**：`src/syz-fuzzer/fuzzer.go`（Fuzzer 结构体字段 + `checkNewDagSignal`/`checkNewDagSchedule` 内 Merge）

**现象**：`newDag*Signal` 两个集合只增不减，实际统计上报走的是原子计数器 `dagPairCount`/`dagSchedCount`（poll 时 `Swap` 清零）。集合是镜像 `maxSignal`/`newSignal` 模式保留的，从未被任何代码读取。

**影响**：仅内存增长（与 `maxDagSignal` 同规模，几万到几十万条 uint32 键）。

**不修理由**：删除需动 4 处字段 + 2 处 Merge；保留可作为将来跨 fuzzer 同步 DAG 信号的基础。

**将来修复**：做跨 fuzzer DAG 去重同步时（`PollArgs`/`PollRes` 增加 `MaxDagSignal` 字段），正好消费这两个集合。

---

## 6. BPF 注释/日志写 "15 functions" 与实际 16 不符

**位置**：`src/executor/hmdfs_trace.bpf.c` 文件头注释；`hmdfs_trace.cc` `init_hmdfs_trace` 日志（"15 kretprobes attached"）

**现象**：功能编号 0–15 共 16 个 kretprobe（15 个 VFS 合并函数 + writepage_cb），注释与日志仍写 15。早期设计为 15 个，后加 `FUNC_WRITEPAGE_CB` 时未同步。

**影响**：纯文档/日志文字问题，无功能影响。

**不修理由**：零风险文本改动，但易在修改时漏改其他引用处；集中记录避免再犯。

**将来修复**：下次 touch 该文件时统一为 "16"，并顺带检查 `STATE_MODELING.md`/`EBPF_TRACE.md` 中的函数数量表述。

---

## 7. 方向 1 探索度偏向是"独占式"的

**位置**：`src/prog/distributed_choice.go` `chooseVariant`

**现象**：方向 1（探索度追踪）实现为：存在"未产出"组合（`!Explored && NoYield < 20`）时，**只在探索池内选择**，已产出（explored）组合完全不被选，直到探索池为空。池 = 全表 − 已产出组合，长期非空（总有新组合或未产出组合加入）。

**影响**：探索倾向是独占而非偏向——高产出组合在探索池非空时被完全压制。若实验发现高产出组合被长期压制、影响 DCT 收敛，需改为概率进入探索池（如 70% 走探索池、30% 正常加权）。

**不修理由**：这是方向 1 设计意图（优先探索未产出组合）的极端化实现；先跑实验数据再决定是否需要软化。

**将来修复**：`chooseVariant` 中以概率 `p`（如 0.7）选择探索池，否则正常加权选择；`p` 作为常量可调。

---

## 8. `mutateHmdfs` 回退条件对全 server 种子无效

**位置**：`src/syz-fuzzer/proc.go` `mutateHmdfs`

**现象**：回退条件为 `!mutated && len(ps) > srvNum`——若种子只有 server prog（`len(ps) <= srvNum`），既不执行任何突变（专门突变器和标准 Mutate 都跳过），execute 直接跑原样 ps，浪费一次执行。

**影响**：corpus 种子通常包含 client（生成时保证），此场景概率低；即使触发也只是浪费一次执行，无正确性问题。

**不修理由**：影响极小，修复（全 server 时随机选一个 server 做标准 Mutate）收益边际。

**将来修复**：`len(ps) <= srvNum` 时回退到 `ps[0].Mutate`（或直接跳过 execute）。

---

## 9. 时间对齐用"前驱 Etime"近似新调用的实际开始时刻

**位置**：`src/prog/mutation.go` `TimeAlignedInsertPos`/`timeAlignedInRange`、`insertCallFromDCT`/`insertCallFromPattern` 的 `refTime` 计算

**现象**：时间对齐的参考时间取"插入位置前一个调用的 `Etime`"（前驱结束时新调用开始执行），对齐目标同样用各 prog 调用边界的 `Etime`。实际执行中新调用在调度后才会真正开始（有调度延迟），且跨 VM 比较精度依赖 `tsc_offset` 的正确性（`raw rdtsc - tscoff` 是否真的全局可比）。

**影响**：对齐是近似值——两个"对齐"的调用可能在毫秒级误差内错开。但对齐目标本来就是"窗口重叠形成并发"，毫秒级误差远小于调用耗时（syscall 通常几十微秒到毫秒级），并发对仍能形成；且 tsc_offset 机制与 DAG 全局时间线共用同一套语义（见 #10），正确性已被 DAG 侧验证过。

**不修理由**：设计近似，误差在可接受范围；若要更精确需要采集调度实际开始时刻（executor 侧记录插入调用的真实窗口），成本高、收益边际。

**将来修复**：若实验发现并发对形成率低，可让 executor 在两次执行间记录插入调用的实际窗口并反馈回种子（复杂）；或引入固定 sleep 微调。

---

## 10. 时间对齐依赖"最近一次执行"的 CheckInfo；`tscoffFor` 逻辑两处重复

**位置**：`src/prog/clone.go`（CheckInfo 复制）、`src/prog/mutation.go` 与 `src/prog/dag.go`（tscoffFor）

**现象**（两点）：
1. `CheckInfo.Stime/Etime` 是种子**最近一次执行**的时间（triage 重执行会更新；`Clone` 共享指针）。同一种子 smash 100 次时参考时间恒定（预期行为——围绕同一时间布局做变体）；但若种子在 corpus 中多次重执行（triage/候选），时间布局会随最新执行漂移。
2. `tscoffFor` 逻辑重复两份：`dag.go` 的包级函数 `tscoffFor(tscoffs, idx)` 与 `distributed_choice.go` 的 `(lcs *LayeredChoiceStrategy) tscoffFor(nodeIdx)`——实现相同（越界回退到最后一个/0），一处改动需同步另一处。

**影响**：第 1 点——时间布局漂移意味着不同轮次 mutate 的对齐基准不同，但每轮内部自洽，不影响正确性；第 2 点——纯代码重复，无行为影响。

**不修理由**：第 1 点是"最近执行时间"语义的自然结果（DAG 侧同样如此）；第 2 点改动需 touch 两个文件、无行为收益。

**将来修复**：第 2 点——让 LCS 方法委托给 `dag.go` 的包级 `tscoffFor`（或抽到公共 helper）。

---

## 11. 移动突变的依赖检查是"执行后状态"近似

**位置**：`src/prog/mutation.go`（规划中）`MoveCallTimeAligned` 的依赖检查

**现象**：移动调用前用 `lcs.FileTree` 检查路径存在性——但 FileTree 反映的是
**最近一次执行之后**的 merge_view 状态，不是目标时刻的状态。跨 prog 移动后，
路径可能在目标时刻已被删除/改名（本 schedule 内其他调用执行的结果），导致
移动后的调用失败，产生 FAILURE 桶 pair（而非预期的 HB/并发对）。

**影响**：移动的"造新对"成功率打折扣，但失败调用无害（FAILURE 桶也是合法
pair 空间的一部分）；依赖检查已过滤 fd 链风险，剩余风险面小。

**不修理由**：精确检查需要按时间线判断路径在目标时刻的存在性（复用 DAG 的
rename/delete 时间线思路，见 dag.go），成本高、收益边际。

**将来修复**：把 DAG 的 rename/delete 时间线暴露给移动突变，移动前查询
"路径在 refTime 是否有效"；或移动后快速验证（重执行成本高，不建议）。

---

## 12. 四个 fuzzer flag 无 manager 配置传入通道

**位置**：`src/syz-fuzzer/fuzzer.go`（flag 定义）与 `src/vm/qemu/qemu.go`（fuzzer flag 构造，930-948 行）

**现象**：以下四个 fuzzer flag 在 `qemu.go` 的 fuzzer 命令构造中**没有传递**，`mgrconfig.Config` 也无对应字段——**只能使用默认值，无法从 manager 配置文件控制**：

| flag | 默认值 | 用途 |
|---|---|---|
| `-MetadataDelayMs` | 10000 | 元数据收集延迟（executor argv[16]） |
| `-EnableDagFb` | true | DAG 反馈总开关 |
| `-EnableDagScheduleFb` | true | schedule 统计开关（见附录） |
| `-MaxDagCorpus` | 0（无限） | DAG 驱动 corpus 上限——设计讨论中的示意值 2000-5000 未经实验校准；启用前需先测量 `dag corpus` 增长率与 smash 执行预算再定（见 #14） |

**影响**：当前默认值均适用（10s 延迟与 hmdfs writeback 周期匹配、DAG 反馈默认开），功能不受影响；但做**对比实验**（如关闭 DAG 反馈的消融组）或**调参**时无法从配置控制，只能改代码或手动传 flag。

**不修理由**：当前实验默认值已满足需求；补通道需改 mgrconfig + qemu.go 两处（+ 配置文件），收益待实验需要时再实现。

**将来修复**（3 处）：
1. `src/pkg/mgrconfig/config.go` — 加 `MetadataDelayMs int json:"metadata_delay_ms"`、`EnableDagFb bool json:"enable_dag_fb"`、`EnableDagScheduleFb bool json:"enable_dag_schedule_fb"`、`MaxDagCorpus int json:"max_dag_corpus"`
2. `src/vm/qemu/qemu.go`（930-948）— 加 4 行 flag 传递（`MetadataDelayMs` 链路已通：fuzzer flag → fuzzer config → ipc.Config → executor argv[16]）
3. hmdfs 配置文件加对应字段

---

## 13. `pickNewBasePath` 路径选择偏置、`excludeCid` 无效、rename 双路径迁移缺陷

**位置**：`src/prog/mutation.go` `pickNewBasePath`/`MutateGroupPathDynamic`/`updateCallPathInProg`

**现象**（三点，随 §6.5.4 路径突变接入而激活）：
1. **`excludeCid` 形同虚设**：`MutateGroupPathDynamic` 调 `pickNewBasePath(lcs.FileTree, seedType, r.Rand, "")` 传空 cid——`GetAllFileNodesExcluding`/`GetAllNonTmpDirNodesExcluding` 对空串不做任何过滤；且选出的新 basePath 有小概率恰与原路径相同，组路径未实际变化（无效突变，仅浪费一次突变机会）。
2. **权重偏置**：`calculateNodeWeight` 对小文件（Size ≤ 4096）×2、大文件（> 1MB）×2、深度 ≥4 ×2、深度 ≥2 ×1.5，另乘路径长度/最大组件长度权重——路径突变偏向深层路径与小/大文件，未必对应 bug 实际分布；深度分桶实验（`dag depth 1/2/3`）数据可用于校准。
3. **rename 双路径迁移缺陷**：`updateCallPathInProg` 只替换 `Args[0]`（源路径），rename 的目标路径留在旧路径域——迁移后变跨目录 rename。合法测试场景，低价值无害。
4. **重复 pos 覆盖（fd 回溯收敛）**：多个并发者的 fd 回溯到同一 open（同一 prog 内多调用共享 fd）→ `updates` 中同一 open 位置出现多次，后写覆盖——并发者 rel 语义收敛到 open 的最终路径（fd 链一致、不崩溃）。与静态版同源（fd 回溯固有近似）。

**不修理由**：偏置合理性需等深度分桶实验数据校准；rename 双路径修正需引入双路径替换，改动中等、收益边际。

**将来修复**：1——`GetAllFileNodesExcluding` 支持空串时按前缀排除原路径（或调用方传入）；2——按实验数据调整 `calculateNodeWeight` 权重；3——`MutateGroupPathDynamic` 对 rename 调用按 `isTwoPath` 语义同时解析源与目标路径。

---

## 14. 方案 2：DAG 感知最小化（pair-preserving minimize）——已设计，待实施

**位置**：`src/syz-fuzzer/proc.go`（triage）、`src/syz-fuzzer/workqueue.go`（WorkTriage）

**现状**：`triageDag` 候选（DAG 驱动的 corpus 条目）在 triage 中跳过 minimize 与稳定性验证（`if !item.triageDag` 整块），corpus 条目保留全部调用。覆盖率驱动的 minimize 对 hmdfs 种子同样运行，但其等价谓词只看覆盖率信号——可能删除产生 DAG pair 的调用（目标函数不一致）。

**方案 2 设计**（动态 pair 保留判定）：
1. `WorkTriage` 加 `dagSignal []uint32` 字段——入队时保存该候选**新 pair 子集**（`DagSignal[k]` 与 `DagPairs[k]` 对应，用 `newBits` 索引提取子集；Go 堆分配非 shmem，无需拷贝）。
2. `triageDag` 分支也执行 `prog.Minimize`，pred 替换为：minimize 中间执行（`StatMinimize`）后，从 `infos[ServNum:]` 取第一个非空 `DagSignal`，断言**去重集合包含**（`newPairBits` 哈希集 ⊆ 当前哈希集，map 判定；pairBits 是变长哈希 slice 非位图）。
3. `minimizeAttempts=3` 次"任一次保留即 true"容忍 schedule jitter 的 flaky pair。
4. 语义：minimize 不会删除任何产生新 pair 的调用；只删对已入队 pair 集合无贡献（不参与或只参与冗余 pair）的调用。组结构保护由 pair 贡献自然覆盖（组并发调用参与 pair），无需静态组 ID（见 #15）。

**实施前置确认**：`info.DagSignal` 为 fuzzer 侧分配（非 shmem，proc.go executeRaw 中 `ComputeFeedback` 新建）；序列化 key 删除对 hub 老种子兼容（encoding.go `eatExcessive` 吞未知 key）。

**不修理由**：minimize 的中间执行本身会触发 DAG 反馈入队（模式已捕获），瘦身失败只影响该种子的后续 smash 变体；且 DAG corpus 无上限（`MaxDagCorpus=0`）时膨胀问题可用实验上限缓解。上限取值（设计讨论中示意 2000-5000）是"数量级示意"（当时原文 "e.g."），未经推导——正确取值需实验测量：corpus 增长率（`dag corpus` 统计）× smash 轮数 × 每轮执行时间 ≤ 可接受执行预算，或对照 pair 类型空间上限（16 func × 16 func × pathrel × temporal × ret 桶）确定冗余阈值。

---

## 15. 静态 GroupID 移除：惰性动态分组（已完成）

**位置**：`src/prog/mutation.go`（动态核心 + 4 个动态突变）、`src/prog/prog.go`（CallProps 字段）

**背景**：GroupID 在生成/插入时静态决定，但后续突变会改变并发与因果结构，静态分组逐步过时。且全部反馈链（DAG pair、DCT 权重、pair 保留判定）均为动态计算、不依赖 GroupID——GroupID 只服务于组级突变操作（路径迁移/组删除/组内删一）。

**方案**：惰性动态分组——需要分组时现场计算，不持久化任何分组状态：
- `pathRelBetween(anchorPath, concPath)`：路径几何关系分类（相同/父子/兄弟/无关）
- `pickAnchor(ps, r, wantReadWrite)`：ps[0] 随机选有路径 + 执行时间线的调用（wantReadWrite 时限定读写调用）
- `findConcurrentCalls(ps, anchor, tscoffs)`：跨 prog 执行窗口重叠判定（`s1<e2 && s2<e1`，tscoff 归一化全局域；同 prog 内串行无重叠）
- 4 个动态突变：`MutateGroupPathDynamic`（锚迁新 basePath、并发者按现场 rel 解析、fd 回溯、主干不迁）、`RemoveGroupDynamic`（删锚+并发者）、`RemoveOneInGroupDynamic`（fd 安全单删）、`MutateGroupDataDynamic`（锚限定读写，集合内全部读写共享 offset/length——确定性对应插入路径的概率性 OffsetSame）
- mutateHmdfs 分发：removeGroup 20% / removeOne 10% / path 10% / data 10% / insert 50%

**删除**：`CallProps.GroupID/PathRel/IsFromDCT/OffsetRel/LengthRel`（5 个序列化 key）、`Prog.Groups/LastGroupID`、`GroupMeta/GroupSourceType`、`AllocGroupID/renumberGroups/GetGroupPositions/RemoveGroup/collectAll*/getDeletable*/findPrimaryDCTCall/MutateGroupData/MutateGroupPath/mutateGroupPathRandom/setGroupMeta/getNewPathRel/getSharedFileSize`、~55 处 SetGroupID 及属性设置点、`ChooseOffsetRel/ChooseLengthRel/getRootOffsetRel/getRootLengthRel/isOffsetSensitiveCall/isLengthSensitiveCall/dctOffsetWeights`（插入路径的 offset 概率选择随之移除，共享 offset 由 MutateGroupDataDynamic 定向提供）。

**保留**：`CallVariant`（归因）、`pickNewBasePath/resolveFdTarget/updateCallPathInProg/getFdFilePathForCall/AnalyzeProgFds`（动态版复用）、`GroupPosition`。

**已知取舍**：
- hub 共享种子不再携带分组（惰性计算），老种子序列化中的 `group_id` 等 key 被反序列化器忽略（兼容）。
- `MutateGroupDataDynamic` 的共享 offset 从插入路径移除后，`insertCallFromDCT`/pattern 生成的新调用不再记录 OffsetRel——共享偏移只由 dataMutate 定向产生，实验观察是否需要恢复插入侧的概率共享。

---

## 16. Temporal 归因错位与形态层（第二层）建模（已修复）

**位置**：`src/prog/dag.go`（`DagPairToVariant`）、`src/prog/distributed_choice.go`（`TemporalWeights`/`ChooseTemporal`/`UpdateTemporalWeight`）、`src/prog/mutation.go`（`firstBoundaryAfter`）、`src/syz-fuzzer/proc.go`（`feedbackDagPairs`）

**现象（原 bug）**：`DagPairToVariant` 原本不读 `p.Temporal`——HB pair（因果对）与并发 pair 归因到同一 (root, variant) 组合 → HB pair 的新颖性错误触发 `MarkYield`（方向 1 探索预算被偶然 HB 截断、方向 2 该降权组合被续命）。且 HB pair 没有任何生成机制定向构造（pattern/DCT/动态突变全以并发为核心）——"半边探索"：反馈算 HB 新颖性、生成侧无回应。

**方案（第二层：Temporal 形态层）**：在每个 (root, variant) 组合下加**形态权重**（并发形态 vs 因果形态，默认 50/50），独立于组合权重（Weights 表）：

- 生成：`insertCallFromDCT` 按 `ChooseTemporal` 选形态——因果形态用 `firstBoundaryAfter`（root 完成后的第一个执行边界，倾向正向 HB pair），并发形态维持时间对齐；
- 反馈：`feedbackDagPairs` 按**实际产出**的 pair Temporal 更新形态权重（并发 pair → 并发形态 +1；HB pair → 因果形态 +1；交叉不奖励）——形态权重学的是"哪个形态可靠产出对应 Temporal pair"；
- 归因：`DagPairToVariant` 返回 `p.Temporal`；**所有新颖 pair（并发或 HB）统一触发 `MarkYield`**（方向 1/2 按组合综合产出驱动——组合由 `ChooseTemporal` 可产生两种形态，反馈应统一；形态差异已由形态层独立学习，不会混淆）。

**修订说明**：原方案（#16 初版）曾限定"HB pair 只更新形态权重、不触发方向 1/2"——当时 HB 形态构造刚引入、担心偶然 HB 污染"并发产出"信号。形态层成熟后该限定不成立：HB 是组合的正常产出形态，方向 1/2 的 Explored/NoYield/权重奖励均按**综合产出**统一判定（2026-08 修订）。

**已知近似**：
- 意图与结果的交叉（并发形态产出 HB pair）按结果给因果形态 +1——统计上自均衡（形态产出对应结果的概率即所学），无意图追踪负担；
- 因果形态构造是"倾向"而非保证（执行时序不受控）——这正是形态权重学习的目标分布；
- **refTime=0 时因果形态退化**：`insertPos==0`（root 插在 p0 开头）时 `firstBoundaryAfter` 返回 0（boundary(0)=0 ≥ 0 恒真）→ 变体插到 p1 开头 = 与 root 同时开始，实际退化为并发形态——形态区分度在该场景下降，不产生错误；
- 随机生成的 seed 路径（`generateFromDistributedChoiceTable`）无执行历史、不做时间对齐，不参与形态选择（仅突变路径 `insertCallFromDCT`）。

**后续方向**：形态权重是"意图→结果"分布的简化（双权重）；更完整的建模（程序结构 → 时序关系映射、DAG 模式合成器）待调研（见 §6.5 讨论）。

---

## 17. 并发/因果建模结构调研（后续研究项）

**位置**：全系统（生成/反馈/突变三侧）

**问题本质**：`Temporal`（并发/因果）是**执行时间线的派生属性**，不是操作对的固有属性——同一 (A,B) 在不同执行中可为 CONCURRENT 或 HB。因此三维张量 `Weights[root][variant][temporal]` 不成立：**Temporal 不是生成器的可控输入**——生成只能构造"倾向"（时间对齐 → 倾向并发；顺序锚定 → 倾向因果），实际时序由执行决定。张量的第三维是虚的。

**现有形态层（#16）的定位**：`TemporalWeights` 双权重（并发/因果形态各一权重）是"意图→结果分布"的**粗糙近似**——按结果无条件奖励、交叉统计自均衡、无意图追踪。它验证了"生成侧可以定向构造因果对"（因果形态 = `firstBoundaryAfter`），但学习能力有限。

**调研方向**：
- **α：分层双通道（完整版）**——组合选择器（现有 DCT 矩阵）+ 时序形态选择器（学习"意图形态 → 实际时序"的转移概率，而非单权重）——在现有形态层基础上增强；
- **β：DAG 模式空间/合成器**——生成目标从"组合"提升为"pair 模式（含 Temporal）"：给定目标模式（一组并发对 + 因果对的组合），合成满足约束的程序——可借鉴 **DPOR（动态偏序归约）** 的 HB 偏序表示，验证领域用它避免探索冗余交错，我们反向用于**定向构造特定 HB 偏序**；
- **γ：窗口布局建模**——执行时间线为第一公民：直接建模"调用窗口布局 → 实际时序"的映射，生成时控制窗口布局（对齐 → 并发；锚定前驱 Etime → 因果）。

**文献方向**：DPOR 与偏序约简；分布式系统 fuzzing 的时序处理（Harmony/DBfuzz/Aurora）；因果一致性验证；Jepsen 风格序列化/线性化测试；DCacti 等基于 DAG 的并发验证。

**实验输入**：`hb pair signal`/`cc pair signal` 占比（新颖 pair 中因果对 vs 并发对）、形态权重数据（因果形态构造有效性）——支撑调研取舍。

**前置条件**：先跑一轮实验收集上述数据（形态层已就位，`hb pair signal`/`cc pair signal` 已上线观测）。

---

## 18. 因果组突变：统一组定义（直接后继版）——已实现

**位置**：`src/prog/mutation.go`（`findHBCalls`/`findGroupCalls` + 4 个动态突变）

**背景**：动态分组突变（#15）以并发为核心——`findConcurrentCalls` 只找窗口重叠的调用，因果对（顺序执行）不是任何突变的操作对象。因果构造此前只存在于插入侧（#16 Temporal 形态层 `firstBoundaryAfter`）；突变侧不作用于因果对——因果模式一旦构造，后续突变不在其上做变体。

**方案（统一组定义）**：
```
组 = 锚 + 并发者（窗口重叠） ∪ 直接因果后继（锚完成后开始）
```
- `findHBCalls(ps, anchor, anchorPath, tscoffs)`：遍历其它 prog，找 `Stime ≥ Etime(锚)`（归一化全局域）的调用中**每 prog Stime 最早的紧邻一个**——时间线上的直接因果边（与并发版"全取重叠"不同：组大小可控、语义精确——每条直接因果对）；
- `findGroupCalls` = `findConcurrentCalls ∪ findHBCalls`（按位置去重）；
- 4 个突变（`MutateGroupPathDynamic`/`RemoveGroupDynamic`/`RemoveOneInGroupDynamic`/`MutateGroupDataDynamic`）共用 `findGroupCalls`——**无新 case、无内部分流**——所有突变自动覆盖因果对：
  - 数据共享 → "顺序 write→read 同 offset"（一致性验证：HMDFS 异步写回下"写返回后读"应读到新数据）；
  - 路径迁移 → 因果链成员一起换路径（rel 现场算）；
  - 删除 → 因果成员一起删 / fd 安全单删。

**已知近似**：
1. **"顺序≈因果"**：CheckInfo 只有时间窗口——`findHBCalls` 判定的是"时间先后"（`Stime ≥ Etime(锚)`），不是 DAG 判定的真因果（后者还需路径关系 + 修改者/观察者条件）——与 `findConcurrentCalls` 的"重叠≈并发"同级近似；
2. **传递性因果问题**：只取直接后继，链上未纳入的后继（B 的因果后继 C）在突变后可能断裂（迁移 B 不迁 C → B→C 对变异/消失）——**实验观测项**：`hb pair signal` 是否明显下降；若下降评估"跨 prog 因果边传递闭包"（同 prog 内串行天然因果，闭包必须限定跨 prog 边否则退化为整 prog）；
3. 每 prog 紧邻一个（非全取）——全取会让组接近其它 prog 全部调用、"因果"语义稀释。

**不修理由**：先实现直接后继版跑实验（`hb pair signal`/`cc pair signal` 占比 + 形态权重数据），数据驱动决定是否需要闭包/全取。

**将来修复**：跨 prog 因果边 BFS 闭包（深度限制）；pair 目标驱动的因果组（#17 调研方向）。

---

## 19. stash/dcache 线评估与 stash 读验证失真修复（已完成）

**位置**：`src/prog/generation.go`/`rand.go`（生成）、`src/prog/mutation.go`（`MutateStashProg`/`MutateDcacheProg`）、`src/checker/symsc/checker.py`（信号通道）

### 线定位评估（维持现状）

- **stash 线有必要**：net 故障注入 + 写-故障-读验证闭环是通用路径（DCT/pattern/动态分组）没有的定向场景；
- **dcache 线有必要但较弱**：barrier 精确同步 + persistence 目录语义独有；目录操作与 inodeops 线重叠；
- 两条线维持现状（生成模板化 + 专用 mutator 微调）——评估层反馈（覆盖率 + DAG pair）对所有种子生效，好种子/变体照常进 corpus。

### dcache 信号通道核实（关键证据）

dcache 的目录可见性 bug 有**确定捕捉通道**：`checker.py:763-772` 显式比较模拟 getdents 返回与实际执行记录（`checkInfo.Dents`，ipc.go:1208 回填）：
```python
emulate_dents = c.BUF_DATA[buf_var]           # 模拟器按模型 DENTRY 状态返回
if emulate_dents != op_runtime_stat['Dents']: # 与实际返回对比
    print("dents differ", ...)
    return False                              # → ConcFSCheck 失败 → bug 报告
```
模拟器建模目录可见性（`c.DENTRY` 由 mkdir/rmdir/unlink/rename 维护，syscalls.py `getdents` 按模型状态填充）。**dcache 的 OpType/Delay/PathName 突变有效**（A 节点 unlink 后 B 节点缓存未失效 → getdents 返回差异 → "dents differ"）。
**附带观察**：symsc 是 POSIX 强一致模型——HMDFS dcache 若为最终一致设计，合法滞后窗口内的 "dents differ" 会成为误报（checker 判 bug，人工判断）。

### stash 读验证失真修复（本次）

**背景**：stash 的读验证（读节点 pread64 按生成时 `WriteInfo` 的 offset/length 读）在突变改写写参数/结构后失真——新写区域不被验证，"写-故障-读"闭环断裂。逐突变评估：

| 突变 | 评估 | 处理 |
|---|---|---|
| `FailPos`/`SyncPos`/`TargetNodes`（故障交错类） | 有效——读验证覆盖全部写区域，闭环完整 | 不动 |
| `WriteParams`（offset/length/data） | **offset/length 突变失真**（新区域无读验证）；data 突变不失真 | **已修**：`syncStashReadVerification` 按 (offset, length) 区域匹配同步读验证；`mutateWriteOffset` 顺带修下溢（负 delta clamp 0） |
| `OpSequence`-insert（插 pwrite64） | **新写区域无读验证**（失真） | **已修**：`addStashReadVerification` 在其它 stash 读节点插入匹配 pread64（`genPreadCallWithFdAt`） |
| `OpSequence`-remove（删写/读） | 轻失真可接受（删写后读验证读"未写区域"仍有效；删读 = 少一个验证点） | 不动 |
| `OpSequence`-swap | 区域不变不失真；但多文件下 fd 范围风险（作者 TODO 自注）且时序重排能力与通用体系重叠（动态分组/移动突变）——**价值不足以支撑修复** | **已删除**（`swapStashCalls` + case 分支） |

**效果**：stash 突变产出的变体写-故障-读验证闭环完整——故障期间写的数据完整性可被验证（读验证 + symsc 读写一致性检查）。

**顺带修复——存量 DirOut 构造错误（9 处）**：`MakeDataArg(t, DirOut, ...)` 必 panic（prog.go:405-408）——全仓核查修复 9 处（mutation.go 的 `genPreadCallWithFdAt`/`genPreadCallWithFd`/`genPreadCall`/2 处读调用生成；rand.go 的 pread/read/getdents64 生成）统一改为 `MakeOutDataArg`；rand.go:4200 的 `offset` 已核实为 read length（自洽非笔误）。此前仅因相关代码路径执行率低而未被发现。

**getdents64 测试调用修复**：`insertDcacheCall` 的 getdents64 分支（作者 TODO）存在三问题并已修：
1. **死代码**：候选条件 `IsOpenDir && TotalUses > 0`——dcache 种子（timeout/persistence/dropPush）从不 open 目录（唯一 open 是文件 flags=2；唯一生成目录 fd 的 `generateGetdents64Calls` 无调用点）——插入恒失败。**已改自包含**：`insertGetdents64Call` 现在从种子目录操作提取路径，一起插入 `open(O_DIRECTORY) + getdents64 + close`（fd 链自洽）；
2. **count=0**：`getdents64(fd, dirp, 0)` 空读不触发真实目录读取，且模拟器 buf=";" vs 实际空返回 → "dents differ" 误报。**已改固定 4096**（buf 与 count 一致；`randBufLen` 不适合——~11% 返回 0、多数 0-511 小值落入截断边界）；
3. **定位**：getdents64 是**测试调用**（触发 dcache 目录读取路径），非验证调用——目录内容验证由 stat/symsc 承担（"dents differ" 检查保留，大 count 下目录条目少时两边读全、无截断差异；条目多时的截断差异是 checker 的 dirent 大小近似（`10+len(name)` vs 实际 padding）误报，记录待 checker 层改进）。

**保留**：`generateGetdents64Calls`（rand.go，count 已修 4096）——当前死代码，作为未来 dcache 生成接入 getdents64 测试调用的起点。

---

## 20. 静态 DCTInfo 归因删除（已完成）

**位置**：`src/prog`（`DistributedChoiceInfo`/`ConcurrentCallInfo` 结构、`Prog.DCTInfo` 字段、`UpdateDCTFromFeedback`/`ClearDCTInfo` 函数、三处构造块）、`src/syz-fuzzer/proc.go`（triageInput 归因块）

**背景**：`UpdateDCTFromFeedback` 在 triageInput 中按 `DCTInfo`（插入时的静态快照：root/variant/路径）对 DCT 权重做一次归因（UpdateWeight + MarkYield）后 `ClearDCTInfo` 清除。两个问题（修正版）：
1. **实现与意图错位**：原设计意图是"DCT 插入后**第一轮 execute** 的新覆盖率 → 更新 DCT，随后无条件清除 DCTInfo（快照只活一轮）"——但被删实现放在 **triageInput**（验证后）——triage 时程序已历经多次执行/突变，静态快照与当前状态脱节。在**严格第一轮语义**下"静态快照随程序演化错位"不成立（第一轮执行时快照即当前状态，无演化空间）；
2. **归因模糊（核心障碍，对任何覆盖率→组合归因方式都成立）**：覆盖率是**程序级**信号，组合是程序的一个**子集**——新覆盖率可能来自组合的调用（归因正确）或主干调用（归因错误）——无法区分——整个执行的功劳归给组合 = **系统性过度奖励**。详见 #21。

**方案**：删除静态归因机制——`DCTInfo` 结构/字段/函数/构造块全部移除（22 处）；triage 成功仅入 corpus、不做归因——DCT 方向 1/2 学习完全由动态 `feedbackDagPairs` 驱动（#16 设计）；"清除时机"问题随机制消失。

**验证**：全仓零残留（grep `DCTInfo`/`DistributedChoiceInfo`/`ConcurrentCallInfo`/`UpdateDCTFromFeedback`/`ClearDCTInfo` 无匹配）；全目标构建通过；rand.go 与备份 diff 36 行删除全部为 DCTInfo 相关（无误删）。

**增补（权重奖励恢复）**：#20 删除 DCTInfo 归因时，`UpdateWeight`（权重 +1）的调用点被连带删除，DCT 权重一度只剩降权路径（只减不加）。已恢复：**正向奖励并入 `MarkYield`**——每次新颖 pair 产出 → 组合权重 +1（上限 `maxComboWeight=100`，防御性；自然上限是组合对应 pair 类型空间）；`Explored=true` + `NoYield=0` 不变。同时清理冗余的 `TotalWeights` 字段（只写不读——`chooseVariant` 现场累加）与 `UpdateWeight` 函数（含 LCS 封装）。降权下限语义：`w > noYieldDelta` 渐进 −5；`w ∈ (1, noYieldDelta]` 再次触发降权时**一步到 1**（消除中间冻结态）；`w=1` 为最终下限（产出后 MarkYield +1 可回升）。

**增补（反馈按 pair 类型计数）**：`filterNewDagPairs` 增加**按哈希去重**——同特征（同哈希）的多对调用只保留一条。语义：反馈（`MarkYield` 权重 +1、形态层加权、hb/cc 统计、深度桶统计）按**新颖 pair 类型**计数而非实例数——与新颖位集合（newBits）和 `dag pair signal`（按 newBits.Len()）的集合语义统一（去重前 hb/cc 计数与 pair signal 口径不一致：实例数 vs 类型数）。量级说明：程序规模（`RecommendedCalls=30`/节点）约束下，同类型对实例数个位数到十几——按类型计数后进一步收敛为 ret 桶/时序/路径关系的真实类型多样性（各类型产出各 +1，合理）。

---

## 21. 覆盖率反馈驱动 DCT 更新（候选增强——实验观测先行）

**背景**：DCT 当前唯一学习信号是 DAG pair 新颖性（#16/#20 后，`feedbackDagPairs` 精准归因）。覆盖率（KCOV）作为 DCT 信号的评估（基于 #20 的修正讨论）。

**可行版本**（优于被删的 triage 版本）：execute 第一轮 + 动态组合记录——
- 时机：DCT 插入后**第一轮 execute**（无演化空间，组合即当前状态）；
- 机制：插入时记录"本执行插入的组合集合"（轻量记录，替代已删的 DCTInfo）→ 该轮产生新覆盖率 → 弱奖励集合内组合；
- 归因模糊（核心障碍）：覆盖率程序级 vs 组合子集——系统性过度奖励（新覆盖率可能来自主干调用而非组合）——但方向性偏差 + NoYield 自修正（无产出组合最终仍降权）使危害有限。

**设计要点**（若实施）：
- 弱奖励：覆盖率奖励增量 < DAG pair 奖励（主信号不变）；
- 与 NoYield 交互：覆盖率奖励延缓降权但不豁免（无产出组合最终仍降权）；
- 与 Explored 交互：覆盖率奖励是否置位 Explored 需单独决策。

**优先级：低**——DAG pair 已是精准主信号；覆盖率是异域弱信号（代码路径维度），归因模糊（#20 问题 2）。

**前置实验**：dashboard 记录"新覆盖率执行的组合分布"（关联强度观测）→ 数据支撑后再定实施。

---

## 22. Offset 特征维度（已实现）

**位置**：`src/executor/hmdfs_trace.bpf.c`（事件结构 + off 采集）、`src/prog/dag.go`（`DAGVertex.Off/Size`、`offsetBucketOf`、`featuresOf` 布局 15 位）、`src/prog/prog.go`/`src/pkg/ipc/ipc.go`（事件结构解析）

**动机**：并发写冲突是分布式文件系统数据丢失/错乱的核心 bug 面——但原有特征向量（FuncID/Ret/Depth/IsDir/Persist/Temporal/PathRel）**无法区分"同位置并发写"（真实数据竞争）与"不同位置并发写"（独立区/合并）**——offset 维度是数据竞争反馈的关键缺失。

**特征定义**（3 bit，5 值）：
```
NA（无偏移语义——mkdir/rmdir/rename/unlink/stat/chmod/getdents64/fsync）
0（pos == 0——文件开头，竞争最危险区）
中部（0 < pos < size−r——整页区域）
尾部（size−r ≤ pos < size——最后不满页，r = size % 4096）
越界（pos ≥ size——稀疏写/truncate 扩展）
```

**分桶推导（尾部 = size % blocksize，无任意阈值）**：
- `HMDFS_PAGE_SIZE = 4096`（hmdfs.h:36）——页写回粒度
- HMDFS 写回显式三分支（file_remote.c `hmdfs_get_writecount`：`pos >= size → count=0` / `size < pos+PAGE_SIZE → count = size−pos`（最后不满页）/ 整页写）——**offset 桶直接对应内核行为**
- 尾部边界 = 块对齐点（`size − (size % 4096)`）——`size % 4096 == 0` 时尾部自然消失（无最后不满页）——无任意常数

**采集链路**（已验证）：
- write/read/pwrite64/pread64（都走 merge_write_iter/read_iter，file_merge.c:537/542 传 `&iocb->ki_pos`）：`BPF_CORE_READ(kiocb, ki_pos)`（args[0]）
- truncate（setattr）：`BPF_CORE_READ(iattr, ia_size)`（args[2]）——**仅当 `ia_valid & ATTR_SIZE`**（chmod/utime 的 ia_size 无效——eBPF 侧检查消除垃圾值噪声）
- 事件结构 +8 字节（72→80）；`DAGVertex.Size` 构建时从 post-exec fsMd 填充（分桶参照，**不进特征向量**）

**已知近似**：
1. **kretprobe 读到结束偏移**：VFS 标准推进 `*ppos += written`（file_local.c:145 vfs_iter_write）——ki_pos 在返回时 = 起始 + 长度——多数情况与起始同桶（粗分桶）；**跨 size 边界时恰好捕捉"扩展"语义**（pos+len ≥ size 落入越界桶——对应文件扩展行为）；
2. **truncate 与 chmod 共用 FuncSetattr**（FuncID 层已合并）——ia_valid 检查已排除非 truncate 的 setattr，但其 `off=0` 在 Go 侧被 `isOffsetFunc(FuncSetattr)=true` 判为真实偏移 → 落入 **Zero 桶**——与真实 `truncate@0` 在 offset 维混叠（低影响：FuncID 层本就合并，offset 桶对 setattr 的细分是近似）。

**类型空间增长估算**：offset 维使读写调用参与的 pair 类型 ×4（0/中/尾/越界）——读写-读写对 ×16、读写-其他 ×4、整体 ×2-3——需实验验证新颖性收敛速度（与深度分桶并行观测）。

**实验验证计划**：对比 offset 维加入前后——`dag pair signal` 收敛曲线、`dag depth 1/2/3` 分布、bug 发现率（尤其同 offset 并发写场景）。

**补注（2026-08）——setattr 反馈盲区**：`callNameOfFunc`（dag.go:341-366）无 `FuncSetattr` 分支——truncate/chmod/utime 事件产出的 pair 在 `DagPairToVariant` 恒 `ok=false`——这些组合在 DCT 中**永远收不到产出信用**（MarkYield/形态学习）——方向 1 探索池永不枯竭（放大 #7）。修复需顶点携带调用名（结构性改动）——记录待办（#27-P2）。

---

## 附录：仅用 pair 反馈、关闭 schedule 统计

DAG 反馈由两部分组成：逐对（pair）新意 bits（驱动 corpus 入队 + 统计）和整体 schedule 哈希 bit（仅统计，不驱动 corpus）。新增 pair 必然导致 schedule 变化，但反过来不成立——schedule 变化也可能来自无新增 pair 的**组合变化**（已有 pair 的重新共现、pair 消失等），因此 schedule 并非 pair 的纯冗余。设计上仍不驱动 corpus，原因：组合空间为指数级（已知 pair 的任意子集都是新 schedule），驱动会导致几乎所有执行判为新颖、稀释种子质量并放大 smash 负担；且 schedule 为集合哈希（排序后），编码"哪些 pair 存在"而非对间结构，信息增量有限。schedule 统计用于观测组合多样性，pair 新意驱动探索。

如需只保留 pair 反馈（如消融实验），传 `-EnableDagScheduleFb=false`：

- pair 反馈（corpus 入队、`dag pair signal`、`dag corpus`）不受影响；
- `checkNewDagSchedule` 不再调用，`maxDagSchedSignal` 不增长，dashboard 的 `dag schedule signal` 恒为 0；
- `schedBit` 仍被计算但不消费（微秒级开销，忽略）。

实现改动（**已接入**，共 3 处）：

1. `src/pkg/ipc/ipc.go` — Config 加 `EnableDagScheduleFb bool`
2. `src/syz-fuzzer/fuzzer.go` — flag `-EnableDagScheduleFb`（默认 true）+ `config.EnableDagScheduleFb = *flag`
3. `src/syz-fuzzer/proc.go` — execute() DAG 块：

```go
if proc.fuzzer.config.EnableDagScheduleFb &&
    proc.fuzzer.checkNewDagSchedule(info.DagScheduleBit) > 0 {
    atomic.AddUint64(&proc.fuzzer.dagSchedCount, 1)
}
```

更小的替代（不可切换）：直接删除 proc.go 的 `checkNewDagSchedule` 两行 + poll 里 `stats["dag schedule signal"]` 一行。不推荐：研究场景需要对比实验，flag 方案无需改代码即可切换实验组。不建议彻底移除 schedule 链路（涉及 ~15 处改动、不可逆、收益为零）。

---

## 23. bucketizeRet 将 write/read 成功判为 Failure（已修复）

**位置**：`src/prog/dag.go` `bucketizeRet`

**现象**：write/read 成功时内核返回**写入/读取字节数**（>0，file_remote.c:540 `return ret`）——`bucketizeRet` 只特判 0/-17/-2——正数落入 `RetFailure`——`isSucceededModifier(write)` 恒 false——**write 参与的 HB/并发对全部丢失**（stash 写-读一致性反馈的核心维度缺失）。

**修复**：`case ret > 0: return RetSuccess`——仅 write/read 返回正数（其他函数成功返回 0——不会误判）。

**影响**：write 修改者恢复；修复后首轮写-读对作为"新颖"信号涌入（一次性）。

---

## 24. rename/delete 时间线混入失败事件（已修复）

**位置**：`src/prog/dag.go` BuildVertices（renameTLByPath/deleteTLByNode）

**现象**：时间线构建不看 `ev.Ret`——失败的 rename/unlink 也进时间线——`renamePathAt` 把后续事件的路径错误重写为不存在的 newPath（文件实际还在 oldPath）——顶点特征/PathRel 全错。失败 rename 在 fuzzing 中常见（随机路径）。

**修复（方案 b——时间线保持完整）**：`renameEvent`/`deleteEvent` 加 `ret` 字段（全部事件进时间线——事实完整）——`renamePathAt`/`deletePathAt` 应用时跳过 `ret != 0` 项（失败的 rename 不改变路径）。

**语义**：失败事件本身仍作为顶点参与配对（HB 接收点/并发对——ExtractPairs 的 B 端不要求 succeeded）——仅路径归一化区分成败。

---

## 25. csan bug 后文件树不一致——集体重启（已修复）

**位置**：`src/syz-fuzzer/proc.go` executeRaw（csan bug 检测处）

**背景**：hmdfs 的 merge_view 每轮不清树（`empty_dir` 被注释——executor.cc:1383——历史遗留）——csan 检测到节点间 fsMd 不一致后文件树保持脏状态——后续用例在脏树上继续测试（污染执行环境）——且可能每轮重复报 bug。

**修复**：csanPassed=false 时——`saveCsanBug` 落盘后 `os.Exit(0)`——利用 manager vmLoop 的固有集体重启（runFuzzer 返回 → 停全部 VM → 重启全部，manager.go:648-739）——qemu `-snapshot` 恢复文件树到镜像初始状态。

**语义**：正常退出（exit 0）——manager 不记崩溃（res.crash == nil，manager.go:760）——干净重启；bug 信息由 saveCsanBug 保留；corpus 由 manager 侧保留（重启后恢复）。

---

## 26. 三个误判关闭记录（防重复调查）

以下三项 review 时被误判为问题——亲验推导后确认**不成立**，记录证据链：

**P3（fsMd 合并 map 随机序）**：`nodeType/pathSize` 的 last-writer-wins 实际由外层 fsMds 数组序决定（确定）——且 **csan 前置过滤保证进入 DAG 的 fsMd 节点间 Mode/Size 一致**（compareFileMeta 比较 Mode/Size——不一致 → csanPassed=false → DAG 不跑，proc.go:1222）——无抖动。

**P4（相邻窗口边界翻转）**：`A.Etime == B.Stime` 时 `overlap`（双向 `<=`）为真——走并发分支——不落 default——default 只在 B 完全先于 A（`A.Stime > B.Etime`）时走——方向正确。

**P7（BPF ring 跨轮 writepage 混入）**：BPF ring 只有 15 个 merge 函数的 kretprobe（func_id 0-14）——**不含 writepage**（func_id 15 是 perf drain 时标记的，hmdfs_trace.cc:246）——轮间残留（同步函数）被 `matchEventToCall` 窗口过滤丢弃；perf ring 轮间 DISABLED（上轮 stop 的 `PERF_EVENT_IOC_DISABLE`）——不采集——无 writepage 残留。

---

## 27. 记录项（未修——待评估）

| # | 问题 | 位置 | 影响 |
|---|---|---|---|
| P2 | setattr 反馈盲区：`callNameOfFunc` 无 `FuncSetattr`——truncate/chmod 组合在 `DagPairToVariant` 恒 ok=false——DCT 无产出信用（#22 补注） | dag.go:341-366 | 方向 1 探索池永不枯竭（放大 #7）——需顶点携带调用名（结构性改动） |
| P5 | **writepage 未建立到 write 系统调用的映射**：路径解析仅靠 post-exec fsMd 的 `inoToPath`（rename 前的事件无法回退到旧路径）；且无 CallName——不参与 `DagPairToVariant`（写回维度不进 DCT 反馈、不指导突变） | dag.go:200-219 | rename 场景路径错（罕见）+ 写回维度 DCT 学习缺失。映射需同 ino+时间关联——但异步写回跨轮（write 在上轮、完成在下轮）导致匹配不可靠——**暂不实施**（异步语义与同步函数不同，待后续评估） |
| P6 | DCT 归因模糊：任何新颖对都归因到 (root, variant)——不区分是否本执行插入的组合产生 | proc.go:682-690 | 系统性设计近似（#20/#21 已讨论） |
| P9 | write/pwrite64 归因坍缩：FuncWrite → "write"——pwrite64 组合收不到产出信用 | dag.go:353-355 | 与 P2 同因（顶点无调用名） |
| P10-P20 | 其余低危：MaxDagCorpus gate 非原子、pair 哈希 32 位截断碰撞、同调用多事件成对噪声、O(n²) 全对遍历、schedule 哈希折叠、perf 时钟域未验证、统计口径（#3）等 | — | 观察即可 |
