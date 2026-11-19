# GlusterFS 适配代码链 Review Backlog

> 日期：2026-11-19  
> 范围：GlusterFS 适配的 profile/config/runtime、tracer/parser/DAG、DCT/seed/path mutation、directed C1/C2、fuzzer/manager feedback/corpus 代码链。测试和文档只作为辅助证据。  
> 状态：living backlog。P0 已修；P1/P2 按状态逐步处理，不要求一次清空。

## 状态口径

```text
fixed                     已修复并有回归测试
confirmed                 代码路径确认，生产可达
needs-runtime-verification 代码路径存在，但当前配置/运行方式可能不触发，需要 Linux 3-3 验证
latent                    当前 baseline/config 不触发，属于防御性或未来风险
not-a-problem             已确认不适用于本项目当前配置
deferred                  已知但暂不处理
out-of-scope              不属于本轮 GlusterFS 适配
```

---

## P0

### P0-1 `causalRootPathsByPath` 在 GlusterFS fresh anchor 上无限循环

**状态：fixed**

- 位置：`src/prog/causal_root.go` 的 `causalRootPathsByPath`
- 原因：GlusterFS 空 prefix 下 `GetParentDir("dirA") == "."`，`GetParentDir(".") == "."`，`ft.FindNode(".") == nil`，循环永远停在 `"."`。
- 触发：`collectCausalRootPlans` 对任何 `role.ByPath && info.Kind.IsFresh()` 的 anchor 调用；fresh `mkdir/creat/link/symlink/rename dst` 都可触发。
- 影响：`PickDirectedBatch` 先走 C1，第一个 directed round 就可能 hang；现有 `TestCausalLinkAnchorRoleEnumeration` 在 Linux 上会 hang 而不是 fail。
- 修复：
  - `PathParent` 循环改用 `parentDirOrRoot`；
  - `PathSibling` 的 parent 查找改用 `parentDirOrRoot`；
  - 回归测试：`TestCausalRootPathsByPathFreshTopLevel`。
- 验证：`GOOS=linux GOARCH=amd64 go test -c ./prog`、`go vet ./prog ./syz-fuzzer`、`go build ./syz-fuzzer` 通过；Linux 运行时待跑。

---

## P1

### P1-A Directed / C1 / C2

