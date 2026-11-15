# PathSet 与突变分析：路径突变、数据突变、删除突变

> 状态：PathSet 核心已实现（2026-10-25）；2026-11-15 补充 GlusterFS v1 双路径 / canonical-payload / prefix 决策；
> cross-compile/vet 通过，运行时测试待 Ubuntu。
> 范围：PathSet 路径突变；附带数据突变、删除突变分析。
> 原则：保留“倾向 / 待定 / 建议”标记，未定方案不写成已决。
> 关联：`docs/paper-draft.md` §4.3、`docs/seed-optimization-plan.md`、`docs/causal-root-cell-design.md`；本文不修改论文正文或既有实施计划。
> 说明：因果锚定插入与 root→variant 两层设计已迁移到 `docs/causal-root-cell-design.md`；本文只保留 PathSet、数据突变和删除突变。

---

## 0. 背景与术语

- **关系对**：反馈的构成单位。两个路径相关的操作，按时间窗判定为并发对或因果对。
- **PathSet**：在路径关系上连通的、带路径参数的调用集合；不是按时间过滤的因果组件，也不声称数学闭包。

### 0.1 当前构造侧事实

- 路径突变：`MutateGroupPathDynamic` 先走 `mutatePathSetDirected` 的 PathSet target arm；旧 `mutateGroupPathDirected` 与普通 PathSet 迁移作为回退。
- 数据突变：`MutateGroupDataDynamic` 只改 offset/length/buffer，不改路径。
- 删除突变：`RemoveGroupDynamic` / `RemoveReservoirDynamic` 删除调用，当前只做 fd 安全过滤，不维护因果链完整性。

### 0.2 反馈侧事实

- `ExtractPairs` 不按 `ProgIdx` 过滤，同节点因果对也会进入反馈。
- 因果对的在先前驱必须是成功完成的修改操作；在后操作可以是读、写，也可以失败/阻塞。
- 并发对要求时间窗重叠且至少一方是修改操作；按类型判定，不要求修改成功。
- `determinePathRel` 对父子采用任意深度前缀判断；构造侧 `GetPathByRelation` 目前只生成直接父子。

---

## 1. PathSet 与路径突变

### 1.1 当前问题

- `findGroupCalls` 只收集锚点所在程序之外的并发调用和直接因果后继；
- 同程序的前驱/后继不在组里；
- `MutateGroupPathDynamic` 只改锚点和跨节点成员；
- 因此，已有路径关系和同节点前后关系可能被拆开；
- 根下 sibling：一个顶层路径被移动时，另一个不会被带上。

### 1.2 定义

PathSet 是调用级的路径关系连通分量：

- 节点是带路径参数的调用；
- 边是 Same / ParentChild / SameParent（sibling）；
- 从 anchor 所在分量取集合；
- 不做时间过滤；
- 包含 sibling，包括根下 sibling；
- 不把“没有调用的路径节点”当作桥接节点。

fd-only 调用（read/write/fsync/close/getdents 等）没有路径槽位，不作为 PathSet 成员；target arm 的 fd anchor 先解析到 owner open/creat，owner 才是迁移对象。fd 使用者随 owner 自动移动，不需要单独改写；其类型约束由 `memberPathConstraint` 经 `ownerPathConstraint` 纳入 owner，因此不会漏检。

PathSet 不是数学闭包，也不要求任意两个成员直接相关；只要求它们通过路径关系连通。
由于不做时间过滤，PathSet 可能覆盖种子的很大一部分；这是有意的 broad 模式，种子规模小，不设置硬性大小上限，只记录诊断。

### 1.3 数据模型

```go
type pathKind int

const (
    pathExisting pathKind = iota
    pathFresh
)

type pathRole int

const (
    pathRoleDirect pathRole = iota
    pathRoleMkdir
    pathRoleCreat
    pathRoleRenameSrc
    pathRoleRenameDst
    pathRoleLinkTarget
    pathRoleLinkPath
    pathRoleSymlinkTarget
    pathRoleSymlinkLinkPath
    pathRoleReadlinkPath
)

type pathSpec struct {
    Path           string
    Kind           pathKind
    Role           pathRole
    ArgIdx         int
    TypeConstraint int
}

type pathMember struct {
    Pos        GroupPosition
    Paths      []pathSpec
    Constraint int
}
```

