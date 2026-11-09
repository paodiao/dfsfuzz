# Seed 纯派生构造设计

> 状态：设计讨论稿（2026-10-28）；初版代码已实现，Linux 交叉编译/vet 通过，Ubuntu runtime 待验证。2026-11-09 复核项代码已落，补充 rename anchor A+、derived mkdir Same/Parent fallback、1 层 fresh 边界和 fresh anchor + rename root Child 负向覆盖；部分计划测试待补，Linux 交叉编译/vet 通过，Ubuntu runtime 待验证。
> 范围：from-scratch seed 的路径派生、`insertCallFromDCT` 的 base 路径选择，以及 mutation/seed expansion 的边界。
> 关联：`docs/causal-root-cell-design.md`、`docs/seed-optimization-plan.md`、`docs/seed-construction-causal-chain-notes.md`。
> 本文只记录设计与待讨论项，不修改论文或代码。

---

## 0. 摘要

当前 from-scratch seed 的 DCT 生成逻辑会从 FileTree 随机选择 root base path；`insertCallFromDCT` 在 seed expansion 和 mutation 中也可能使用随机或 fresh 基路径。这样生成的 seed 可能包含多个互不相关的路径分量，而不是围绕已有调用形成长程路径链。

本设计把 seed 构造拆成：

```text
initial group   // 第一组，特殊处理，允许随机 base
derived groups  // 后续组，base 必须从 p0 已有调用派生
```

核心目标是：

- 除第一组外，不再随机选择 FileTree base；
- 新 root 的 base path 从 p0 anchor 派生；
- root→variant 关系继续使用现有 DCT；
- 新增 anchor→root 的 causal derivation table；
- NoRel 保留低概率，不强制 seed 单一路径分量；
- seed expansion 的 root 插在 anchor 后面；
- mutation 保留随机插入位置，但 base path 仍走派生；
- seed derivation 本身不改变 C1/C2 算法；C1 anchor freshness 在 causal-root 文档单独跟踪；
- 不改变 `MutateGroupPathDynamic` 和 `filterBaseBAuthority`。

---

## 1. 当前行为与问题

### 1.1 Initial group 随机 base

`generateFromDistributedChoiceTable` 先选 root call，再从 FileTree 随机选 base path：

- `src/prog/rand.go` 中的 `generateFromDistributedChoiceTable`；
- 对 `IsDirOnlyCall` / `IsFileOnlyCall` / 其它 root 分别选择目录或文件；
- `mkdir/creat/rmdir` 再做 fresh/空目录预处理。

这是合理的“第一组特殊处理”，因为此时没有已有 anchor 可以派生。

### 1.2 `insertCallFromDCT` 也有随机 base

`src/prog/mutation.go` 的 `insertCallFromDCT`：

- 约 30% 概率使用 fresh/random base；
- 否则从 p0 调用路径中选候选；
- 没有兼容候选时回退到 `GetRandomFile` / `GetRandomDir`；
- `applyFailureShape` 还可能重写 base path / rename source/destination。

该函数被两个入口使用：

- seed expansion：`src/prog/generation.go` 的 `expandWithDCT`；
- mutation：`src/prog/mutation.go` 的 `MutateFsopsWithDCT` 回退路径。

因此只改 seed expansion 不够，mutation 仍会重新引入随机路径分量。

### 1.3 DCT 的职责

DCT 的 `root → variant + relation + weight` 同时用于并发和因果：

- `ChooseVariantFiltered` / `chooseVariant` 选 variant 和路径关系；
- `ChooseTemporal` 另外选 CC/HB；
- 现有 `PathNoRel` 权重为 10，低于 Same/Parent/Child/Sibling。

普通 DCT/2a 不包含 `anchor → root` 的关系模型。纯派生需要一个新的 causal derivation table，或者复用 DCT 的权重公式；本设计选择新增表，但初始权重复用现有公式。

### 1.4 DCT、C1、C2 的边界

这里必须区分三条不同的构造线：

| 构造线 | 方向 | 选择依据 | 是否使用 p0 CheckInfo |
|---|---|---|---|
| 普通 DCT/2a | root → variant | DCT root/variant 权重 | 否 |
| C1 causal root | anchor → root | 未覆盖 C1 cell + p0 anchor | 是 |
| C2 fixed-root variant | 固定 root → variant | C1 固定 root + C2 cell | 是 |

具体含义：

- **普通 DCT/2a**：
  - 先选 root call；
  - 再从 FileTree 或已有路径选 root base；
  - 只用 DCT 规划 root→variant；
  - 没有 anchor→root 目标。
- **C1**：
  - 从 p0 已有调用选 anchor；
  - 从 anchor 推导 root path；
  - 支持普通/fresh/rename root；
  - 需要 p0 调用的 `CheckInfo`，并以未覆盖 C1 cell 为选择目标。
- **C2**：
  - root 已由 C1 固定；
  - 只规划 root→variant；
  - 使用 C2 pending 和 validation。

`PickDirectedBatch` 的顺序是：

```text
先尝试 C1/C2 causal batch
失败后回退普通 DCT
```

因此：

- “定向插入”作为整体可以有 anchor→root，因为它是 **C1**；
- “普通 DCT”本身没有 anchor→root；
- seed generation 目前走普通 DCT/`insertCallFromDCT`，不直接调用 C1，因为生成阶段没有 p0 `CheckInfo`，也没有 C1 未覆盖 cell。

seed derivation 可以复用 C1 的 root path 派生约束，例如：

- `causalRootPaths` 的关系路径枚举；
- C1 的 fresh root 只允许 Child/Sibling；seed 额外允许 NoRel，见 §6.2；
- rename root 的 source/destination 规则；
- root 类型和路径合法性检查。

但不能直接复用 C1 的：

- C1 cell 选择；
- anchor `CheckInfo` 时间校验；
- C1 pending/反馈目标选择。

本设计的 `CausalDerivationTable` 是一个 **seed-level anchor→root 选择器**，不是 C1；它只借用 C1 的路径派生语义。

---

## 2. 目标与非目标

### 2.1 目标

- 第一组之后，新 root base 必须来自 p0 已有调用；
- seed expansion 的 root 插在 anchor 后面；
- mutation 的 root 保留随机插入位置；
- 本轮 seed expansion 和 mutation 都切换到 derived path；mutation 只保留随机 `insertPos`，不保留随机 base 选择；
- anchor→root relation 有独立概率表；
- `mkdir/creat/rename` 的 fresh path 仍然可构造；
- NoRel 保留低概率；
- 不随机 fallback，找不到兼容 anchor 时允许失败；
- 保持既有 DCT root→variant、pending、fd lifecycle、时间对齐逻辑。

