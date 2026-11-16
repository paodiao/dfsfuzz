# 跨节点 fresh 路径插入记录

> 状态：设计记录 / 已知边界（2026-10-28；2026-11-09 对齐 seed derivation / derived insertion；2026-11-15 增加 GlusterFS v1 决策），跨节点 fresh child 仍暂不实现。
> 范围：C2（固定 root 的 variant 规划）与 DCT/derived 跨节点插入中的 fresh 路径。
> 关联：`docs/causal-root-cell-design.md`、`docs/seed-derivation-design.md`。
> 本文只记录现状、边界和待研究项，不修改 C1/C2/DCT 代码或现有设计文档。
>
> 2026-11-17 GlusterFS：`freshSameForms` 仍作为完整 fresh-fresh 竞赛矩阵；
> GlusterFS 的 plannable 只在 `Rel=Same` 且 root 是 `mkdir`/`creat` 时使用该
> 矩阵，并在对应 combo 的 model 中补充 race ENOENT。最终语义见
> `docs/glusterfs-adaptation-plan.md` §8.2.3。

---

## 1. 结论

- C2/DCT 当前只覆盖部分 **安全的 fresh 组合**。
- `fresh root + fresh child` 的跨节点插入尚未实现。
- 不能把 C1 的 `both fresh` 关系矩阵直接外推为 C2/DCT 的跨节点支持承诺。
- 跨节点时间关系和目录可见性比单节点路径关系复杂，本阶段先记录，不据此修改代码。

---

## 2. C2 当前行为

### 2.1 已覆盖

- **root fresh + variant Same**
  - fresh root 通过 `freshSameForms` 枚举 fixed-root 的 Same cell；
  - `FreshSame` 在 batch 中把 variant 的 `BaseB` 对齐到 root 的 fresh path；
  - 跨节点插入时 root 在 node 0、variant 在 `NodeX`，只依赖同一路径，不要求 fresh parent 在其它节点先行存在。

- **root fresh + variant fresh sibling**
  - variant 可以通过 `FreshB` 在已有父目录下创建 fresh sibling；
  - `FreshB` 的 `BaseB2` 来自已有 `FileTree` 父目录，因此跨节点插入时父目录是已存在的。

- **root normal + variant FreshB**
  - `FreshB` 的父目录必须已存在；
  - 适合跨节点插入，因为 parent 不需要由当前 root 刚创建。

- **variant fresh rename destination**
  - source 是已有路径，destination 是 fresh sibling；
  - 跨节点插入时 source 和 parent 都是已有状态。

### 2.2 未覆盖

**root fresh + variant fresh child** 当前不会生成：

- `planCausalC2Cells` 在 `rootKind == causalRootFresh` 时只处理 `freshSameForms`；
- `FreshB` 只从 `directedView.bNodes` 的已有父目录生成；
- 没有把 fresh root path 作为 parent 合成 `FreshB` 的路径；
- 因此 fresh root 下的 `Child` variant 没有 C2 cell。

这不是简单的 relation 过滤问题，还涉及：

- root 在 node 0 创建 fresh parent；
- variant 在 node X 创建 child；
- child 插入时 parent 是否已可见；
- HB/CC 只表达时间意图，不保证跨节点目录状态已经同步。

### 2.3 C2 相关代码

- `src/prog/causal_root.go`
  - `planCausalC2Cells`：fresh root 的 Same 分支，以及 `FreshB` 的已有父目录来源；
  - `pickCausalRootBatch`：`FreshB` / `FreshRenameDst` 的路径 materialize；
  - `validateCausalPlan`：`FreshSame`、`FreshB`、rename 的 live validation。
- `src/prog/directed_plan.go`
  - `insertDirectedBatch`：root 与各 `NodeX` variant 的分节点提交；
  - `directedVariantCallsSeq`：variant 调用生成；
  - `freshSameForms`：fresh Same 的固定形态表。

---

## 3. DCT 当前行为

### 3.1 已覆盖

- fresh base + `Same`
  - `GetPathByRelation` 对不在 FileTree 的 fresh base 返回 base path 本身。
- fresh base + `Parent/Sibling/NoRel`
  - Parent 从已有父目录派生；
  - Sibling 从已有父目录的兄弟集合派生；
  - NoRel 从无关已有路径派生。
- 已有目录下 `mkdir` variant 的 `Child/Sibling/NoRel`
  - `insertCallFromDCT` 对 `mkdir` 使用 `freshChildPath` 生成新名字；
  - 适用于已有 base，不属于 fresh base 下的 fresh child。

### 3.2 未覆盖

**fresh base + Child variant** 当前被显式排除：

- `availableRelationsForBase` 对 fresh base 把 `Child` 设为 false；
- `GetPathByRelation` 对 fresh base 的 `Child` 返回空；
- `insertCallFromDCT` 中 mkdir Child 的 `freshChildPath` 分支只有在 relation 被选中时才执行，因此从 fresh base 出发实际不可达。

现有注释把 fresh base 视为“没有已有 child”，这是为了避免跨节点依赖尚未创建的 parent。它是有意的限制，不是完整的 fresh-child 支持。

### 3.3 其他缺口

- `freshPathKindsBefore` 已扫描 `mkdir`、`creat`、`rename` destination，并支持 `open(O_CREAT)` 的 fresh file 类型提示；`open(O_CREAT)` 的跨节点可见性和 runtime 结果仍待验证。
- fresh base 作为 rename source 时，`GetPathsForRenameVariant` 可能返回该 fresh path；如果 variant 在另一个节点执行，source 是否已经可见没有显式保证。
- generic `insertCallFromDCT` 的跨节点 loop 使用 p0 的 basePath 和已有 FileTree 选 variant 路径；它没有表达“fresh parent 必须先于 variant 可见”的跨节点约束。
- derived insertion 使用 `insertDerivedCallFromDCT`，root 使用 `rootObservedPath`（rename 为 destination）；它同样没有表达 fresh parent 的跨节点可见性。