- 路径先归一化到当前 FileTree 根前缀，剥离挂载根前缀；`path_set.go` 不得硬编码
  `MergeViewPrefix`，使用 `FileTree.Prefix`/`RootPrefix`。
- target arm 的 fd anchor 解析到 owner open/creat 的路径，owner 加入 PathSet；
- fd-only 调用本身不进入成员列表，也不单独作为路径突变目标；
- `rename` 的源和目标都进入 `Paths`，通过 `Role` 区分
  `pathRoleRenameSrc` / `pathRoleRenameDst`；
- `link` 的 target/linkpath 都进入 `Paths`，分别用
  `pathRoleLinkTarget` / `pathRoleLinkPath`；
- `symlink` 的 target/linkpath 都进入 `Paths`，分别用
  `pathRoleSymlinkTarget` / `pathRoleSymlinkLinkPath`；
- `readlink` 的 linkpath 用 `pathRoleReadlinkPath`，只作为 symlink linkpath 的
  follower，不独立迁移；
- `Constraint` 用 `memberPathConstraint` 得到，已包含 owner 的全部同程序 fd
  使用者，保证移动后类型合法。

GlusterFS v1 的路径表示：

- 所有 `pathSpec.Path` 都是相对测试根的 canonical 路径，不使用绝对路径；
- `link/symlink` 的 target 是 canonical 路径，**不是 fixed baseline**；它和
  `rename` source 一样参与 PathSet 映射，映射后再校验存在性/类型；
- `symlink` 的 target 不在 `pathSpec` 中保存 payload；payload 由
  `dirname(linkpath)` 到 canonical target 计算，只在构造调用时生成；
- `linkpath` 可以在任意已有父目录；fresh 且不得等于 target；
- `readlink` 的 linkpath 必须与对应 symlink 的 linkpath 同步迁移；
- `link` 的 target 是普通路径参数，按 cwd/test root 解析，不做 payload 转换。

路径类型必须参与校验：

- `pathExisting`：目标必须存在，类型匹配；
- `pathFresh`：父目录必须存在且类型匹配，或由同程序更早的创建调用产生；目标本身可以不存在；跨程序创建只有上一轮执行时间满足 `mkdir.Etime < consumer.Stime` 时才作为 soft valid，否则保守失败；
- `pathRoleRenameSrc`：源必须存在；
- `pathRoleRenameDst`：按原语义判断覆盖已有节点或新建；旧目标存在时按 existing 校验，不存在时按 fresh 校验，源路径始终按 rename source 校验。

### 1.4 组件构建

种子规模小，直接用 O(N²) + DSU：

```text
BuildPathSet(ps, anchor):
  refs = buildPathMembers(ps)
  for each i < j:
    if pathRelated(refs[i], refs[j]):
      union(i, j)
  return component containing anchor
```

`pathRelated`：

- 任一路径相同；
- 任一方向父子前缀（任意深度）；
- 同一直接父目录（sibling）；
- rename 两条路径任一相关即算相关；
- link/symlink 的 target 和 linkpath 任一相关即算相关；readlink 通过相同
  linkpath 与 symlink 连接。

注意：

- `merge_view/d1/x` 与 `merge_view/a` 在没有 `merge_view/d1` 调用时是 norel；
- 只有存在 `merge_view/d1` 这样的调用，才会通过 `d1` 与 `a` 的 sibling 边连接；
- 不引入无语义调用的路径节点。

### 1.5 迁移方式

#### 单顶层路径

PathSet 的路径只有一个顶层分量时：

```text
oldBase = 分量路径的 LCA
newBase = 从 FileTree 选出的新路径
p -> newBase + suffix(p, oldBase)
```

保持 Same / Parent / Child / Sibling。

#### 多顶层路径