| ID | 状态 | 问题 | 位置 | 处理方向 |
|---|---|---|---|---|
| P1-A1 | fixed | client-only mutation 用了 server TSC offset。`MutateFsopsWithDCT(cps)` 的 `ps[0]` 是第一个 client，但 `lcs.tscoffFor(0..2)` 取的是 VM 0..2（server） | `proc.go:327-331,857-862`；`distributed_choice.go:2225-2232`；`directed_plan.go:2926,2982,2986`；`causal_root.go:779,1498` | 给 planner 传 `tscoffs[ServNum:]`，或让 LCS 带 client base index；DAG 侧仍用绝对 `proc.tscoffs`。加 `ServNum>0` + 不同 offsets 的测试 |
| P1-A2 | fixed | C2 rename root 的 CC relation 用 destination 算；rename CC 的 A 侧实际是 source。`B=src` 计划 Sibling、观测 Same；`B=dst` 计划 Same、观测 Sibling；source 子树 Child 漏掉 | `causal_root.go:297-317,492,653-658,709-712`；`dag.go:753-768` | 按 temporal 分别取 `rootSource`/`rootDest`；CC 用 source，HB 用 dest；destructive gate 也按 temporal rel |
| P1-A3 | fixed | rename root 后仍给 source 上的 variant 预测 Success。`destructiveSame` 只对 `Rel=Same` 生效，而 rename root 的 dest/source 是 Sibling | `causal_root.go:499-504,581-715` | 对 `causalRootRename` 排除 `bn.path == rootSource` 及 source 子树（HB）；加 `SourceMoved` 类似的 per-temporal gate |
| P1-A4 | fixed | unlink/rmdir 作为 C1 anchor 时 root 路径已删除。C1 root 插在 anchor 后，root 失败；C1 不进 pending，无法被 cell-suppress | `causal_root.go:756-784,1342-1351`；`anchor_path.go:31-38`；`causal_root.go:1476-1515` | C1 anchor 要求 `callSucceeded`；对 unlink/rmdir anchor 过滤 anchor path/subtree；考虑 C1 选择失败计数 |
| P1-A5 | fixed | GlusterFS 顶层 fresh path 在 C1/C2/PathSet 中不完整：`GetParentDir` 得到 `"."`，`directedFreshName` 空 parent 生成 `"/mut_dir_x"`，validator 要求 `GetParentDir(BaseA)==BaseA2` | `causal_root.go:1311-1336,986-990,1104-1106`；`directed_plan.go:1027-1050,2776-2786`；`path_set.go:924-926,1574-1611` | 统一 `parentDirOrRoot`/`joinTestPath`；允许空 parent 并让 validator 用同一 helper |
| P1-A5b | fixed | HMDFS `FindNode("merge_view")` 返回 nil：`normalizePath` 未把裸 root FullPath 映射到 root，导致 HMDFS 顶层 fresh parent 被拒 | `distributed_choice.go:350-360`；`seed_derivation.go:296-311` | `normalizePath` 精确匹配 `ft.Root.FullPath`；`seed_derivation` 的 existing-parent 分支用 `freshParentEligible`；更新旧的拒绝测试 |
| P1-A6 | fixed | C1 E-overwrite 采样没有 batch 路径保留；E variant 可能落到 root rename destination，预测 Success 实际 ENOTEMPTY | `causal_root.go:229-239` | 复用 normal 的 reserved set，并把 `batch.BaseA2`/`RootDest` 纳入 authority |
| P1-A7 | deferred | C1 plan 排序对 tie 不确定；link/symlink target/linkpath 同路径 tie 时顺序取决于 map 迭代 | `causal_root.go:1172-1202` | 用 `Cell.toKey()` 或补全 `Role/DepthB/Temporal/RetB` 排序 |

2026-11-19 修复记录：P1-A1/A2/A3/A4/A5/A5b/A6 已落地。