### 2.2 非目标

- 不修改 `MutateGroupPathDynamic` 的组路径迁移；
- seed derivation 本身不修改 C1/C2 算法；C1 fresh-anchor / C2 fresh-parent 扩展在 `docs/causal-root-cell-design.md` 单独实现，与本轮代码一起落地；
- 不修改 `filterBaseBAuthority`；
- 不强制 seed 只有一个路径分量；
- 不重写 DCT 的 root→variant 权重；
- 不在本阶段引入完整文件状态模拟；
- 不修改论文/PPT。

---

## 3. 两阶段构造

### 3.1 Initial group

这是唯一允许随机 FileTree base 的阶段。

流程：

1. 按 DCT 选 root call；
2. 从 FileTree 选类型兼容的 base path；
3. `mkdir/creat/rename` 走 fresh 预处理；
4. 生成 root 和第一组 variants；
5. 记录 root/variant 路径，供后续派生使用。

initial group 保留当前行为，不强制改造成派生。

### 3.2 Derived groups

后续所有组：

1. 只从 p0 已有调用中收集 anchor；
2. 选 anchor call/path；
3. 用 `CausalDerivationTable` 选 `(new root call, anchor→root relation)`；
4. 从 anchor path 派生 root path；
5. 生成 root，并插在 anchor 调用后面；
6. root 的 variants 继续用 DCT 选；
7. variant 的位置相对 root 计算，不相对 anchor 计算；
8. 不随机回退到 FileTree。

因此 seed expansion 的结构是：

```text
anchor call → root call sequence → variants
```

其中 variants 的 HB/CC 放置规则见 §6.5。mutation 只复用路径派生，不复用“root 必须紧跟 anchor”的规则。

---

## 4. CausalDerivationTable

### 4.1 结构

```go
type CausalDerivationTable struct {
    Weights map[string]map[CallVariant]int
}
```

- key：anchor call name；
- value：new root call + `anchor → root` relation；
- relation 集合：Same / Parent / Child / Sibling / NoRel；
- 初始权重复用 `DistributedChoiceTable.getInitialWeight` 的分类公式；
- NoRel 保持低权重；
- 表是静态权重，不引入学习；
- 与 DCT 权重公式重复是已接受的初版取舍；两表独立，不做独立调参或公共函数抽取。

### 4.2 与 DCT 的边界

| 表 | 方向 | 职责 |
|---|---|---|
| DCT | root → variant | 现有 CC/HB variant 选择 |
| CausalDerivationTable | anchor → root | 纯派生 seed 的新 root 选择 |

DCT 不替换，也不合并。初版权重复用同一套权重公式是已接受的取舍；第一版不抽公共权重函数，也不做独立调参。

### 4.3 特殊 root 的 relation

- `mkdir/creat` 只允许可生成 fresh child/sibling 的 relation：
  - Child；
  - Sibling；
  - NoRel。
- `Same` / `Parent` 对 fresh 创建无效，选择时跳过。
- `rename` 的 relation 用于 source path；destination 只允许 fresh sibling，不使用 existing E。
- 普通 root 使用完整 relation 集合。

### 4.4 表项不是可行性保证

`CausalDerivationTable` 是偏好表，不是“表里存在就一定能构造”的可行性表：

- 表项可能因 anchor PathKind、root 类型、relation 或 source/destination 约束不可生成；
- 采样前必须先按 §6.2、§6.3、§6.6 的规则过滤；
- 不可生成的表项直接跳过，不进入 retry；
- 过滤后没有候选时，按 §6.4 处理，不使用随机 fallback。

DCT 同理：`DistributedChoiceTable` 本身不硬排除 relation，实际由 `chooseVariant` 的 `availableRelationsForBase`、base 类型、call 类型和 `GetPathByRelation` 过滤；路径仍可能为空，此时 insertion 跳过该 variant，而不是当作 root 失败。

---

## 5. p0 Anchor

### 5.1 收集范围

只扫描 `p0`，不扫描 p1..pn。

理由：

- 用户明确要求第一版 anchor 只从程序 0 选；
- p0 会随着 expansion 不断增加 root，因此可以形成连续派生链；
- 避免一开始就把所有节点的路径混入 anchor 池。

每轮 derived insertion 前都必须重新扫描当前 p0，而不是只收集一次：

- 重新解析所有 p0 call 的路径；
- 更新因 root 插入而变化的 `CallIdx`；
- 把上一轮插入的 root call 纳入下一轮 anchor 候选；
- 重新按 `(CallName, Path)` 去重。

这样 p0 增长和 anchor 池增长保持一致，避免使用过期的 `GroupPosition`。

anchor path 必须是 root-base 可用的路径：跳过 `merge_view` 根和 TmpDir；fresh path 的 parent 不能是文件或 TmpDir，且第一版必须在 FileTree 中已存在（只支持 1 层 fresh；nested fresh parent 不支持）。

p0-only 是已接受的初版限制：不加入 p1..pn anchor，也不为此引入跨程序 fallback。

### 5.2 路径来源

每个 p0 调用：

- 普通调用：`extractPathFromCall`；
- fd 调用：解析 owner open/creat 路径；
- rename：同时收集 source 和 destination，并记录 Role：
  - source -> `seedAnchorRenameSrc`；
  - destination -> `seedAnchorRenameDst`；
- fresh path：按 §5.4 现场分类 PathKind，并用 `freshPathKindsBefore`/`freshKinds` 补充类型。

跳过空路径；按 `(CallName, Path)` 去重。rename source/destination 属于同一个 anchor call，但后续按 Role 使用不同 relation 集合，见 §6.3。

### 5.3 类型

anchor 记录：

```go
type seedAnchorRole int

const (
    seedAnchorNormal seedAnchorRole = iota
    seedAnchorRenameSrc
    seedAnchorRenameDst
)

type seedAnchor struct {
    Pos      GroupPosition
    CallName string
    Path     string
    IsDir    bool
    PathKind AnchorPathKind
    Role     seedAnchorRole
}
```

普通 anchor 使用 `seedAnchorNormal`；rename 的 source/destination 分别使用 `seedAnchorRenameSrc` / `seedAnchorRenameDst`。

`PathKind` 和 `IsDir` 都是现场分类结果，不是 seed/call 上持久记录的字段；`IsDir` 由路径 creator 类型决定，不能从 anchor call 名猜。定义见 §5.4。

第一版 anchor 选择为均匀采样，后续再讨论深度、组龄、最近使用等权重。

### 5.4 Anchor PathKind 现场识别

不持久记录 `Fresh` 字段，而是在选择 anchor 时现场分类：