PathSet 跨多个顶层路径（例如根下 sibling 组）时，不移动挂载根，而是选择新的父目录 `P`：

```text
merge_view/a    -> P/a
merge_view/b    -> P/b
merge_view/d1   -> P/d1
merge_view/d1/x -> P/d1/x
```

这里 **`merge_view/d1` 必须本身是一个被调用的路径**；否则 `d1/x` 与根下的 `a/b` 是 norel，不会进入同一个 PathSet。

约束：

- `P` 必须已存在；
- `P` 不能是挂载根；
- `P` 不能位于被移动的子树内部；
- 根下 sibling 整体进入 `P`，保持 sibling 关系；
- `P` 的深度按目标 member 的深度反推：若目标 member 的旧路径为 `merge_view/<top>/<rest>`，映射后为 `P/<top>/<rest>`，则新深度为 `depth(P) + 1 + (rest 非空 ? 1 + depth(rest) : 0)`；
- 先按目标深度筛选候选 `P`；若所有 `P` 都无法命中目标深度，按 level 1a 处理，而不是硬 abort。

#### 双路径成员（link / symlink / readlink / rename）

GlusterFS v1 的 PathSet broad 支持双路径：

```text
link:     target 和 linkpath 都映射
symlink:  canonical target 和 linkpath 都映射；payload 在调用物化时重算
readlink: linkpath 跟随 symlink linkpath
rename:   src/dst 都映射，但 dst 由 RenamePolicy 选，不做 generic suffix map
```

规则：

- `link/symlink` 的 target 是 canonical 测试根路径，参与映射；映射后必须是
  已有文件/目录，或由同一 plan 中更早的 creator 产生；target 不得逃出测试根。
- `symlink` 的 arg0 是 payload，不是 canonical target。`buildPathMembers` 必须用
  共享 `symlinkCanonicalTarget(linkpath, payload)` 还原 canonical target；
  同一 helper 也用于 `collectSeedAnchors`、DCT baseCandidate、DAG/checker。
- `symlink` 的 target 不保存 payload；迁移后按
  `dirname(newLinkpath)` 到 `newCanonicalTarget` 重算。
- `symlink` apply 必须两阶段：先写 linkpath，再算 payload，最后写 target；
  不能沿用 generic 逐 spec 直接写 arg0。
- `readlink` 必须和 symlink 同进同出；不能只移动 readlink 的 linkpath。
- `link` 的 target 是普通路径参数，按 cwd/test root 解析；不需要 payload 转换。
- `pathSetSpecValidWithParent` 要区分 target/linkpath/readlink role，不能只按
  “存在/不存在”判断。target 校验要检查 FileTree、`freshKinds`/`freshLinks`、
  creator 位置/类型；不能只 `FindNode(target)`。
- `pathSetMemberValid` 检查 `link/symlink` 的 target != linkpath、类型、
  linkpath freshness、readlink binding；不能只靠 `pathSetConnected`。
- `pathSpec.Kind`/`TypeConstraint` 按 role 填：link target（existing/fresh
  file）、linkpath（fresh file）、symlink target（existing/fresh file/dir）、
  symlink linkpath（fresh symlink）、readlink path（binding follower）。
- PathSet directed target arm v1 要求**整个成员 `len(Paths)==1`**；rename/
  link/symlink 整体不参与，readlink 由 call-name gate 显式排除。不是“从双路径
  成员里选一个 spec”。以后扩展 target arm 时再加 `AnchorSpec`/`MemberSpec`。

#### rename 的迁移顺序

`rename` 的 dst 不能先用 generic `mapPathSetPath` 算出来再验证；迁移阶段就要
按 `RenamePolicy` 选：

```text
1. mapped existing dst（保持原路径关系）
2. 其他能保持关系的 existing dst
3. policy 允许的 fresh dst（parent 为已有目录或同一 plan 中更早的 mkdir）
4. reject
```

