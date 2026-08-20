# HMDFS 种子生成与突变问题清单

> 来源：2026-08-10 两轮全面 review（结构/一致性/边界 + 手工调用构造，4 路代理并行审查 + 人工核实）
> 状态：✅ 已修复 / ⏳ 待处理 ｜ 批次：批1=崩溃级 / 批2=语义级 / 批3=低危级
> 计数：已修复 2 + 崩溃级 5 + 语义级 16 + 低危级 25 = 48 项

## 一、已修复（✅）

**F1. multi-file stash：Cids 越界 panic + Node_num=2 多 prog**
- 位置：generation.go:70 / 148-151 / 200（三处）
- 描述：mode 2（multi-file）的 `numFiles` 硬编码 3/2，与 `Cids` 长度（=Node_num）无约束——`filePaths[i] = selectFileInOneNode(hmcfg, Cids[i+1])` 在 Node_num≤3 时越界（官方配置 Node_num=3：i=2 → `Cids[3]`）→ Go 数组越界 panic（stash 生成 1/3 概率崩溃）；另有 Node_num=2 时读循环 `startIdx=1` 生成第 3 个 prog——ps 数 > executor 数 → fuzzer 挂死
- 状态：✅ 已修复（三处）——(1) 入口守卫 `mode==2 && Node_num>=2`（Node_num=1 自然落单文件路径，等效 mode 0）；(2) `numFiles = Node_num-1`（Node_num<4 时——`Cids[i+1]` 恒安全：3→2、2→1、4→3 原行为）；(3) startIdx 恒 2（Node_num=2 时 ps=[p0,p1] 双写+故障、无读——与单文件 mode1 一致）。自适应已验证：netInsertPos=Intn(len*2)、写/close/read 循环全基于 len(filePaths)——无其它硬编码 3

**F2. timeout 分支 GeneralBarrierPos nil 赋值 panic + 表实时维护**
- 位置：rand.go:1637 + mutation.go（updateBarrierPosTable 新增；SyncPos/insert/remove 表同步）
- 描述：timeout 分支 `ps[0].GeneralBarrierPos[0] = barrierPosSlice`——`GeneralBarrierPos`（[][]int，prog.go:37）全仓无任何初始化，p0 构造后为 nil——`nil[0]` 赋值 → panic（dcache 生成 1/3 概率、占 hmdfs 新种子约 1/12——timeout 线从未产出过种子）；且 mutateDcacheSyncPos 移动 barrier 后不维护位置表（表设计意图为实时状态记录）
- 状态：✅ 已修复（panic + 表维护）——(1) `append` 初始化表 `[pos_0..pos_{N-1}]`（每 prog 一条目）；(2) 新增 `updateBarrierPosTable(ps, pidx, shiftPos, delta)`——插入/删除位置 ≤ barrier 位置时偏移 delta（含表空/pidx 越界守卫）；(3) SyncPos 移动后绝对更新 `GeneralBarrierPos[0][progIdx] = newCallIdx`；(4) insertDcacheCall 插入后 +1、insertGetdents64Call 三调用块插入后 +3、removeDcacheCall 删除后 -1（签名加 ps/pidx，mutateDcacheOpType 改 pidx 循环）；(5) swap 无需更新——候选 `syz_failure` 子串过滤已排除 barrier（亲验）；修复后 timeout 种子首次存活 → mutateDcacheSyncPos（已匹配 syz_failure_barrier）首次真正工作

## 二、崩溃级（批1，5 项）

**C1. recv/send 同步死锁（server_num=0 时）**
- 位置：common_linux.h:187-195 + rand.go:1474-1483
- 描述：`syz_failure_recv(0)` 等待含 executor0 自身 bit0，但 node0 用 send 设的是 MAXCLT 位——官方配置（server_num=0）下 stash 种子执行必挂；synchBit 跨执行不清零使死锁间歇性
- 状态：✅ 已修复（executor 侧 `syz_failure_recv` 跳过自身 bit：`executor_index >= server_num && i == executor_index - server_num` 时 continue——server_num≥1 零回归；"synchBit 不清零"经核实不成立——ipc.go:1876 每轮 c.exec 重建零值 execCtl 已清零，无需改动）

**C2. persistence/inodeops 空 ps → `ps[0]` panic**
- 位置：proc.go:1012/1149 + rand.go:1646-1662 + generation.go:269-291
- 描述：persistence 三处提前返回空 ps（Persistence_dir 空/ownerNode 越界/filesInDir 空）、inodeops fallback 返回空——调用侧无条件访问 ps[0]；delete 种子清空 persistence 目录后可自触发
- 状态：✅ 已修复（调用侧兜底：execute/executeRaw 开头 len(ps)==0 返回；生成器降级：filesInDir 空强制 create 分支（修复误伤）、dcache persistence 空回退 dropPush、inodeops fallback 目录改 "merge_view" 相对根）

