# Rename Policy 抽象记录

> 状态：设计记录（2026-10-28；2026-11-09 对齐 A+ rename anchor role 边界；2026-11-15 定 GlusterFS v1 policy/PathSet 迁移，代码待实现）。
> 范围：rename 的 source/destination 路径规则如何跨文件系统复用。
> 关联：`docs/seed-derivation-design.md`、`docs/causal-root-cell-design.md`、`docs/cross-node-fresh-path-notes.md`。
> 本文只记录抽象方向和接口草案，不修改代码。

---

## 1. 结论

- 只增加一个“允许的 relation 集合”不足以支持 future DFS 的跨目录 rename。
- 当前 same-parent、fresh/existing destination、类型/空目录等规则散落在 C1/C2、DCT、seed derivation 和 validation 中。
- 建议引入 `RenamePolicy`：由 policy 提供 source relation、destination 候选和校验；核心只负责派生、抽样、插入和记账。
- A+ 的 rename source/destination role 属于 **C1 / seed derivation 的 anchor 选择语义**；`RenamePolicy` 只负责 rename 操作级的候选与 validate，不负责 anchor→root relation。
- 不要求现在就实现，但后续 rename 相关修改应尽量朝这个边界收敛。

---

## 2. 为什么 relation 集合不够

rename 至少涉及三个决策：

```text
1. source 与 anchor 的关系
2. destination 的 parent 选哪里
3. destination 是 fresh 还是 existing，以及类型/空目录/overwrite 规则
```

当前 HMDFS 规则主要是：

```text
destination parent = source parent
destination = fresh sibling / existing same-class sibling
```

“同父”不是 relation 集合能表达的东西；它同时影响：

- destination 路径生成；
- validation；
- E overwrite 候选；
- fresh path 类型；
- failure shape；
- DCT / C1 / C2 / seed 的复用。

因此 future DFS 如果允许跨目录，只扩展 source relation，代码仍然会被 same-parent 检查挡住。

---

## 3. 接口草案

```go
type RenameRole int

const (
    RenameRoot RenameRole = iota
    RenameVariant
)

type RenameContext struct {
    Role       RenameRole
    AnchorPath string
    Source     string
    FileTree   *FileTree
}

type RenameDest struct {
    Path  string
    Fresh bool
    IsDir bool
}

type RenamePolicy interface {
    SourceRelations() []PathRelation
    DestParentCandidates(ctx RenameContext) []string
    DestCandidates(ctx RenameContext, parent string) []RenameDest
    Validate(ctx RenameContext, source, dest string) bool
}
```

核心流程保持稳定：

```text
source = 按 anchor→source relation 派生
for parent in policy.DestParentCandidates(...)
    for dest in policy.DestCandidates(..., parent)
        if policy.Validate(source, dest)
            materialize(rootSource, rootDest)
```

约束：

- planner 生成和 live revalidation 必须调用同一个 `Validate`；
- policy 不负责 anchor→root 的 relation 分类；
- policy 不负责跨节点 placement / timing；
- policy 返回候选，核心负责 RNG 抽样、`freshKinds`、插入和 batch authority。

### 3.1 Anchor role 边界

A+ 的 rename source/destination role 不属于 `RenamePolicy` 的选择职责：

- source anchor 只允许 `Parent/Sibling/NoRel`；
- source 的 `Same/Child` 排除；
- root path 不得落在成功 rename 的 source 子树内；
- 这些规则由 C1 / seed derivation 的 anchor→root 候选层负责。

如果未来 caller 需要在 `RenameContext` 中携带 `AnchorRole` / `AnchorSource`，它只是上下文 metadata；`RenamePolicy` 不应据此替代 anchor→root relation 选择，也不应把 `pathUnderRoot` 过滤塞进 `Validate`。

---

## 4. HMDFS Policy 映射

当前行为可以作为第一个实现：

```text
SourceRelations:
  按现有 rename source relation 集合；
  anchor source role 的 Same/Child 过滤由 C1/seed 层负责

DestParentCandidates:
  [parent(source)]

DestCandidates:
  freshRenameDst(source)
  existing same-parent same-class sibling（E）

Validate:
  source/dest 同父
  source != dest
  destination 类型匹配
  existing directory 必须为空（E）
```

对应现有代码：