- policy 负责 `DestParentCandidates` / `DestCandidates` / `Validate`；
- PathSet 负责 `pathSetSpecValid` / `pathSetMemberValid` / `pathSetConnected`；
- existing replace 只允许类型匹配、目录为空、不是 source 后代、且没有被同一
  PathSet 其他成员依赖。依赖的精确规则：
  ```text
  任一其他成员的 pathSpec.Path == dst
  或任一 fd-only 成员的 resolveFdTarget owner 路径 == dst
  ```
  命中就改用 fresh；
- fresh dst 是 total fallback：source parent 有效时 fresh sibling 总能构造，
  不应因为 existing dst 不存在而让 rename 失败；
- `pathSetFallbackPath` 对 rename dst 不能绕过 policy；无候选就失败；
- HMDFS policy 仍只返回 same-parent 候选，行为不变；
- directed target arm 对 rename 仍不支持；双路径只在 broad relocation 中处理。

#### 没有合法 P

按关系组回退，而不是直接 abort：

1. 先尝试让 sibling 组在类型兼容的 mapped parent 下构成父子关系；
2. 再尝试 sibling 组回退到同一路径，Sibling → Same；
3. 再尝试类型兼容的 fallback path（anchor 类型用 `newBase`，其它成员用 mapped parent 下的 child/parent）；
4. 仍无合法路径才 abort。

这里 `newBase` 是具体路径，不是一个单独的关系级别；不要在“Same 之后”再把它作为关系回退项。

### 1.6 回退规则

不使用关系分数，也不做全局优化。使用固定顺序的回退规则：

```text
1. 原关系精确保持
2. 同关系换候选
3. 目标邻接关系保持
4. 若原关系为 sibling 且类型冲突，优先 Parent/Child，再 Same；其余情况降级为任意仍相关关系
5. 类型兼容的 fallback path / newBase
6. 仍不合法 → abort
```

“目标邻接关系”指目标对完成映射后，其余成员优先与目标对路径保持 Same / Parent / Child / Sibling，而不是只与任意旧邻居相关。

目标等级只用于记录，不用于算术评分：

```text
level 0：目标关系和目标深度都命中
level 1a：目标关系命中，深度不中
level 1b：目标关系降级，深度命中
level 2：目标关系丢失
```

选择顺序：

```text
level 0 > 1a > 1b > 2
```

同一等级内不再计算 group score；按固定回退顺序取第一个合法候选。
`level 1a/1b/2` 可以提交种子，但只有 `level 0` 算 exact hit。

候选选择保持确定性：

1. 目标对先映射；
2. 其余成员按路径深度从浅到深；
3. 同层按路径字典序；
4. 同关系候选按字典序取第一个；
5. 若需要随机多样性，再在同一关系候选间用 `r.Rand` 取样，并记录到诊断。

当前路径突变尚未接入覆盖板 pending cell 账目；因此 level 目前只用于选择与诊断，接入账目后 level 1/2 不得把目标 cell 标记为已覆盖。

普通路径突变没有明确 target 时，不使用目标等级，直接按固定回退顺序和连通性校验选择映射。

迁移后必须满足**路径连通性不变量**：

- PathSet 内所有成员仍位于同一个路径关系连通分量；
- 关系类型可以变化；
- 不允许整体碎成多个 NoRel 分量；
- 目标关系丢失但组仍连通时，可以提交，只是不计 exact hit。

### 1.7 硬校验与提交

路径突变只改程序中的路径参数，不直接改文件系统，因此回退在程序层完成：

1. 在 clone 上构建完整映射；
2. 校验 existing / fresh（父目录存在或同程序先前创建）/ rename source / rename destination；
3. 校验 fd owner 和所有使用者；
4. 校验 rename 两条路径；
5. 校验 plan 完整性和 fresh parent 可执行性；
   - 重复 fresh path 不作为硬冲突，保留并发竞争；
   - parent 为 file、父目录创建顺序不成立、跨程序时间不可用等情况 hard invalid；
6. 校验 PathSet 连通性；
7. 目标等级达标（或允许降级）；
8. 全部通过后整体替换，否则丢弃 clone。

