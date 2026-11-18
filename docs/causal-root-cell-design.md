# 因果根与 root→variant 两层种子构造设计

> 状态：设计定稿（2026-10-27）；普通/fd/fresh/rename root、FreshB、C2 combo filter、完整 pre/post relation 已实现；Linux 交叉编译/vet 通过，Ubuntu runtime 待验证。2026-10-28 补充 C1 anchor PathKind、fresh anchor 关系矩阵、C2 fresh-parent 与 `open(O_CREAT)` 支持；已实现，Ubuntu runtime 待验证。2026-11-09 设计复核：补充 rename anchor 的 source 子树 root path 约束、1 层 fresh/nested fresh parent 边界、C2 FreshA normal parent 的 `aEligible` 防御，并把 anchor success 偏好文档与当前实现对齐；复核项代码已落，Linux 交叉编译/vet 通过，Ubuntu runtime 待验证。2026-11-15 补充 GlusterFS v1 的 link/symlink target/linkpath、canonical/payload、readlink follower 与 PathSet 双路径边界。2026-11-18 复核：#7 rename source 子树过滤改为显式 `SourceMoved`，link/symlink target 不再被误过滤；同批启用 GlusterFS dual-path causal 与 role-aware coverage（`comboKey` role-free，`cellKey` 增加 linkpath role）；Linux 交叉编译/vet 通过，Ubuntu runtime 待验证。
> 范围：`PickDirectedBatch` 的根节点因果锚定、固定 root 的 variant 规划、rename 支持。
> 术语：本文的 C1/C2 指 causal 两层（anchor→root、root→variant），不是论文中的 C1/C2 两个挑战。
> 关联：Seed 纯派生设计见 `docs/seed-derivation-design.md`；PathSet/数据/删除部分见 `docs/seed-construction-causal-chain-notes.md`；本文不修改论文或 PPT。
>
> 2026-11-17 GlusterFS：normal directed planner 的 link/symlink root 复用
> `planCausalC2CellsWithParent(..., causalRootNormal, ...)`，`RootLinkPath`
> 只进 plan/batch；该 pass 与 causal C1 共用 `DualPathCausalEnabled` gate，
> GlusterFS 默认 true、HMDFS false。语义见
> `docs/glusterfs-adaptation-plan.md` §8.2.2。

---

## 0. 已定结论

- 因果根进入 `PickDirectedBatch`，不是独立 mutation。
- 新 root `r` 仍是 target cell 的 root 调用；已有调用 `m` 只是它的因果前驱。
- 根节点构造顺序是：

  ```text
  m → r → variants
  ```

- 两层 cell 分开：
  - **C1**：`m → r` 的 same-node HB cell；
  - **C2**：`r → variant` 的现有 cross-node directed cell。
- C1 用于由 anchor 推导 root；C2 用于由 root 推导其它节点 variants。
- C1 必须来自当前 `p0` 可构造的候选，不能只选抽象 board cell。
- C1 的 root `RetB` 必须是 `Success`；C2 的 root `RetA` 沿用 `Success`。
- anchor 的 `CheckInfo` 必须能提供有效时间；prior `Retv` 不参与硬过滤，当前实现也不按 prior 成功/失败排序；fresh creator 成功仍是硬条件。
- C1 计分要求本次执行的 anchor 成功；prior 失败但本次可能成功的 anchor 不提前丢弃。
- anchor path 的 existing/fresh 状态现场分类，不在 call/seed 上持久记录。
- `open(O_CREAT)` 可以作为 fresh path creator，但不能作为 C1 anchor call；C1 anchor 仍必须是 modifier。
- C2 找不到兼容的未覆盖 cell 时，回退普通 `PickDirectedBatch`。
- cell 按“任意 pair 命中该 cell”计算，不要求精确 root→variant pair。
- C1 不额外建 pending；C2 沿用现有 pending。
- root rename 支持 F/E 成功形态；root 失败形态不作为 directed root。
- C1/C2 的关系分类统一复用 DAG 的 pre/post 逻辑：`determinePathRel` + `dagRelToDctRel`。
- destination-Same、pre/post 交叉 Same 都属于合法 relation；`FreshSame` 仍然单独处理。
- 不进行跨节点完整状态模拟；允许路径碰撞，碰撞导致的 miss 先按现有 C2 账目处理。
- **不做跨计划路径保留**；anchor/root/variant 的路径碰撞是有意的冲突测试，E destination 也不做跨计划 used-path 保留。
- C2 枚举经过 combo-level retirement 过滤；C1 暂不加同样的 combo filter。
- 不增加 causal size-changing offset skip；普通 planner 的对应 skip 已单独移除。

---

## 1. 两层 cell

### 1.1 C1：causal cell

```text
Root      = p0 已有 anchor 的调用名
Variant   = 新 root 的调用名
Rel       = root 相对 anchor 的路径关系
Temporal  = HB
RetA      = Success
RetB      = Success
OffB      = root 调用对应的 offset 桶
DepthB    = root 路径深度桶
TypeB     = root 路径类型
PersistB  = root 路径 persist 标记
```

C1 的 `Root` 是 anchor，`Variant` 是新 root。它不是现有 `PickDirectedBatch` 中 `root → variant` 的 cell。

### 1.2 C2：root → variant cell

沿用现有 directed cell 语义：

```text
Root      = 新 root 调用名
Variant   = 其它节点调用名
Rel       = variant 相对 root 的路径关系
Temporal  = CC / HB
RetA      = Success
```

C2 的 root 在 C1 阶段已经固定，不能重新选择 root 路径或 root source/destination。

### 1.3 规划顺序