- `renameSameParent` → HMDFS policy 的 `Validate` 核心；
- `pickRenameDst` / `freshRenameDst` / `existingRenameDstSibling` → `DestCandidates`；
- `pickRenameReplaceDst` → E destination 候选；
- C1/C2、DCT、seed derivation 的 rename validation → 统一调用 policy。
- `seed_derivation.go` 的 `seedAnchorRole` / `seedRootPathAllowed` → C1/seed 层的 source role 和 source 子树过滤，不属于 `RenamePolicy.Validate`。
- `insertDerivedCallFromDCT` 使用 `rootObservedPath`；rename root 使用 destination。
- `causal_root.go` 的 rename source 子树过滤同样属于 root candidate 层。

保持 `rootPre = source`、`rootPost = dest` 的双路径语义不变。

### 4.1 GlusterFS v1 Policy 与 PathSet 迁移

GlusterFS v1 的 policy 更宽松：

```text
SourceRelations:
  沿用 anchor→source 的 Same/Parent/Child/Sibling/NoRel；source 子树过滤仍由
  C1/seed 层负责

DestParentCandidates:
  source parent
  其他已存在且不是 source 后代的目录

DestCandidates:
  fresh child name
  existing same-class file / empty dir（replace）

Validate:
  source 存在
  destination parent 存在
  source != destination
  destination 不是 source 后代
  file -> dir = EISDIR，dir -> file = ENOTDIR
  dir -> 非空目录 = ENOTEMPTY，dir -> 空目录 = Success
  file -> existing file = Success（replace）
  所有路径在测试根/同一 volume 内
```

PathSet 的 rename 迁移必须在**生成阶段**调用 policy，而不是先 generic
`mapPathSetPath` 再验证：

```text
1. mapped existing dst（保持原路径关系）
2. 其他能保持关系的 existing dst
3. policy 允许的 fresh dst（parent 为已有目录或同一 plan 中更早的 mkdir）
4. reject
```

- existing replace 还要检查目标没有被同一 PathSet 其他成员依赖；精确规则：
  任一其他成员的 `pathSpec.Path == dst`，或任一 fd-only 成员的 owner 路径
  `== dst`，就拒绝 existing，改用 fresh；
- fresh dst 是 total fallback：source parent 在 plan 中有效时，fresh sibling 总能
  构造；existing dst 不存在不应让 rename 失败；
- `pathSetFallbackPath` 对 rename dst 不能绕过 policy；无候选就失败；
- 最终仍跑 `pathSetSpecValid` / `pathSetMemberValid` / `pathSetConnected`；
- HMDFS policy 只返回 same-parent 候选，PathSet 现有行为不变；
- GlusterFS policy 允许 cross-parent/fresh/replace；
- legacy path fallback（`updateCallPathInProg`）的 rename 也调用同一 policy：
  HMDFS 复现现有 same-parent `._renamed_` suffix 行为，GlusterFS 可选
  cross-parent/fresh/replace；不能硬编码 HMDFS same-parent。
- PathSet directed target arm v1 不选 rename；跨目录 rename 只作为 broad
  relocation 成员。

---

## 5. Future DFS Policy

跨目录 DFS 可以这样表达：

```text
SourceRelations:
  由具体文件系统决定

DestParentCandidates:
  根据 source parent / anchor / FileTree 生成允许的 parent 候选

DestCandidates:
  fresh child name
  existing file / empty dir（是否允许由 FS 决定）

Validate:
  由具体文件系统实现
```

如果 future FS 的约束只是路径关系，可用配置型 policy；如果还涉及 mount、权限、快照、原子性等，policy 可以返回更少候选或在 `Validate` 中拒绝。

---

## 6. 不放进 RenamePolicy 的内容

- anchor→root 的 source relation 选择；
- rename source 子树 / root path 过滤；
- root→variant 的 DCT 选择；
- 跨节点插入位置和可见性；
- C1/C2 cell 选择；
- pending/miss 记账；
- seed anchor 选择。

这些仍由各自层次负责。

---

## 7. 当前不做 / v1 边界

- HMDFS 的 `renameSameParent`/`pickRenameDst` 行为不变；内部可以保留现有实现，
  由 policy wrapper 调用，不要求先做无行为重构。
- HMDFS 不新增跨目录 rename。
- GlusterFS v1 启用跨目录 rename；DCT seed/mutation/derived、C1/C2 和 PathSet
  的 rename dst 统一走 policy hook，不能各写一套。
- Policy 只负责 candidate/validate；anchor→root relation、source 子树过滤、
  C1/C2 cell、PathSet 连通性仍由各自层次负责。
- 不修改测试、论文或 PPT；测试在实现阶段补。

本记录固定抽象边界：**RenamePolicy = source relation + destination parent
candidates + destination candidates + validate**，而不是单一 relation 集合。