- 新增 `path_effect.go`：`unlink` 文件 → Remove，`rmdir` 空目录 → Remove，`rename` → Move(source,dest)；C1 anchor 用 `callReturnedSuccess`（`Retv>=0`）判定，destructive effect 仍用 `callSucceeded`（`Retv==0`），失败 anchor 直接跳过。
- C1 root candidate、C1 anchor 后的 C2 variant（CC+HB）都按 anchor effect 过滤；C2 root 的 HB variant 按 root effect 过滤，CC 保留 race。
- rename root 的 CC A 侧改用 source，HB 仍用 dest；A2 的 dual 分支只在 `DualPathCausalEnabled` 下启用。C1 root 的 path effect 是共享修复，HMDFS 也会过滤 unlink/rmdir anchor 后的 root。
- E-overwrite destination 现在把 root 的 source/dest/parent 和全部 plan 路径纳入 reserved set，并在 picker/insert 两处用 `batchRootReservationsOK` 兜底。
- directed variant 不复用 root 的 fd（`directedVariantCallsSeq` 始终重新 open），所以 fd variants 不豁免删除路径；只有 `creat` 在精确 source path 上、RetB=Success 时豁免。
- P1-B5a：`findAnchorCreator` 改用 `callReturnedSuccess`，成功 `open(O_CREAT)` 返回 fd>=0 时仍被识别为 creator；失败 `open`（`Retv<0`）仍拒绝。
- `pathEffect` 在构造和 `allows` 边界 normalize `./`/前导 `/`，避免 p0 anchor 的 `./dirA/a` 与 tree `dirA/a` 字符串不一致导致漏过滤；P1-B6 的生成点（`GetParentDir`/`joinTestPath`）仍在 backlog。
- P1-A1：`newProc` 给 `lcsFsops` 传 `plannerTscOffsets(procTsc, hc.Serv_num)`（即 `procTsc[Serv_num:]`）；DAG 仍用完整 `proc.tscoffs`。`mutation.go` 的 `findConcurrentCalls`/`findHBCalls`/`findGroupCalls` 也走同一 LCS slice，一并修正。
- P1-A5：新增 `freshParentEligible`；`directedFreshName`/`directedFreshPlaceholder` 改用 `joinTestPath`；C1/C2/normal 2b 的 fresh parent 统一 `parentDirOrRoot`；validator 接受 `BaseA2`/`BaseB2 == ""`（仅 GlusterFS namespace root）；HMDFS 空 parent 仍拒绝。P1-B6 的 `GetParentDir` 生成点未动。
- P1-A5b：`FileTree.normalizePath` 把裸 `ft.Root.FullPath`（HMDFS 的 `merge_view`）映射到 root；`seed_derivation` 的 existing-parent 分支改用 `freshParentEligible`。HMDFS 顶层 fresh path（C1/C2/normal/PathSet/seed）现在可用；`merge_view/merge_view` 仍按顶层同名子节点处理。
- `pathSetParentValidAt("")` 继续按 root alias 处理（HMDFS/GlusterFS 都接受），生成侧仍由 `freshParentEligible` 把关；没有给 PathSet 加额外的空 parent 拒绝。
- PathSet 的 `pathSetSpecValidWithParent` 拒绝 `spec.Path == ft.Root`：root 可以作为 parent，但不能作为 call path；`merge_view/merge_view` 顶层同名子节点仍允许。回归测试 `TestPathSetRejectsRootCallPath`。
- PathSet fallback/relation 候选显式跳过 `ft.Root.FullPath`：`pathSetFallbackCandidates` 不再把顶层 parent（HMDFS `merge_view`）当 call path；`pathSetTargetMemberPaths`/`pathSetDirectedCandidates` 也跳过 root 输出。`pathSetParentValidAt` 仍允许 root 作为 parent。回归测试 `TestPathSetFallbackCandidatesSkipRoot`。
- 回归测试：`TestPathEffect*`（含 `TestPathEffectNormalizesDotPaths`）、`TestCausalRootRelPathsForDualRenameTemporal`、`TestCausalRenameRootC2TemporalRelations`（不绑定 stat/Same）、`TestCausalUnlinkAnchorFiltersRemovedPath`、`TestCausalRmdirAnchorFiltersRemovedPath`、`TestCausalRenameAnchorFiltersSourceSubtree`、`TestCausalFailedAnchorRejected`、`TestCausalWriteAnchorAccepted`、`TestFindAnchorCreatorAcceptsOpenCreate`、`TestPickCausalRootBatchFiltersAnchorRemovedPath`、`TestBatchRootReservationsOK`、`TestPlannerTscOffsetsClientView`、`TestLayeredChoiceStrategyClientTscOffsets`、`TestDirectedFreshNameTopLevel`、`TestFreshParentEligible`、`TestCausalTopLevelFreshRoot`、`TestCausalExistingTopLevelFreshRoot`、`TestCausalC2TopLevelFreshRoot`、`TestValidateTopLevelFreshPlan`、`TestPathSetTopLevelParent`、`TestPathSetRejectsRootCallPath`、`TestPathSetFallbackCandidatesSkipRoot`、`TestFindNodeHMDFSRootName`、`TestCausalRootPathsByPathFreshTopLevel`（HMDFS case）、`TestPlanCausalC2AcceptsTreeRootFreshParent`、`TestPlanCausalC2RejectsFileFreshParent`、`TestCausalRootC1FreshParentEligibility`、`TestHMDFSTopLevelFreshParent`。
- 验证：`GOOS=linux GOARCH=amd64 go test -c ./prog`、`GOOS=linux GOARCH=amd64 go vet ./prog ./syz-fuzzer` 通过；Linux 3-3 运行时待跑。

### P1-B DCT / seed / PathSet