**C3. RandSetExcept off-by-one panic + 0 基偏移**
- 位置：rand.go:1108-1142 + mutation.go:3630（mutateStashTargetNodes）
- 描述：`setSize` 可 > `valRange`（1/Node_num 概率）触发 panic；返回 0 基值未加 startRange——节点选择含自身、漏末尾节点
- 状态：✅ 已修复（RandSet/RandSetExcept 统一返回绝对索引 startRange+val——三个 startRange=0 调用者零回归；3630 加 Node_num≤1 守卫 + setSize 改 Intn(Node_num-1)）

**C4. moveFailCalls 越界 panic**
- 位置：mutation.go:1620-1639
- 描述：`oldPos+6` 无上界检查；`newPos >= len-5` 时 `remainingAfter[:delta]` 越界——与陈旧 GeneralFailPos 联动可达
- 状态：✅ 已修复（校验改 `oldPos+6 > len || newPos+6 > len`——一个检查覆盖两处越界；与 S6 先写后移联动：越界时安全返回 false，元数据陈旧累积留待 S6）

**C5. 编码层 1MB vs 4MB 不一致**
- 位置：encodingexec.go:55 vs executor.cc:202 / ipc.go:238
- 描述：executor 输入上限已改 1MB（上游 4MB），Go 侧 ExecBufferSize 仍 4MB——stream 超限 → ErrExecBufferTooSmall → log.Fatalf（与 mutateWriteLength 变长 data 联动有触发面）
- 状态：✅ 已修复（Go 侧对齐 1MB——与 tao 的输入链 1MB 意图一致（executor kMaxInput + ipc inmem 配套降；4MB 为上游原值遗留，"keep in sync"失联）；输出侧 16MB（kMaxOutput/outmem，fsMd dump 需求）不受影响；超限行为从"编码成功→executor failmsg"提前为"Go 侧确定性 ErrExecBufferTooSmall"）

## 三、语义级（批2，16 项）

**S1. dropPush 固定 4 prog vs 官方 Node_num=3**
- 位置：rand.go:1777-1849
- 描述：p3（unlink 后验证 stat）无对应 executor——删除后验证永不执行，dropPush 退化为 create+stat
- 状态：✅ 已修复（阶段数按 Node_num 动态化，保验证链：≥4 完整四阶段；==3 create→unlink→验证 stat（跳中间 stat，保删除可见性验证——bug 面更大）；==2 create+填充 stat（保 create 推送验证）；==1 仅 create。语义说明：dropPush 测 HMDFS 的 drop_push 失效广播机制（hmdfs_client.c:1031 send_drop_push / hmdfs_dentryfile.c:2423 目录变更触发 / hmdfs_server.c:2086 接收丢弃缓存）——目录变更后其它节点 stat 验证失效正确性）

**S2. persistence ownerNode≠0 跳过逻辑错位**
- 位置：rand.go:1691-1704 / 1725-1735 / 1760-1770
- 描述：操作固定节点 0、stat 循环按 ps 索引错位——仅 ownerNode==0 时正确；owner≠0 时"owner 执行变更、非 owner 验证"语义完全走样
- 状态：✅ 已修复（三处子测试统一改为 ps 按节点索引定长排布：`ps := make([]*Prog, Node_num)`、操作 prog 放 `ps[ownerNode]`、验证 stat 放 `ps[i]`（i≠ownerNode）——owner=0 原行为零回归，owner≠0 语义修正；Review 补两处：delete/rename 的 filesInDir 空 `return ps` → `return nil`（防返回全 nil 数组致外层 IsDCache 赋值 panic）；删外层 filesInDir 空提前返回——使 C2 的 opType=0 强制 create 分支真正生效（此前为死代码））

**S3. "/merge_view" 前导斜杠（11 处）**
- 位置：rand.go:1272/1282/1584/1779/3814/3871/3906 + mutation.go:4325/4372/4423/4458
- 描述：FileTree 用相对路径 "merge_view"（无前导斜杠），fallback 用绝对路径 "/merge_view"——executor cwd 是挂载点，绝对路径解析到根目录 → 全 ENOENT
- 状态：✅ 已修复（11 处全部 `"/merge_view` → `"merge_view`——与 FileTree/executor cwd 约定一致；原文档清单 6 处不完整——实际 11 处，本次补全）

**S4. stash 元数据不序列化**
- 位置：encoding.go（Serialize 只含 Calls）
- 描述：IsStash/SyncIdx/GeneralFailPos/HasNetFail 语料库 round-trip 丢失——取出后退化普通 Mutate，同步配对被破坏
- 状态：✅ 已修复（最小修：仅序列化类型标记 4 bool——`# prog-meta: {"is_stash":true,...}` JSON 行，仅非默认时输出（普通 prog 文本不变、旧语料库兼容）；parseProg 注释分支识别解析（容错）。SyncIdx/位置表不编——已核实无消费（生成器用新编号、InsertFailure 用局部计数）；单 fuzzer 会话内 corpus 为内存对象元数据完好——影响仅重启后初始退化，本次修复使重启后恢复结构化突变（MutateStashProg 4/5 槽位 + dcache 突变；FailPos 仍失效——位置表不编）；新增 round-trip 测试 TestSerializeMetaRoundTrip）