```text
1. 从 p0 选择 anchor m
2. 从可行未覆盖 C1 cell 中选择一个
3. 由 anchor path + C1 推导 root path/call
4. 把 root 插到 m 后面
5. 在固定 root 上规划 C2 cell 和 variants
6. 任一步失败，回退普通 PickDirectedBatch
```

---

## 2. Anchor 与 C1 候选

### 2.1 Anchor 条件

anchor `m` 必须来自 `p0` 已有调用：

- `isModifierCall(m)`；
- effective path 可解析：
  - 普通调用使用自身路径；
  - fd 调用回溯 owner open/creat，并确认该 fd 在 anchor 位置仍处于有效生命周期；
  - rename 使用 destination post path；
- Anchor PathKind 可分类：
  - `existing`：`FileTree.FindNode(path) != nil`；
  - `fresh`：创建调用成功且 path 当前不存在；
  - `unknown`：非创建调用、创建失败或 flags 不可解析；
  - `unknown` 不作为 C1 anchor；
- `CheckInfo` 有效：
  - 非 nil；
  - `Etime > 0`；
  - `Stime <= Etime`；
  - 相对前后调用单调；
- `m` 后面有合法插入位置；
- prior `Retv` 不做硬过滤；当前实现不按 prior 成功/失败排序，同一 C1 cell 保留第一个可构造 witness；
- fresh anchor 例外，创建调用必须成功；
- C1 cell 的 `RetA=Success` 指本次执行；prior anchor 失败不代表一定丢弃，但本次执行失败时不会产生 C1 HB pair。

### 2.2 Root effective path

anchor 和 root 的路径关系必须基于 root 实际事件路径：

| root 形式 | root 事件路径 |
|---|---|
| 普通 root / fd root | `BaseA` |
| fresh mkdir/creat | materialize 后的 `BaseA`，父目录为 `BaseA2` |
| fresh-fresh | materialize 后的 `BaseA == BaseB` |
| root rename | destination `BaseA2` |

C1 的 `Rel` 通过实际 pre/post 候选计算，而不是只比较单一 effective path：

```text
anchor 候选 = {anchor.Path, anchor.Source}
root 候选   = root rename ? {RootDest, RootSource} : {RootPath}
relation   = dagRelToDctRel(determinePathRel(anchor, root))
```

关系方向统一为“root 相对 anchor”：

- Same：任一 anchor/root 候选路径相等；
- Parent：root 是 anchor 的祖先；
- Child：root 是 anchor 的后代；
- Sibling：root 与 anchor 同父目录。

因此 C1 的 root path 枚举会同时考虑 anchor 的 `Path` 和 `Source`，但 rename anchor 的 `Source` 只用于 relation/pre-path 分类以及 source parent/sibling 派生；source 本身和 source 后代不能作为 root 路径候选，详见 §2.6。

fresh mkdir/creat root 不存在 Same/Parent；C1 的 relation 集合本身不包含 NoRel。fresh root 的 parent 必须是 `aEligible`，即不能是 `merge_view` 根或 TmpDir。

fresh anchor 与 fresh root 的矩阵见 §2.5。`§2.2` 的 Same/Parent 限制描述的是 root fresh；anchor fresh + normal root 时 Parent/Sibling 仍可成立，Child 不可用。

C1 root 路径枚举必须基于 anchor PathKind：

- existing anchor：从 `anchor.Path` 枚举 FileTree 关系路径；rename anchor 额外允许 source parent/sibling 派生，但 source 子树过滤；
- fresh anchor + normal root：从 anchor path 派生 Parent/Sibling，不生成 Child；source parent/sibling 若有效同样保留；
- 无合法 root path 时返回 false，回退普通 batch。

### 2.3 C1 cell 可行性

C1 cell 必须同时满足：

- 未覆盖；
- `Root` 是某个 p0 anchor 的调用名；
- `Variant` 是支持的 root 调用，并且该调用也必须被 C2 的 fixed-root 规划器支持；
- `Temporal=HB`；
- `RetA=Success`；
- `RetB=Success`；
- B 侧路径深度/类型/persist 与推导出的 root 路径一致；
- root 与 anchor 的实际 pre/post relation 等于 C1 `Rel`；
- rename anchor 的 root 路径不能位于 `anchor.Source` 子树；source parent/sibling 不受影响，见 §2.6；
- anchor PathKind 与 `Rel` 兼容：fresh anchor + normal root 不允许 Same/Child；fresh mkdir/creat root 不允许 Same/Parent；C1 不枚举 NoRel；fresh anchor 的 Parent/Sibling 受 §2.5 矩阵约束；
- both fresh 的 Child：anchor 必须是 fresh 且 `IsDir == true`（包括 source 为目录的 `freshRenameDst`），且 `RootParent == anchor.Path`；该 fresh parent 的 validation 例外见 §2.5；
- root 调用类型约束满足；
- 对 rename root：source/destination 可构造；
- 对该 root，后续 C2 至少存在一个兼容的未覆盖 cell（可在 root 确定后检查）。

同一 C1 cell 可以有多个 anchor/root witness；按 cell 去重，再选择 anchor。

### 2.4 C1 采样

在可行未覆盖 C1 cell 中采样。

选定 cell 后：

- 在支持它的 anchor 中选一个；
- 当前不按 prior `CheckInfo.Retv` 做 anchor 成功偏好；同一 cell 保留第一个可构造 witness；
- 不按 anchor 支持的 cell 数量加权，避免 broad anchor 吃掉预算；
- C1 不写 pending，实际 `m → r` pair 由正常反馈记录。

### 2.5 Anchor PathKind 与 `open(O_CREAT)`

不持久记录 Fresh 字段，在选择 anchor 时按当前 FileTree + 调用信息现场分类：

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