| ID | 状态 | 问题 | 位置 | 处理方向 |
|---|---|---|---|---|
| P1-B1 | confirmed | depth bucket 用全局 `DepthBucketOf`，GlusterFS 顶层节点 bucket 0 被 `pickByBucket` 丢掉；DCT seed/pattern base 退化，coverage/directed depth-0 也被 `depths[0]=0` 排除 | `distributed_choice.go:872,914,252-258`；`coverage_board.go` depth 初始化；`path_set.go:712`；`mutation.go:3079` | 用 `ft.DepthBucketOf`；空 prefix 下不要强制清零 bucket 0；`countDagDepthBucket` 支持 bucket 0 |
| P1-B2 | confirmed | derived setxattr root 把文件路径当 xattr name；`RootPaths.Dest()` 对 DualNone 返回 `PostPath=path` | `mutation.go:5220-5222,5271-5284`；`dual_path.go:46-55`；`seed_derivation.go:92,362-363`；`rand.go:3352-3357` | `rp.Kind==DualNone` 时 `basePath2=""`，让 `randomXattrName` 生效；或 setxattr 特判 shared key |
| P1-B3 | confirmed | rename variant 绕过 profile RenamePolicy；`GetPathsForRenameVariant` → `pickRenameDst` → `pickRenameDstPolicy(hmdfsProfile,...)`，GlusterFS 永远 same-parent | `distributed_choice.go:2239-2248,2280-2282`；`mutation.go:5129,5372`；`rand.go:2897` | 改成 `lcs.pickRenameDstFor(src, rel, r)`；HMDFS 保持 wrapper |
| P1-B4 | confirmed | PathSet broad relocation 用 newBase 当 oldBase 映射；`choosePathSetBase` 丢掉 oldBase，`mapPathSetPath` 得到 `newBase+path` 或 no-op | `path_set.go:811-841,1494-1502,872-875` | 返回 `oldBase/newBase`，用 `oldBase+suffix→newBase+suffix`；拒绝 ancestor newBase，要求至少一个路径变化 |
| P1-B5 | confirmed | anchor/rename source 不跟踪 in-program unlink/rmdir/rename；FileTree 只保留 baseline，derived root 落在已删除路径；fresh rename source 永远 `pathExisting` | `anchor_path.go:31-38,53-104`；`seed_derivation.go:115-192`；`mutation.go:4570-4612`；`path_set.go:466-470,1009-1010` | anchor/creator 查询加 `freshPathAbsentBefore`/`freshRenameEventsBefore`；rename src 用 `pathSetCreatorState`；至少拒绝 anchor 自身成功删除的路径 |
| P1-B5a | fixed | `findAnchorCreator` 的 `requireSuccess` 用 `callSucceeded`（`Retv==0`），成功 `open(O_CREAT)` 返回 fd>0 会被当成失败跳过；`classifyAnchorPath` 看不到这个 fresh anchor | `anchor_path.go:58,122-124` | 改用 `callReturnedSuccess`（`Retv>=0`）；补 `TestFindAnchorCreatorAcceptsOpenCreate` |
| P1-B6 | confirmed | `GetParentDir` 对顶层路径产生 `"./..."`；`joinTestPath(".", name)`/`freshChildPath(".",...)` 生成 `"./mut_dir_x"`，DAG/coverage/freshKinds 字符串不一致 | `distributed_choice.go:2318,2375-2380`；`mutation.go:5121,5365`；`rand.go:2889`；`seed_derivation.go:291-292` | 统一 `parentDirOrRoot`/canonical join；normalize freshKinds/freshLinks 键 |
| P1-B7 | deferred | PathSet 可生成 `rename(src, src)` no-op | `path_set.go:1107-1140,1011-1021` | `pathRoleRenameDst` 拒绝 dst==src；fallback 排除 source |
| P1-B8 | deferred | derived readlink variant 允许 CC；root symlink 是 modifier，50% CC，readlink 在 root 前执行 → ENOENT | `mutation.go:5314,5322-5340,5350-5355`；`distributed_choice.go:1975-1983` | readlink variant 强制 `TemporalHB`/`rootEnd` |
| P1-B9 | test-only | `dctBaseCandidatesFor` 包含 insertPos 之后的 creator / 已删除路径；当前仅测试调用 `insertCallFromDCT` | `mutation.go:4622-4682` | 只收 `before` 或现有节点；跳过成功删除；或删除死函数 |
| P1-B10 | latent | FailurePolicy rename ENOTEMPTY 硬编码 same-parent；GlusterFS cross-parent ENOTEMPTY 不构造 | `mutation.go:3688-3695,3728-3751` | 走 RenamePolicy 或允许 cross-parent source |
| P1-B11 | latent | `NewLayeredChoiceStrategy` choice table 用 `hmcfg.Profile` 而非 `profileOrDefault()`；`Profile=nil` 的 GlusterFS config 得到 HMDFS ChoiceCalls | `distributed_choice.go:2085-2090` | 用已计算的 profile |
| P1-B12 | confirmed | `IsFdRequiredCall("readlink")` 被 `"read"` 子串命中；readlink 被当 fd call，timing 回退 index 对齐 | `distributed_choice.go:108-115`；`hmdfs_values.go:489-490` | 精确匹配/profile `FdRequiredCalls` |
| P1-B13 | latent | PathSet generic member 不排除 linkpath；`stat/open/chmod` 若路径是 symlink linkpath，PathSet 可迁移 consumer | `path_set.go:492-535` vs `332-455` | tracked profile 下检查 `freshLinkPathsBefore` |
| P1-B14 | latent | cross-program fresh parent 不要求 mkdir 成功；失败 mkdir 被当 soft parent | `path_set.go:1595-1608` | 要求 `mkdir.CheckInfo.Retv==0` |
| P1-B15 | confirmed | PathSet rename source 是 fresh path 时永远 `pathExisting`；fresh rename source 无 tree node，plan 被拒 | `path_set.go:466-470,1009-1010` | 绑定 creator，允许 fresh src |