```text
existing:
  FileTree.FindNode(path) != nil

fresh dir:
  mkdir 成功 + path 当前不存在

fresh file:
  creat 成功 + path 当前不存在

fresh file:
  open 成功 + O_CREAT + path 当前不存在
  + 不是 O_TMPFILE

fresh rename destination:
  rename 成功 + destination 当前不存在

unknown:
  非创建调用，或创建失败，或 flags 无法解析
```

成功判定：

- C1/mutation 中 p0 已有 `CheckInfo`，用 `CheckInfo.Retv == 0`；
- seed generation 阶段调用尚未执行，生成出来的创建调用按 prospective fresh 处理；
- 创建 call 失败时不得判为 fresh。

对于本身不是创建调用的 anchor（例如 `write`），PathKind 仍按 anchor path 判断：

- FileTree 已有该 path → existing；
- FileTree 没有 → 在 anchor 位置之前（含 anchor）查找同路径 creator；
- creator 成功 → fresh，类型取 creator 的目录/文件类型；
- 找不到 creator，或 creator 失败 → unknown；
- 不能只看 anchor 自身 call 名就判 unknown，否则 `open O_CREAT` 之后 modifier anchor 的 fresh path 会丢失。

现有 `freshPathKindsBefore` 只扫描 `mkdir`/`creat`/`rename` destination，必须扩展 `open O_CREAT`；否则不能用于本节的 open 判定。

PathKind 必须同时给出 `IsDir`，不能只保留 fresh bool：

| creator | PathKind | IsDir |
|---|---|---|
| `mkdir` | `freshDir` | `true` |
| `creat` | `freshFile` | `false` |
| `open(O_CREAT)` | `freshFile` | `false` |
| rename destination | `freshRenameDst` | 取 source 类型 |
| 已有 FileTree 节点 | `existing` | `FileNode.Type != NodeTypeFile` |
| 无法确定 creator | `unknown` | 不参与派生 |

`seedAnchor.IsDir` 必须由这张表的 creator 类型填充；both fresh 的 `Child` 要求 `PathKind == freshDir`，或 `PathKind == freshRenameDst && IsDir == true`。`freshRenameDst` 在关系矩阵中按 `IsDir` 归入 fresh dir 或 fresh file。

`open O_CREAT` 识别：

- 用带 `ok` 的 `parseOpenFlags(call)` 取 flags；不要用 `extractOpenFlags` 的 `0` 同时表示不可解析；
- `O_CREAT` 用 `GetConst("O_CREAT")`；
- `O_TMPFILE` 用 `LookupConst("O_TMPFILE")`，不存在时不得调用 `GetConst`；
- `O_TMPFILE` 存在且 flags 命中时，不是命名路径，不判 fresh；
- `O_TMPFILE` 缺失但 flags 同时带 `O_CREAT|O_DIRECTORY` 时，保守判 unknown/不 fresh；
- flags 不可解析或关键常量都不可用时返回 unknown。

边界：

- `open O_CREAT` 可以作为 **seed anchor path** 的 fresh path creator；
- `open O_CREAT` **不能作为 C1 anchor call**，因为 C1 anchor 必须是 modifier；
- `FileTree` 是当前快照，不能证明调用时刻一定 fresh；路径创建后被删除/rename 的情况不做完整时间线模拟。

### 5.5 Path existence 判断时机

seed 生成阶段没有逐调用执行，存在性判断必须区分三个时间层次：

```text
1. 当前 FileTree 快照
2. 同一程序内尚未执行的 prospective 创建
3. 执行后由真实 syscall/FileTree 同步得到的结果
```

#### Anchor 选择时

- 先查 `FileTree.FindNode(anchor.Path)`；
- 找到 → `existing`；
- 找不到 → 在 anchor 位置之前查找同路径 creator；
- creator 成功 → `fresh`，类型取 creator；
- 找不到 creator 或 creator 失败 → `unknown`；
- anchor call 本身不是 creator 时，必须回溯更早的 creator，不能只看 anchor call 名。

#### Root path 派生时

- normal root：使用当前 FileTree 的已有路径；
- fresh root：**不要**用 `FindNode(rootPath)` 判断失败，fresh root 本来就预期不在 FileTree；
- fresh root 的 parent 必须是 FileTree 中已存在的目录；第一版只支持 1 层 fresh，nested fresh parent 不生成 plan，不使用 `freshKinds` 放行；
- rename source 必须可解析；rename destination 按 fresh sibling 处理。

#### Variant 生成时

- 普通 base 使用当前 FileTree；
- fresh base 走 fresh/freshKinds 特判；
- `GetPathByRelation` 返回空表示当前快照下不可生成，不等于执行期一定失败。

#### 执行时

- 只有真实 syscall 才判断文件系统当前是否存在；
- 执行后 FileTree 才同步；
- 生成期不能把 FileTree 快照当作 anchor 时刻的完整时间线。

#### 快照近似

- anchor 前创建、之后被删除：FileTree 可能看不到，误判 fresh；
- anchor 后才创建、最终留在 FileTree：FileTree 可能看到，误判 existing；
- 这两类属于 §11 风险 20 的已接受近似。

---

## 6. Root Path 派生

### 6.1 普通 root

使用：

```go
GetPathByRelation(anchor.Path, "", relation, r.Rand, false, constraint)
```

relation 由 `CausalDerivationTable` 给出。路径为空、类型不兼容，或路径不是 `aEligible` 的普通 root base（根/TmpDir）时跳过，不随机 fallback。

### 6.2 mkdir / creat

只允许：

- `Child`：anchor 是目录时，在 anchor 下生成 fresh child；
- `Sibling`：在 anchor 父目录下生成 fresh child；
- `NoRel`：在无关目录下生成 fresh child。

生成后写入 `freshKinds`：

- mkdir → dir；
- creat → file。

`Same` / `Parent` 对 fresh 创建无效，直接跳过。

Seed Derivation 允许 fresh root 使用 NoRel；C1 的 relation 集合不包含 NoRel，fresh mkdir/creat root 只允许 Child/Sibling。这是 seed 为保持路径分量多样性而做的扩展，不改变 C1 语义。

### 6.3 rename

rename root 使用两条路径，不能统一成一个 `rootPath`：

```text
rootPre  = source   // anchor→root 连通性
rootPost = dest     // root→variant 的 observed path
```

#### source 派生

- source 按 anchor→source relation 派生；
- source 必须是 rename 时可用的已有文件/目录；
- anchor existing：source 必须是 FileTree 中的已有路径；
- anchor fresh：
  - `Same` 可以使用 anchor 自己已创建的 fresh path；
  - `Parent`/`Sibling`/`NoRel` 从已有路径派生；
  - `Child` 第一版不支持；只有后续引入 `freshKinds` 子路径识别后才允许；