识别细节：

- fresh creator 分类使用 p0 已有 `CheckInfo`；creator 成功判定为 `CheckInfo.Retv == 0`；
- `open` flags 用带 `ok` 的 `parseOpenFlags(call)` 读取；
- `O_CREAT` 用 `target.GetConst("O_CREAT")`；
- `O_TMPFILE` 用 `target.LookupConst("O_TMPFILE")`，不存在时不得调用 `GetConst`；
- flags 不可解析或关键常量不可用时返回 unknown，不能默认按 O_CREAT 处理；
- `O_TMPFILE` 存在且命中时不判 fresh；常量缺失但 flags 同时带 `O_CREAT|O_DIRECTORY` 时也保守判 unknown；
- 非创建 anchor（如 `write`）按 anchor path 回溯同路径 creator：
  - FileTree 有 path → existing；
  - 无 path 时在 anchor 位置之前（含 anchor）查找 creator；
  - creator 成功 → fresh，类型取 creator 的目录/文件类型；
  - 无 creator 或 creator 失败 → unknown；
- 现有 `freshPathKindsBefore` 只覆盖 `mkdir`/`creat`/rename destination，必须扩展 `open(O_CREAT)`。

C1 anchor 边界：

- C1 anchor 必须是 modifier（`isModifierCall`）；
- `open(O_CREAT)` 只是 fresh path creator，不是 modifier，因此不能作为 C1 anchor call；
- `open(O_CREAT)` 创建的路径可以被后续 modifier anchor 使用；
- `mkdir`/`creat`/rename destination 本身就是 modifier 或可作为 anchor path，仍按 `isModifierCall` 判断。

PathKind 必须同时携带目录类型，不能只保留一个 `Fresh` bool：

| creator | PathKind | IsDir |
|---|---|---|
| `mkdir` | `freshDir` | `true` |
| `creat` | `freshFile` | `false` |
| `open(O_CREAT)` | `freshFile` | `false` |
| rename destination | `freshRenameDst` | 取 source 类型 |
| 已有 FileTree 节点 | `existing` | `FileNode.Type != NodeTypeFile` |
| 无法确定 creator | `unknown` | 不参与 C1 |

- `freshRenameDst` 的 `IsDir` 必须来自 rename source，不能因为它是 rename 就按目录处理；
- anchor 是后续 modifier 时，`IsDir` 取更早 creator 的类型；
- both fresh 的 `Child` 只能在 anchor fresh 且 `IsDir == true` 时生成（包括 source 为目录的 `freshRenameDst`）；
- `Fresh` 不能单独作为判定条件，否则 fresh file 会被误判成可生成子路径；
- C1 必须把 `PathKind` / `IsDir` 带进 `causalAnchor` 或等价的 plan 上下文，供 root 路径枚举和 fresh-parent validation 使用；
- `freshRenameDst` 在关系矩阵中按 `IsDir` 归入 `fresh dir` 或 `fresh file`。

Anchor PathKind 的 C1 root 关系（不含 root rename F；rename 见 §3.3）：

| anchor | root | Same | Parent | Child | Sibling | NoRel |
|---|---|---|---|---|---|---|
| existing dir | normal | 可用 | 可用 | 可用 | 可用 | 不可用 |
| existing file | normal | 可用 | 可用 | 不可用 | 可用 | 不可用 |
| existing dir | fresh mkdir/creat | 不可用 | 不可用 | 可用 | 可用 | 不可用 |
| existing file | fresh mkdir/creat | 不可用 | 不可用 | 不可用 | 可用 | 不可用 |
| fresh dir/file | normal | 不可用 | 可用 | 不可用 | 可用 | 不可用 |
| fresh dir | fresh mkdir/creat | 不可用 | 不可用 | 可用 | 可用 | 不可用 |
| fresh file | fresh mkdir/creat | 不可用 | 不可用 | 不可用 | 可用 | 不可用 |

- 三种涉及 fresh 的情况都不成立 Same；
- C1 的 relation 集合不包含 NoRel；NoRel 只属于 Seed Derivation；
- fresh anchor + normal root 的 Child 不可用，因为 fresh anchor 的当前路径不存在，不能提供已有子树；
- fresh anchor 的 Parent 可从 anchor 路径的祖先派生；
- fresh anchor 的 Sibling 使用 anchor 父目录；
- fresh root 的 Child 要求 anchor 是目录；anchor 是文件时不可用；
- fresh mkdir/creat root 在 C1 中只允许 Child/Sibling；NoRel 是 Seed Derivation 的扩展，不属于 C1；
- FileTree 只是当前快照，不能证明 anchor 调用时刻一定 fresh；创建后被删除/rename 的路径不做完整时间线模拟。

both fresh 的 `Child` 有一个额外 validation 例外：

```text
RootParent = anchor.Path
RootParent 不在 FileTree
RootPath   = directedFreshPlaceholder(rootCall, RootParent)
```

此时不能走普通的 `ft.FindNode(parent)` + `aEligible(parent)` 校验，因为 parent 是尚未写入 FileTree 的 fresh path。放行条件必须同时满足：

- `PathKind == freshDir`，或 `PathKind == freshRenameDst && IsDir == true`；
- `plan.RootParent == anchor.Path`；
- `rootCall` 是 `mkdir/creat`；
- `plan.RootPath` 确实是由该 parent 生成的 fresh child；
- anchor path 不是 `merge_view` 根或 TmpDir；
- anchor path 的父目录必须是 FileTree 中已存在的目录（1 层 fresh），且不能是文件或 TmpDir；`merge_view` 根可以作为 fresh path 的父目录。