### P1-C Tracer / parser

| ID | 状态 | 问题 | 位置 | 处理方向 |
|---|---|---|---|---|
| P1-C1 | confirmed | offset bucket 用 request length / 0，而不是 entry file size。truncate `size=0`，Mid/Tail 永不出现；directed pread/pwrite 计划 Mid/Tail 但请求长度 1，观测总是 Beyond | `glusterfs_trace.bpf.c:284-296`；`dag.go:95-96,1786-1802`；`directed_plan.go:1083-1088`；`coverage_board.go:1979-1984` | wire 加 entry file size，或 GlusterFS profile-aware bucket；同步收窄 producible/planner |
| P1-C2 | confirmed | LOOKUP nodeid fallback 用 parent path 代替 child；R path 丢失/out-of-root 时产生假 Same/Parent pair | `glusterfs_trace.cc:376-384`；`dag.go:468-473` | fallback 只用于 target-inode opcode；LOOKUP 用 parent+name 重建，否则 drop |

### P1-D Fuzzer / feedback

| ID | 状态 | 问题 | 位置 | 处理方向 |
|---|---|---|---|---|
| P1-D1 | confirmed | `ApplyProgTimings` 恢复 `CheckInfo` 时 `Retv=0`；fuzzer restart 后 failed creator/anchor 被 `callSucceeded` 当成功 | `mutation.go:5520-5568`；`anchor_path.go:58,122-124`；`fuzzer.go:716-732` | Timings 带 `Retv`/success flag；或只恢复成功调用 |

### P1-E Profile / config / runtime