### 3.4 DCT 相关代码

- `src/prog/distributed_choice.go`
  - `GetPathByRelation`：fresh base 的 Same/Parent/Sibling/NoRel 路径；
  - `availableRelationsForBase`：fresh base 的 relation 过滤；
  - `getRandomUnrelatedPath`：NoRel 路径。
- `src/prog/mutation.go`
  - `insertCallFromDCT`：generic DCT root 与各节点 variant 的插入；
  - `insertDerivedCallFromDCT`：derived root/variant 插入；root 使用 `rootObservedPath`，rename 使用 destination；
  - `freshPathKindsBefore`：同一程序内 fresh path 类型提示，支持 `open(O_CREAT)`；
  - `freshChildPath`：mkdir/creat 新名字；
  - `findOpenFdForPath`：跨节点 fd 复用。
- `src/prog/seed_derivation.go`
  - `seedAnchorRole` / `seedRootPathAllowed`：derived seed 的 rename source/destination role 和 source 子树过滤；
  - 1 层 fresh parent 检查。
- `src/prog/rand.go`
  - `freshChildPath` 的路径命名约定。

---

## 4. 跨节点时间与可见性问题

以下问题尚未建模：

1. root 在 node 0 创建 fresh parent，variant 在 node X 创建 child 时，parent 是否可见？
2. HB 是否要求 parent 创建完成后 child 才开始，还是只要求最终路径关系成立？
3. CC 下 variant 可能先于 fresh parent 执行，失败是否能作为合法的并发结果记录？
4. 跨节点 fresh child 的 miss 应记在 C2 cell、DCT 账目，还是只作为 insertFail？
5. 是否允许 fresh parent 只存在于 node 0，而其它节点通过全局合并视图最终看到？
6. 同节点 fresh child 与跨节点 fresh child 是否需要不同的 relation/validation 规则？
7. fresh rename source 与 root 的跨节点可见性是否应禁止，还是作为竞态保留？

在这些问题确定前，不能把 `fresh root + fresh child` 视为安全的跨节点组合。

---

## 5. 当前决策

```text
当前不实现 fresh root + fresh child 的 C2/DCT 跨节点插入。
C1 的 both fresh Child 只支持同一 p0/C1+C2 本地链路，且第一版只支持 1 层 fresh；
nested fresh parent 不生成 fresh-root/both-fresh Child related plan。
C2 的 fresh-parent validation 只服务于该本地链路，不代表跨节点支持。
fresh same-path 与 fresh child 分开处理：
  - FreshSame：保留现有固定形态；
  - FreshB：只允许已有 parent；
  - fresh root child：待跨节点可见性模型确定后再设计。
```

### 5.1 GlusterFS v1

GlusterFS 是共享 volume，但 v1 仍按上面的保守规则执行：

- 不实现 C2/DCT 的 fresh root + fresh child；root 在 node 0 创建目录、variant
  在 node X 创建 child 的组合不生成；
- `FreshSame` 只保留现有 `creat`/`mkdir` 固定形态；
- `FreshB` 只允许 baseline 已有 parent；fresh sibling 的 parent 必须已存在；
- C1 both-fresh Child 只支持同一 p0/C1+C2 本地链路；
- 只支持 1 层 fresh：fresh anchor/root 的 parent 必须在 FileTree 中已存在；
  nested fresh parent 不生成；
- `open(O_CREAT)` 作为 anchor path 的 fresh file creator 可以使用，但不新增
  fresh-root + fresh-child 的 C2/DCT 形态；
- PathSet 迁移 fresh path 后仍走同一 `freshParentNodeEligible`/
  `pathSetPlanParentValid` 校验，不因为 GlusterFS 共享 volume 放宽。

理由：`HB` 只表达时间意图，不保证目录已跨 client 可见；共享 volume 的缓存/xattr
异步状态也不能替代可见性证明。等跨节点可见性模型和 runtime 测试完成后再放开。

---

## 6. 后续测试清单

新增支持前至少需要：

- C2 fresh root + `FreshSame` 的跨节点插入测试；
- C1 both-fresh Child 的本地 C1+C2 fresh-parent 正向测试；
- C2 fresh root + fresh child 的跨节点负向测试（当前应不生成）；
- C2 root normal + `FreshB` 的跨节点插入测试；
- DCT fresh base + `Child` 的负向测试（当前应被过滤）；
- DCT fresh base + `Same/Parent/Sibling/NoRel` 的正向测试；
- `open(O_CREAT)` fresh path 类型识别已有代码支持；runtime/negative 仍待补；
- `insertDerivedCallFromDCT` 的 fresh base / rename destination 跨节点负向测试；
- nested fresh parent 不生成 fresh-root / both-fresh Child related plan 的负向测试；
- fresh base rename source 的跨节点行为测试；
- 跨节点 parent 可见性/时序的 runtime 测试，需 Ubuntu 环境；
- GlusterFS v1 负向测试：C2/DCT fresh root + fresh child 不生成；
- GlusterFS v1 正向测试：FreshSame、FreshB、C1 本地 both-fresh 1 层仍生成。

---

## 7. 不在本文范围

- 修改 C2/DCT 的跨节点 fresh-child 支持；
- 修改 `availableRelationsForBase` / `GetPathByRelation` 以开放 fresh base Child；
- 新增跨节点 fresh-child C2 cell；
- 修改 C1 both-fresh 关系矩阵；
- 修改 seed 生成逻辑；
- 修改论文或 PPT。