**S5. updateGeneralFailPos off-by-one**
- 位置：mutation.go:1919-1930
- 描述：`targetIdx = progIdx*100+syncId` vs 条目编码 `node*100+{1,2}`——正确应为 `+syncId+1`——移动 END sync 命中 START 条目、移动 START 无条目命中——每次 SyncPos 突变后 GeneralFailPos 必然过期
- 状态：✅ 已修复（`targetIdx := progIdx*100 + syncId + 1`——syncId=0→101（start 键）、1→102（end 键），与 generation.go 的 `node*100+{1,2}` 编码对齐；dcache 侧无 GeneralFailPos 不受影响；连带修复 L1（mode 0 幽灵条目）使 mode 0 更新不再被幽灵拦截）

**S6. mutateStashFailPos 先写元数据后移动 + clamp 硬编码**
- 位置：mutation.go:1584-1618
- 描述：(a) 先写 failPos[1]/[3] 再 moveFailCalls——后者失败时元数据永久陈旧；(b) `newFailStart<1` 时硬编码 `newFailEnd=6`——假定窗口恒在 [1,6]
- 状态：✅ 已修复（(a) 先 `moveFailCalls` 成功后再写元数据——失败 `continue` 尝试其它窗口条目（multi-file 多窗口）——与 C4 形成闭环（移动安全 + 元数据不再陈旧）；(b) clamp 改 `newFailEnd = newFailStart + (failEnd - failStart)`——保持窗口长度，不硬编码 6；mutateDcacheFailPos（已注释死代码）同步同款修复——避免未来恢复启用时携带旧 bug）

**S7. insertStashCall 可插进 6 块故障窗口**
- 位置：mutation.go:2019-2027
- 描述：insertPos 范围覆盖 recv/down/send/recv/up/send 窗口内部——窗口变 7 调用后 moveFailCalls 必然拆散 recv/send 配对 → 死锁风险
- 状态：✅ 已修复（方案 X：插入位置分段剔除窗口区间（窗口前段 [validStart,failStart) + 后段 [failEnd+1,validEnd) 随机选段）——窗口恒 6 调用，C4/S6 闭环保持；窗口前插入时同步表（failPos[1]/[3]+1）——"故障期间写"场景放弃（经评估：插入窗口内本身不死锁，死锁来自 moveFailCalls 6 块硬编码拆散变长窗口——权衡后选结构简单）；Review 补 O2：addStashReadVerification 插入 reader 后同步表（新增 shiftGeneralFailPos——插入位置 ≤ 记录位置时偏移 node 条目 +1——与 F2/S7 表维护模式统一——reader 索引经 readersIdx 并行记录）

**S8. persistence rename 验证目标错误 + delete 含目录条目**
- 位置：rand.go:1656 / 1694 / 1763
- 描述：rename 后 stat 目录而非新文件名（可见性验证为空）；GetEntriesUnderDir 含目录——delete 对目录 unlink 必 EISDIR
- 状态：✅ 已修复（3 处：create/rename 的验证 stat 目标改为 newFileName（新文件/重命名后可见性验证恢复）；filesInDir 改用 GetFileEntriesUnderDir（候选仅文件——delete 不再 EISDIR、rename 不再改目录）。delete 的 stat targetFile 本就正确未动）

**S9. DCT 突变 fd 按 basePath 错配**
- 位置：mutation.go:3222-3233（insertCallFromDCT）
- 描述：按 basePath 找 fd 但调用作用于 concurrentPath——路径关系被静默丢弃；目录基路径下 fd 是 O_DIRECTORY → EISDIR
- 状态：✅ 已修复（findOpenFdForPath 查找路径改 concurrentPath（两处：主查找 + 时序回退分支）——变体的实际读写作用于其派生路径（PathChild/Sibling/NoRel 关系保留）；找不到时 useExistFd=false 自包含 open 回退不变；concurrentPath==basePath 时行为不变；dir-only 变体不经 fd 分支）

**S10. PredefinedPattern 一半恒失败**
- 位置：distributed_choice.go:934-1041
- 描述：concurrent_mkdir_same/mkdir_stat_concurrent 恒 EEXIST；create_unlink_same 恒 EISDIR（creat 打目录）；rmdir_mkdir_child 常 ENOTEMPTY——fileops pattern 无此问题
- 状态：✅ 已修复（根调用路径预处理——mkdir/creat 作为根时生成新名（创建成功路径）、rmdir 作为根时选空目录（GetRandomEmptyDir——接口已存在：删除成功路径；ENOTEMPTY 否则必败）——预处理在调用方完成（generateCallByName/generateCallFromPatternOp 无感知——不违背 PathRelation 语义：变体仍打已有路径（并发冲突测试价值）；pattern 侧 basePath 更新使后续 client 的 PathSame 共享根新名（并发 mkdir 同一新路径——原子性可测）；DCT 变体 PathSame 从 p0 提取根路径（已有机制）。附带：generate_test_files.py 的 --num-empty-dirs 默认 5 → 100（1 小时重启周期内 rmdir 根成功路径供给——空目录零成本；FileTree 经 SyncFileTreeFromFsMd 同步删除——空目录真实消耗）。相邻修复：unlink 从 FileOrDirCalls 移入 FileOnlyCalls（unlink 不能删目录——目录基下其 PathSelf/Same/Parent 变体此前未排除 EISDIR 恒失败；顺带根 unlink 路径选择走 GetRandomFile——正确；IsFileOrDirCall 无调用者零回归））