- source 类型必须已知；不能把尚未创建的 fresh child 当作 rename source。

#### destination 派生

- 只允许 fresh destination：
  ```go
  dest = freshRenameDst(source, r)
  ```
- 不使用 `pickRenameDst` 的 existing E 分支；
- dest 必须：
  - 当前不存在；
  - 与 source 同父；
  - 与 source 同文件/目录类型；
  - 不等于 source。
- 生成后写入 `freshKinds`：
  ```text
  freshKinds[dest] = source type
  ```
- 不能覆盖 source 的已有类型信息。

#### root→variant

- root rename 的 `rootObservedPath` 是 destination；
- variant 的路径关系基于 destination 派生，不基于已经消失的 source；
- HB variant 在 root 完成后执行，不能落在 source 上；
- `insertDerivedCallFromDCT` 必须显式携带 `rootSource` / `rootDest`，不能只复用现在的 `basePath`。

#### anchor 本身是 rename

rename 的 source/destination 都进入 anchor 池，但 Role 不同：

- destination（`seedAnchorRenameDst`）：按普通 anchor 使用完整 relation 集合；
- source（`seedAnchorRenameSrc`）：只允许 rename 成功后仍然存在的路径关系：

| root 类型 | source 可用 | source 不可用 |
|---|---|---|
| normal root | Parent / Sibling / NoRel | Same / Child |
| fresh mkdir/creat | Sibling / NoRel | Child |
| rename root | Parent / Sibling / NoRel | Same / Child |

规则：

- source 的 `Same` / `Child` 排除：source 本身和 source 后代已被 rename 移走；
- source 的 parent/sibling 仍然存在，可以作为 root 路径候选；
- source parent/sibling 候选仍必须通过普通 `FindNode`、`aEligible`、type/depth/persist 约束；
- `deriveSeedRootPath` 在生成 root path 后必须对 source anchor 再加一层 `pathUnderRoot(rootPath, anchor.Path)` 兜底过滤，覆盖 relation 分支遗漏；
- seed expansion 的 root 插在 rename 后；mutation 使用随机插入位置，不假设 root 在 rename 前，因此 source 的 `Same` / `Child` 同样排除；
- destination anchor 不受 source 过滤影响。

第一版接受 source/destination 同父时产生的重复候选，不做去重；如果后续 benchmark 显示 rename anchor 权重过高，再按 `(RootCall, Relation, RootSource, RootDest)` 去重。

### 6.4 失败处理

- 不调用随机 FileTree 选择；
- derived root 不执行 `applyFailureShape`，避免 base path 被重写；
- `insertDerivedCallFromDCT` 保持单次尝试，不在内部按 anchor 重试；
- 复用 `expandWithDCT` 的外层重试：每轮重新扫描 p0 后再尝试；
- `expandWithDCT` 开头检查 p0 是否有可解析 anchor，没有则直接返回，避免 3 次无意义失败；
- 连续 3 次失败停止 expansion，保留当前 seed；
- 无可行 `(anchor, root, relation)` 时接受 seed 变短，不引入 p1..pn anchor 或随机路径 fallback；

### 6.5 Variant 插入位置

seed expansion 中，root 插在 anchor 后，但 variant 不再统一使用 anchor 或 root 的 `insertPos`。先定义：

```text
rootStart = root 的插入位置
rootEnd   = rootStart + root 调用序列长度
```

root 可能是多调用序列，例如 `open + write + close`，因此 `rootEnd` 必须按实际插入的 root call 数计算。

#### seed expansion：索引近似

生成阶段没有 `CheckInfo`，不能用执行时间精确对齐，只能用索引表达 temporal 意图：

- **HB variant**：插到 `rootEnd` 之后；
  - 表示 root 序列完成后才执行；
- **CC variant**：插到 `rootStart` 附近；
  - 表示和 root 序列同窗口开始；
- 多调用 variant 整组插在同一位置；
- 插入位置 clamp 到 host 程序边界；
- 当前每个节点只插入一个 variant；如果未来同一 host 插入多个 variants，需要独立的稳定顺序 helper。

#### mutation：时间优先，索引回退

mutation 的 seed 已有执行时间：

- HB：优先 `firstBoundaryAfter`；
- CC：优先 `TimeAlignedInsertPos`；
- 没有 timing 或对齐失败时：
  - HB 回退到 `rootEnd`；
  - CC 回退到 `rootStart`。

mutation 的 root 插入位置仍随机；anchor 只是 path anchor，不要求 root 插在 anchor 后。

rename root 的 `rootObservedPath` 是 destination；`rootStart`/`rootEnd` 仍按 rename 调用本身的插入位置和调用序列长度计算，variant 的路径关系基于 destination。

#### semantic note

```text
seed expansion: anchor 同时是 path anchor 和 temporal anchor
mutation:       anchor 只作为 path anchor
```

无 `CheckInfo` 时，seed expansion 的 CC/HB 只是索引近似；实际 temporal 关系仍由执行时的时间窗口决定。

### 6.6 Anchor PathKind 的 relation 矩阵

`§6.2` 的 Child/Sibling/NoRel 约束只适用于 **root fresh**。anchor fresh 时，root 的可用 relation 取决于 anchor 的 PathKind；existing anchor 的行作为对照：

| anchor | root | Same | Parent | Child | Sibling | NoRel |
|---|---|---|---|---|---|---|
| existing dir | normal | 可用 | 可用 | 可用 | 可用 | 可用 |
| existing file | normal | 可用 | 可用 | 不可用 | 可用 | 可用 |
| existing dir | fresh mkdir/creat | 不可用 | 不可用 | 可用 | 可用 | 可用 |
| existing file | fresh mkdir/creat | 不可用 | 不可用 | 不可用 | 可用 | 可用 |
| fresh dir/file | normal | 不可用 | 可用 | 不可用 | 可用 | 可用 |
| fresh dir | fresh mkdir/creat | 不可用 | 不可用 | 可用 | 可用 | 可用 |
| fresh file | fresh mkdir/creat | 不可用 | 不可用 | 不可用 | 可用 | 可用 |

规则：

- fresh anchor 不在 FileTree 中，`Same` 不能成立；
- anchor fresh + normal root 的 `Child` 不可用，因为 fresh anchor 的当前路径不存在，不能提供已有子树；
- fresh anchor 的 `Parent`：root 是 anchor 的祖先，可从 anchor 路径或父目录派生；
- fresh anchor 的 `Sibling`：在 anchor 父目录下生成 root；
- fresh dir anchor + fresh root：可以生成 Child（anchor 创建目录后 root 在其下创建）；
- fresh file anchor + fresh root：不能生成 Child；
- both fresh 的 Child 条件必须显式写成 `PathKind == freshDir` 或 `PathKind == freshRenameDst && IsDir`，不能只看 fresh；
- fresh root 的 Parent 仍不可用，与 `§6.2` 一致；
- 关系方向仍是“root 相对 anchor”。