不能用“parent 不在 FileTree”作为通用放行条件。

nested fresh parent 限制（2026-11-09 复核）：

- 第一版只支持 **1 层 fresh-root parent**：当 fresh anchor path 作为 fresh root 的 parent 时，anchor path 的 parent 必须在 FileTree 中已存在；
- 如果 anchor path 的 parent 也是 fresh path，即 nested fresh parent，不生成 fresh root / both-fresh Child 相关的 C1/C2 plan；
- nested fresh anchor 的 normal root Parent/Sibling 仍可使用已有祖先/兄弟路径，不受本条禁止；
- C1 `addFreshPlan` 不应使用 `freshPathParentEligible` 放宽 nested；应与 C2 一样使用 `freshParentNodeEligible`；
- `freshPathParentEligible` 是 future nested fresh helper；当前 C1/C2/seed derivation 生产路径均使用 `freshParentNodeEligible`，不重新引入 nested fresh 前不得作为生产 gate；
- 2026-11-09 已修正：C1 与 C2 的 fresh-root parent 均按 1 层 fresh 处理，不再出现 C1 过度接受 nested、C2 拒绝的不一致。

#### 2.5.1 GlusterFS v1：link / symlink / readlink

GlusterFS v1 的链接路径分两层：

```text
link:     target = 测试根内普通文件 canonical（baseline existing，或同一程序/plan
                  更早 creator 的 fresh 路径）
          linkpath = fresh binding（可在任意已有父目录）

symlink:  target = 测试根内文件/目录 canonical（baseline existing，或同一程序/plan
                  更早 creator 的 fresh 路径）
          linkpath = fresh binding（可在任意已有父目录）
          payload = relpath(target, dirname(linkpath))

readlink: path = symlink 的 linkpath
          返回 payload，不独立选择 target
```

规则：

- `AnchorPathKind` 增加 `AnchorPathFreshSymlink`；`anchorPathInfo`/`seedAnchor`
  增加 `FollowIsDir`，但 fresh symlink 本身不作为目录 parent。
- `collectSeedAnchors` 同时收集 link/symlink 的 target 和 linkpath 两个 role；
  `symlink` 的 arg0 是 payload，必须用 `symlinkCanonicalTarget(linkpath, payload)`
  还原 canonical target。
- `causalAnchor` 的兼容投影：
  ```text
  link:    Path=linkpath, Source=target, Dest=linkpath
  symlink: Path=linkpath, Source=target, Dest=linkpath
  readlink: 不做 C1 anchor
  ```
- link/symlink anchor 的 `Source` 是 target，不是被移走的 rename source；
  这两个 anchor 的 `SourceMoved=false`，因此 §2.6 的 source 子树过滤不适用。
- C1 cell 的 B 侧只使用 target；C2 target-role 的 B 侧使用 target，
  linkpath-role 只在 HB 的 A 侧使用 linkpath；readlink 的 B 侧使用 linkpath。
- C1 link/symlink anchor 按 A 侧 role 枚举：target role 用 target FileTree
  node，linkpath role 用 `causalRootPathsByPath(linkpath)`；C1 cell 的
  `Role` 与 anchor role 对齐。
- C2 cell 的 `Role` 与 `RootRelationPath` 对齐：target/普通 cell 用
  `cellRoleNone`，linkpath-role 用显式 role 且只计 HB；readlink 用
  `cellRoleSymlinkLinkPath`。C1 link/symlink anchor 的 linkpath-role HB 与
  C2 link/symlink root 的 linkpath-role HB 是同一个 cell。plannable 对
  linkpath role 只保留 Parent/Sibling（readlink Same）。
- rename 不受 `DualPathCausalEnabled` 影响；C1/C2/normal directed/PathSet 的
  rename 继续按 source/destination 和 `SourceMoved` 处理，cell `Role` 始终为
  `cellRoleNone`。
- link/symlink target 是 **anchor 关系派生** 的 canonical 路径，不是 fixed
  baseline；映射/迁移后必须仍在测试根内、存在且类型匹配。
- `RootPaths`/`seedAnchor`/PathSet 只保存 canonical target；symlink payload
  只在构造调用时生成。
- `readlink` 只读一跳；DAG/checker 用 `linkpath + payload` 还原 canonical
  target，不能把 payload 当 FileTree 路径。
- 禁止嵌套链接：link/symlink target 不能是已有 linkpath/symlink。
- PathSet broad 的 link/symlink target-only / readlink follower 规则见
  `docs/seed-construction-causal-chain-notes.md` §1.5；C1/C2 不另写一套绑定逻辑。
- C2 FreshB/FreshSame、fresh root + fresh child、nested fresh parent 的限制不变；
  GlusterFS v1 不扩展这些形态。

### 2.6 Rename anchor 的 root 路径时间语义

rename anchor：

```text
before: source 存在
anchor: rename(source, destination) 成功
after:  source 子树消失，destination 存在
root:   只能使用 rename 成功后的路径
```

因此：

- `anchor.Source` 可以用于 anchor 成功判断、类型继承和 pre/post relation 分类；
- `anchor.Source` 的 parent/sibling 在 rename 成功后仍然存在，可以作为 root 路径候选；
- `anchor.Source` 本身和 source 后代位于被 rename 移走的子树，不能作为 root 路径候选；
- source parent/sibling 候选仍必须通过普通的 `FindNode`、`aEligible`、type/depth/persist 约束，不是无条件合法路径；
- 该过滤只对成功 rename anchor 生效，条件是
  `anchor.SourceMoved && source != "" && source != anchor.Path`。
  `SourceMoved` 由 anchor call 显式标记；不能用 `source != anchor.Path`
  推断，因为 link/symlink anchor 的 `Source=target` 也满足该条件，会把
  target 子树误删。普通 anchor 的 `source == path` 同样必须跳过过滤；