新增 `applyPathSetToCall`，按规划好的路径精确写入；不要复用 `updateCallPathInProg` 的 rename 自动 `._renamed_` 派生逻辑。

### 1.8 效率、测试与实施顺序

种子路径参数通常几十个：

- O(N²) 路径关系判断足够；
- 不设 PathSet 大小上限；
- 每次路径突变为毫秒级；
- 常驻诊断只累计，不保留 last target 或 per-attempt 日志。

诊断口径（字段以 `PathSetDiagnostics` 为准）：

- `Builds` 统计全部 PathSet 构建；`DirectedAttempts` 只统计 target arm 构建，比例必须用后者作分母。
- `DirectedSingleRoot` / `DirectedMultiRoot` 统计 target arm 的单顶层/多顶层分量数。
- `ExternalParentCandidates` / `SelectedDirCandidates` 是候选来源计数；`CandidateCount` 是候选总数；`TargetSelected` 是选中 target 的次数。
- `NoTarget` / `NoCandidates` / `NoPlan` 分别是 target 未选出 / 候选为空 / target 已选但无最佳 plan；已构建 PathSet 的 target arm 中，无 plan 总数 = `NoTarget + NoCandidates + NoPlan`，三者不要与 `NoPlan` 再重复相加。
- `CommittedFallbackCount` 只统计成功 apply 的 plan 所用 fallback 数；`ApplyFail` 是 apply 失败次数。
- `Exact` / `Level1a` / `Level1b` / `Lost` 只在 apply 成功后按目标等级计数。
- `CandidateConnectivityFailures` 是候选 plan 级连通性失败；`FailOther` 是 attempt 级其它失败，二者不能直接相加比较。
- `PathSetSizeSum/Max` 统计全部构建；`DirectedPathSetSizeSum/Max` 只统计 target arm。

测试：

- 根下 sibling 组迁移；
- 无 directory bridge 时 `d1/x` 与 `a/b` 分离；
- fresh mkdir/creat 路径；父目录由同程序先前创建；跨程序 fresh parent 只有在时间关系成立时 soft valid，否则保守失败；
- fd anchor 解析到 owner；fd-only 调用不进入 PathSet；
- rename 双路径；
- sibling → 父子 → Same 回退；
- PathSet 迁移后仍连通；
- level 1/2 提交但不计 exact hit；目标丢失但组仍连通；
- 普通路径突变、没有 target 时的回退与连通性；
- 多个独立 PathSet 分量时，只迁移 anchor 分量；
- 森林迁移的 P 深度导致目标深度不命中时，按 level 1a 处理；
- 硬约束失败时原种子不变。

实施状态（2026-10-25）：

1. `path_set.go`：数据模型 + PathSet 构建 —— 已实现；
2. 路径映射与 hard validation —— 已实现；
3. 回退规则与连通性校验 —— 已实现；
4. 接入 `MutateGroupPathDynamic` —— 已实现；
5. 目标选择与目标等级 —— 已实现；
6. 数据突变、删除突变回归 —— 代码未改，运行时回归待 Ubuntu。

当前 `path_set.go` 仍硬编码 `MergeViewPrefix`；GlusterFS v1 必须按
`docs/glusterfs-nfs-adaptation-plan.md` §3.6.8 的 prefix 方案改成
`FileTree.Prefix`/`RootPrefix`，并在空前缀下重新跑 PathSet 构建/迁移/target
arm 测试。双路径 link/symlink/readlink 与 rename policy 迁移也在 Phase 4 落地。

### 1.9 已知限制

- **fresh parent 是静态近似**
  - `pathSetFreshParentCreated()` 只检查同一程序里当前 member 之前是否有 `mkdir` 路径等于目标 parent。
  - 不把上一轮执行的 `CheckInfo` 成功/失败作为 hard gate；文件树每轮恢复后，上一轮的 fresh parent 结果不能可靠外推到下一轮。
  - 跨程序 fresh parent 的上一轮 `CheckInfo` 时间关系只作为 soft valid 证据，不作为 hard guarantee。
  - 不跟踪“前面的 mkdir 之后、当前 member 之前又被 rmdir/unlink 删除”的情况；这类路径可能通过校验但在实际执行时 parent 已不存在。
  - 当前接受这个近似，不把它当作高优先级正确性问题。