矩阵默认 anchor path 的 parent 已在 FileTree 中；nested fresh parent 不适用，第一版不生成对应 fresh-root plan。

这张矩阵描述 anchor PathKind 与 root relation 的兼容性，不包含 rename Role。rename source anchor 仍要叠加 §6.3 的 source 过滤；destination anchor 使用本矩阵的完整关系集合。

both fresh 的 Child 还有 fresh-parent 处理：

```text
parent    = anchor.Path
parent    不在 FileTree
rootPath  = fresh child of parent
```

第一版只支持 **1 层 fresh**：`deriveSeedRootPath` 用 `freshParentNodeEligible` 检查 anchor path 的 parent 必须在 FileTree 中已存在；nested fresh parent 不支持，不把 `freshKinds` 作为 `deriveSeedRootPath` 的放行输入。放行条件必须同时满足：

- `PathKind == freshDir`，或 `PathKind == freshRenameDst && IsDir == true`；
- parent 就是 anchor path；
- anchor path 的 parent 在 FileTree 中且不是 file/TmpDir；
- root call 是 `mkdir/creat`；
- root path 确实由该 parent 派生；
- anchor path 不是 `merge_view` 根或 TmpDir。

PathKind 分类必须与 C1 共用同一套 helper，不能 seed 和 causal root 各写一份。

本表是 Seed Derivation 的可用关系；C1 完全不枚举 NoRel，具体见 `docs/causal-root-cell-design.md` §2.5。

---

## 7. insertCallFromDCT 改造

### 7.1 模式

```go
insertDerivedCallFromDCT(
    ps, r, sCalls, hmcfg, lcs,
    allowDirected bool,
    insertAfterAnchor bool,
) bool
```

- seed expansion：`insertAfterAnchor = true`；
- mutation：`insertAfterAnchor = false`，保留随机 `insertPos`；
- mutation 只保证路径派生，不保证 root 与 anchor 的时间链；这是已接受的初版取舍；
- 两者都使用纯派生 base path；
- derived rename 额外携带 `rootSource` / `rootDest`，root 调用使用 source→dest，variants 使用 dest 作为 observed base。
- 实现采用新增 `insertDerivedCallFromDCT` 作为入口，不把 derived 分支硬塞进普通 `insertCallFromDCT`；
- `expandWithDCT` 和 `MutateFsopsWithDCT` 都改走该入口，普通 DCT 入口保留兼容。

### 7.2 删除的路径

derived 模式不再执行：

- 30% `useFreshPath` 随机路径；
- 无兼容候选时的 `GetRandomFile` / `GetRandomDir` fallback；
- derived root 的 `applyFailureShape` 路径重写。

不为 derived root 设计独立 failure shape 是已接受的初版取舍；由此减少的失败形态覆盖暂不补偿。

### 7.3 保留的逻辑

- fd 生命周期与 `findOpenFdForPath`；
- root 生成前的 `freshKinds`；
- root→variant DCT 选择：
  - 非 rename 使用 `rootObservedPath`；
  - rename 使用 destination；
  - `mkdir` variant：
    - `Child` / `Sibling` / `NoRel` 使用 `freshChildPath`；
    - `Same` / `Parent` 使用 `GetPathByRelation(rootObservedPath, ...)`；
    - 不得因为 derived 分支没有对应 case 而静默 skip；
- NoRel variants；
- pending / 时间对齐逻辑；
- mutation 的随机插入位置。

`mkdir` 的 `Same` / `Parent` 不是 fresh child/sibling，而是 existing path 目标：

- `Same` 只可能落在非文件 root base；
- `Parent` 使用 root base 的 parent；
- 两者都使用 `GetPathByRelation` 和 `callNameConstraint("mkdir")`；
- `GetPathByRelation` 返回空时才 `continue`，不引入随机 fallback；
- `Child` / `Sibling` / `NoRel` 继续使用各自的 fresh path 语义，生成失败时不改走 existing path。

### 7.4 插入位置

- seed expansion：`insertPos = anchor.Pos.CallIdx + 1`；root 调用序列整体插在这个位置；
- mutation：继续使用现有随机 `insertPos`。

### 7.5 Variant 放置

当前实现由 `insertDerivedCallFromDCT` 内联完成，不新增独立 helper。逻辑职责：

- 计算 `rootEnd = rootStart + rootCallCount`；
- seed expansion：
  - HB → `rootEnd`；
  - CC → `rootStart`；
- mutation：
  - 有 timing 时优先 `firstBoundaryAfter` / `TimeAlignedInsertPos`；
  - 失败时 HB → `rootEnd`，CC → `rootStart`；
- 输出位置 clamp 到 `[0, len(host.Calls)]`；
- 多个 variants 在同一 host 插入时保持稳定顺序。

当前 `insertDerivedCallFromDCT` 每个节点只插入一个 variant；如果未来需要同一 host 的多个 variants，必须补独立的稳定插入 helper 和测试。当前不新增 `placeDerivedVariants`。

---

## 8. Initial Group 与 Pattern

- `generateFromDistributedChoiceTable` 的 initial group 保留随机 FileTree base；
- `generateFromPredefinedPattern` 的 pattern 作为 initial group，也允许随机 base；
- initial 之后的 `expandWithDCT` 全部走 derived；
- pattern 是特殊 initial group，第一版不对其内部 `NoRel`/多路径做过滤；
- pattern 生成后只要求 p0 至少有一个可解析 anchor path；
- p0 没有可用 anchor 时按风险 3 停止 derived expansion，不回填随机 anchor；
- pattern 的 Child/Sibling/rename 分支保留，不为了单连通改写 pattern 语义。

---

## 9. NoRel

- root NoRel 保留；
- root NoRel 低权重，允许产生额外路径分量；
- variant NoRel 保持现有 DCT 行为；
- 不为了单连通而删除 NoRel；
- 不做全局 component 合并；
- 测试只要求非 NoRel 的扩展链接入 p0 anchor，不要求全图单分量。

---

## 10. 测试计划

> 本节是测试计划；未在 `anchor_path_test.go` / `seed_opt_test.go` 中出现的条目为待补。

- `TestCausalDerivationTable`：
  - 权重稳定；
  - NoRel 低权重；
  - 固定 seed 可复现。
- `TestCausalDerivationTableFilters`：
  - fresh root/rename/PathKind 不兼容的表项被过滤；
  - 过滤项不进入 retry；
  - 过滤后无候选时返回失败，不随机 fallback。