- 该过滤对 normal root、fresh root、rename root 都生效；
- existing root path 在 `ft.FindNode(rootPath)` 前过滤；fresh root 在 `directedFreshPlaceholder` 生成后、plan/cell 生成前过滤；不要在 fresh parent lookup 之前做路径级过滤；
- 过滤还必须覆盖由 destination sibling 枚举间接产生的 source 子树路径；
- 过滤条件使用 `pathUnderRoot(rootPath, source)`；
- root rename 当前要求 source/destination 同父，因此过滤 `RootDest` 同时覆盖同父的 `RootSource`；若未来放开跨父 rename，必须同时过滤 `RootSource`；
- rename 失败时 source 可能仍然存在，但 C1 的 HB pair 要求 anchor 成功，失败执行不会产生 C1 cell；因此不为了失败场景保留 source 子树 root 路径。

---

## 3. 由 anchor + C1 推导 root

### 3.1 普通 root / fd root

- root 路径 = C1 关系从 anchor 路径推导出的路径；
- 路径类型、深度、persist 必须匹配 C1；
- root 调用必须成功；
- root 路径可以是已有路径，也可以是 root 创建的 fresh 路径；
- `mkdir/creat` 之外的 fresh root 暂不支持。

### 3.2 fresh mkdir/creat root

```text
BaseA2 = 父目录
BaseA  = parent + 新目录/文件名
```

C1 的 `DepthB/TypeB/PersistB` 对应 materialize 后的 `BaseA`。
anchor 与 root 的关系只能是 Child/Sibling；Child 要求 anchor 是目录。
parent 必须是 `aEligible`：不能是 `merge_view` 根，也不能是 TmpDir；这与普通 planner 的 `parentEligible` 语义一致。
both fresh 的 Child 是例外：parent 是 fresh anchor path，不在 FileTree；按 §2.5 的 fresh-parent validation 放行，不能套用普通 `FindNode` 检查。
C2 fixed-root 的 normal fresh parent 也必须独立执行同一 `aEligible` 检查；不能依赖 C1 预过滤。
本轮只支持同一 p0/C1+C2 链路上的 both-fresh Child，且只支持 1 层 fresh-root parent；nested fresh parent 不生成对应的 fresh-root C2 plan；normal root 的 Parent/Sibling 不受影响；不承诺 C2/DCT 跨节点 fresh child。

### 3.3 rename root

统一使用：

```text
BaseA  = source
BaseA2 = destination
```

C1 的 B 路径是 rename destination。source 另外选择：

- 存在；
- 与 destination 同父；
- 与 destination 同类型；
- 不等于 destination；
- 尽量不是 TmpDir；
- 不要求与 anchor、C2 variants 或其它 plan 的路径不冲突；路径碰撞保留为冲突测试。

root rename 只允许成功形态：

- **F**：fresh destination，成功；
- **E**：existing same-class destination，成功：
  - 文件；
  - 空目录。

destination 是非空目录时不能做 root E。只有 C1 `Rel` 为 `PathChild` 或 `PathSibling` 时，才可能用同父 fresh destination 的 F 保持关系；`PathSame` 和 `PathParent` 不能通过换 fresh destination 保持原关系，直接丢弃该 C1。

root rename 失败形态（ENOTEMPTY 等）不进入 C1，也不进入 C2 root。

### 3.4 C1 root 路径采样

如果同一 C1 cell 有多个可行 root 路径：

- 路径关系必须一致；
- 深度/类型/persist 必须一致；
- 多个路径时只按构造可行性选择；
- 当前不实现 anchor prior 成功偏好排序，先到 witness 保留；
- 不引入独立的关系分数。

---

## 4. C2：固定 root 的 variant 规划

C1 成功后固定：

```text
RootCall
RootEffectivePath
RootSource / RootDest   // rename
RootFresh               // mkdir/creat
RootRenameKind          // F / E
```

C2 只做：

- 枚举 variant 调用、路径关系、路径；
- 检查 C2 cell 未覆盖；
- 检查 combo 是否仍可被 directed selection 使用（`rootDirectedSelectableFlags` 等价的 retired 过滤）；
- root 必须 `RetA=Success`；
- variant 可以 Success/ENOENT/Failure；
- variant 路径以实际 pre/post relation 为准；
- root 是 rename 时，PathSame 不再只按 source/pre 分类；destination-Same 和 pre/post 交叉 Same 也由同一关系分类器产生；
- `FreshSame` 继续使用独立特判；
- 生成 batch plans；
- 如果没有任何兼容的 C2 cell，回退普通 batch。

C1 both-fresh Child 时，C2 的 `RootParent` 是尚未写入 FileTree 的 fresh anchor path。C2 plan 必须携带 `RootParentKind` / `RootParentIsDir`：

- `planCausalC2Cells` 不能只用 `ft.FindNode(rootParent)` 判断 parent；
- 仅当 parent 是 `RootParent == anchor.Path` 的 fresh dir 时放行；
- fresh-root parent 只支持 1 层：anchor path 的 parent 必须在 FileTree 中已存在；nested fresh parent 不生成对应的 fresh-root C2 plan；normal root Parent/Sibling 仍可使用；
- normal parent（`RootParentIsAnchor == false`）必须同时通过 `aEligible(parent, ft.Root)`；C2 builder 和 validator 都不能只检查 file/TmpDir；
- `validateCausalPlan` 对 `FreshA` 使用同一例外和同一 `aEligible` 防御；
- 本轮只支持本地 C1+C2 链路，不支持 C2/DCT 跨节点 fresh child。