**S11. fallback 普通 Mutate 破坏结构**
- 位置：proc.go:643-649
- 描述：全局 math/rand 不可复现；可删 open/改 fd 参数（读验证链破坏）；可动 ps[0]（ServNum=0 时动到故障窗口 prog）；mutateFailPos 移动 sync 调用不更新元数据
- 状态：✅ 已修复（方案 B 修正版——风险链重推演：普通 Mutate 的随机破坏是 Monarch 原始设计的有意组成部分（fallback 兜底——失败路径覆盖多样性——非 bug）；表陈旧仅致 FailPos 突变移动普通块（无意义但不致命——窗口本体不动、recv/send 配对保持、无死锁路径——死锁链（S7 插入窗口场景）已修）。修复：hmdfs && srvNum==0 时 randIdx ∈ [1, len)（ps[0] 主 prog 保护——故障窗口/位置表/同步结构不动——len==1 时跳过 fallback）——ps[1+] 的随机破坏保留（原始设计）。`proc.rnd.Intn` 已回退为 `rand.Intn`（可复现性非目标——种子被语料库记录、复现依赖种子回放——生成随机性无需一致）；观察项（proc.go 其它全局 rand 使用 seedType/repeatNum、rand.Seed 重设）关闭——非功能问题；subset1/subset2 死代码链（enumFailures 已注释）记录为清理候选）

**S12. creat mode 笔误 `066` → `0o666`**
- 位置：rand.go:3330（generateCreateFileCalls）
- 描述：Go 中 `066` 是十进制 66=0o102——文件属主无 r/w（umask 后实际 0o100）——后续 open 读写 EACCES（当前只 stat/unlink 影响有限）
- 状态：✅ 已修复（`MakeConstArg(modeType, DirIn, 066)` → `0o666`——umask 022 后实际 0o644（属主 rw ✓）——影响面：dcache persistence create / dropPush p0 的创建文件——当前 stat/unlink 不受影响，修复使未来 open 读写路径可用）

**S13. genNanosleepCall timespec 断言失效（潜伏）**
- 位置：mutation.go:4587-4627
- 描述：timespec 字段是 ResourceType，generateTimespec 产出 *ResultArg——断言 *ConstArg 恒失败——HmdfsTimeoutTable 超时值永远写不进；唯一调用者 mutateDcacheDelay 已注释——恢复启用前必修
- 状态：✅ 已修复（断言 `*ConstArg` → `*ResultArg`（2 处）——编码链确认：`ResultArg(Res=nil)` exec 编码写 `a.Val`（encodingexec.go:258-260）——超时表值生效；`useHmdfsTimeout=false` 分支的随机 sec/nsec 同步生效（行为合理）；仍为潜伏（唯一调用者 mutateDcacheDelay 注释中——恢复启用即正确））

**S14. MutateFileopsProg 系死代码炸弹（潜伏）**
- 位置：mutation.go:5565-5629 等
- 描述：findAvailableFd 取全程序第一个 open 的 Ret，无插入位置约束——插入到 open 前 → use-before-produce → 序列化 panic——当前全部为死代码（活动入口是 MutateFileopsWithDCT）——一旦启用即崩
- 状态：✅ 已修复（方案 A——死代码可用化，6 处：mutateFileopsFsync 插入位置约束在第一个 open 后；insertConcurrentRead/Write 改用 findOpenFdForPath（位置感知）+ genPreadCallWithFd/genPwriteCallWithFd（fd==nil continue）；insertOverlappingWrite 的 fd 位置感知 + genPwriteCallWithOffsetAndLength 改签名接收 fd（唯一调用者已核实）；mutateInodeOpsSequence 的 swap 候选排除 open/close（对齐 swapDcacheCalls）；genPreadCallWithPath/genPwriteCallWithPath 修复 filePath 未用（findFdForPath——L15 顺带闭合——无调用者但保持可用））

**S15. timeout barrier 位置无观察点**
- 位置：rand.go:1615-1616
- 描述：barrier 在 rmdir 之后且其后无调用——rmdir 与各节点 stat 的竞态无同步保障，验证确定性弱
- 状态：✅ 关闭（设计确认——非缺陷）：barrier 置于末尾是**有意设计**——初始种子中 barrier 不干扰各节点目录操作（无同步基线）；**barrier 位置是突变探索空间**（mutateDcacheSyncPos 动态定位 barrier 移动 ±1——各位置的同步影响由突变探索）；"删除观察"由 **checker 的 fsMd 抓拍**完成（执行结束时各节点树状态对比——删除传播不一致可检测——无需显式 rmdir 后 stat）。代理建议的"barrier 移前/后置 stat"（方案 Z）撤回——会破坏基线语义与探索空间。关联：L17（barrierCount 跨执行不清零）仍为独立低危项