- `TestPatternInitialAnchor`：
  - pattern initial group 不在 derived 阶段过滤内部路径；
  - pattern 后 p0 有可解析路径时进入 anchor 池；
  - pattern 后 p0 无可用 anchor 时 derived expansion 停止；
  - 不为 pattern 内部 NoRel/多路径补随机 anchor。
- `TestCollectP0SeedAnchors`：
  - 只收 p0；
  - rename source/destination 都进入 anchor；
  - rename source role = `seedAnchorRenameSrc`，destination role = `seedAnchorRenameDst`；
  - 普通 anchor role = `seedAnchorNormal`；
  - fd owner；
  - p1 路径不进入 anchor。
- `TestP0AnchorRefresh`：
  - 每轮重新扫描 p0；
  - root 插入后的 `CallIdx` 已更新；
  - 新插入的 root 进入下一轮 anchor 池；
  - 不保留过期 anchor。
- `TestDerivedAnchorExhaustion`：
  - 无可解析 p0 anchor 时 `insertDerivedCallFromDCT` 返回 false；
  - 连续 3 次失败停止 expansion；
  - 不产生 p1..pn anchor 或随机 FileTree root。
- `TestClassifyAnchorPath`：
  - `mkdir`/`creat` 成功且路径不存在 -> fresh；
  - 创建失败但路径不存在 -> unknown；
  - `open O_CREAT` 成功且路径不存在 -> fresh file；
  - `open` 无 `O_CREAT` -> 不 fresh；
  - `open O_CREAT` 且路径已存在 -> existing；
  - `open O_CREAT|O_TMPFILE` -> 不算 fresh（target 定义该常量时）；
  - `O_TMPFILE` 未定义但 `O_CREAT|O_DIRECTORY` 存在 -> 保守 unknown/不 fresh；
  - flags 不可解析 -> unknown；
  - 后续 modifier anchor 可回溯更早的 creator 并判为 fresh。
- `TestAnchorPathKindIsDir`：
  - `mkdir` -> freshDir / IsDir true；
  - `creat` / `open O_CREAT` -> freshFile / IsDir false；
  - rename destination 取 source 类型；
  - existing 取 FileNode 类型；
  - 无法确定 creator -> unknown。
- `TestFreshAnchorRootRelations`：
  - fresh anchor + normal root：Parent/Sibling/NoRel 可生成，Child 不可生成；
  - fresh dir anchor + fresh root：Child 可生成；
  - fresh file anchor + fresh root：Child 不可生成；
  - both fresh Child 的 parent 是 anchor path 且其 parent 已在 FileTree 时，按 freshDir 派生；
  - existing dir anchor + fresh root：Child/Sibling/NoRel 可生成；
  - existing file anchor + fresh root：仅 Sibling/NoRel 可生成；
  - fresh anchor 不生成 Same。
- `TestSeedFreshAnchorDepth`：
  - 1 层 fresh anchor（parent 已在 FileTree）被接受；
  - nested fresh anchor（parent 也是 fresh）不被接受；
  - nested fresh anchor 不产生 fresh root Child plan。
- `TestFreshAnchorRenameRootChild`：
  - fresh anchor + rename root 的 Child 第一版拒绝；
  - `seedRenameSource` 对 fresh anchor 的 PathChild 返回空。
- `TestOpenCreateIsNotC1Anchor`：
  - `open O_CREAT` 不进入 C1 anchor 候选；
  - 后续 modifier 可以使用该 fresh path。
- `TestDeriveSeedRootPath`：
  - normal 的 Same/Parent/Child/Sibling；
  - mkdir/creat 的 Child/Sibling/NoRel；
  - rename source/destination；
  - 类型不兼容时失败。
- `TestDerivedRenameSourceDest`：
  - anchor→root relation 使用 source，不使用 destination；
  - destination 必须是 fresh sibling，不能选 existing E；
  - destination 与 source 同父、同类型、不相等；
  - fresh anchor + rename root 的 Child 第一版整体拒绝，不依赖是否存在真实子路径；
  - root→variant 使用 destination；
  - HB variant 不落在 source 上；
  - anchor 本身是 rename 时按 Role 区分，source 和 destination 都进入 anchor 池。
- `TestSeedRenameAnchorRoles`：
  - `collectSeedAnchors` 返回 source/destination 两个 anchor，Role 正确；
  - source + normal root：Parent/Sibling/NoRel 可生成，Same/Child 拒绝；
  - source + fresh root：Sibling/NoRel 可生成，Child 拒绝；
  - source + rename root：Parent/Sibling/NoRel 可生成，Same/Child 拒绝；
  - source 的 `pathUnderRoot(rootPath, source)` 兜底过滤生效；
  - destination anchor 保持完整 relation 集合。
- `TestDerivedRenameSourceInsertion`（待补）：
  - seed expansion 选中 source anchor + Parent/Sibling 时，root 插在 rename 后且路径仍有效；
  - destination anchor 的 insertion 行为不变。
- `TestDerivedInsertUsesP0Anchor`：
  - root base 与 p0 anchor 相关；
  - seed expansion root 在 anchor 后。
- `TestDerivedVariantPlacement`（待补）：
  - 单调用 root：HB variant 在 root 后，CC variant 在 root 附近；
  - 多调用 root：HB variant 不在 root 序列中间；
  - 当前每节点一个 variant；若未来同一 host 插入多个 variants，再补稳定顺序测试；
  - 无 CheckInfo 时走索引 fallback。
- `TestDerivedMkdirExistingVariant`：
  - `mkdir + PathSame`：路径等于 `rootObservedPath`；
  - `mkdir + PathParent`：路径等于 `rootObservedPath` 的 parent；
  - `mkdir + Child/Sibling/NoRel`：仍走 fresh path；
  - Same/Parent fallback 失败时正常 skip，不产生空路径；
  - derived insertion 不静默丢弃已选中的 `mkdir Same/Parent` variant；
  - 与普通 `insertCallFromDCT` 的 mkdir Same/Parent 行为对齐。
- `TestMutationInsertKeepsRandomPosition`：
  - mutation derived root 仍可随机插入位置。
- `TestDerivedMutationUsesP0Anchor`：
  - mutation 走 `insertDerivedCallFromDCT`；
  - base path 来自 p0 anchor；
  - insertPos 仍随机；
  - 不使用随机 FileTree base。
- `TestDerivedInsertNoRandomFallback`（待补）：
  - 无 anchor 时返回 false；
  - 不产生随机 FileTree root。
- `TestSeedExpansionPathDerivation`：
  - 排除 NoRel 后，扩展 root 接入 p0 路径图。