C2 继续沿用现有 `PendingDirected` / `ResolveDirectedPending`。
C2 的 pending cell 是 root→variant cell，不是 C1 cell。

C2 的 existing B / FreshB live recheck 与普通 planner 对齐：

- existing B 检查 `variantKindAllowed` 和 offset bucket；
- FreshB 要求 `RetB=Success`、`OffB=NA`、`mkdir/creat` 类型匹配，且 fresh path 不存在；
- persistence 目录使用真实 `Persistence_dir`；
- rename ENOTEMPTY 只接受 source 目录、destination 为非空普通目录；
- root validation：FreshA2 必须是 rename，FreshA 必须是 mkdir/creat；普通 root 和 FreshA 的 normal parent 都必须 `aEligible`。

---

## 5. Rename 设计

### 5.1 Anchor 是 rename

```text
m: rename(src_m, dst_m)
anchor effective path = dst_m
```

source 用于 anchor 路径/类型合法性判断（包括 fresh rename destination 的类型继承），也作为 anchor 的 pre-path 参与 `m → root` 的关系分类。
anchor 的 source/destination 不做跨计划路径保留；它们只参与单次 anchor 合法性/关系判断。

rename 成功后 source 子树消失：

- source 本身和 source 后代不能作为 root 路径候选；
- source parent/sibling 仍然存在，可以继续作为 root 路径候选；
- root 路径过滤按 §2.6 执行，适用于 normal/fresh/rename root；
- rename 失败时 source 可能仍在，但 C1 HB pair 要求 anchor 成功，失败场景不产生 C1 cell，因此不保留 source 子树 root 路径。

### 5.2 Root 是 rename

C1：

```text
Root    = anchor 调用名
Variant = rename
Rel     = root 与 anchor 的实际 pre/post relation
RetB    = Success
```

destination 由 C1 关系和 anchor 路径推导；source 由同父同类节点选择。

F/E 选择：

- destination 不存在 -> F；
- destination 存在且同类文件/空目录 -> E；
- destination 存在但类型不同 -> 不能 E；
- destination 是非空目录 -> E 失败，改 F 或放弃。

root rename 的 `BaseA/BaseA2` 固定后，C2 只使用这两个路径，不重新选择。

### 5.3 C2 root 是 rename

C2 继续使用 C1 固定的：

```text
RootSource
RootDest
RootKind = F/E
```

root `RetA=Success`。
`FreshA2` 已覆盖 root rename F/E；E destination 的类型、同父、空目录条件及 validation 已接入。

### 5.4 C2 variant 是 rename

保留现有形态：

- F：fresh destination，Success；
- E：existing destination，文件/空目录，Success；
- ENOTEMPTY：非空目录 destination，Failure。

variant source/dest 必须：

- 同父；
- 同类；
- source 存在；
- F destination 不存在；
- E destination 合法；
- ENOTEMPTY destination 确实非空；
- relation 使用完整 pre/post 分类，destination-Same 和交叉 Same 也合法；
- root rename 的 destructive Same 只在匹配 root source 时生效，destination-Same 不按 source 被移走处理；
- 不要求与 root source/dest、anchor source/dest、其它 plan 的路径不冲突；碰撞保留为冲突测试。

### 5.5 Rename 路径策略

- **不做跨计划路径保留**：root/anchor/variant 的路径碰撞是允许的冲突测试；
- E destination 不额外做跨计划 `used-path` 保留；
- `filterBaseBAuthority` 保持现状；
- 只保留单个操作的合法性校验：
  - HMDFS 只允许同父 rename；
  - source/destination 同类型；
  - destination 不能是 merge_view 根；
  - root source / root E destination 不能是 TmpDir；
  - variant rename 的 source 可以是 TmpDir，但 E destination 不能是 TmpDir；
  - `._renamed_` 后缀用 `renameSuffixBase` 归一化，不能累积。

---

## 6. 插入、回退与记账

### 6.1 插入

- 生成 root 调用序列；
- root 插在 anchor 后面；
- `refTime` 取 anchor 的 `CheckInfo.Etime`；
- variants 按 root 的时间参考对齐；
- C2 的时间形态仍沿用现有 `refTime` 近似：HB 目标可能实际运行成 CC，这是现有 directed insertion 的已知限制，不由 anchor 修复；
- `insertDirectedBatch` 全有或全无；
- 任何生成/校验失败都不提交，不写 pending。

### 6.2 回退

```text
C1 无可行 anchor/root
  -> 普通 PickDirectedBatch
C1 成功但 C2 无兼容 cell
  -> 普通 PickDirectedBatch
insertDirectedBatch 失败
  -> 现有 DCT 回退
```

C1 已选出但 C2 失败时，不把 C1 cell 记为 miss。

### 6.3 路径碰撞与 batch authority

- 不进行全局跨节点状态模拟。
- **不做跨计划路径保留**：允许 anchor/root/variant 的路径碰撞，例如 `rename(a,b)` 与 `rename(c,b)` 竞争同一 destination，或 `write(a)` 后 `rename(a,b)`；这些碰撞本身可能产生 bug。
- 碰撞导致的 C2 miss 接受；不为了预测安全而排除这些路径。
- 只保留单个操作的合法性校验：
  - rename `source != destination`；
  - source/destination 同父、同类；
  - E destination 合法；
  - root/variant source 存在。