**S16. GetPathsForRenameVariant 空路径被覆盖**
- 位置：distributed_choice.go:1634-1650
- 描述：srcPath=="" 时守卫被 else 分支覆盖——生成 `rename("", "._renamed_xxx")`——空路径恒 ENOENT，并发 rename 测试失效
- 状态：✅ 已修复（方案 B——无匹配时不产出：GetPathsForRenameVariant 返回 `("", "")`（明确失败标记——不再派生假路径——路径关系语义保持——退化 basePath 会改变 PathRelation（PathChild 变体实际打 basePath）污染 DCT 反馈）；3 个调用方加跳过检查（insertCallFromDCT / generateFromDistributedChoiceTable：`concurrentPath=="" → continue`——跳到下一节点（与 variant==nil/fd==nil 既有模式一致——root 已插入——突变不整体失败）；generateCallFromPatternOp：`path=="" → 返回空`——该 op 跳过、pattern 其它 op 继续）

## 四、低危级（批3，25 项）

**L1. mode 0 幽灵 failPos 条目** — generation.go:118 —— syncstartidx/syncendidx 恒 -1 无条件 append——updateGeneralFailPos 匹配到错误条目（已随 S5 连带修复：条件 append（值 ≥0 才写）——mode 1 行为不变，mode 0 幽灵条目消除——真实条目（循环添加）不再被拦截）
**L2. idxSlice 含节点 0** — generation.go:82-85/164-167 + mutation.go:3689 —— 故障窗口 targetNodes 含自身 IP（与 mutateStashTargetNodes 排除自身的语义不一致）（已修复：排除执行节点语义——生成器 idxSlice 从 [0,N) 排除执行节点（net_down 恒在 p0——行为与现状等价）；突变器 RandSetExcept 范围改 [0,N-1] + except=pidx（执行节点——当前 pidx=0 行为等价）——未来 net_down 位置变化时自动正确。iptables 语义确认：drop 自身 IP 只禁自连（INPUT -s 自身IP / OUTPUT -d 自身IP——其他节点流量不匹配——无实际效果）——修复为一致性/净化命令集合）
**L3. multi-file 读写同步位置不对齐** — rand.go:3442/3521 —— 写侧 currentPos 硬编码 2（writeIdx*2——假设 2 文件——len≠2 时（Node_num=2 → 1 文件）currentPos 不连续（0,2）——netInsertPos 不匹配时窗口不插入）；读侧 currentPos 1 基偏移（++ 后比较——netInsertPos=0 时 sync 不插入）（已修复：写侧 `writeIdx*len(filePaths)+fileIdx`（动态文件数——len=1 时 0,1 连续）；读侧 `currentPos := -1`（0 基对齐）——len=2 时行为不变、len=1 全对齐。修正：代理原判断"写侧 write-major vs 读侧 file-major 顺序不一致"不成立——两侧遍历顺序本就一致——真实问题为硬编码 2 与 1 基偏移）
**L4. mode1 + Node_num=1 多 prog** — generation.go:100 —— 无条件生成 p1——Node_num=1 时 ps=[p0,p1]（2 prog vs 1 节点——不匹配挂死）（已修复：`if mode == 1 && hmcfg.Node_num >= 2`——与 F1 的 mode 2 守卫对齐——Node_num=1 时退化单写者（ps=[p0]——syncstartidx 保持 -1 幽灵条目不 append、startIdx=2 读循环空、allWriteInfos 无人消费）——Node_num≥2 行为不变）
**L5. multi-file 读节点 O_RDWR|O_CREAT** — rand.go:3499-3500 —— 读验证以写+创建模式打开——可能改变文件状态（已修复：openflag 2|64 → 0（O_RDONLY）、openmode 0o666 → 0o444——对齐单文件读节点（1513-1514）——消除 O_CREAT 在文件缺失时创建空文件的副作用（读验证不再改变文件状态）——残留 3 处 2|64 均为写节点（正确场景））
**L6. genNetDownCall 分配簿记** — rand.go:1161-1180 —— 新建 state 分配地址 + 覆写 data 后 PointerArg 尺寸陈旧（当前单窗口无实际破坏，多窗口/未来 netdelay 会互相覆盖）（已修复：显式构造——按 custData 实际长度分配（MakeDataArg + allocAddr——对齐其他手工构造模式）——消除 generateArgs 随机分配 + 覆写导致的尺寸陈旧（copyin 越界覆盖相邻数据）——独立 state 簿记保留为审计级已知（运行时无害——copyin 先于执行）——调用处签名不变）
**L7. mutateWriteData 不同步读验证 + 指针尺寸** — mutation.go:1784-1813 —— 改 data 后不调 syncStashReadVerification；count 与读验证长度必然失配（已修复方案 A：mutateWriteData 签名加 ps（对齐 Offset/Length）+ 读验证同步（syncStashReadVerification——(off,len) 匹配的 pread 更新）——参数检查提前（消除原"部分修改后回滚"旧逻辑）；新增 reassignDataArg 辅助（mutateWriteData/mutateWriteLength 共用）——改 data 后重分配（新地址按新长度——消除 allocAddr 分配区陈旧导致的 copyin 越界覆盖——与 L6 同类问题）——编码链确认（PointerArg.Size()=8 类型宽度、DataArg.Size()=len(data) 动态——"指针尺寸"的真实问题为分配区陈旧））
**L8. swapDcacheCalls 可换 getdents64 到 open 前** — mutation.go:4136-4164 —— 候选未排除 getdents64（已排除 open/close/syz_failure）——自包含块可能被拆（已修复：候选过滤加 getdents64——消除 use-before-produce（getdents64 的 fd 引用 open.Ret——swap 到 open 前时 SerializeForExec 逐调用即时注册 copyout（encodingexec.go:74-77/98-104）——open.Ret 未注册——writeArg 查 w.args 失败——`panic("no copyout index")`（encodingexec.go:264）——确定性 fuzzer 崩溃——非内核）——swap 候选收敛为目录操作（无 fd 依赖——路径参数随调用移动安全——与 S14 的 mutateInodeOpsSequence 修复一致））
**L9. mutateDcachePathName 不更新 rename 第二参数** — mutation.go:4169-4232 —— 仅 Args[0] 替换；FileTree==nil 时恒失败落回 fallback（已修复 2 处：替换循环加 rename Args[1] 检查（复用 extractPathFromCallByArgIdx/updateCallPathByArgIdx——rename 目标路径同步更新——种子内路径一致）；needDir=false 且 extractFileFromTree 空时直接 return false（移除目录 fallback——消除"文件路径替换为目录"的语义错误——修正代理原"恒失败"判断：实际为多数情况替换为目录））
**L10. Syscalls[id] 无 nil 检查** — rand.go/mutation.go 共 102 处 `Syscalls[sCalls.XxxId]` 直接下标（含 genPathCall 的 FlagsType 断言脆弱）—— ID 解析失败时用错 meta（决定不修——记录：enable_syscalls 列表完整（hmdfs-net-failure.cfg:7-12 覆盖全部所需）——实际不触发；配置写错时 Syscalls[0] 的类型断言 panic 反而暴露问题（静默失败隐藏配置错误）；102 处全量加 nil 检查改动巨大（~200-300 行）收益有限——配置正确性由初始 config 文件保证）
**L11. mutateFileopsOffset 负数回绕** — mutation.go:5345 —— 大 offset 减 delta → 2^64 巨大值（length 有钳制 offset 没有）（已修复：加 newVal 钳制（0 下限——对齐 mutateWriteOffset 1727-1730——stash/fileops 两线 offset 突变一致性）——消除负值回绕 2^64 的无意义调用——死代码内保持 S14 可用化完整性）
**L12. MAXCLT=3 约束** — common_linux.h:140/160-163 —— clt-server_num >= MAXCLT 时 executor fail——Node_num>=5 的 stash 种子会崩；idx*4>=64 位移回绕（决定不修——记录：官方 3 节点完全不触发（barrier 同步无此限制——dcache 线正常）；Node_num>=5 的扩展需求当前不存在。扩展时统一重构方案——计数化（数组标志版）：executeControl 的 `synchBit` 位图 → `syncArriveFlag[MAX_SYNC_POINTS][SUPPORT_VMS]`（无位宽限制——保留"逐节点到达"细粒度语义——recv 逐客户端检查对齐原位图逐 bit）；`syncReleaseFlag[idx]` 放行标志；crash_client 共享 `syncArriveFlag[0]`（原 `1<<clt` 位域共享语义保持）；srvSetupBit 保留（服务器数小——位图够）；每轮清零由 ipc.go:1876 execCtl 零值重建覆盖（顺带解决 L17 族）；改动面 = common_linux.h（4 原语 + 结构）+ ipc.go 结构 + csource 模板（pkg/csource/generated.go:2360-2371——其位布局 `1<<idx` 与 common_linux.h 不一致为预先存在独立观察）+ checker 模拟——评估中等偏大——语义等价、无位宽限制、crash 共享保持）
**L13. O_DIRECTORY 硬编码** — rand.go:2264/2310/2605/2652/3748 + mutation.go:2815/3248/3333/4055 —— amd64=65536 正确；arm/arm64/ppc64le=16384 错（已修复：9 处 flags + 1 处测试掩码（dynamic_mutation_test.go:456）统一改 `target.GetConst("O_DIRECTORY")`（target.go:165——架构常量解析——amd64 返回 65536 行为不变——跨架构动态正确——消除 getdents64/目录 open 的静默失效（O_DIRECT 误用）——非 flags 的 65536（lengthBuckets/uid-gid）保留。附：GetConst 是架构常量解析基础设施——未来 flag 生成/突变策略可直接受益（flag 策略为以后方向））
**L14. getdents64 fd 类型 fd/fd_dir** — mutation.go:4086-4115 —— 运行时无影响；validate/回放路径资源检查失败（已修复：args[0] 改 `MakeResultArg(fdType, DirIn, fd, 0)`——类型 = fd_dir（参数类型）——Res = open.Ret（fd 值）——validate 通过（validation.go:100-102 的 `arg.Type() != typ` 严格类型检查）——修复"重启后含 getdents64 种子被 validate 拒（丢失）"（Deserialize 无条件 validate——encoding.go:271）——运行时不变（executor 只编码 fd 数值）——无需 open$dir（sys.txt:86 存在但不需要——类型包装即可）
**L15. genPreadCallWithPath/genPwriteCallWithPath 忽略 filePath** — mutation.go:5756/5796 —— 用 findAvailableFd 而非按路径取 fd——并发读写不命中目标文件（与 S14 同源；已随 S14 修复：改用 findFdForPath(p, filePath)——位置约束由调用方保证——函数当前无调用者（insertConcurrentRead/Write 已改用 fd 版本）但保持可用）
**L16. mutateStashOpSequence 33% 空转** — mutation.go:1970 —— Intn(3) 只有 case 0/1——1/3 概率落空（fallback 兜底）（已修复：Intn(3) → Intn(2)——insert/remove 等概率——消除 33% 槽位空转——fallback 触发率下降（S11 的 ps[0] 保护仍保留）——操作失败（fd 区间不满足）仍可返回 false——合理）
**L17. executor barrierCount 跨执行不清零** — common_linux.h:134/149/153 —— 首个 timeout 种子后，后续同 idx barrier 立即放行——同步退化为 no-op（✅ 关闭——核实为误判：Go 侧 ipc.go:1876 每轮 `c.exec` 重建零值 `executeControl{}`（barrierCount[64] 全 0）并写入共享内存——executor 看到 hasTestcase 时 barrierCount 已是 0——每轮无残留——与 C1（synchBit）同理（当时同样修正为"Go 侧已清"）——两处 Go/C 结构字段一致（ipc.go:1376-1386 / common_linux.h:128-138））
**L18. GeneralBarrierPos 无消费者（保留备用）** — prog.go:37 —— 已定保留作实时状态记录——无需处理
**L19. multi-file fallback 文件路径可能重复** — rand.go:1271 —— FileTree 缺文件时多文件同路径——测试弱化不崩溃（已修复：selectFileInOneNode fallback 加随机后缀唯一化（`default_test_file_<suffix>.txt`）——多文件 fallback 独立路径。排查结论：selectRemoteFile（1281）同款 fallback 但全为单文件调用（generation.go:74 / rand.go:3848/3905/3940——各一次）不触发重复；目录/操作目标 fallback（"merge_view"/"merge_view/test_dir"）非"多实体分配"场景——无其他同类）
**L20. generateParticularCall(nil state) 脆弱** — rand.go:720-728 —— 仅 0 参数调用安全——未来加参数立即 nil 解引用（已修复：函数内自动 newState（`if s == nil { s = newState(...) }`——1 行）——4 个 nil 调用点（genNetUpCall / genNetDelayDelCall / mutator 侧 2 处）全部受益——防御未来加参数的 nil 解引用——当前 0 参数行为不变）
**L21. executor 无 prog fail 约束** — executor.cc:1340-1343 —— ps 数 ≠ vm_count 时 fail("need_prog")——与 S1（dropPush 4-prog）关联的执行约束（S1 修复后触发面缩小：dropPush 不再产生超数 prog——剩余触发仅 "Node_num > vm_count" 配置不符——仍待处理）（补充：空 prog（p.Calls 空——prog_size=0）也触发该 fail——来源包括 variant==nil 的节点跳过（既有）与 S16 的 rename 变体无匹配 continue（新增来源）——ps 切片恒非空（root 已插入）——不涉及空 ps（C2/deserializeInput 的防护与结论不变）——executor fail 为重启重试（非 fuzzer 崩溃）——低危）
**L22. useExisting 分支 rmdir FileTree 现存目录** — rand.go:1589-1595 —— GetNonTmpDirEntriesUnderDir 可能返回非空目录——rmdir 必败；成功后 FileTree 不同步分叉（已修复：useExisting 候选改用 `GetRandomEmptyDir`（distributed_choice.go:654——S10 确认的辅助）——只选空目录——rmdir 必成功——删除传播验证恢复——范围由 parentDir 下改为 cid0 全局（语义可接受——useExisting 时不 mkdir——位置与 parentDir 无强关联）；rmdir 调用生成（generateRmdirCalls）不改；树分叉弱化说明（SyncFileTreeFromFsMd 每轮执行后同步——分叉窗口为生成到执行间））
**L23. mutateStashTargetNodes 只改第一个 net_down** — mutation.go:3697-3702 —— multi-file 多故障窗口仅第一个被改——能力受限（✅ 关闭——核实为误判：netInsertPos 单点插入（rand.go:3445——`currentPos == netInsertPos`——单一值）——multi-file 写节点单窗口（一个 net_down）——`findCallsByName[0]` = 唯一——"只改第一个" = 改唯一的——无能力缺失；net_down 数量不随突变累积（TargetNodes 只改命令内容不增删调用；S7 修复后窗口内不插调用）；代理"每文件一窗口"假设不成立（netInsertPos 全局单值）；读侧 sync 插入（rand.go:3531 等多点）是 sync 非 net_down——不涉及）
**L24. insertOverlappingWrite src 文件与 pwrite 文件可能不一致** — mutation.go:5491-5502 —— srcFilePath 取第一个 open 路径、pwrite 随机——overlap 相对错误文件计算（已修复：重排——srcCall 先选、srcFilePath 从 srcCall 的 fd 解析（resolveFdToPath——优先；extractFilePath 兜底）——src 文件 = pwrite 实际写的——overlap 相对正确文件（多 open 场景修正）——单 open 行为不变——死代码内保持 S14 可用化完整性）
**L25. MutateGroupPathDynamic rename 只改 arg0** — mutation.go:2970-2995 —— rename 目标路径仍基于旧路径派生——迁移语义残缺（已修复：updateCallPathInProg 加 rename 分支——目标路径跟随源重新派生（`newPath + "._renamed_" + randomSuffix`——新后缀方案）——语义区分：与 L9 的"路径替换"（Args[1]==oldPath 时替换）不同——L25 为"路径迁移"（目标基于旧源派生——源迁移后目标同步重新派生）——唯一调用者 MutateGroupPathDynamic（2489）——修改只影响锚定迁移）