- `TestDerivedRootNoRel`（待补）：
  - root NoRel 低权重但可生成；
  - 允许产生独立路径分量；
  - 不要求全 seed 单分量。
- 回归：
  - `TestGenerateProgsForHmdfsFsopsNodeBudget`
  - `TestHmdfsGenNoOverlap`
  - 现有 seed/DCT/pattern 测试。

---

## 11. 风险与待讨论项

1. **fresh root relation 复杂**  
   `mkdir/creat` 的 selected relation 必须与 fresh child/sibling 的几何位置一致；`Same`/`Parent` 要过滤。  
   这里描述的是 **root fresh**；variant `mkdir` 的 `Same`/`Parent` 仍可存在，由 DCT 按 base 类型和 relation 可用性选择，derived insertion 必须生成对应路径，不能静默跳过。  
   第一版只支持 1 层 fresh；nested fresh parent 和 fresh anchor + rename root Child 明确不支持。

2. **rename source/destination（已决策，A+）**  
   rename 使用双路径：`rootPre=source` 只用于 anchor→root 连通性，`rootPost=dest` 用于 root→variant。destination 只允许 fresh sibling，不使用 existing E；source 必须是已存在的 rename source。`insertDerivedCallFromDCT` 需要显式携带 `rootSource` / `rootDest`，当前只用一个 `basePath` 的做法不适用于 derived rename。

   rename 的 source/destination 都进入 anchor 池，但带 Role：source 只允许 `Parent/Sibling/NoRel`，destination 使用完整 relation 集合。source 的 `Same/Child` 排除，source parent/sibling 仍走普通节点约束，并加 `pathUnderRoot` 兜底过滤。第一版接受同父 source/destination 重复候选，不做去重；只有 benchmark 显示权重偏高时再按 `(RootCall, Relation, RootSource, RootDest)` 去重。

3. **p0 anchor 耗尽（已决策，低概率边界）**  
   每轮重新扫描 p0，包含新插入的 root 并更新 `CallIdx`；`insertDerivedCallFromDCT` 保持单次尝试，复用 `expandWithDCT` 的外层重试。连续 3 次失败停止 expansion，接受 seed 变短；不引入 p1..pn fallback、随机 FileTree 补 anchor 或 per-anchor 内部重试。

4. **NoRel 多分量（已决策）**  
   root NoRel 保留低权重，允许产生新的路径分量；variant NoRel 保持现有 DCT 行为。本阶段不强制 seed 单连通，也不做全局 component 合并。测试只要求非 NoRel 的 root 接入 p0 anchor 链。

5. **mutation 随机插入位置（已接受）**  
   路径是派生的，但 root 的 `insertPos` 仍随机；这保留 mutation 多样性。该模式只保证路径派生，不保证 root 与 anchor 的时间链，也不要求 root 插在 anchor 后。已有 `TestMutationInsertKeepsRandomPosition` 覆盖。

6. **applyFailureShape 禁用（已接受）**  
   derived root 第一版不执行 `applyFailureShape`，也不单独设计 derived-root failure shape。接受由此带来的失败形态覆盖减少，不为此增加额外重写逻辑。

7. **p0-only anchor 限制（已接受）**  
   第一版只从 p0 选 anchor，不加入 p1..pn。p0 随 expansion 增长；无可行 anchor 时按风险 3 停止，不引入跨程序 anchor fallback。

8. **CausalDerivationTable 与 DCT 权重重复（已接受）**  
   初版权重复用 `getInitialWeight`，DCT 与 CausalDerivationTable 保持独立。第一版不引入学习、不做独立调参、不抽公共权重函数；后续如果 benchmark 显示需要再调整。

9. **pattern initial group（已决策，低优先级边界）**  
   pattern 作为特殊 initial group，第一版不按 derived group 规则过滤内部 NoRel/多路径，也不改写 Child/Sibling/rename 分支。pattern 生成后只检查 p0 是否至少有一个可解析 anchor path；没有 anchor 时按风险 3 停止 expansion，不随机补 anchor。

10. **MutateGroupPathDynamic（已接受）**  
    PathSet 主路径会迁移包含 anchor 的整个路径连通分量，并在迁移后做 `pathSetConnected` 校验；但新 base 仍从 FileTree 随机选择，不是 seed 派生。PathSet 失败后的 legacy fallback 使用 `findGroupCalls`，不保证整个分量一起迁移。第一版保留现状：PathSet 保持分量，fallback 只作兜底；不把 mutation 路径迁移改成纯派生，也不因 fallback 不保分量而禁用 fallback。

11. **路径删除/rename 后的 anchor（已接受）**  
    这是风险 20 的路径生命周期实例。当前不做完整时间线模拟；anchor 路径可能已被同 seed 中更早调用移动或删除，接受该近似，不额外过滤。

12. **anchor 选择概率（已接受）**  
    第一版均匀采样；深度、组龄、最近使用、跨节点距离等权重不在第一版引入。

13. **对 seed 规模和性能的影响（已接受）**  
    derived insertion 可能失败或使 seed 变短；第一版不增加细粒度失败统计，也不为低概率失败引入额外重试。用 seed 长度/轮次和 benchmark 观察整体影响，优先通过 §4.4 的可行性过滤避免可预见的无效插入。

14. **与 C1/C2 的关系（已接受）**  
    本设计只改 DCT seed 生成，不改变 causal root 的 C1/C2 逻辑；需要时通过共享 PathKind/root path helper 对齐，不在本轮修改 C1/C2。

15. **多调用 root 的 variant 位置（已接受）**  
    root 可能是 `open + write + close` 等多调用序列；variant 的 HB 位置必须按 root 序列结束位置计算，不能只按 root 起始索引。

16. **无 CheckInfo 时 temporal 只是索引近似（已接受）**  
    seed generation 阶段没有执行时间，CC/HB 只能通过程序索引近似；真实 temporal 关系仍由执行时间窗口决定。

17. **variant 插入顺序（当前边界）**  
    当前 `insertDerivedCallFromDCT` 每个节点只插入一个 variant，不存在同一 host 的多 variant 排序问题。如果未来允许同一 host 插入多个 variants，必须补稳定的插入 helper 和测试，避免后插入的 variant 反转到前面。

18. **C1 与 seed derivation 的规则漂移（已接受）**  
    seed derivation 借用 C1 的 root path 约束，但不复用 C1 的 cell 选择和 CheckInfo。通过共享 root path/type helper 和测试对齐；本轮不重构 C1。

19. **fresh anchor 与 fresh root 混淆（已解决）**  
    已由 §5.4 和 §6.6 明确：PathKind + IsDir 必须由 creator 决定；fresh anchor/root 的 Parent/Child/Same 按矩阵过滤；rename destination 类型取 source。该项不再是待讨论风险。