- **`filterBaseBAuthority` 暂时保持现状**：C2 variants 仍按现有规则限制同一 `BaseB` 的并发写，本轮不放开。
- C2 枚举经过 combo-level retirement 过滤；已 retired combo 不再生成 C2 plan。
- C1 暂不加同样的 combo-level filter，这是有意保留的差异。
- E destination 不做跨计划 used-path 保留。
- 后续可选放宽项：如果要让 C2 variants 之间也允许同一路径竞争，需要重新评估 batch authority、pending 归属和 miss 记账；本轮不实现，也不提前改变 cell/pending 语义。
- C2 仍使用现有 pending miss 机制。
- 如果后续发现 anchor 路径碰撞导致可构造 cell 被错误标记 `cellUnreachable`，再增加 anchor-risk miss 类别；本轮不扩展全局状态模型。

---

## 7. 性能

- C1 只从 p0 anchor 出发，不重扫全局 FileTree；
- anchor rename 时 `Path` 和 `Source` 用于 relation/pre-path 分类；root path 候选会过滤 source 子树，但保留 source parent/sibling；
- C2 在固定 root 上枚举，不重复运行完整 `PickDirectedBatch` 四次；
- 同一 C1 cell 去重；
- C1 阶段只生成路径描述符，不生成实际调用；
- C2 生成实际 variants 后才 clone；
- fresh/rename 候选设置上限；
- 无可行 C1/C2 时立即回退，不消耗多次重规划。

---

## 8. 测试

- C1 普通 root、fd root、fresh root、rename root；
- C1 anchor 普通/fd/rename；
- C1 anchor pre-path：source parent/sibling 参与 root 路径；source 本身/source 后代被过滤；
- rename anchor：不生成 source 本身/source 后代 root；
- fresh rename destination：source 子树同样过滤；
- C2 fresh-root parent 只支持 1 层；nested fresh parent 不生成 fresh-root C1/C2 plan；normal root Parent/Sibling 保留；
- C2 `FreshA` normal parent 为 `ft.Root` 时 builder/validator 都拒绝；
- C1 anchor PathKind：existing / fresh / unknown；
- creator 类型：mkdir -> freshDir，creat/open O_CREAT -> freshFile，rename destination -> source 类型；
- rename source 是目录/文件时，freshRenameDst 的 IsDir 分别为 true/false；
- 后续 modifier anchor 使用更早 creator 的 IsDir；
- `mkdir`/`creat` 成功且路径不存在 -> fresh；
- 创建失败但路径不存在 -> unknown；unknown 不作为 C1 anchor；
- `open O_CREAT` 成功且路径不存在 -> fresh path；
- `open` 无 `O_CREAT` / 路径已存在 / `O_TMPFILE` -> 不按 fresh 处理；
- `open O_CREAT` 不进入 C1 anchor 候选，后续 modifier 可使用该路径；
- 后续 modifier anchor 通过更早的 `open O_CREAT`/mkdir/creat/rename creator 分类为 fresh；
- GlusterFS link/symlink anchor：target/linkpath 两个 role 都收集；C1 cell 只用 target；
- link/symlink target 由 anchor 关系派生，不是 fixed baseline；映射后仍在测试根内；
- symlink payload 由 canonical target/linkpath 计算，解析后不逃出测试根；
- readlink 跟随 symlink linkpath；payload 能还原 canonical target；
- PathSet broad 的 link/symlink target-only、readlink follower、rename src/dst
  与 rename policy 顺序回归；
- both fresh Child：`RootParent == anchor.Path` 的 fresh parent validation 通过；
- C2 both-fresh Child：`planCausalC2Cells` 接受同一 anchor path 的 fresh dir parent；
- C2 both-fresh Child：`validateCausalPlan` 的 `FreshA` 放行同一 fresh parent；
- C2 fresh parent 仅限本地 C1+C2 链路；C2/DCT 跨节点 fresh child 负向测试；
- 非 fresh 或 `IsDir == false` 的 anchor，或 parent 不是 anchor path 时，fresh-parent validation 必须拒绝；
- fresh anchor + normal root：Parent/Sibling 可生成，Child 不可生成；
- fresh dir anchor + fresh root：Child 可生成；
- fresh file anchor + fresh root：Child 不可生成；
- existing dir anchor + fresh root：Child/Sibling 可生成；
- existing file anchor + fresh root：仅 Sibling 可生成；
- C1 不枚举 NoRel；fresh mkdir/creat root 不生成 Same/Parent；
- C1 四种路径关系；
- C1 root `RetB=Success`；
- anchor prior 成功偏好当前不实现；同一 cell 保留 first witness；
- 普通 fresh root 只允许 Child/Sibling anchor，parent 必须 `aEligible`；both fresh 的 fresh-parent 例外见 §2.5；
- root rename F/E；
- root rename 非空目录 E 不可用；
- root source/dest 同父、同类、source 存在；
- C2 root rename F/E；
- C2 variant rename F/E/ENOTEMPTY；
- C2 destination-Same / pre-post 交叉 Same；
- C2 combo retirement；
- FreshB validation、existing B recheck、persistence；
- `._renamed_` 不累积；
- anchor/root/variant 路径碰撞允许；
- C1 成功但 C2 无 cell -> 回退；
- C2 pending hit/miss；
- 固定 RNG 可复现；
- 现有 `PickDirectedBatch`/`insertDirectedBatch` 回归。

---

## 9. 实施顺序

1. 普通 root/fd root 的 C1 + C2 链路：已实现；
2. fresh mkdir/creat root：已实现；
3. root rename F：已实现；
4. root rename E：已实现；
5. anchor rename：已实现；
6. variant rename F/E/ENOTEMPTY：已实现；
7. FreshB、C2 combo filter、pre/post relation、validation：已实现；
8. 路径碰撞策略：不做跨计划保留；E destination 不做跨计划保留；
9. C1 anchor PathKind、fresh anchor matrix、`open(O_CREAT)` 边界：已实现；
10. 2026-11-09 复核：rename source 子树过滤、1 层 fresh/nested fresh parent 限制、C2 FreshA normal parent 防御、anchor success 偏好文档对齐：代码已落，Linux 交叉编译/vet 通过；
11. 2026-11-18 复核：rename source 子树过滤改用显式 `SourceMoved`；link/symlink 的 `Source=target` 不再触发过滤；Linux 交叉编译/vet 通过，Ubuntu runtime 待验证；
12. Ubuntu runtime 与 benchmark：待执行。