| ID | 状态 | 问题 | 位置 | 处理方向 |
|---|---|---|---|---|
| P1-E1 | confirmed | `glusterfs-node-up.sh` 参数契约不匹配；`$cur_idx` 空，else 分支在 server 上也 mount client FS | `executor.cc:1490`；`glusterfs-node-up.sh:2-8` | 脚本内 `set -- $2` 解析 params，或 executor 传 5 个参数；加 role 检查/mount 选项/错误处理 |
| P1-E2 | confirmed | server 数量/replica mode 不校验；`serverCnt=2` 或 `"3 3"`（mode 空）被接受，实际 distributed volume | `glusterfs_config.go:34-43`；`glusterfs-config.sh:43-47` | 要求 `serverCnt==3` + replica 3 mode；mgrconfig 启动前校验 |
| P1-E3 | needs-runtime-verification | C2SAN 与 client-only empty server batch 不兼容；若 config 关闭 C2SAN 则暂不触发 | `proc.go:1424-1436,1478-1487`；`mutation.go:1566-1577`；`dfs_profile.go:215` | 按 profile/DFSName gate；GlusterFS 启用时启动报错 |
| P1-E4 | confirmed | trace init 失败静默；fuse.so/BPF/ring/uprobe 失败只打印，executor 继续，DAG/directed 全空 | `glusterfs_trace.cc:670-731`；`dfs_runtime.cc:58-61`；`executor.cc:717-719` | init 返回 status，client 失败即 fail |
| P1-E5 | confirmed | restore 不验证 client root 真的是 GlusterFS mount；mount 失败时 `rm -rf`/rsync 在本地目录执行 | `dfs_runtime.cc:97-129`；`executor.cc:3720-3725`；`glusterfs-config.sh:69-76` | statfs magic 校验；mount 脚本失败即退出 |
| P1-E6 | needs-runtime-verification | profile calls 未与 enabled syscalls 校验；缺 call 时 DCT/planner 仍可能选中，首次执行 panic | `distributed_choice.go:1581-1604`；`coverage_board.go:380-398`；`fuzzer.go:804-819`；`proc.go:1493-1495` | 启动时 profile lists ∩ EnabledCalls，缺 call 清晰报错 |
| P1-E7 | needs-runtime-verification | 多 proc 默认 6，若未设 procs=1 会共享同一 shm | `mgrconfig/load.go:89`；`proc.go:58`；`ipc.go:248-262` | 每 Proc 独立 shm，或 GlusterFS 强制 procs=1 并校验 |

---

## P2

### P2-Tracer / parser

| ID | 状态 | 问题 |
|---|---|---|
| P2-T1 | confirmed | LOOKUP nodeid fallback 父路径（见 P1-C2，保留细节） |
| P2-T2 | confirmed | truncated path 未过滤；`BuildVertices` 不检查 `Truncated` |
| P2-T3 | latent | symlink empty/unresolvable payload 退化成 target-role；`target==""` 时 target 变 linkpath |
| P2-T4 | deferred | 非 LOOKUP 事件不 fallback `ev.Path`，也没有 path mismatch 诊断 |
| P2-T5 | deferred | FUSE interrupt 重复 final event（同 unique 二次 S） |
| P2-T6 | deferred | `gf_send_ret` 在 uretprobe 重读 reply error，而不是用 `val->error` |
| P2-T7 | deferred | 无 pid correlation；其他进程 FUSE op 可被匹配进程序 call window |
| P2-T8 | deferred | ring drop 静默；path ring 4MiB < 8192 events |
| P2-T9 | latent | server metadata root=/root；`lfs_based` 打开时会遍历 server rootfs |
| P2-T10 | deferred | `InoByFunc` capture-failure alarm 对 GlusterFS 永久为真（诊断） |
| P2-T11 | deferred | delete timeline 以 ino 0 为 key；当前 writepage 不产生，死代码 |
| P2-T12 | deferred | output buffer overflow 直接 abort round（低概率） |
| P2-T13 | latent | `RecordCoveragePairs`/`ComputeFeedback` 对 malformed `DAGPair`（A/B nil）没有防御；正常 `ExtractPairsFor` 不会产生 nil，但 IPC 异常时 `cellKeyOf`/`hashPair` 可能 panic；加 nil guard + drop 诊断 |

### P2-Directed