## 五、已排除/误报（核实否定，避免重复审查）

**X1. DcacheMutTypeCount=5、dcache 突变 40% 空转**（代理误判）
- 核实：Delay/FailPos 注释后 iota 重排，SyncPos=2、Count=3——Intn(3) 三槽位全覆盖——排除
- 注：mutateStashOpSequence 的 Intn(3) 空转（L16）属实——两者勿混淆

**X2. stash 元数据"陈旧"导致 moveFailCalls 拆散其它节点 recv/send 配对**（代理推断）
- 核实：配对按 sync 值跨 prog 汇合，6 块整体移动保持内部顺序——不会因只移动 ps[0] 而死锁——排除（但 C4/S6/S7 的窗口拆分路径仍真实存在）

## 六、环境/配置问题（✅ 已修复）

**E1. agent devsl=1 导致跨节点 F_OPEN 必拒（Permission denied）**
- 位置：hmdfs_agent.c 793 / 1078 / 1117 / 1210 / 1249 / 1827（`cmd.devsl = 1`）+ 1395 / 1578（配置默认值）
- 现象：metadata 收集期 `get_file_checksum open failed ... Permission denied (errno 13)` → SYZFAIL；内核日志 `hmdfs_open_file() devsl permission denied`（hmdfs_server.c:357）。仅跨节点打开 merge_view 下对端已有文件时出现；同节点文件、创建、stat、xattr 全部正常——表现为"时有时无"的偶发失败
- 根因：内核 `check_sec_level()`（hmdfs_server.c:291-341）对**无 `user.security` xattr** 的文件要求 `node->devsl >= DATA_SEC_LEVEL3(3)` 才放行（有标签则 `devsl >= 标签级别`）；agent 将 devsl 写死为 1（真实 HarmonyOS 设备为 3）→ 无标签文件跨节点 F_OPEN 必被服务端拒绝。检查只存在于 F_OPEN 回调链（hmdfs_server_open → hmdfs_open_file），F_CREATE/F_GETATTR/F_READPAGE 等均不检查——故测试期跨节点 open 静默失败（executor 仅记录 errno，默认不打印），仅 write_metadata 的 checksum open（getmetadata.h:210-223 的 fail()）暴露为 SYZFAIL
- 修复：8 处 devsl=1 → 3（与内核 DATA_SEC_LEVEL3 对齐，恢复真实设备语义——无标签文件跨节点可访问）
- 状态：✅ 已修复（待 fuzz 回归验证：重跑 2 节点 hmdfs 用例，确认 `devsl permission denied` 与 checksum SYZFAIL 消失）

## 附：修复批次与优先级

- **批1（崩溃级）**：C1 → C2 → C3 → C4 → C5（顺序：先两侧语义对齐项 C1，再空 ps 防护 C2，后 panic 类 C3/C4/C5）
- **批2（语义级）**：S1-S16（按影响：静默失效 > 弱化 > 潜伏；潜伏项 S13/S14 可延后至启用前）
- **批3（低危级）**：L1-L25（精选处理，L18 无需处理）