---

## 10. 代码触点

- `src/prog/causal_root.go`：
  - C1 anchor/root 规划；
  - C2 fixed-root 枚举；
  - full pre/post relation helper；
  - C2 combo filter；
  - causal validation；
  - FreshB/rename 支持；
  - `classifyAnchorPath`、`causalRootPathsByPath`；
  - `causalAnchor` 的 `PathKind` / `IsDir`；
  - `planCausalC2Cells` / `validateCausalPlan` 的 both fresh Child fresh-parent 支持；
  - C2 plan 的 `RootParentKind` / `RootParentIsDir`；
  - 2026-11-09 复核已落：rename anchor 的 source 子树 root path 过滤；C1 `addFreshPlan` 使用 `freshParentNodeEligible`；C2 FreshA normal parent 的 `aEligible` 防御；
  - 2026-11-18 复核已落：`causalAnchor.SourceMoved` 与
    `causalRenameRootPathAllowed`，只对成功 rename anchor 过滤 source 子树。
- `src/prog/target.go`：
  - `LookupConst(name) (uint64, bool)`，可选常量不触发 `GetConst` panic。
- `src/prog/distributed_choice.go`：
  - `extractOpenFlags`；
  - `parseOpenFlags` 的 `(flags, ok)` 形式；
  - optional const lookup（`O_TMPFILE` 等）。
- `src/prog/directed_plan.go`：
  - directed plan/batch 类型；
  - `PickDirectedBatch` causal-first 入口；
  - `insertDirectedBatch` anchor 插入与 root 元信息；
  - C2 plan/batch 的 `RootParentKind` / `RootParentIsDir`。
- `src/prog/dag.go`：
  - 复用 `determinePathRel` / `dagRelToDctRel`。
- `src/prog/mutation.go`：
  - effective path、fd owner、rename source/dest 辅助；
  - anchor 时间校验；
  - `freshPathKindsBefore` 支持 `open(O_CREAT)`。
- `src/prog/coverage_board.go`：
  - C1/C2 未覆盖 cell 查询；
  - C2 combo retirement；
  - 如需 anchor-risk 诊断再扩展。
- `src/prog/causal_root_test.go`：
  - C1/C2、rename、FreshB、relation、combo filter、fallback、RNG、pending；
  - 2026-11-09 复核测试：rename source 子树过滤、source parent/sibling 保留、nested fresh-root plan 拒绝、normal root Parent/Sibling 保留、C2 FreshA normal parent `aEligible` 拒绝；
  - 2026-11-18 复核测试：link/symlink target 子树不被 rename source 过滤，rename source 子树仍拒绝。

---

## 11. 当前明确不做的内容

- 跨节点完整文件状态模拟；
- nested fresh parent 作为 fresh-root parent / both-fresh Child parent（第一版只支持 1 层 fresh-root parent；normal root Parent/Sibling 不受影响）；
- 自动合成新的 anchor；
- root 失败形态作为 directed root；
- C1 的独立 pending/miss 账目；
- 跨计划路径保留（包括 E destination 的 used-path 保留）；
- C1 的 combo-level filter；
- causal size-changing offset skip；
- 全局关系评分；
- 论文/PPT 修改。

---

## 12. 实现落点（2026-10-28；2026-11-09 复核）

- `classifyAnchorPath`：existing / freshDir / freshFile / freshRenameDst / unknown，并返回 `IsDir`；
- `causalAnchor` 保存 `PathKind` / `IsDir`；modifier anchor 使用更早 creator 的类型；
- both fresh Child 的 fresh-parent validation 例外：只允许 `RootParent == anchor.Path`，且 `PathKind == freshDir` 或 `PathKind == freshRenameDst && IsDir`；
- C2 plan 携带 `RootParentKind` / `RootParentIsDir`；`planCausalC2Cells` 和 `validateCausalPlan` 对同一 fresh parent 放行；
- C1 both-fresh Child 本轮只支持同一 p0/C1+C2 链路；C2/DCT 跨节点 fresh child 继续不做；
- `LookupConst` + `parseOpenFlags(...) -> (flags, ok)`：`O_TMPFILE` 等未定义常量不能触发 `GetConst` panic；
- 扩展 `freshPathKindsBefore` 支持 `open(O_CREAT)`，并保留成功判定；
- `causalRootPathsByPath`：fresh anchor + normal root 的 Parent/Sibling 路径枚举；
- 2026-11-09 复核后：rename anchor 保留 source parent/sibling 候选，过滤 source 子树；C1 `addFreshPlan` 与 C2 统一为 1 层 fresh-root parent；C2 FreshA normal parent 独立检查 `aEligible`；
- fresh anchor + normal root 不生成 Child；both fresh 的 Child 仅在 fresh dir anchor 下构造；
- C1 不加入 NoRel；NoRel 只在 Seed Derivation 中使用；
- `collectCausalRootPlans` 去掉仅凭 `FindNode(path) == nil` 的硬跳过；
- `open O_CREAT` 只作为 fresh path creator，不加入 C1 anchor 候选；
- §8 中对应的 PathKind / fresh anchor / C2 fresh parent 测试；
- Ubuntu runtime 验证。