| ID | 状态 | 问题 |
|---|---|---|
| P2-D1 | deferred | C1 E-overwrite 未保留 batch 路径（见 P1-A6） |
| P2-D2 | deferred | C1 tie 排序不确定（见 P1-A7） |
| P2-D3 | confirmed | plannable/planner phantom：mkdir Success、rmdir Success、rename Failure、getxattr/setxattr 上下文、unlink/unlink Same 等（已知边界） |
| P2-D4 | latent | symlink-to-dir 类型；baseline 不建 symlink 时不触发 |
| P2-D5 | deferred | 同路径 size mutation 的 offset bucket 以 pre-root size 计算 |
| P2-D6 | latent | validator FreshB/profile 不一致（主要 HMDFS） |
| P2-D7 | deferred | fresh linkpath 未做 batch-local 保留，随机碰撞可能把 Success 变 EEXIST |
| P2-D8 | deferred | directed variants 不复用已有 fd（`directedVariantCallsSeq` 总是重新 open）；fd-survives-unlink 的 HB pair 当前不可构造 |
| P2-D9 | latent | `pathEffect` 仍是字符串比较；HMDFS bare path / future `NormalizePath` source 与 tree `merge_view/` prefix 不一致时会漏过滤。当前 p0/tree 同 profile prefix，生产不触发 |

### P2-DCT / PathSet

| ID | 状态 | 问题 |
|---|---|---|
| P2-B1 | confirmed | `GetParentDir` 生成 `"./..."`（见 P1-B6） |
| P2-B2 | deferred | PathSet rename dst=src no-op（见 P1-B7） |
| P2-B3 | deferred | derived readlink CC（见 P1-B8） |
| P2-B4 | test-only | `insertCallFromDCT` 死路径/过期 base（见 P1-B9） |
| P2-B5 | latent | FailurePolicy rename same-parent（见 P1-B10） |
| P2-B6 | latent | `NewLayeredChoiceStrategy` profile=nil（见 P1-B11） |
| P2-B7 | confirmed | witness symlink 用 file-only 约束；`callNameConstraint` 与 `TargetConstraint` 不一致 |
| P2-B8 | confirmed | `IsFdRequiredCall("readlink")` 子串误判（见 P1-B12） |
| P2-B9 | latent | PathSet generic linkpath（见 P1-B13） |
| P2-B10 | latent | cross-program fresh parent 不要求 mkdir 成功（见 P1-B14） |
| P2-B11 | confirmed | PathSet rename source fresh（见 P1-B15） |

### P2-Fuzzer / manager

| ID | 状态 | 问题 |
|---|---|---|
| P2-F1 | deferred | initial corpus pump 不把 pending DAG bits 当进度；restart 后旧 pair 可能重新 novel |
| P2-F2 | deferred | depth 0 统计缺失；GlusterFS 顶层顶点不计入 `dag depth` |
| P2-F3 | deferred | DAG corpus provenance `Call="dag"` 不持久化；restart 后 retention/per-call stats 改变 |
| P2-F4 | deferred | `MaxDagCorpus=0` 仍受 50% DAG pruning/saturatedCalls 限制；需明确 unlimited 语义 |
| P2-F5 | needs-runtime-verification | `EnableDagFb=false` 时 directed planner/pending 仍活跃 |
| P2-F6 | deferred | DAG flags 无 manager/qemu plumbing，`MaxDagCorpus`/`EnableDagFb` 无法通过 manager 配 |
| P2-F7 | operational | `appendDiagLog` 硬编码 `/home/user/dfsfuzz/dag.log`，每执行重试 open |

### P2-Profile / config

| ID | 状态 | 问题 |
|---|---|---|
| P2-E1 | latent | `ClientView`/`RootProg` 短 batch 把 server 当 client；`Serv_num >= len(ps)` 时 `RootProg` 回退 `ps[0]`；配置/批次校验会先拒绝 |
| P2-E2 | latent | HMDFS prefix 在缺 Profile/FileTree 时泄漏到 GlusterFS |
| P2-E3 | design-debt | profile 大量字段 dead：Feature/Checker/Gen/Trace/Paths.MaxClientNum/Plan.DCT* |
| P2-E4 | deferred | 启动/config panic 风险：`ipcconfig.Default` err 前赋值、`argv[15]`、`InitIp` 索引、`tscoffs[idx]` |
| P2-E5 | deferred | manager 不能覆盖 `MetadataDelayMs`/DAG flags |
| P2-E6 | not-a-problem | restore 不保留 xattr/ACL/hardlink：初始文件树不设置这些属性，`rsync -a` 足够 |
| P2-E7 | needs-runtime-verification | `write_dir_info` 依赖 `dirent.d_type`，DT_UNKNOWN 不递归；需看 GlusterFS FUSE 返回 |
| P2-E8 | deployment | `glusterfs-config.sh` 不检查 volume/mount 失败 |
| P2-E9 | design-debt | `DFSProfile.DualPathCausalEnabled` 注释仍写 "v1 leaves it false"，与 GlusterFS profile 的 true 不一致；更新注释即可，无运行时影响 |
| P2-E10 | latent | `ProfileForName` 未知 `DFSName` 返回 nil，`ProfileOrDefault` 静默回退 HMDFS；`proc.go` 按字符串分支，拼错可能走非预期路径；启动时校验 `DFSName ∈ {hmdfs, glusterfs}` |