- **directed target arm 不支持 rename**
  - `pickPathSetTarget()` 要求 anchor 和 member 都是 `pathSetSinglePath`，即每个 member 只有一个 `pathSpec`。
  - rename 在 `buildPathMembers()` 中同时携带 source/destination 两条路径，因此不会成为 directed target 的 anchor 或 member。
  - broad `relocatePathSet()` 仍可以把 rename 作为 PathSet 成员整体迁移，只是不能对它做定向 target 规划。
  - 如果后续需要支持，需要给 `pathTarget` 增加 `AnchorSpec` / `MemberSpec`（选择 source 或 destination），并同步改造候选生成、target level 和校验；当前版本不实现。

### 1.10 Plan-level 路径校验

在 `buildPathPlan()` / `buildTargetPathPlan()` 生成 plan 后、`applyPathSetPlan()` 之前增加 plan 级校验，避免只做逐 path 校验而漏掉成员数量、父目录可执行性和跨程序 fresh parent。

#### Hard valid

以下条件必须全部满足，否则该候选 plan 直接丢弃：

1. plan 完整性：
   - `len(plan.Updates) == len(set.Members)`；
   - 每个 `update.Pos` 与对应 `member.Pos` 一致；
   - 每个 `update.Specs` 数量与 `member.Paths` 数量一致。
2. existing path 存在且类型匹配。
3. fresh parent：
   - parent 在 baseline FileTree 中是目录；或
   - parent 是同一 Prog 里更早的 mapped mkdir 创建；
   - parent 不能是 file。
4. rename：
   - source 存在且类型匹配；
   - destination 按 existing/fresh 语义校验；
   - source/destination 不能形成 no-op；
   - destination 由 RenamePolicy 选，不能绕过 policy fallback。
5. link/symlink：
   - target 是 canonical 测试根路径，映射后存在且类型匹配；
   - linkpath fresh，parent 有效，target != linkpath；
   - symlink payload 由映射后的 canonical target/linkpath 重算，解析后不逃出
     测试根；
   - readlink linkpath 必须和对应 symlink linkpath 一致。

#### Cross-Prog fresh parent：soft valid

如果 parent 不在 baseline，也不是同 Prog 的更早 mapped mkdir：

- 允许从其他 Prog 的 mapped mkdir 寻找创建者；
- 双方 `CheckInfo` 必须存在；
- 使用 `tscoffFor(tscoffs, progIdx)` 归一化时间；
- 只有 `mkdir.Etime < consumer.Stime` 时才允许；
- 这种情况标记为 soft parent：可以提交，但作为降级候选；
- 时间关系不成立、`CheckInfo` 缺失，或无法形成跨节点可见性证据时，hard invalid。
- 这里不是判断运行时“不可见”，而是没有可证明的跨节点创建顺序；运行时可见性无法仅靠程序路径静态确认。

#### 重复 fresh path

- 不 hard reject；
- 不降低候选优先级；
- 只增加诊断计数，例如 `DuplicateFresh`；
- 多个调用并发创建同一 fresh path 是预期副作用，最终由执行时的 EEXIST/覆盖/竞争决定结果。

#### 候选选择

`relocatePathSet()` 与 `mutatePathSetDirected()` 选择 plan 时：

- hard valid 优先；
- target level 优先于 soft parent；
- duplicate fresh path 不参与降权；
- hard valid 候选全部失败时，维持 legacy dynamic path fallback。

#### Legacy fallback 边界

legacy fallback（`mutateGroupPathDirected` / `findGroupCalls` /
`updateCallPathInProg`）也必须 profile-aware：

- `rename`：保留在 legacy fallback 中，但 dst 必须走 `RenamePolicy`，不能硬编码
  HMDFS same-parent `._renamed_`。HMDFS policy 复现现有 same-parent suffix 行为；
  GlusterFS policy 可选 cross-parent/fresh/replace。