20. **FileTree 只是当前快照（已接受）**  
    路径当前不存在不等于 anchor 调用时刻一定 fresh；创建后被删除/rename 的路径可能被误判。本阶段接受该近似，不做完整时间线模拟；风险 11 是其路径生命周期实例。

21. **open O_CREAT 的常量与语义（已决策）**  
    现有 API 只有会 panic 的 `GetConst`，没有 `HasConst`/`LookupConst`。新增 `LookupConst(name) (uint64, bool)`；必需常量才使用 `GetConst`，`O_TMPFILE` 等可选常量只用 optional lookup。`open O_CREAT` 只能作为 path creator，不能作为 C1 anchor call。

22. **open flags 成功判定（已决策）**  
    `CheckInfo.Retv == 0` 只说明调用成功；还要确认 flags 可解析、带 `O_CREAT`、路径不存在，并排除 `O_TMPFILE`。使用 `parseOpenFlags` 返回 `(flags, ok)`，不要用 `0` 同时表示“无 flags”和“参数不可解析”。`O_TMPFILE` 常量缺失时，若 flags 同时带 `O_CREAT|O_DIRECTORY`，保守判为 unknown/不 fresh；常量都不可用时也返回 unknown，不默认按 fresh 处理。

23. **anchor 与 creator 分离（已决策）**  
    后续 modifier anchor 的路径可能由更早的 `mkdir`/`creat`/`open O_CREAT`/rename 创建；PathKind 必须回溯 anchor 位置之前的 creator，并检查 creator 的成功信息。只看 anchor call 自身会把这类 fresh path 误判为 unknown。具体存在性判断时机见 §5.5；FileTree 快照的时间误差按风险 20 接受。mutation 使用 `requireSuccess=true`，seed generation 使用 prospective fresh。

### 11.1 append-only vs anchor-after

另一种简化方案是：anchor 只用于派生路径，root 和 variants 每次都追加到程序末尾。

#### append-only 的优点

- 不改已有调用索引；
- 不会打乱已有 fd/close 生命周期；
- 实现简单、稳定、易复现；
- root 在程序序上一定在 anchor 之后。

#### append-only 的缺点

- 所有新组都在程序尾部，结构单调；
- 新 root/variants 很少与已有调用形成交错；
- variants 都在尾部，目标 CC/HB 更难表达；
- 长程关系只体现在路径上，时间线上缺少嵌套；
- 弱化“在种子上组合交错结构”的设计核心。

#### anchor-after 的优点

- `anchor → root → variants` 在路径和时间上形成完整链；
- root 可以插入已有调用中间，支持嵌套和交叉；
- HB/CC variant 的位置有明确语义；
- 更适合复杂长程交错的论文叙事。

#### anchor-after 的缺点

- 需要处理索引移动、fd 生命周期和 root 多调用序列；
- 无 `CheckInfo` 时只能索引近似；
- 实现和测试更复杂。

#### 决策

```text
seed expansion：anchor-after
mutation：derived path + random insert position
append-only：仅作为 initial group / fallback 的简化策略，不作为主策略
```


---

## 12. 代码触点（实现）

- `src/prog/distributed_choice.go`
  - `CausalDerivationTable`
  - `LayeredChoiceStrategy` 字段和初始化
  - 表项仅作权重候选；采样前调用 PathKind/root/relation feasibility 过滤
- `src/prog/seed_derivation.go`
  - `seedAnchorRole`：`normal` / `renameSrc` / `renameDst`
  - `seedAnchor.Role` 与 `collectSeedAnchors` 的 rename source/destination 标记
  - 每轮 rescan p0 / 更新 `CallIdx`
  - `classifyAnchorPath` / 共享 PathKind
  - creator 类型回填 `seedAnchor.IsDir`
  - `seedRelationAllowed` 的 source anchor 过滤：排除 `Same` / `Child`
  - `deriveSeedRootPath` 的 source 子树 `pathUnderRoot` 兜底过滤
  - `seedRenameSource` 的 source/destination 双路径派生
  - 第一版 fresh parent 限制：`collectSeedAnchors` 使用 `freshParentNodeEligible`；nested fresh parent 不进入 anchor 池
  - `deriveSeedRootPath` 只支持 1 层 fresh；不把 `freshKinds` 作为 fresh-parent 放行输入
  - `freshPathParentEligible` 标注为 future nested fresh helper；当前 C1 / seed derivation 生产路径统一使用 `freshParentNodeEligible`，不重新引入 nested fresh 前不得作为生产 gate
- `src/prog/mutation.go`
  - `insertDerivedCallFromDCT` 派生入口
  - `MutateFsopsWithDCT` 改走 `insertDerivedCallFromDCT`
  - 普通 `insertCallFromDCT` 入口保留兼容
  - `freshPathKindsBefore` 支持 `open(O_CREAT)`
  - rename 返回 `rootSource` / `rootDest`；destination 只接受 `freshRenameDst`
  - `insertDerivedCallFromDCT`：root 用 source→dest，variants 用 destination
  - `insertDerivedCallFromDCT`：mkdir Same/Parent 使用 `GetPathByRelation(rootObservedPath, ...)` 的 generic fallback；Child/Sibling/NoRel 保持 fresh path
  - variant 放置逻辑内联在 `insertDerivedCallFromDCT`，不新增 `placeDerivedVariants`
- `src/prog/distributed_choice.go`
  - `extractOpenFlags` 复用
  - `parseOpenFlags` 返回 `(flags, ok)`，区分不可解析与 flags=0
  - optional const lookup（`O_TMPFILE` 等）
- `src/prog/target.go`
  - 新增 `LookupConst(name) (uint64, bool)`，避免可选常量触发 `GetConst` panic
- `src/prog/causal_root.go`
  - 与 seed 共用 anchor PathKind/root path helper，避免规则漂移
- `src/prog/generation.go`
  - `expandWithDCT` 使用 seed expansion 模式
  - 每轮 rescan p0；保留连续 3 次失败的 stop 规则
- `src/prog/rand.go`
  - initial group 保持特殊
  - `generateFromPredefinedPattern` 生成后检查 p0 anchor 可用性
- `src/prog/*_test.go`
  - 上述测试

---

## 13. 当前不做

- 跨节点完整文件状态模拟；
- 初始组派生（没有 anchor 时无法派生）；
- 强制 NoRel 删除；
- 强制全 seed 单一路径分量；
- mutation 的 `MutateGroupPathDynamic` 纯派生；
- seed derivation 不修改 C1/C2 算法；C1 anchor freshness 由 `docs/causal-root-cell-design.md` 单独跟踪；
- 论文/PPT 修改。