---

## 已确认不是问题 / 需要修正的 review 结论

- **restore ACL/xattr/hardlink**：初始文件树不设置这些属性，`rsync -a` 足够；不作为 P1/P2 修复项。
- **HMDFS 顶层 fresh parent**：`merge_view` 会 normalize 到 Root，问题只限 GlusterFS 空 prefix；原 review 中 HMDFS 部分不成立。
- **symlink-to-dir 类型**：baseline 不建 symlink，当前不触发；保留为 latent。
- **server metadata root**：GlusterFS 当前 `lfs_based=false`，不触发；保留为 latent。
- **InoByFunc / delete timeline**：诊断/死代码，不影响当前执行。
- **output overflow**：低概率，先观察。
- **insertCallFromDCT 相关**：当前只有测试调用，生产走 `insertDerivedCallFromDCT`；不阻塞。
- **NewLayeredChoiceStrategy profile=nil**：生产 `newProc` 会设置 Profile；latent。
- **`DualPathCausalEnabled` 过期注释**：只影响可读性，不是运行时 bug；更新注释即可。
- **未知 `DFSName`**：配置校验问题，正常 config 不会触发；后续启动校验。
- **malformed `DAGPair` nil**：防御性问题，正常 DAG pipeline 的 pair 都非 nil；加 nil guard 即可。
- **ClientView 短 batch**：配置/批次校验会先拒绝；防御性。
- **profile dead fields**：设计债，不是当前运行时 bug。
- **启动 panic**：配置错误路径，正常配置不触发；后续做启动校验。
- **MaxDagCorpus 50% pruning**：`triage-minimize-redesign.md` 已把它写成既定行为；如果 D4 需要真正 unlimited，再改语义。

---

## 验证计划

```text
P0:
  TestCausalRootPathsByPathFreshTopLevel
  TestCausalLinkAnchorRoleEnumeration（Linux，确认不再 hang）

P1-A1:
  ServNum=3 + 不同 tscoffs，断言 root/variant insertion boundary

P1-A2/A3:
  rename C2 CC/HB relation、source-moved、destructive anchor 测试

P1-A4:
  top-level fresh mkdir/creat/link/symlink C1/C2/PathSet 测试

P1-B1:
  顶层节点 SelectFile/SelectDir + coverage depth 0 + PathSet/directed depth 0

P1-B2:
  derived setxattr name 以 user. 开头

P1-B3:
  GlusterFS rename variant cross-parent

P1-B4:
  broad PathSet oldBase→newBase + 至少一个路径变化

P1-B5:
  unlink/rmdir anchor + fresh rename source

P1-C1:
  truncate/pread/pwrite bucket 与观测一致

P1-D1:
  Timings 带 Retv，failed creator 不被当成功

P1-E1/E2/E5:
  3-3 config/mount/restore smoke

最终:
  go test ./prog -count=1
  D2 executor smoke
  D3 short/medium run
```

---

## 维护方式

- 新发现先加 P2，有生产证据再升级 P1。
- `not-a-problem` 条目不要删除，保留“为什么不是问题”的结论，避免重复 review。
- 每修一项，更新状态并在对应 commit/测试中留引用。