- `link/symlink/readlink`：不进入 legacy fallback。它们只走 PathSet broad、
  DCT mutation、derived、directed；legacy `updateCallPathInProg` 只改 arg0，会
  破坏 linkpath/payload/readlink binding。
- `pickAnchor`/`findGroupCalls`/owners 更新加 `legacyPathMutableCall` gate，不能
  只在 apply 阶段跳过。
- `updateCallPathInProg` 接收 `lcs`/`RenamePolicy` 上下文；rename 先算 dst 再写
  source/dst，policy 失败则不修改调用。
- PathSet target arm 仍然不选 rename；legacy rename 是 path mutation，不是 target
  arm。

#### 实现落点

- 新增 `pathPlanQuality` 或等价返回值：
  ```go
  type pathPlanQuality struct {
      HardOK         bool
      SoftParent     bool
      DuplicateFresh int
  }
  ```
- 新增 `validatePathSetPlan(ps, set, plan, ft, tscoffs)`。
- 在 `buildPathPlan()`、`buildTargetPathPlan()` 返回前调用。
- 在 `relocatePathSet()`、`mutatePathSetDirected()` 应用前再防御一次。
- 跨程序校验需要 `tscoffs`；当前 `lcs.tscoffs` 已可用。
- `pathSetConnected()` 继续只做连通性校验，不承担冲突策略。

实现约束：

- `pathSetSpecValid()` / `pathSetParentValid()` 不能只用原始调用路径判断 mapped fresh parent；否则前面的 mkdir 被 plan 映射到新路径后，后面的 child 会在构造阶段被误拒。
- 增加 `pathSetSpecValidProvisional(...)`，或在构造 plan 时使用 provisional parent check：构造阶段只拒绝确定错误（baseline 类型、file parent、spec 结构），把 fresh parent 的最终判断交给 `validatePathSetPlan()`。
- `pathSetTargetMemberPaths()` 和 `pathSetFallbackPath()` 的调用也要走 plan-aware 校验，不能继续只调用原始 `pathSetSpecValid()`。
- `validatePathSetPlan()` 必须基于 mapped path 查找 same-Prog 更早的 mapped mkdir；跨 Prog 才使用 `tscoffs` 归一化后的 `CheckInfo` 时间关系，并将其标记为 soft parent。
- 重复 fresh path 只记 `DuplicateFresh` 诊断，不 hard reject、不降权。

实现状态（2026-11-09）：`pathPlanQuality`、`validatePathSetPlan()`、provisional spec/parent 校验和 `DuplicateFresh` 诊断已实现；Linux cross-compile/vet 通过，Ubuntu runtime 待验证。

---

## 2. 数据突变分析

### 2.1 当前逻辑

`MutateGroupDataDynamic`：

- 选一个读/写/truncate 作为锚点；
- 用 `findGroupCalls` 取跨节点并发集；
- 为整个集共享 offset/length；
- 写调用重建数据缓冲区，读调用调整接收缓冲区，truncate 改长度。

### 2.2 结论

- 基本不需要改：
  - 只改 offset/length/buffer；
  - 不改路径、不改调用顺序；
  - 不会像路径突变那样打断已有路径关系。
- 同节点数据链目前不在组内：
  - 例如 `write → read`、`truncate → write`；
  - 不破坏结构，只是数据共享范围比完整因果链小；
  - 如果论文声称操作组包含因果链，需明确数据突变是否也覆盖同节点数据前驱/后继。
- 风险：
  - 数据突变可能改变 ret/success；
  - 如果相关修改失败，已有因果边可能消失；
  - 这是语义探索结果，不是路径断裂。

建议：先保持现状；如果评估显示“同节点数据链共享同一区段”有价值，再把同节点数据前驱/后继加入 `positions`，做成可选开关。

---

## 3. 删除突变分析

### 3.1 当前逻辑

- `RemoveGroupDynamic`：删除锚点 + 跨节点并发/因果组，fd 安全过滤。
- `RemoveReservoirDynamic`：随机删除每个程序至多一个 fd 安全调用。
- 两者都不改变剩余调用的路径。

### 3.2 需要区分两层“连接”

- **路径相关性**：删除不改路径，剩余调用之间的同一/父子/兄弟关系仍在；反馈的任意深度父子判定仍成立。
- **因果 HB 链完整性**：删除中间节点会打断 `m1 → m2 → m3`，当前实现不保证。

### 3.3 结论

只要删除的语义是“破坏已有交错结构、控制规模”，当前实现可以保留：

- 已保证 fd 安全；
- 不留下悬空 fd；
- 删除后后续调用可能 ENOENT，但程序结构仍合法；
- 不需要 PathSet 识别。

如果要求“删除后因果长链仍完整”，则需要：

- 整条链一起删；或
- 只删链端点；或
- 删除连续子段并重新连接边界。

成本较高，暂不做。路径层面的引用检查可在后续按需增加。

---

## 4. 决策与待定项

| 项目 | 当前倾向 | 状态 |
|---|---|---|
| PathSet 范围 | 路径关系连通分量，包含 sibling，不做时间过滤 | 已定 |
| 根下 sibling | 纳入 PathSet；LCA 为根时做森林换父目录 | 已定 |
| fresh path | 允许参与 PathSet；父目录存在或同程序先前创建，跨节点保守失败 | 已定 |
| 回退策略 | 固定顺序，不使用关系分数；目标关系优先 | 已定 |
| sibling 类型冲突 | 先父子，再 Same，最后 newBase/abort | 已定 |
| level 1/2 | 可提交种子，但不算 exact hit | 已定 |
| 数据突变 | 保持现状 | 倾向 |
| 删除突变 | 保持现状 | 倾向 |
| 因果根 / root→variant | 已迁移到 `docs/causal-root-cell-design.md` | 已迁移 |

---

## 5. 代码位置索引

行号可能随实现变动，以下为讨论时位置。

- `findGroupCalls`：`src/prog/mutation.go:2634`（保留给数据/删除突变）
- `MutateGroupPathDynamic`：`src/prog/mutation.go:2766`（已接入 PathSet target arm）
- `mutatePathSetDirected`：`src/prog/path_set.go:1003`
- `MutateGroupDataDynamic`：`src/prog/mutation.go:3270`
- `RemoveGroupDynamic`：`src/prog/mutation.go:3081`
- `PathSet` 实现：`src/prog/path_set.go`（已实现）
- `PathSetDiagnostics`：`src/prog/path_set.go:95`
- `pickNewBasePath`：`src/prog/mutation.go:3836`
- `memberPathConstraint`：`src/prog/mutation.go:2722`
- `resolveFdTarget`：`src/prog/mutation.go:3798`
- `updateCallPathInProg`：`src/prog/mutation.go:4000`（legacy path mutation；
  rename dst 走 `RenamePolicy`，不用于 PathSet apply）
- 因果根设计：`docs/causal-root-cell-design.md`

---

## 6. 暂不纳入

- 因果根 / root→variant 两层设计（见 `docs/causal-root-cell-design.md`）；
- 反馈状态空间大小与“合适性”论证；
- 具体 Go 实现参数与调参；
- 覆盖板 cell 公式修改；
- 评测指标与消融设计；
- 与论文 C1/C2 挑战重构的最终措辞（等论文逻辑讨论后再定）。

---

## 7. 一句话总结

> PathSet 路径突变保持路径关系连通：单顶层做前缀迁移，多顶层做森林换父目录，回退按固定关系阶梯执行，不引入关系分数；GlusterFS v1 broad 支持 link/symlink/readlink/rename 双路径，canonical target/linkpath 一起迁移，symlink payload 在调用物化时重算，rename dst 由 RenamePolicy 选（existing 优先、fresh 兜底）；数据突变路径不变，基本不动；删除突变只要接受“破坏性删除、只保证 fd 安全”，当前实现可以保留。因果根与 root→variant 两层设计见 `docs/causal-root-cell-design.md`。
