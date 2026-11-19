# GlusterFS 适配总计划（v1）

> 状态：设计定稿（P1 已收敛）；核心实现已落；P2 review #7–#11 已收口；
> GlusterFS dual-path causal 与 role-aware coverage 已启用；D1 BPF standalone
> smoke 已通过，D2/D3/D4 待执行
> 日期：2026-11-15（2026-11-18 更新 P2 收口、PathSet current state /
> creator binding / rename prefix remap、role-aware coverage、dual-path gate）
> 范围：GlusterFS 3 server + 3 client、replica 3；HMDFS 行为保持不变；NFS 暂不实现
> 权威关系：本文是 GlusterFS 适配的总入口。tracer 的 BPF 原始细节参考
> `docs/glusterfs-tracer-implementation-spec.md`；两者冲突时以本文为准。
> 相关：`docs/glusterfs-config-and-scripts.md`、`docs/executor-file-tree-restore.md`、
> `docs/kernel-build-glusterfs-bpf.md`、`docs/seed-optimization-plan.md`

## 0. 文档目的

本文把 GlusterFS 适配的调用集、事件集、双路径关系、DAG/覆盖板、角色模型、
executor runtime、初始树加载、config/脚本和验收标准写成可直接实施的规格。

实现顺序：先按本文写文档和补丁说明，文档冻结后再改代码。实现需要偏离本文时，
先改本文，再改代码。

## 1. 范围与已锁定决策

### 1.1 v1 调用集

GlusterFS v1 的 `enable_syscalls` 固定为 22 个，顺序不敏感：

```text
open read write close stat pread64 pwrite64 fsync fdatasync truncate
getdents64 rename mkdir rmdir creat unlink chmod link symlink readlink
getxattr setxattr
```

新增调用是 `link`、`symlink`、`readlink`、`getxattr`、`setxattr`；其余是 HMDFS
已有调用在 GlusterFS profile 下重新确认。config 必须与这 22 个逐字一致；
`readv/writev/preadv/pwritev` 不启用，也不做 alias。

明确不做：

- `readv` / `writev` / `preadv` / `pwritev`：v1 不启用，不做 alias；
  只用 `read/pread64/write/pwrite64`；
- `openat` / `mkdirat` / `renameat` / `linkat` / `symlinkat` / `readlinkat` /
  `unlinkat` 等 at 变体：v1 不启用，matcher 也不为它们补规则；
- `fstat` / `lstat` / `chown` / `fchown` / `lchown` / `fchmod` / `ftruncate` /
  `getdents` / `sendfile` / `faccessat` / `fcntl` / `flock` / `mknod` /
  `fallocate` / `lseek` / `lsetxattr` / `lgetxattr` / `removexattr` /
  `listxattr`：v1 不启用；其中 `lstat`/`lgetxattr`/`lsetxattr` 也不做
  NoFollow 生成；
- `renameat2`：当前 FUSE build 无 handler，明确不支持；v1 config 只生成
  `rename`，不生成 `renameat`/`renameat2`；
- stash、dcache、failure/C2san、NetFailure、NodeCrash：当前不支持；
  C2san 代码未完成、HMDFS 当前也未启用；persistence 是配置/覆盖维度，
  不是 seed family；
- RENAME2 45、COPY_FILE_RANGE 47、SYNCFS/TMPFILE/STATX、IOCTL/POLL/BMAP；
- NFS。

profile 启动校验必须检查 `enable_syscalls`：出现 22 个之外的调用时报告
unsupported/ignored，不能静默保留（尤其避免 `readv/writev` 这类“启用了但
结构化生成器不生成”的假支持）。

### 1.2 拓扑与运行参数

| 项目 | 决定 |
|---|---|
| 拓扑 | 3 server + 3 client（6 VMs） |
| volume | `replica 3` |
| `server_num` | 3 |
| `fuzzing_vms` | 6 |
| `dfs_setup_params` | `3 3 'replica 3'` |
| `enable_csan` | true |
| `enable_c2san` | false |
| `net_failure` / `node_crash` | false |
| `data.img` | 仍只挂 server，但当前不使用；brick 在 server rootfs |
| client cwd | `/root/glusterfs-client/eifuzz_root` |
| server cwd | `/root` |
| baseline | `/home/baseline/glusterfs/`，烤进 client 镜像 |
| reset client | `executor_index == server_num`（3-3 下 index 3） |
| cid | 路径所有者为 `shared` |
| HMDFS | `ServNum=0`、现有 0–15 事件、`merge_view`、same-parent rename 不变 |

> v1 固定 3 个 client（`MAXCLT=3`，`loadGlusterfsConfig` 拒绝其它值）。共享的
> HMDFS seed/legacy 生成器只在 2–3 个 slot 上做过预算验证；1 client 会触发
> over-budget/截断，2 client 虽有 fold 逻辑但 v1 不启用。1/2 client 支持需要
> 先做 generator slot folding。

### 1.3 兼容性约束

- HMDFS 的 wire event、BPF 探针、DAG 语义、coverage cell、same-parent rename 不变。
- RNG 顺序不作为兼容约束；固定 seed 的序列化字节不要求一致。
- GlusterFS 与 HMDFS 共用 Go/DAG 接口，但不共用事件语义和 profile 策略。

### 1.4 P1 决策摘要（2026-11-15）

| 问题 | v1 结论 |
|---|---|
| FileTree 链接模型 | **不在 FileTree 建模 link/symlink**。baseline 无链接，每轮 restore 回 baseline；FileTree 只保存普通文件/目录/空目录/TmpDir。`link/symlink` 的 target 是测试根内 canonical 路径（baseline 或 anchor 关系派生），linkpath 是 program-local fresh binding；DCT/定向不能 `FindNode(linkpath)`。每轮 execute 前 restore，FileTree 保持 baseline，`SyncFileTreeFromFsMd` 停用；链接只记录/跳过，不建模。 |
| linkpath AccessMode | `readlink/unlink/rename` NoFollow；`stat/open/read/write/truncate/chmod/xattr` Follow；`link` 的 old NoFollow、new Create；`symlink` 的 target 是 Payload、linkpath 是 Create；fd 类调用在 open 时已解析。详见 §6.2。 |
| 跨目录 rename | GlusterFS 支持文件和目录跨目录 rename，destination 支持 fresh 与 existing replace；HMDFS 保持 same-parent。`renameat2` 不支持。详见 §6.5。 |
| readv/writev alias | 不做 alias；config 不启用 `readv/writev/preadv/pwritev`，只用 `read/pread64/write/pwrite64`。配置校验报告 unsupported。 |
| Call → Event matcher | 22-call 表；`creat` 和 `open(O_CREAT)` 只映射 `CREATE`，不映射 `FILE_OPEN`；`open` 按 flags 区分 CREATE/DIR_OPEN/FILE_OPEN；`getdents64` → ITERATE。详见 §5.3。 |
| PathSameInode / hardlink identity | **GlusterFS v1 不做 `PathSameInode`**，不增加 `RelKind`，不增加 `entry_ino`；`comboKey` 保持 role-free，`cellKey` 只增加 `Role`（target/普通 cell 用 `cellRoleNone`，linkpath 用显式 role）。保留 link/symlink 的 `Path/Path2`、program-local `RootLinkPath`/`freshLinks`、`HardlinkGroupKey` 和 authority；HMDFS 保持现有分类并折叠为 `PathSame`。源码已确认 LINK/SYMLINK/UNLINK/RMDIR/RENAME 的 `loc.inode` 可用，但 v1 的 DAG 路径改用调用参数/fd provenance，不依赖它（见 §4.3、§5.2）。 |
| PathSet 双路径 | broad 支持 `link/symlink/readlink/rename`；v1 只迁移 link/symlink 的 target，linkpath 是 program-local fresh binding 不随 PathSet 迁移；rename src/dst 都迁移，dst 由 `RenamePolicy` 在迁移阶段选，existing 优先、fresh 兜底。详见 §8.7。 |

补充：

- DCT seed 没有显式 Success/Ret 规划；`readlink` 不能做 root，只能作为
  `symlink` root 的 variant（`PathSame`），实际 Success/ENOENT
  都是合法结果。
- DCT mutation 有 `refTime`/`ChooseTemporal`，可把 symlink 放在 readlink 前形成
  HB；仍没有显式 RetB 目标，CC 按竞态处理。Directed 才显式规划 RetB。
- 跨节点可见性：同节点按调用顺序；跨节点用归一化 `CheckInfo.Stime/Etime`，
  只有 `creator.Etime < consumer.Stime` 才规划 success；重叠只做 CC/竞态；
  没有时间证据不规划 success。详见 §8.4。

### 1.5 P0 追补（2026-11-16）

- **hardlink alias**：FUSE `inode_path` 对 hardlink 返回任意 dentry；
  `truncate("q0")` 的事件 path 可能是 `h1`。调用参数仍是 canonical 路径，
  DAG 非 LOOKUP 用调用参数/fd provenance；planned linkpath 可以作为
  `write/truncate` 的调用参数，内核随机 alias 不是调用参数。
- **RELEASE/RELEASEDIR**：两者都没有 R。v1 保留文件 `RELEASE 18`
  （HMDFS 也 trace `hmdfs_file_release_local`），过滤目录 `RELEASEDIR 29`
  （HMDFS 不 trace 目录 release）。18 的 path 走 fd provenance/nodeid，
  不计 missing_r；29 不产生 raw/final event。
- **close 的 DAG 形态**：close 不做 DCT/定向/causal 的 root/variant，只在
  open/read/write/getdents 序列里配套生成；文件 close 事件仍匹配 `close`
  调用，作为非 modifier 的 B/CC 侧进入 DAG pair；目录 close 没有事件。
  coverage board 的 root/variant 网格不含 close。

## 2. Profile、角色空间与 CID

### 2.1 DFSProfile

新增 `src/prog/dfs_profile.go`：

```go
type DFSProfile struct {
    Name    string
    Calls   CallProfile
    Events  EventProfile
    Paths   PathPolicy
    Plan    PlanProfile
    Tree    TreeProfile
    Gen     GenProfile
    Check   CheckerProfile
    Trace   TraceProfile
    Feature FeatureProfile
}
```

- `CallProfile`：调用集、路径参数角色、类型约束、modifier/offset、生成器键。
- `EventProfile`：wire event → 语义事件、call → events、路径来源、默认类型；
  `CallFuncClasses(call, isDir, flags)` 正向映射、`funcMatchesCall` 反向映射；
  `SupportsSameInode`（HMDFS=true，GlusterFS v1=false）。
- `PathPolicy`：namespace 前缀、根、深度、same-parent/cross-parent、free path 约束。
- `PlanProfile`：DCT/定向板/因果链的 root/variant/causal 集、variant kinds、
  state effect、`PathStateTracking`（in-program current state；HMDFS=false，
  GlusterFS=true）。
- `TreeProfile`：FileTree 前缀、节点类型；v1 不在 FileTree 建模
  symlink/hardlink（链接路径走 program-local `RootLinkPath`/`freshLinks`）。
- `GenProfile`：生成器与取值模型、参数策略。
- `CheckerProfile`：A/B 字段、supported/ignored calls、restore。
- `TraceProfile`：parser、executor init/start/stop、wire 布局。
- `FeatureProfile`：HMDFS-only 能力的 supported/enabled 两层开关（stash、
  dcache、persistence、failure/NetFailure/NodeCrash、C2san、server programs）。

注册 `"hmdfs"`、`"glusterfs"`；未知 DFS 使用显式 `generic` 或启动失败，
不得静默回退 HMDFS。

### 2.2 DFSConfig

`Hmdfs_config` 继续复用，但语义改为 `DFSConfig`：

```go
type DFSConfig struct {
    Profile *DFSProfile

    Node_num    int // seed-generation slots; GlusterFS sets this to ClientNum
    ServNum     int // leading empty server programs
    ClientNum   int // client programs in the executor batch
    TotalNum    int // full batch: ServNum + ClientNum
    RootProgIdx int // absolute index of the first client (= ServNum)
    RootPrefix  string

    DfsName string
    InitIp  string
    Cids    []string // client-only

    FileTree *FileTree

    // HMDFS 专用；GlusterFS 留空
    Init_file               map[string][]string
    Init_dir                map[string][]string
    Init_tmpdir             map[string][]string
    Init_empty_dir          map[string][]string
    Node_idx_of_persistence int
    Persistence_dir         string
    File_in_persistence_dir map[string][]string
    FileSize                map[string]map[string]uint64
}

type Hmdfs_config = DFSConfig
```

- 所有现有函数先继续接收 `*Hmdfs_config`，避免大面积改签名。
- 新逻辑只从 `Profile` 取策略，不从 HMDFS 专用字段推断。
- `Node_num` 保留为 seed/DCT/legacy 生成器的 slot 数；GlusterFS 下
  `Node_num == ClientNum`。`TotalNum` 是完整 executor batch（含空 server）。

### 2.3 角色空间

```text
ClientNum   = FuzzingVMs - ServNum   // GlusterFS v1 固定为 3
Node_num    = ClientNum              // GlusterFS；HMDFS 为总节点数
TotalNum    = FuzzingVMs
RootProgIdx = ServNum
```

- prog 层算法只看 client view：`ps[ServNum:]`，root 是 client view `[0]`。
- 生成器返回 client-only 程序；在 IPC/executor 边界补 `ServNum` 个空 server 程序。
- mutation/planner 直接拿 client view，server progs 不被修改。
- IPC/executor/DAG/checker 使用绝对 executor index。
- server programs 默认必须为空；failure 未支持前不允许任何 server 文件操作。

辅助 API：

```go
func (c *DFSConfig) ClientView(ps []*Prog) []*Prog
func (c *DFSConfig) WrapClientProgs(cps []*Prog) ([]*Prog, error)
func (c *DFSConfig) ClientProgIdx(clientIdx int) int
func (c *DFSConfig) ClientIdx(progIdx int) int
func (c *DFSConfig) CidForClient(clientIdx int) string
func (c *DFSConfig) CidForProg(progIdx int) string
func (c *DFSConfig) PersistenceProgIdx() int
```

启动/批次校验：

- `TotalNum == FuzzingVMs == len(Executor)`
- `ServNum < TotalNum`
- `len(Cids) == ClientNum`
- GlusterFS `DfsSetupParams` 的 server/client 计数与 config 一致
- full batch `len(ps) == TotalNum`
- `ps[0..ServNum-1]` 为空
- client programs 非空
- `ClientNum == 3`（v1 固定；`MAXCLT=3`，1/2/4 均拒绝）

### 2.4 CID 模型

| DFS | 模型 | Cids | FileTree owner |
|---|---|---|---|
| HMDFS | 每节点本地命名空间 | 每 client 一个 | `Cids[i]` |
| GlusterFS | 共享 volume | 重复 `"shared"` 到 `ClientNum` | `"shared"` |

- GlusterFS config 不传 cid。
- `InitialDir` 仍读取，但 GlusterFS 只加载 `shared` 清单一次。
- `Init_file / Init_dir / Init_tmpdir / Init_empty_dir / FileSize` 只挂 `shared`。
- `InitFromConfig` 对重复 shared owner 去重。
- client identity 使用 `ClientIdx`/`ProgIdx`，不再用 cid 区分客户端。

### 2.5 未来 server 端种子扩展

当前只启用 client view，但模型预留 server view：

```go
type NodeRole uint8 // Server / Client

type RoleView struct {
    Role    NodeRole
    Progs   []*Prog
    RootIdx int
    Cids    []string
    Gen     *GenProfile
    Events  *EventProfile
}
```

- 将来可增加 `ServerView`，但 server seed 不能复用 client FUSE/syscall 生成器；
  需要 server operation/event profile（brick 文件、gfapi、故障注入、协议动作）。
- server 事件进入独立 server DAG/覆盖板，不混入 client board。
- `DFSProfile` 预留 `SupportsServerSeed`、`ServerGenProfile`、`ServerEventProfile`。

### 2.6 HMDFS-only 能力与通用化边界

通用化不是删除 HMDFS-only 代码，而是让 `FeatureProfile` 决定是否启用。
HMDFS 的代码路径和现有 config 行为必须保留。

| 能力 | HMDFS | GlusterFS v1 |
|---|---|---|
| Stash seeds | 支持；当前 seed mix 为 10% | 不支持/不生成 |
| Fsops seeds | 90% | 100% |
| Dcache seeds | 遗留代码；当前不作为生成桶 | 不支持/不生成 |
| Persistence | 支持；读 `large_dir.info` | 不支持；`Persistence_dir=""`、`Node_idx=-1` |
| CSAN | `enable_csan=true` | `enable_csan=true`（A 类字段） |
| C2san | 代码未完成，实际不启用；不在 v1 适配范围 | 不支持 |
| NetFailure | failure config 可为 true | false |
| NodeCrash | 代码存在；当前 HMDFS config 均为 false | false |
| Server programs | 无（`ServNum=0`） | 有；server slots 为空 |
| Client-only generation | 否 | 是 |

约定：

- HMDFS seed mix 固定为 **10% stash + 90% fsops**；不要改成 stash/dcache/fsops
  三分。
- GlusterFS seed mix 为 **100% fsops**；选到 stash/dcache 的路径不进入。
- `GenerateProgsForHmdfsStash`、`MutateStashProg`、`MutateDcacheProg` 保留；
  只在 HMDFS dispatch 中调用。
- C2san 不进入 v1 适配，也不作为 HMDFS 能力开关；代码保留但不启用。
- Persistence 不是 seed family，没有 `IsPersistence`；GlusterFS 只是不加载
  `large_dir.info`、清空 `Persistence_dir`、关闭 coverage 的 persist 维度。
- corpus/batch 校验可以拒绝 `IsStash`/`IsDCache`/`C2test`/`HasCrashFail`/
  `HasNetFail`；不要用“persistence seed”这个概念做拒绝。
- HMDFS 的 `ServNum=0` 让 client view == 原 `ps`；GlusterFS 使用
  `ClientView=ps[ServNum:]`、`RootProgIdx=ServNum`，server slots 原样保留。

### 2.7 Fuzzer 角色 view 与 seed/mutation 入口

`src/syz-fuzzer/proc.go` 不能再以 `DFSName == "hmdfs"` 作为唯一分支，否则
GlusterFS 会落到 HMDFS 的通用 `Target.Generate` / `p.Mutate`，新的
DCT/derived/PathSet 代码不会被调用。所有入口改为 profile 选择：

| 入口 | HMDFS | GlusterFS v1 |
|---|---|---|
| seed generation | 10% stash + 90% `GenerateProgsForHmdfsFsops` | 100% `GenerateProgsForDfsFsops` |
| mutation / smash | `mutateHmdfs` 按 `IsStash`/`IsDCache`/`IsFsops` 分派 | `MutateFsopsWithDCT`，无 stash/dcache |
| fallback mutation | client view 上标准 `Mutate` | 同左，`srvNum=0` |
| lcs / coverage board | HMDFS profile | GlusterFS profile |
| DAG feedback | `EnableDagFb` + client events | 同左；不能只对 HMDFS 开启 |
| batch 校验 | `len(ps)==Node_num` | `len(ps)==TotalNum`、server 空、client 非空 |

规则：

- GlusterFS 的 `GenerateProgsForDfsFsops` 必须返回 client-only 程序，并设置
  `IsFsops=true`；IPC/executor 边界再补 `ServNum` 个空 server 程序。
- `mutateDfs`/`smashInput` 只把 client view 交给 `MutateFsopsWithDCT`；
  server progs 不修改，写回时保留原 slots。
- `MutateFsopsWithDCT`/`insertDerivedCallFromDCT` 内部只认 client-relative
  index；`hmcfg.Cids` 是 client-only。
- 所有 `ps[0]`/`seedPS[0]` 的 client 语义（`IsFsops`/`IsStash`/`IsDCache`、
  `Target`、`SyncIdx`、`HasCrashFail`/`HasNetFail`、`GeneralFailPos`）改为
  `ClientView[0]`/`RootProgIdx`；绝对 index 只用于 IPC/executor/DAG 诊断。
- `executeRaw` 的 batch 长度、server 空、client 非空校验对两个 profile 都启用。
- `lcsFsops`、`InitCoverageBoard`、DAG feedback/build 由 profile 初始化；
  GlusterFS 不能因为 `DFSName != "hmdfs"` 而跳过。

### 2.8 路径根与 filename 生成

GlusterFS executor 在 `chdir` 到固定测试根后，逻辑根是空字符串：

```text
HMDFS:     RootPath = "merge_view"; JoinRoot("file0") = "merge_view/file0"
GlusterFS: RootPath = "";           JoinRoot("file0") = "file0"
```

- `FileTree.Prefix`/`Hmdfs_config.RootPrefix`：HMDFS `merge_view/`，GlusterFS `""`。
- `NewFileTreeForConfig(hmcfg)` 是 config-derived 构造器：按
  `Profile.Tree.Prefix`/`RootPrefix` 建树；`NewLayeredChoiceStrategy` 在
  `hmcfg.FileTree == nil` 时用它再 `InitFromHmdfsConfig`，不能退回固定
  `merge_view/` 的 `NewFileTree()`。`NewFileTree()` 仍作为 HMDFS/测试默认。
- `RootPath()`/`JoinRoot(rel)` 是 profile 路径原语；不能生成 `./file0` 或 `/file0`。
- `rand.go:389 filenameImpl` 不能再用 `dir="."` 或 HMDFS 的 `merge_view`；
  改用 `JoinRoot(...)`。`rand.go`/`generation.go`/`mutation.go` 里所有
  `merge_view`/`merge_view/test_dir` 硬编码一起按 NFS 文档 §3.6.8 改。
- `filenameImpl` 会服务 `Target.Generate()`、标准 `Mutate()`、`filename()` 的
  BufferType 路径参数和 DCT/derived fallback，不能漏。
- 空前缀下 `DepthBucketOf`/DAG/coverage/PathSet 必须用
  `DepthBucketOfWithPrefix` 或 `FileTree.Prefix`，不能整体错位。
- **FileTree.Prefix 与 tracer trace root 是两回事**：GlusterFS 的
  `FileTree.Prefix=""`、`RootPath()=""`；tracer 的 `trace_root` 是
  `eifuzz_root`（相对挂载根 `/root/glusterfs-client`），只用于把 FUSE
  `/eifuzz_root/q0` strip 成 `q0`。不要把 `eifuzz_root` 写进 FileTree/DCT。

## 3. GlusterFS 源码结论

源码基线：`glusterfs-79154ae538f4539ccf69272b2b4736d891262781`，
FUSE protocol 7.24。

### 3.1 FUSE opcode 与系统调用

| FUSE op | 系统调用 | v1 决策 |
|---|---|---|
| READLINK 5 | `readlink` | 启用；`readlinkat` 不启用 |
| SYMLINK 6 | `symlink` | 启用；`symlinkat` 不启用 |
| MKNOD 8 | `mknod/mknodat/mkfifo` | 不做 |
| LINK 13 | `link` | 启用；`linkat` 不启用 |
| SETXATTR 21 | `setxattr` | 启用 path 形态；`lsetxattr/fsetxattr` 不启用 |
| GETXATTR 22 | `getxattr` | 启用 path 形态；`lgetxattr/fgetxattr` 不启用 |
| REMOVEXATTR 24 | `removexattr/...` | v2 |
| LISTXATTR 23 | `listxattr/...` | v2 |
| READDIR 28 | `getdents64` | 映射 ITERATE；`getdents` 不启用 |
| READDIRPLUS 44 | `getdents64` 默认路径 | 映射 ITERATE |
| GETLK/SETLK/SETLKW 31–33 | `fcntl`/`flock` | v2 |
| FALLOCATE 43 | `fallocate` | v2 |
| FSYNCDIR 30 | 目录 fsync | v2 |
| STATFS 17 / ACCESS 34 / LSEEK 46 | 观测型 | v2/低优先 |

明确不可达/不实现：

- RENAME2 45：定义了 opcode，但 `fuse_std_ops` 无 handler，返回 ENOSYS；
- COPY_FILE_RANGE 47：协议 7.24，handler 被 `>=28` 条件编译掉；
- SYNCFS 50 / TMPFILE 51 / STATX 52：当前 header 不存在；
- IOCTL/POLL/BMAP：无 handler；
- FORGET/INIT/DESTROY/INTERRUPT：控制面。

### 3.2 rename

GlusterFS 支持跨目录 rename：

- FUSE `fuse_rename` 分别解析 `finh->nodeid` 和 `fri->newdir`；
- server 仅拒绝跨 `trusted.glusterfs.namespace`，不拒绝不同目录；
- DHT/AFR/POSIX 均支持跨目录 rename。

v1 规则：

- v1 只生成 `rename(src,dst)`；`renameat` 不启用（即使 flags=0）；
- `renameat2` 的 flags 不支持，不生成；
- HMDFS profile 保持 same-parent；
- GlusterFS profile 允许跨目录；
- destination 支持 fresh 和 existing replace；
- 禁止 src==dst、目录移入自身后代、目标父目录不存在。

### 3.3 路径与状态陷阱

- `LINK`：`loc2.path` 是源 inode，`loc.path` 是新链接路径；顺序与 rename 相反。
- `SYMLINK`：`loc.path` 是新链接路径，`state->name` 是 target 字符串。
- `XATTR`：`state->name` 是 key；虚拟 `glusterfs.gfid` 等在 FUSE 层直接处理，
  不能当后端事件。
- `SETATTR`：truncate 新长度在 `state->off`/`attr.ia_size`，不是 `state->size`。
- fd 操作通常没有 `loc.path`，使用 nodeid→path/fd→inode 映射。
- `send_fuse_iov` 没有 `fuse_state_t`；路径必须在 `fuse_fop_resume` 按 `unique` 暂存。
- `GETATTR` 根 inode 特判会走 LOOKUP。
- `SETATTR` 可能 fan-out 成 `setattr+truncate` 两次后端 fop。

## 4. Tracer 与事件契约

### 4.1 新增 FuncID

HMDFS 现有 0–14 不变，15 为 HMDFS `WRITEPAGE_CB`。GlusterFS 使用：

```text
16 LINK
17 SYMLINK
18 READLINK
19 GETXATTR
20 SETXATTR
```

`FuncIDCount=21`。`packFeatures` 的 5 bit funcID 足够；`DagDiag` 的 `[16]`
数组扩展为 `[21]` 或动态。

### 4.2 事件字段

Go 共享事件增加：

```go
type TraceEvent struct {
    Timestamp uint64
    FuncID    uint32
    Ret      int32
    Ino      uint64
    Args     [6]uint64
    ProgIdx  int
    Off      uint64
    Size     uint64
    EntryIno uint64 // HMDFS only; GlusterFS v1 保持 0
    Mode     uint32
    EntryTs  uint64

    // GlusterFS only
    NodeID     uint64
    Path       string
    Path2      string
    XattrName  string
    Truncated  uint32
}
```

`HmdfsTraceEvent` 保留为兼容别名。HMDFS parser 不填 Path/Path2/XattrName。
`EntryIno` 继续服务 HMDFS；GlusterFS v1 parser 不填 `EntryIno`，也不把
`Ino` 当 hardlink identity（见 §4.3 和 §5.2）。

### 4.3 Final wire event（GlusterFS）

固定 1228 字节：

```text
offset  size  field
0       8     timestamp
8       8     entry_ts
16      4     func_id
20      4     ret
24      8     nodeid
32      8     unique
40      8     off
48      8     size
56      4     mode
60      4     path_len
64      4     path2_len
68      4     xattr_name_len
72      4     truncated
76      512   path
588     512   path2
1100    128   xattr_name
```

- 序列化使用长度字段，不依赖 NUL；parser 按固定 1228B stride 读满
  path/path2/xattr 数组再截取。
- `path_len <= 512`、`path2_len <= 512`、`xattr_name_len <= 128`。
- 8192 events 约 10.06 MB；16 MB 输出缓冲足够。
- `path` 相对**测试根**：FUSE 的 `/eifuzz_root/...` 必须 strip 成 `q0/...`；
  `/eifuzz_root` → `""`；mount root `/` 和 `.glusterfs` 内部路径丢弃。
  rename/link 的 `path2` 同样 strip；symlink 的 `path2` 是 raw payload，
  不 strip。
- `mode` 来自 `inode_t.ia_type`（+92）映射 `S_IFMT`，不是 `i_mode`；
  `link` 用 `loc2.inode`，`rename` 用 `loc.inode`。`LOOKUP` 的 R 时机
  `loc.inode` 是父目录，entry 类型由 S reply 的
  `fuse_entry_out.attr.mode` 解析（`iov_out[1].iov_base + 100`；
  `iov_out[0]` 只是 16 字节 `fuse_out_header`），negative lookup 为 0。
  `symlink/mkdir/create` 在 R 时 `ia_type=0`，用函数默认；symlink target
  类型由 DAG 侧的调用链近似解析。`truncate` 的 `off` 是
  新大小、`size=0`；read/write、readdir、xattr 的 off/size 语义见 tracer
  spec §4.1/§6.5。
- **C 结构必须 `__attribute__((packed))` 或手写序列化**；默认 8 字节对齐时
  这个布局是 1232B，不是 1228B。C 端 `sizeof`、executor 输出长度和 Go
  parser 必须使用同一个 packed 尺寸。
- **v1 不增加 `entry_ino`**。源码已确认 `fuse_fop_resume` 时
  LINK/SYMLINK/UNLINK/RMDIR/RENAME 的 `loc.inode` 可用，但 v1 不需要它做
  hardlink identity：
  - `fuse_loc_fill` 对 inode/fd 类操作走 `inode_path(inode,NULL)`；
  - `inode_path` 选 `dentry_list` 的第一个 hashed dentry，hardlink 时可能是
    target，也可能是某个 alias，不保证是本次 syscall 使用的路径；
  - 因此 DAG 非 LOOKUP 顶点改用调用参数/fd provenance，`ev.Path` 只做
    fallback/校验（§5.2），不依赖 `entry_ino`/canonicalization。
- **RELEASE/RELEASEDIR 没有 R**：`fuse_release`/`fuse_releasedir` 直接
  `send_fuse_err`，不经过 `fuse_fop_resume`。v1 保留文件 `RELEASE 18`
  （HMDFS 也 trace `hmdfs_file_release_local`），过滤目录 `RELEASEDIR 29`
  （HMDFS 不 trace 目录 release）。18 的 path 走 fd provenance/nodeid，
  不计 missing_r；29 不产生 raw/final event。

### 4.4 Raw path event（GlusterFS）

固定 1204 字节：

```text
offset  size  field
0       8     ts
8       8     unique
16      8     off
24      8     size
32      4     mode
36      4     path_len
40      4     path2_len
44      4     xattr_name_len
48      4     truncated
52      512   path
564     512   path2
1076    128   xattr_name
```

raw path event 也按 packed/手写序列化；默认 8 字节对齐时是 1208B。

`ctl_event` 为 44 字节：

```text
ts u64, unique u64, phase u32, opcode u32, nodeid u64, pid u32, error s32,
mode u32
```

ring：

- ctl：1 MB
- path：4 MB
- 若压测出现 drop，可将 path ring 提升到 6/8 MB；先保持 4 MB。

### 4.5 探针与 offset

源码：`xlators/mount/fuse/src/fuse-bridge.c`。

uprobe：

```text
send_fuse_iov
fuse_fop_resume
fuse_dispatch
```

`fuse_dispatch`：

```text
finh = *(u64 *)(arg0 - 24)
finh: opcode=+4 unique=+8 nodeid=+16 pid=+32
```

`fuse_fop_resume`：

```text
fuse_state_t.finh       = state + 160
fuse_state_t.loc        = state + 32
fuse_state_t.loc2       = state + 96
fuse_state_t.off        = state + 176
fuse_state_t.size       = state + 200
fuse_state_t.name       = state + 248
loc.path                = loc + 0
loc.inode               = loc + 16
loc2.inode              = loc2 + 16
inode_t.ia_type         = inode + 92
```

`send_fuse_iov`：

```text
finh    = arg1
iov_out = arg2
fuse_out_header = iov_out[0].iov_base
error   = *(s32 *)(fuse_out_header + 4)
```

S 用入口 uprobe 保存 `{finh, unique, opcode, pid, nodeid, error}`，uretprobe
只取 `bpf_ktime_get_ns()` 写 S；不能在返回时读 `arg1/arg2`（尾调用/栈帧）。

本 build 的 `bpf_uprobe_opts` 没有 `func_name`；attach 用
`bpf_program__attach_uprobe(prog, ret, -1, fuse_so, file_offset)`。
`fuse_dispatch`/`fuse_fop_resume` 的 `fuse.so` 是 LOCAL `.symtab`，用 libelf
解析符号，并按所在 PT_LOAD 计算
`file_offset = st_value - p_vaddr + p_offset`；不要直接把 `st_value` 当
文件偏移。`send_fuse_iov` 用 `ret=true`。

P0 已验证 `ia_type=+92`、`finh=arg0-24`、`pid=+32`；`link` 的源 inode 在
`loc2.inode`（R 时 `loc.inode` 为空），`rename` 的源在 `loc.inode`。如果
GlusterFS 版本变化，offset 必须随文档更新。

### 4.6 路径选择

| opcode | Path | Path2 | 其他 |
|---|---|---|---|
| RENAME 12 | `loc.path`（旧） | `loc2.path`（新） | 原样保留 |
| LINK 13 | `loc.path`（新） | `loc2.path`（源） | v1 不写 `EntryIno` |
| SYMLINK 6 | `loc.path`（新） | `state->name`（target） | v1 不写 `EntryIno` |
| READLINK 5 | `loc.path`（链接） | 空 | 不记录 target |
| SETXATTR 21 / GETXATTR 22 | `loc.path` | 空 | `XattrName = state->name` |
| READ 15 / WRITE 16 / FSYNC 20 / READDIR 28/44 | nodeid→path 或 fd→path | 空 | fd 操作 |
| RELEASE 18 | nodeid→path 或 fd→path | 空 | 文件 close 有 D/S、无 R；保留 |
| RELEASEDIR 29 | — | — | 过滤，不产生事件 |
| LOOKUP 1 | `loc.path`（已解析的 parent+name child path） | 空 | 直接使用；nodeid 是父目录，不做 fallback；path 缺失/out-of-root/path 截断时 drop |
| MKDIR 9 / CREATE 35 / UNLINK 10 / RMDIR 11 | 调用参数路径 | 空 | 不做 nodeid 目标映射 |

`nodeid -> path` 只对目标 inode 型 opcode 建立；LOOKUP/MKDIR/CREATE/UNLINK/RMDIR/RENAME
的 nodeid 是父目录，不能建立目标映射。`nodeid=1` 是 FUSE 挂载根
`/root/glusterfs-client`，不是测试根，必须 out-of-root 丢弃；测试根
`/eifuzz_root` 才 strip 成 `""`。所有 path/path2（rename/link）都按
tracer spec §6.2 strip `eifuzz_root`；symlink 的 path2 payload 不 strip。

hardlink 访问（GETATTR/OPEN/WRITE 等）只带目标 inode 的 `nodeid`；事件 path
由 GlusterFS `inode_path` 选择，可能是 target，也可能是某个 alias。源码依据：

```text
fuse-helpers.c: fuse_loc_fill -> inode_path(inode, NULL)
fuse-resolve.c: loc_touchup -> inode_path(loc->inode, 0)
inode.c: inode_path -> __dentry_search_arbit
inode.c: inode_link -> list_add(head)
```

所以 v1 不把 `ev.Path` 当 hardlink 的权威路径，也不做 canonicalization；
非 LOOKUP 顶点用调用参数/fd provenance（§5.2），`ev.Path` 只做 fallback/
校验。P0 已实测 `truncate("q0")` 的事件 path 是 alias `h1`，但调用参数仍是
`q0`；planned linkpath 也可以作为 write/truncate 的调用参数，内核随机 alias
不是调用参数。事件 path 无论选到 target 还是 alias 都不影响 DAG 顶点路径。

symlink follow（`stat(L1)`，`L1 -> T`）由 VFS 先 `READLINK L1`，再按 target
继续 lookup；FUSE 的 GETATTR/OPEN 事件在 target `T` 上，alias `L1` 只出现在
READLINK/LOOKUP 事件里。`READLINK` 可能匹配不到原始 `stat` 调用（
`funcMatchesCall(FuncReadlink, "stat")` 为 false），不能把它强行归到 stat；
显式 `readlink` 调用才作为 READLINK 顶点。DAG 的 `stat(L1)` 顶点路径仍取
调用参数 `L1`，不取 FUSE GETATTR 的 `T`；`T` 事件保留为底层访问证据。

### 4.7 过滤

- 保留 `p=0` 过滤。
- 继续过滤：FORGET 2、FLUSH 25、INIT 26、RELEASEDIR 29、INTERRUPT 36、
  DESTROY 38、BATCH_FORGET 42。
- 不再整体过滤 GETXATTR；SETXATTR/GETXATTR 只保留 `user.*` key。
- LISTXATTR/REMOVEXATTR v1 继续过滤。
- 过滤虚拟 key：`glusterfs.*`、GFID、`inode-invalidate`、日志控制命令。
- 过滤 `security.*`、`trusted.*`、`system.*` 噪声，除非后续 profile 明确启用。
- `READDIR 28` 和 `READDIRPLUS 44` 都映射 ITERATE。

## 5. IPC 与 Go 数据路径

### 5.1 IPC

- `parseOutputs` 显式按 executor index 区分 server/client。
- server output 只解析控制信息；server fsMd 丢弃，不进入 client 一致性检查。
- client output 解析 `parseGlusterfsTraceEvents` + `parseFsMd` + testdirIno。
- `parseGlusterfsTraceEvents` 读取：

```text
ratio u64
offset u64
gap u64
count u32
events[count]
```

每个 event 按 §4.3 解析，严格检查长度；不足时保留已解析事件并停止。
- 两个 parse 入口（server/client）都按 profile 选择 parser。

### 5.2 Go 事件到 DAG

- `FuncID` 0–14 与 HMDFS 共用语义；GlusterFS 16–20 只在 GlusterFS profile 解释。
- `HmdfsFuncName` 扩展为 profile `EventNames`；HMDFS profile 只显示 0–15。
- `funcMatchesCall`、`callNameOfFunc`、`funcDefaultIsDir`、`isEntryInoFunc`、
  `isModifierFunc`、`isOffsetFunc` 改为 profile 方法或表。
- HMDFS 保留现有 `PathSameInode` 分类，`dagRelToDctRel` 继续把它折成
  `PathSame`。GlusterFS v1 设 `SupportsSameInode=false`：`determinePathRel`
  不比较 `Ino`，只做路径相等、父子、同父和 `PathNone`；不新增 `RelKind`；
  `comboKey` 不变，`cellKey` 只增加 `Role`（见 §6.3、§8.6.6）。
- GlusterFS parser 固定 `Ino=0`，`NodeID` 单独保存；不要把 `nodeid` 填进
  `Ino`，否则 `determinePathRel` 的 same-inode 分支和 delete timeline 会误用
  inode 指针。`isEntryInoFunc` 不为 link/symlink/unlink/rmdir/rename 新增
  条目。`RelHist` 在 GlusterFS 下不会产出 `PathSameInode`；HMDFS 不受影响。
- 文件 `RELEASE 18` 建 `FuncRelease` 顶点；close 不是 modifier，但可以作为
  HB/CC 的 B 侧。目录 `RELEASEDIR 29` 已过滤，不建顶点。
- hardlink 的事件 path 可能是 target/alias（P0 `truncate("q0")` 显示
  `h1`）；DAG 非 LOOKUP 顶点按调用参数/fd provenance，不据此新增
  `PathSameInode`。planned linkpath 可以作为 truncate/write 的调用参数。
- **事件实例匹配**继续用 `matchEventToCall`：FuncID 类型 + 调用
  `CheckInfo.Stime/Etime` + 事件 `[EntryTs, Timestamp]` + 容差，最近窗口优先。
  v1 不新增 `CallIdx`/语义顶点过滤层。
- **每个匹配到的事件都建顶点**；LOOKUP 也建；同一调用内部的事件之间也保留
  现有 pair 行为（v1 已知近似，先不额外过滤）。HMDFS 行为不变。
- **非 LOOKUP 顶点路径优先级**：
  1. 路径型调用的参数路径（`stat/open/creat/mkdir/rmdir/unlink/chmod/
     truncate/getxattr/setxattr` 等）；
  2. fd 类调用的 provenance：从同程序产生该 fd 的 `open/creat` 回溯路径，
     再按 open 之后发生的 rename 时间线修正；
  3. 以上都没有时用 `ev.Path` 做 fallback。
- **GlusterFS LOOKUP 顶点**：直接用 `ev.Path`（FUSE parent+name 的精确分量），
  正/负由 `Ret` 区分；rename 的源/目标用调用参数和事件 `Path/Path2` 区分，
  不走 HMDFS 的 ino 反查逻辑。HMDFS 继续用现有 `lookupPath`。
- 路径角色：
  - `link(old,new)`：`Path=new`（linkpath），`Path2=old`（target）；
  - `symlink(target,linkpath)`：`Path=linkpath`，`Path2=target`；
  - `readlink(linkpath)`：`Path=linkpath`；
  - `rename(src,dst)`：`OldPath=src`，`Path=dst`。
- `isPathResolvingCall` 补上 `link/symlink/readlink/getxattr/setxattr`，与 §5.3
  的 22-call 表一致。
- `ev.Path` 与调用参数/fd provenance 不一致时，以调用侧为准，并计入
  path-mismatch 诊断；LOOKUP/无调用事件才以 `ev.Path` 为权威。

### 5.3 Call → Event matcher（GlusterFS v1）

只覆盖 §1.1 的 22 个调用；未启用调用不补 matcher 规则。

| 调用 | Event | 备注 |
|---|---|---|
| `mkdir` | MKDIR | |
| `creat` | CREATE | GlusterFS 只映射 CREATE，不映射 FILE_OPEN |
| `open(O_CREAT)` | CREATE | FUSE CREATE 35 回复带 fd，没有独立 OPEN |
| `open(O_DIRECTORY)` | DIR_OPEN | flags-aware |
| `open`（普通） | FILE_OPEN | 不带 O_CREAT/O_DIRECTORY |
| `rmdir` | RMDIR | |
| `unlink` | UNLINK | |
| `rename` | RENAME | `renameat2` 不支持；v1 不启用 `renameat` |
| `link` | LINK | `Path`=linkpath，`Path2`=target |
| `symlink` | SYMLINK | `Path`=linkpath，`Path2`=target 字符串 |
| `readlink` | READLINK | `Path`=linkpath |
| `stat` | GETATTR | `Ino` 只诊断 |
| `chmod` | SETATTR | |
| `truncate` | SETATTR | |
| `read` / `pread64` | READ | |
| `write` / `pwrite64` | WRITE | |
| `fsync` / `fdatasync` | FSYNC | |
| `close` | RELEASE | 文件 close 有 D/S、无 R；目录 close 是 RELEASEDIR 29，v1 过滤 |
| `getdents64` | ITERATE | READDIR 28 / READDIRPLUS 44 都映射 |
| `getxattr` | GETXATTR | 只保留 `user.*` |
| `setxattr` | SETXATTR | 只保留 `user.*` |

规则：

- `creat` 和 `open(O_CREAT)` 在 GlusterFS 只产生 CREATE；不能照搬 HMDFS 的
  `CREATE + FILE_OPEN`。
- `open` 必须按 flags 判断 CREATE/DIR_OPEN/FILE_OPEN。
- `LOOKUP` 是路径解析产生的通用事件，不专属某个调用；`isPathResolvingCall`
  覆盖这 22 个路径调用，并补上 `link/symlink/readlink/getxattr/setxattr`。
- GlusterFS 的 LOOKUP 顶点直接用事件 `Path`（父目录+name 的精确分量）；
  HMDFS 继续用 `lookupPath` 的 inode 反查。LOOKUP 顶点也参与 DAG/coverage，
  v1 不做同调用过滤。
- `getdents64` 的 READDIR 28 与 READDIRPLUS 44 都归 ITERATE。
- 未启用调用（`openat/chown/sendfile/fcntl/...`）没有 matcher 规则；config
  出现时由 profile 启动校验拒绝或标记 ignored。

#### 5.3.1 定向/coverage 的 Call → FuncClass

`callFuncClass`/`callFuncClasses` 是 directed shape guard 和 coverage shape
guard 使用的正向映射；它和 `funcMatchesCall` 的反向匹配必须同源，但不能
直接复用 HMDFS 的硬编码。

接口改为 profile 方法：

```go
CallFuncClasses(call string, isDir bool, flags uint64) []uint32
```

- directed plan 有真实 `Call` 时传实际 flags；
- coverage board 只有 call name/isDir、没有 flags 时，`open` 返回保守集合
  `{CREATE, FILE_OPEN, DIR_OPEN}`；其他调用按表返回；
- `predictedEventFuncs` 继续：`ENOENT + HB` 只返回 `{LOOKUP}`，
  `ENOENT + CC` 返回 `{LOOKUP} ∪ op funcs`，两个 profile 相同；
- `funcMatchesCall` 反向匹配仍按 call name；GlusterFS 的 CREATE/FILE_OPEN
  事件类型已经足以区分，不要求在反向路径读 flags；
- HMDFS 保持现有 mapping：`creat -> {CREATE, FILE_OPEN}`，`open` 按现有
  行为处理；
- GlusterFS：
  - `creat -> {CREATE}`；
  - `open(O_CREAT) -> {CREATE}`；
  - `open(O_DIRECTORY) -> {DIR_OPEN}`；
  - 普通 `open -> {FILE_OPEN}`；
  - `link/symlink/readlink/getxattr/setxattr` 分别映射 16–20。
- `randOpenFlags` 在 write-capable 时有 15% 带 `O_CREAT`，所以不能假设
  directed open 永远无 O_CREAT；shape guard 必须按 flags 或保守集合处理。

其他老调用映射：

```text
mkdir -> MKDIR
rmdir -> RMDIR
unlink -> UNLINK
rename -> RENAME
stat -> GETATTR
chmod/truncate -> SETATTR
read/pread64 -> READ
write/pwrite64 -> WRITE
fsync/fdatasync -> FSYNC
close -> RELEASE（仅文件；目录 RELEASEDIR 29 过滤）
getdents64 -> ITERATE
```

`close` 不进入 DCT/定向/causal 的 root/variant；它只作为
open/read/write/getdents 的配套调用。文件 close 的 RELEASE 事件仍参与
DAG pair（非 modifier B/CC）；目录 close 不产生事件。

## 6. 双路径关系模型

### 6.1 核心规则

DAG 和定向构建不引入完整 role-pair 矩阵。每个**生成的 pair** 的每一侧只选一条
关系路径；link/symlink 作为 HB A 时生成两个 pair，分别选 target 和 linkpath：

| 调用 | HB 前操作 A | HB 后操作 B | CC |
|---|---|---|---|
| rename | `dst` | `src` | `src` |
| link | `target` 或 `linkpath` | `target` | `target` |
| symlink | `target` 或 `linkpath` | `target` | `target` |
| readlink | 不允许 | `linkpath`，self | 不作为定向候选 |
| 单路径调用 | primary | primary | primary |

- link/symlink 作为 HB A：
  - DAG 两条路径各生成一个 pair，都统计；GlusterFS v1 不区分
    `PathSameInode`，两个 pair 只按路径相等/父子/同父/None 分类；
  - 同一事件对的两个 role pair 若落到同一 cell，由覆盖板的 `cellKey` 去重；
  - C1 anchor 和 normal dual 都分别枚举 target/linkpath 两个 role，不是二选一
    丢弃；target-role 计 CC+HB，linkpath-role 只计 HB。
- link/symlink 作为 HB B/CC：用 target；linkpath 自由。
- readlink：
  - 定向只允许 HB B；
  - DAG 若出现 CC，用 linkpath 记录反馈，但不去向；
  - A 必须是同一 linkpath 上的 modifier（实际成功形态主要是 symlink）。
- 自由路径由 `RootPaths`/`RenamePolicy`/`freshLinkPathsBefore` 解析，必须满足
  profile：
  - HMDFS rename same-parent；
  - GlusterFS rename 可跨目录；
  - link/symlink 的 fresh/existing、类型、父目录存在；
  - linkpath follow/nofollow；
  - **不能对 linkpath 调 `FileTree.FindNode`**；linkpath 只来自 program-local
    `RootLinkPath`/`freshLinks`。

### 6.2 linkpath 访问

```go
type AccessMode uint8
const (
    AccessFollow
    AccessNoFollow
    AccessCreate
    AccessPayload
)
```

| 调用 / 角色 | AccessMode | 说明 |
|---|---|---|
| `readlink(linkpath)` | NoFollow | 只能读 symlink 对象 |
| `unlink(linkpath)` | NoFollow | 删除 symlink entry 本身 |
| `rename(src,dst)` | NoFollow | 移动/替换 symlink entry，不跟随 |
| `link(old,new)` 的 `old` | NoFollow | v1 限普通文件 |
| `link(old,new)` 的 `new` | Create | 新 hardlink path 必须 fresh |
| `symlink(target,new)` 的 `target` | Payload | 只保存 target 字符串 |
| `symlink(target,new)` 的 `new` | Create | 新 symlink path 必须 fresh |
| `stat(path)` | Follow | 默认跟随 |
| `open` / `creat` | Follow | v1 不生成 `O_NOFOLLOW` |
| `read/pread64/write/pwrite64` | fd 已解析 | open 时已指向 target |
| `truncate(path)` | Follow | 截断 target |
| `chmod(path)` | Follow | 修改 target 属性 |
| `fsync/fdatasync` | fd 已解析 | open 时已指向 target |
| `getxattr/setxattr` | Follow | 操作 target xattr |
| `getdents64` | Follow | 目标必须是目录 |

- target 是目录时，只允许目录兼容调用（`open(O_DIRECTORY)`、`getdents64`、
  `stat`、`chmod`、xattr）；target 是普通文件时，只允许文件兼容调用
  （`open`、`read/pread64`、`write/pwrite64`、`truncate`、`chmod`、xattr）。
- hardlink 的 linkpath 与 target 同 inode、同类型；它不是 symlink，
  `readlink(linkpath)` 不成立。
- v1 不启用 `lstat`/`lgetxattr`/`lsetxattr`/`lchown`，因此没有额外的
  NoFollow 生成器；NoFollow 形态只由 `readlink`/`unlink`/`rename` 覆盖。

### 6.3 数据模型

- `DAGVertex` 保留 `Path`，增加角色化备选路径；`OldPath` 只做 rename 兼容。
- `DAGPair` 用 role-selected `APath/BPath` + `APathType/BPathType` 记录 role；
  `PathRel` 仍只有一个。
- `CallVariant` 不加 role；DCT 表仍是 root/variant/relation。双路径生成和
  planner 使用 §8.5.6 的 `DualPathKind`/`RootPaths`，不进入 `CallVariant`。
- **`comboKey` 不增加 role/RelKind**；`cellKey` 增加 `Role`，只区分
  link/symlink A 侧的 target/linkpath role。target/普通 cell 用零值，
  linkpath 用显式 role；legacy 键保持 `(Root, Variant, Rel, ...)` 的零 role
  行为。`cellKey` 不增加 `RelKind`/HardlinkGroupKey。
- `directedPlan` 用 `BaseA/BaseA2/BaseB/BaseB2` 保存构造路径；link/symlink
  root 的 program-local linkpath 用 `RootLinkPath`，cell/relation 用的 A 侧
  路径用 `RootRelationPath`（空值表示 `BaseA`）。`FreshA/FreshA2/FreshB/
  FreshSame` 标记 batch 物化时生成的 fresh 路径。
- `planStateEffect` 返回多路径 effect；authority 按路径 + `HardlinkGroupKey`。
- **GlusterFS v1 不做 `PathSameInode`**；HMDFS 保留现有分类并折叠为
  `PathSame`。GlusterFS 的 `Ino` 不进入 path relation。
- `HardlinkGroupKey` 是 program-local binding，不来自 FileTree：
  - 只对 `link` 生效；`symlink` 不建组；
  - v1 不允许嵌套链接，group key = link target 的 canonical 测试根路径；
    target 由 anchor 关系或 baseline 选出，key 从当前 Link call 重算，不持久化
    旧 baseline；
  - `target` 和 `linkpath` 共享同一个 group；
  - `write`/`truncate` 通过任一 alias 修改，按组级 size effect 处理；
  - planned linkpath 是 canonical binding，可以作为 write/truncate 的调用
    参数；FUSE `inode_path` 任选的 dentry 不是调用参数，DAG 不能据此
    canonicalize；
  - `unlink`/`rename` 只改变被操作路径的存在性，不把整个组标成 destroyed；
  - 只用于 directed/C2 batch 的 authority，不进 DCT/cellKey/checker；
  - `Nlink` 不进 cell、不做预测，仍只做 B 类诊断。

### 6.4 定向生成

- 先按 §6.1 选 root/variant 的关系角色。
- link/symlink 作为 HB A 时，target/linkpath 两个 role 都枚举；C1 在所有
  C1 cell 里随机选一个，C2/normal dual 分别生成 target-role 和 linkpath-role
  plan。
- readlink 只做 HB B self 形态；A 选 linkpath role。
- 路径解析使用 `RootPaths`/`DualPathKind`；rename destination 走
  `RenamePolicy`，fresh rename destination 仍由 `freshRenameDst` 物化。
- `insertCallFromDCT`/`insertDerivedCallFromDCT` 使用 `dctBaseCandidatesFor`、
  `freshLinkPathsBefore` 和 `RootPaths`；link/symlink 的 linkpath 是
  program-local fresh binding，不进入 FileTree。

### 6.5 RenamePolicy（v1）

| 字段 | HMDFS | GlusterFS |
|---|---|---|
| `SameParentOnly` | true | false |
| `SrcMustExist` | true | true |
| `DstMode` | 保持现状（same-parent；fresh/replace 按现有规则） | Fresh + existing replace |
| `AllowFileRename` | true | true |
| `AllowDirRename` | true | true |
| `AllowedRels` | 保持现有 | Same/Parent/Child/Sibling/NoRel |

GlusterFS 规则：

- source 可以是文件或目录；destination 可以在同一父目录，也可以在另一个
  已存在父目录；
- destination 支持 fresh 名字，也支持已有同类目标替换；
- source/destination 不能是同一路径；
- 目录不能 rename 到自己的后代；
- destination 父目录必须存在；
- `file -> dir` 返回 EISDIR，`dir -> file` 返回 ENOTDIR；
- `dir -> 非空目录` 返回 ENOTEMPTY，`dir -> 空目录` 成功；
- source 缺失返回 ENOENT；
- v1 只生成 `rename(src,dst)`；不启用 `renameat`（包括 flags=0），也不生成
  `renameat2`。

关系与可见性：

| rename 角色 | 路径 |
|---|---|
| HB 的 A | `dst` |
| HB 的 B | `src` |
| CC 任一侧 | `src` |

- 同节点 consumer：按程序调用顺序判断 dst 是否可见。
- 跨节点 consumer：用归一化 `CheckInfo.Stime/Etime`；只有
  `rename.Etime < consumer.Stime` 才规划 HB success。
- DCT seed 没有时序证据，rename 作为 root 时 destination 是自由角色，
  按 `RenamePolicy` 选 cross-parent/fresh/replace；实际结果按 CC/自然结果记录。
- DCT mutation 有 `refTime`：HB A 用 `dst`，HB B 用 `src`，CC 用 `src`。
- Directed 继续显式规划 `RetB`；跨目录只扩大合法路径集合，不改变关系角色规则。
- PathSet broad 的 rename 迁移必须调用同一 `RenamePolicy`：mapped existing dst
  优先，其他保持关系的 existing dst 次之，fresh dst 兜底；不能先 generic map
  再验证。细节见 §8.7.3。

## 7. 生成参数

| 调用 | 非路径参数 | v1 规则 |
|---|---|---|
| `link(old,new)` | 无 | old 是测试根内普通文件：baseline existing，或同一程序/plan 中更早 creator 产生、在 link 执行时存在的 fresh 路径；new fresh 且可在任意已有父目录；禁止 old=new |
| `symlink(target,linkpath)` | target 字符串 | target 是测试根内文件/目录的 canonical 路径：baseline existing，或同一程序/plan 中更早 creator 产生、在 symlink 执行时存在的 fresh 路径；linkpath fresh 且可在任意已有父目录；禁止 target=linkpath |
| `readlink(path,buf,size)` | buf out、size | size=4096 |
| `setxattr(path,name,val,size,flags)` | name/value/size/flags | key=`user.eifuzz`；value 0/16/64/256；flags 0 / XATTR_CREATE / XATTR_REPLACE |
| `getxattr(path,name,buf,size)` | name/buf/size | key=`user.eifuzz`；size 0/16/256/4096 |

- xattr 只用 `user.*`；不使用 `security.*`/`trusted.*`/`system.*`。
- flags 常量缺失时回退 0。
- setxattr 作为 root、getxattr 作为 variant 才能覆盖成功路径。
- symlink 作为 root、readlink 作为 HB B 覆盖 linkpath self。
- link 作为 root，后续写/stat 打 target 或 linkpath，分别覆盖 target 路径和
  linkpath 路径的 `PathSame`/几何关系；GlusterFS v1 不引入 `PathSameInode`。
- link/symlink 的 linkpath 是 program-local fresh binding，不写入 FileTree；
  target 只从测试根内选。`HardlinkGroupKey` 按 §6.3、§8.6.4 处理。
- **所有调用路径参数统一是相对测试根的 canonical 路径**，不生成绝对路径。
- `symlink` 是唯一需要在调用物化时转换的参数：target 在 `RootPaths`/DCT/
  PathSet 中保存 canonical 路径，构造 `symlink` 时转换为相对 linkpath 父目录
  的 payload：
  ```text
  canonical target = a/b
  linkpath         = sub/link
  实际调用          = symlink("../a/b", "sub/link")
  ```
- payload 可以包含 `..`，但 `clean(dirname(linkpath) + "/" + payload)` 必须仍在
  测试根内；target 不允许指向测试根外，也不允许绝对 target。
- `link` 的 oldpath 是普通路径参数，按 cwd/test root 解析，不做 payload 转换。
- `readlink` 返回 payload；DAG/checker 必须用 `linkpath + payload` 还原
  canonical target，不能把 payload 直接当 FileTree 路径。

## 8. DCT、覆盖板与 DAG

### 8.1 Profile 化

- `FsopsSubCalls`、`FsopsChoiceCalls`、`DirOnlyCalls`、`FileOnlyCalls`、
  `FileOrDirCalls`、`FdRequiredCalls` 改为 profile 表。
- `BuildDefaultDCT(profile)` 替代全局 `initDefaultConfig`；HMDFS 表内容不变。
- `directedRootCalls`、`directedVariantCalls`、`causalRootCalls` 改为 profile 表。
- `directedVariantKinds`、`variantKindAllowed`、`planStateEffect`、
  `freshSameForms`、`offsetVariants`、`fdHBVariants` 改为 profile 方法/表。
- `RenamePolicy`、`DualPathKind`、`RootPaths` 放入 profile/planner；不进入
  `CallVariant`/coverage key。
- `callNameConstraint`、`isFileOnlyCallByName`、`isDirOnlyCall`、
  `basePathIsFile`、`resolveFdTarget`、`ownerPathConstraint`、`isModifierCall`
  改为 profile 方法/表；新调用不能只靠 HMDFS 全局列表判类型。
- `PathSet` 的 prefix/depth/mapping 使用 `FileTree.Prefix`/`RootPrefix`；
  `normalizePathSetPath`/`pathSetTopRoot`/`pathSetTopPath`/`mapPathSetPath`/
  `pathSetTargetLevel` 不得硬编码 `MergeViewPrefix`。
- 覆盖板每个 run/profile 一个实例，不再使用全局 `covBoard` 单例。

### 8.2 GlusterFS v1

- root 19 个、variant 21 个；完整列表和类型/权重见 §8.5.2–§8.5.3。
- `setxattr` 可作为 root/variant；`getxattr` 只作为 variant。
- `readlink` 只作为 variant，跟在 symlink root 后；不能做 root。
- `link/symlink` 先做 seed root；derived DCT 共用 §8.5.6 的 `RootPaths`；
  causal C1 和 normal directed dual root 的 link/symlink 插入都由
  `DualPathCausalEnabled` 控制；GlusterFS 默认开启，HMDFS 保持 false。
- GlusterFS 允许跨目录 rename；HMDFS 保持 same-parent。
- **GlusterFS v1 不做 `PathSameInode`**：DAG 只按路径相等/几何关系；
  HMDFS 无 hardlink，保留现有分类并继续折叠。
- DAG 只消费 client events；过滤 `ev.ProgIdx < ServNum`。
- coverage pending 用 client view；ProgIdx 诊断保留绝对索引。
- `DepthBucketOf`/namespace root 按 profile；GlusterFS 根为空。
- `readv/writev/preadv/pwritev` 不在 22-call config 中，也没有 alias 映射；
  结构化生成器只认 `read/pread64/write/pwrite64`。

### 8.2.1 Coverage 三层

覆盖板拆成三层，禁止混用：

- **observed**：`discovered` + `knownShapes`。每个首次观察到的 cell/shape
  都记录，包括模型外返回值；这一层不受 model/plannable 的 gate 控制。
- **model**：`producible` + `found`。静态自然可达性估计，只用于
  `model=` / `outModel=` 等有界诊断，不是完整性声明，也不驱动选择。
- **plannable**：`plannable` + `plannableFound`。generic directed-pair
  构造层：FailurePolicy 和 directed pair（root 或 anchor）主动构造的形态；
  Direction-3（2a variant 偏置、2b exact plan、mutation offset/depth 偏置）
  只消费这一层。`comboKey.Root` 是 A 侧 call 名，不区分 root/anchor。
  注意：plannable 是 variant-form 静态上界，不等同于当前 planner 的精确
  构造能力；非 creation-root 的 context-dependent 形态仍可能有 phantom cell，
  见 §13.1 已知边界。
- **root-directed**：`rootDirected*` 从 plannable 里按
  `profile.DirectedRootCalls()` 快照再过滤一层；过滤是精确 call-name 匹配
  （`profileExactListContains`），不是子串/前缀匹配，`open$dir` 等变体不会
  被当成 `open` root。`rootDirectedPotentials` 和 `RootPlannableTarget`
  属于这一层。rmdir-root 是 model-only：DCT root、C2 variant、C1 anchor
  支持，但不是 C1/normal directed root。

指标统一由 `CoverageLayerStats()` 提供，日志 `dct pool:` 使用：

```text
obs        = len(discovered)                  // observed cell
outModel   = observed - modelFound             // 模型外观察
modelOnly  = modelFound - plannableFound       // generic 层：自然可达但 planner 不构造
model      = modelFound/modelTarget            // model 层覆盖
plan       = plannableFound/plannableTarget    // generic directed-pair 层覆盖
rootPlan   = rootPlannableTarget               // root-directed 子集 target
shapes     = len(knownShapes)                  // observed event shape
```

`CoverageStats()` 仍返回 model 层的 `(found, target)`，仅用于兼容旧测试/调用；
HMDFS 的 generic `plannable` 恒等于 model，保持旧行为；其
`RootPlannableTarget` 仍按 HMDFS effective root list 过滤。

### 8.2.2 Normal dual root

normal dual root 是 **normal directed planner 的一个 pass**，不是独立的
producer，也不是 C1 的降级实现。调用链：

```text
PickDirectedBatch
  ├─ pickCausalRootBatch              // C1/C2；能产出则直接返回
  └─ pickDirectedBatchNormal
       └─ planDirectedCellsScan
            ├─ main loop（含 synthesized FreshB B nodes）
            ├─ FreshA
            ├─ FreshA2
            ├─ FreshSame
            └─ normal dual-root pass
```

实现规则：

- 普通 pass 的 root 列表排除 `link/symlink`；
- dual pass 对每个 target/root 生成一个 fresh `RootLinkPath`，并分别以
  target/linkpath 作为 `RootRelationPath` 调用
  `planCausalC2CellsWithParent(..., rootKind=causalRootNormal, ...)`；
  target-role 生成 CC+HB，linkpath-role 只生成 HB（pair extractor 只在
  link/symlink 是 HB A 侧时枚举 linkpath）；
- `BaseA` 是 target；`RootLinkPath` 是 linkpath；cell/relation 用
  `RootRelationPath`（target 或 linkpath），它不进入 `cellKey/comboKey`；
- target-role 和 linkpath-role plan 可以共享同一个 root execution 和 batch；
  `pickDirectedBatchNormal` 仍按 `RootLinkPath` 分组，不按角色拆 group；
- `insertDirectedBatch` 对 `Anchor==nil` 的 link/symlink root 用
  `causalRootCallsSeq` materialize target + linkpath；
- normal dual plan 标记 `Causal=true` 只是为了复用 `validateCausalPlan` 对
  `RootLinkPath`/readlink 的校验，不表示它来自 C1；
- 由 `DualPathCausalEnabled` gate。GlusterFS 默认 true，causal C1 和 normal
  dual root 都可用；gate=false 只保留为 A/B 对照，此时 plannable 与 planner
  同步排除 link 系。

### 8.2.3 FreshSame 竞赛矩阵与 combo-aware plannable

单操作 FailurePolicy 仍是独立通道，只主动构造：

```text
rmdir ENOTEMPTY
rename ENOTEMPTY
getxattr ENODATA
setxattr EEXIST
```

普通已有对象上的 `mkdir EEXIST`、`creat EISDIR`、`open/stat/... ENOENT`、
`unlink/rmdir/rename ENOENT` 等不主动构造。

fresh-fresh 竞赛使用完整 `freshSameForms` 矩阵，表示 root/variant 在同一
fresh 路径上的并发结果，不是单操作失败注入：

```text
creat/creat Success
mkdir/mkdir EEXIST
creat/mkdir EEXIST
mkdir/creat Failure
creat/unlink Success/ENOENT
mkdir/rmdir Success/ENOENT
creat/stat Success/ENOENT
mkdir/stat Success/ENOENT
creat/open Success/ENOENT
mkdir/open Success/ENOENT
creat/chmod Success/ENOENT
mkdir/chmod Success/ENOENT
creat/read Success(offset 0)
creat/write Success(offset 0)
mkdir/getdents64 Success
```

GlusterFS 的 plannable 因此是 combo-aware 的：

```text
Rel=Same 且 root ∈ {mkdir, creat}:
    使用 freshSameForms 中该 (root, variant) 的形态；
    这是 creation-root Same 的唯一构造通道。
其它 combo:
    使用 directedVariantKinds 的单操作形态。
```

`mkdir EEXIST` 只在 `creat/mkdir Same`、`mkdir/mkdir Same` 这类竞赛 combo 中
plannable；普通已有目录上的 `mkdir EEXIST` 仍不主动构造。race 中自然可达但
单操作 model 未列出的形态（如 `mkdir/rmdir Same ENOENT`、`creat/unlink Same
ENOENT`）只在对应 creation-root Same combo 的 model/plannable 中扩展，不污染
其它 combo，从而保持 `plannable ⊆ model`。

`planDirectedCellsScan` 的 `add` 检查是最后一道不变量：任何 normal plan
进入 batch 前都必须落在 combo-aware plannable 内。HMDFS 的 plannable 恒等于
model，保持旧行为；GlusterFS 默认 `DualPathCausalEnabled=true`，
`link/symlink/readlink` 进入 plannable。gate=false 的 A/B 配置下 link 系在
plannable 与 planner 中同步排除；非 link 的 context-dependent 形态仍按
§13.1 已知边界处理，不能据此认为不存在 plannable/planner 错配。

### 8.2.4 DAG feedback

DAG feedback（board learning + DAG corpus/triage）对 HMDFS/GlusterFS 都启用。
DAG 只消费 client events，`directDagTriage` 的 owner 是第一个非 nil client，
`subNum` 使用绝对 executor index。

corpus 侧沿用全局机制，不做 per-DFS 特判：

- `addInputToCorpus` 按 hash 去重，只有真正新增才增加 `dagCorpusEntries`；
- `MaxDagCorpus` 在 DAG triage 前做硬 gate（0 = unlimited，长跑按规模配置）；
- `minimizeCorpus`/`signal.Minimize` 负责修剪并保留 DAG 条目；
- `saturatedCalls["dag"]` 阻止饱和调用继续吃 corpus；
- `WorkSmash` 每次固定 `smashIterations`。

D2/D3 观察 `dagCorpusEntries`、`dagCorpusCount`、`MaxDagCorpus`、`dag`
saturation 和 smash 执行预算。

### 8.3 接入顺序

1. 先做 GlusterFS trace/parser，确认 link/symlink/readlink/xattr 事件。
2. 再做 DAG/coverage 的 role/path 映射。
3. 最后启用 GlusterFS DCT/定向；没有 trace 时 planner 不启用，避免 phantom cell。

### 8.4 DCT seed、readlink 与可见性

- **DCT seed 没有显式 Success/RetB 规划**：只选择 root call、variant call、
  path relation（以及 role/binding），不写 `RetA/RetB` 目标。
- DCT seed 的 `readlink`：
  - 不能作为 root（baseline 没有 symlink）；
  - 只允许作为 `symlink` root 的 variant；
  - relation 只取 `PathSame`；
  - 实际 `Success` 和 `ENOENT` 都是自然结果，DCT 不预设。
- DCT mutation 有 `refTime`/`ChooseTemporal`：
  - HB 可以把 symlink 排在 readlink 前，形成可证明的可见性；
  - 仍没有显式 `RetB` 目标；CC 只保留竞态/ENOENT 形态。
- Directed 显式规划 `RetB`；`readlink` 只做 HB B self 形态。
- 跨节点可见性（link/symlink/readlink/rename 新路径同理）：
  - 同节点按程序调用顺序判断；
  - 跨节点用归一化 `CheckInfo.Stime/Etime`；
  - 只有 `creator.Etime < consumer.Stime` 才规划 HB success；
  - 时间窗重叠按 CC/竞态处理，Success/ENOENT 都合法；
  - 任一方没有 `CheckInfo`、或 consumer 在前：不规划 success；
  - DCT seed 没有时序证据，路径管理只保证逻辑存在/fresh，不保证跨节点可见；
    跨节点 success 留给 DCT mutation 的 HB 或 directed。
- DAG 反馈不依赖静态预测：它按实际事件时间/RetBucket 记录 pair；计划侧才使用
  上述 `VisibilityPolicy`。

### 8.5 DCT 表与双路径生成（GlusterFS v1）

#### 8.5.1 表结构

`DistributedChoiceTable` / `CallVariant` 结构保持不变：

```go
type DistributedChoiceTable struct {
    RootCalls map[string]bool
    Variants  map[string][]CallVariant
    Weights   map[string]map[CallVariant]int
}

type CallVariant struct {
    CallName     string
    PathRelation PathRelation
}
```

- 不给 `CallVariant` 加 role、rename destination 形态或 `RelKind`；
- 不改 `Weights` key、`CallVariant.String()`、`SortCallVariants`、`comboKey`；
  `cellKey` 只增加 `Role`（见 §8.6.6），其余维度不变；
- 新增 profile-specific `BuildDefaultDCT(profile)`；
- HMDFS 保持现有 DCT 表和行为不变。

#### 8.5.2 root / variant

GlusterFS root（19）：

```text
open read write pread64 pwrite64 fsync fdatasync truncate
mkdir rmdir creat unlink rename chmod stat getdents64
link symlink setxattr
```
`close` 只作为 root-choice bias 源，不作为 root 结果；`GlusterfsSubCalls`
的 22 个允许值含 close，但 `RootCalls`/`VariantCalls`/`CausalRootCalls` 都不含。
文件 `RELEASE 18` 仍产生 `FuncRelease` DAG 顶点；目录 `RELEASEDIR 29` 过滤。
coverage board 的 root/variant 网格用 19/21 列表，不用 22 个 SubCalls，避免
不可生产的 close-root cell。

GlusterFS variant（21）：上述 root 集合加 `getxattr/readlink`；其中 `readlink`
只挂在 `root=symlink` 下，relation 只取 `PathSame`，不能做 root。
`getxattr` 只做 variant/observer，不做 root。

#### 8.5.3 类型与权重

类型约束：

```text
link          file-only（old target 必须是普通文件）
symlink       target 可 file/dir；linkpath 是 fresh symlink entry
readlink      只接受 symlink linkpath
getxattr      file-or-dir
setxattr      file-or-dir
```

权重分类：

```text
namespace:    mkdir rmdir creat unlink rename link symlink
data-mod:     write pwrite64 truncate fsync fdatasync setxattr
data-observer: read pread64 stat getdents64 chmod readlink getxattr
open:         保持特殊系数 120
```

冲突系数沿用现有公式；same-call 150，namespace×namespace 150，
namespace×data-mod 120，namespace×observer 100，data-mod×data-mod 120，
data-mod×observer 120，observer×observer 50。

#### 8.5.4 DCT seed

- 没有 p0 已有路径选择；root basePath 从 FileTree 选，variants 从 root anchor
  派生。
- `link` root：

```text
Target   = FileTree 普通文件
Linkpath = fresh
Anchor   = Target
root call: link(Target, Linkpath)
```

- `symlink` root：

```text
Target   = FileTree 文件/目录
Linkpath = fresh
Anchor   = Target
root call: symlink(Target, Linkpath)
```

- variants 默认从 `Target` 派生；`readlink` variant 特殊使用 root `Linkpath`。
- `rename` root：anchor=`src`，destination 由 `RenamePolicy` 选；DCT 表项不
  编码 dst 形态。
- DCT seed 不预设 Success/RetB；实际 Success/ENOENT 由执行结果记录。
- `close` 不进入 DCT seed 的 root/variant；只在 open/read/write/getdents
  序列尾部生成。文件 close 的 RELEASE 事件仍参与 DAG pair。

#### 8.5.5 DCT mutation

`insertCallFromDCT` 从 p0 已有调用选 root base。下面是 **新 root call 的
basePath 选择**，不是 DAG pair 的 role 规则。

```text
rename:
  选中调用在 insertPos 之前（已执行）-> base=dst
  选中调用在 insertPos 之后（未执行）-> base=src

link:
  选中调用在 insertPos 之前且成功 -> target / linkpath 随机
  选中调用在 insertPos 之后 -> target

symlink:
  选中调用在 insertPos 之前且成功 -> target / linkpath 随机，
    按新调用 AccessMode/类型过滤
  选中调用在 insertPos 之后 -> target

readlink:
  只能 linkpath，且 symlink 必须在 insertPos 之前
```

- 新 root 自己就是 link/symlink：`Target=basePath`、`Linkpath=fresh`，
  不参与 target/linkpath 随机。
- variant 跟随同一 root base 派生；`readlink` 例外，始终用 root symlink 的
  linkpath。
- FileTree 不建模链接，linkpath 是 fresh base：`PathSame` 返回 linkpath，
  `PathParent` 返回父目录，`PathSibling` 从父目录已有子节点选，
  `PathNoRel` 选无关路径，`PathChild` 为空。
- `close` 不进入 DCT mutation 的 root 选择；仅在 open/read/write/getdents
  序列尾部生成。目录 close 的 RELEASEDIR 不产生 trace 事件。

#### 8.5.6 统一双路径抽象：DualPathKind / RootPaths

只用于生成器/planner，不进入 DCT key / coverage：

```go
type DualPathKind uint8
const (
    DualNone DualPathKind = iota
    DualRename
    DualLink
    DualSymlink
    DualReadlink
)

type RootPaths struct {
    Kind         DualPathKind
    PrePath      string // rename src / link target / symlink target
    PostPath     string // rename dst / link linkpath / symlink linkpath
    TargetPath   string
    LinkPath     string
    ReadlinkPath string
}
```

`RootSource/RootDest` 兼容投影：

```text
rename:        Source=src,     Dest=dst
link:          Source=target,  Dest=linkpath
symlink:       Source=target,  Dest=linkpath
readlink:      Source=Dest=linkpath
```

DCT/derived 的 observed path 按 temporal 选：CC 取 `Source`（rename=src），
HB 取 `Dest`（rename=dst）；非 rename root 取 `EntryPath`
（link/symlink=target，readlink=linkpath）。`ObservedPath` 仅保留为 deprecated
兼容别名，不能用于 rename 的 HB 观察路径。derived DCT、causal root/directed
共用这套 `RootPaths`，不另写双路径逻辑。
`RootPaths` 中的 target/linkpath 都是相对测试根的 canonical 路径；symlink
payload 只在构造 `symlink` 调用时由 `dirname(linkpath)` 到 canonical target
计算，不能存进 `RootPaths`/DCT/PathSet key。
causal root 的 link/symlink 插入由 profile gate（如
`DualPathCausalEnabled`）控制；GlusterFS 默认开启，HMDFS 保持 false。

#### 8.5.7 实现落点

1. DCT base 选择：
   - `dctBaseCandidatesFor(profile, p0, insertPos)` 收集 p0 中可作为新 root
     base 的路径；GlusterFS 对 rename/link/symlink/readlink 按 before/success
     规则过滤；
   - `freshPathKindsBefore` 记录 p0 中更早的 mkdir/creat 产生的 fresh path
     类型；
   - `freshLinkPathsBefore` 记录更早 link/symlink 的 program-local linkpath
     （`linkPathHardlink`/`linkPathSymlink`、`FollowIsDir`、`Target`、
     `CreatorIdx`）；
   - `rootBaseConstraintFor` 按 root call 的类型约束过滤候选；link/symlink
     target 不能是另一个 linkpath。
2. derived/causal：
   - `AnchorPathKind` 增加 `AnchorPathFreshSymlink`；
   - `anchorPathInfo`/`seedAnchor` 增加 `FollowIsDir`；
   - `collectSeedAnchors` 收集 target/linkpath 两个 role；
   - `deriveSeedRootPath` 增加 link/symlink 分支；
   - `insertDerivedCallFromDCT` 使用 `seedDerivation`、`RootPaths`、
     freshLinks 和 readlink 特例。
3. RenamePolicy：
   - profile hook `pickRenameDstPolicy` 已落地（`pickRenameDstFor` 调用）；
     HMDFS 保持 same-parent，GlusterFS 负责 same/cross-parent、fresh/replace、
     类型/非空检查；
   - DCT seed/mutation/derived 的 rename dst 都走该 hook；directed 复用。
4. coverage：
   - `DagPairToVariant` 不变；`comboKey` 不变；`cellKey` 只增加 `Role`；
   - link/symlink 两个 role pair 仍只产出 `CallVariant{CallName, PathRelation}`；
   - 同 relation 落同一 cell 时由 cellKey 去重，不同 relation 按现有逻辑记录；
   - readlink 非 symlink root 组合由 DCT producible/cellInModel 过滤；
   - 不把 role/RelKind 写进 comboKey。

#### 8.5.8 与 causal 的衔接

DCT 表/生成规则到此冻结。causal root/directed 的 C1/C2 规则见 §8.6；
两者共享 §8.5.6 的 `DualPathKind`/`RootPaths`，不另写双路径逻辑。

normal directed planner 的 link/symlink root 同样复用该抽象：
`planDirectedCellsScan` 的 dual pass 调 `planCausalC2CellsWithParent`，由
`causalRootNormal` 固定 target/linkpath；`RootLinkPath` 只进 plan/batch，
再经 `insertDirectedBatch` → `causalRootCallsSeq` materialize。该 pass 与
C1 共用 `DualPathCausalEnabled` gate；GlusterFS 默认 true，HMDFS false；详见 §8.2.2。

### 8.6 Causal Root / Directed（GlusterFS v1）

#### 8.6.1 C1 anchor / root role

`causalAnchor` 增加 `RootPaths`，保留兼容投影：

```text
普通调用:    Path=effective, Source=Dest=Path
rename:      Path=dst,      Source=src,  Dest=dst
link:        Path=linkpath, Source=target, Dest=linkpath
symlink:     Path=linkpath, Source=target, Dest=linkpath
readlink:    不做 C1 anchor
```

- link/symlink anchor 的 A 侧按 role 枚举：target role 用 target FileTree
  node；linkpath role 用 `causalRootPathsByPath(linkpath)`，不把 linkpath
  当 FileTree node；
- 两个 role 分别用各自的 `{pre, post}` 调用 `causalPairRelation`，C1 cell
  的 `Role` 与 anchor role 对齐，不再把 linkpath relation 压到 target cell；
- rename anchor 的 A 侧只用 post/dst，`src` 只作为 B/CC 候选，不把 src
  当成 A；
- C1 root 候选的 source 子树过滤只对成功 rename anchor 生效，由
  `causalAnchor.SourceMoved` 显式标记；不能用 `Source != Path` 推断，
  因为 link/symlink anchor 的 `Source=target` 也满足该条件。link/symlink
  target 及其后代不能被当成“被移走的 source 子树”；
- C1 link/symlink root 复用 `causalRootNormal`：
  - `RootPath = target`（root materialization）
  - `RootSource = target`
  - `RootDest = linkpath`
  - `RootRelationPath = target`（C1 cell 的 B 侧/relation）
  - C1 link/symlink 是 pair 的 B 侧，DAG 只按 target 报 B 侧；不生成
    linkpath-role C1 cell。
- C1 root 是 rename 时，B 侧 cell/relation 用 `RootSource`（src），
  `RootDest`（dst）只用于 materialize。

调用集：

```text
C1 anchor:
  mkdir, creat, rename, unlink, rmdir, write, pwrite64, truncate,
  chmod, fsync, fdatasync, link, symlink, setxattr

C1 root:
  open, read, write, pread64, pwrite64, truncate, stat, chmod,
  fsync, fdatasync, getdents64, unlink, mkdir, creat, rename,
  link, symlink, setxattr

rmdir:
  DCT/model root, C2 variant, C1 anchor 支持；
  C1/normal directed root 不支持（Success 空目录 / ENOTEMPTY 非空目录
  两种 root 形态尚未同时进入 fixed-root planner）。

`directedRootCalls` 是 HMDFS 的 legacy/effective root 列表，也是未设置
`Plan.DirectedRootCalls` 的 profile 的兜底；GlusterFS 使用
`GlusterfsCausalRootCalls` 覆盖。不要为了 GlusterFS 改全局
`directedRootCalls`：改全局等同改 HMDFS，必须同步 HMDFS 测试。

C2 variant:
  open, stat, chmod, fsync, fdatasync, getdents64, read, write,
  pread64, pwrite64, truncate, rmdir, rename, unlink, mkdir, creat,
  link, symlink, readlink, getxattr, setxattr
```

`readlink` 不做 C1 anchor/root，只做 C2 variant；`getxattr` 只做 C2 variant。
`close` 不做 C1 anchor/root，也不做 C2 variant；它只作为 open/read/write/
getdents 的配套调用。文件 close 的 RELEASE 事件仍可作 DAG B/CC；目录 close
的 RELEASEDIR 29 被过滤。
`isModifierCall` profile 增加 `link/symlink/setxattr`。

禁止嵌套链接：

- 结构化 link/symlink target 只从测试根内选：可以是 baseline 路径，或由
  anchor 关系派生的已有文件/目录 canonical 路径，或同一程序/plan 中更早
  creator 产生、执行时存在的 fresh 路径；
- linkpath 一律 fresh，可在任意已有父目录；
- DCT mutation root=link/symlink 时 target 重新从测试根内选，不能拿 p0 的
  linkpath 当 target；
- generic mutation 的 link/symlink target 不能是已有 linkpath/symlink；
- link target 必须是普通文件，symlink target 必须是文件/目录；
- symlink target 在 `RootPaths`/DCT 中保存 canonical 路径；构造调用时转换为
  相对 linkpath 父目录的 payload，解析后不得逃出测试根；
- readlink 只读一跳，返回 payload；DAG/checker 用 linkpath + payload 还原
  canonical target。

`DualPathCausalEnabled=false` 的 A/B 配置下，link/symlink 的 C1/C2 插入不生成；
GlusterFS 默认 true，gate=false 只用于对照实验。

#### 8.6.2 C1 root 路径候选

- link root：
  - target 从 anchor 关系路径枚举，必须存在且是普通文件；
  - linkpath fresh；
  - `causalRootFits("link", node)`：`!node.IsDir`；
  - `causalRootArgs("link")`：OffB=NA；
  - 不使用 `addFreshPlan`。
- symlink root：
  - target 从 anchor 关系路径枚举，文件/目录都可；
  - linkpath fresh；
  - `causalRootFits("symlink", node)`：true；
  - `causalRootArgs`：NA；
  - 不使用 `addFreshPlan`。
- setxattr root：单路径，`causalRootKind=causalRootNormal`，OffB=NA。
- readlink/getxattr 不做 C1 root。

`causalRootCallsSeq`/`directedVariantCallsSeq` 增加：

```text
link:    link(target=rootPath, linkpath=fresh)
symlink: symlink(target=rootPath, linkpath=fresh)
```

C1 cell 的 `DepthB/TypeB/PersistB/Rel` 全用 target；linkpath 只用于 root
物化和 readlink，不进入 C1 cell。
C1 root link/symlink 的 `RetB` 只规划 Success；不规划 EEXIST/ENOENT。
`DualPathCausalEnabled=false` 的 A/B 配置下 C1 link/symlink 不生成。

#### 8.6.3 C2 variant 枚举

`directedVariantCalls` 增加 `link/symlink/readlink/getxattr/setxattr`。

| variant | B 路径 | RetB | Temporal | TypeB |
|---|---|---|---|---|
| `link` | target | Success | root 是 modifier 时 CC+HB | file |
| `symlink` | target | Success | root 是 modifier 时 CC+HB | file/dir |
| `readlink` | root `LinkPath` | Success | **HB only** | false |
| `getxattr` | target | Success / RetFailure（ENODATA） | root 是 modifier 时 CC+HB | file/dir |
| `setxattr` | target | Success / RetEEXIST（XATTR_CREATE） | root 是 modifier 时 CC+HB | file/dir |
| `rmdir` | target | Success / RetFailure（ENOTEMPTY） | root 是 modifier 时 CC+HB | dir |
| `rename` | src | Success / RetFailure（ENOTEMPTY） | root 是 modifier 时 CC+HB | file/dir |

- C2 root 是 link/symlink 时，普通 variant 的 root A：
  - target-role：CC+HB 都用 target；
  - linkpath-role：只 HB 用 linkpath；CC 不生成（pair extractor 只在
    link/symlink 是 HB A 侧时枚举 linkpath）；
  readlink 例外：B 用 root `LinkPath`，root A 也强制 linkpath，relation
  只能是 `Same`；普通 variant 循环不生成 readlink；
- target-role 和 linkpath-role plan 可以共享同一个 root execution，同 batch
  只需 target/linkpath 相同；`cellKey` 用 `Role` 区分 target/linkpath cell；
- link/symlink 的 linkpath fresh，不进 `BaseB`/cell；
- link/symlink 的 EEXIST 不进入 v1 cell 模型；
- readlink 只允许 `root=symlink`，不规划 CC，不规划 EINVAL；
- getxattr/setxattr 不规划 ENOENT/ERANGE/EINVAL；缺失路径只作为 LOOKUP
  自然观测，不进入定向失败 cell；
- rmdir/rename 不规划 ENOENT；缺失 source/parent 是 LOOKUP-ENOENT。这里只
  规划非空目录导致的 ENOTEMPTY。
- `causalVariantRelPaths`：
  ```text
  link/symlink      -> target, target
  readlink          -> linkpath, linkpath
  getxattr/setxattr -> path, path
  rename            -> src, src   // HB/CC B uses the entry/source path
  其他               -> path, path
  ```

`directedVariantKinds`：

```text
link/symlink:       Success
readlink:           Success
getxattr:           Success, RetFailure
setxattr:           Success, RetEEXIST
rmdir:              Success, RetFailure
rename:             Success, RetFailure
```

`variantKindsFor` 对 GlusterFS 不补 ENOENT；HMDFS 保持原行为。

`variantKindAllowed` 是 **上下文门**，不是 form 表；它决定某个 form 在具体
root/B/relation 下能不能构造：

```text
existing B:
  mkdir:    RetEEXIST only when B is dir
            （GlusterFS form 表只有 Success，所以普通已有 B 实际不会 plan；
             Success 只由 FreshSame/freshB 构造）
  creat:    RetSuccess only when B is file（truncate）；fresh B 只由 fresh 通道
  rmdir:    RetFailure only when B is dir && nonEmpty；Success 只由 FreshSame
  unlink:   RetSuccess when B is file；RetENOENT 只在 root=unlink && rel=Same
  rename:   false（rename variant 只用 synthesized rename B）
  link:     RetSuccess when B is file
  symlink:  RetSuccess
  readlink: RetSuccess when B is non-dir
            （root=symlink 由 combo/plannable 层约束）
  getxattr: RetSuccess only when root=setxattr && rel=Same；否则 RetFailure
  setxattr: RetSuccess any；RetEEXIST only when root=setxattr && rel=Same
freshB:
  only the named call (mkdir/creat) with RetSuccess
synthesized rename B:
  rename with renameKindRet(bn)，或 root=unlink && rel=Same 的 RetENOENT
```

##### FailurePolicy（GlusterFS v1）

主动构造的失败只有 4 个：

| 调用 | errno | RetBucket | 构造条件 |
|---|---|---|---|
| `rmdir` | ENOTEMPTY | `RetFailure` | 目标是非空目录 |
| `rename` | ENOTEMPTY | `RetFailure` | source 是目录，destination 是同类型非空目录 |
| `getxattr` | ENODATA | `RetFailure` | 路径存在，`user.eifuzz` 不存在；root 不能先设置同一 key |
| `setxattr` | EEXIST | `RetEEXIST` | 同一 path/key 先 set 成功，variant 再用 `XATTR_CREATE` |

不主动构造：

```text
mkdir EEXIST
unlink ENOENT
rmdir/rename ENOENT
open/stat/truncate/chmod/getdents64 ENOENT
link/symlink/readlink 失败
readlink EINVAL
link EMLINK
getxattr ERANGE
setxattr EPERM/EINVAL
```

这些限制只针对单操作/已有对象上的失败构造：路径缺失只作为 LOOKUP 自然观测，
不进入普通 GlusterFS directed/plannable 失败 cell；model 仍保留自然可达上界。

fresh-fresh 竞赛是独立通道，使用完整 `freshSameForms` 矩阵：

```text
mkdir/creat Same EISDIR
creat/mkdir Same EEXIST
mkdir/mkdir Same EEXIST
mkdir/rmdir Same ENOENT
creat/unlink Same ENOENT
creat/stat Same ENOENT
mkdir/open Same ENOENT
...
```

这些形态只在 `Rel=Same` 且 root 是 `mkdir`/`creat` 的 combo 中进入
plannable；对应的 model 也只为这些 combo 补充单操作表未列出的 race 形态
（如 `mkdir/rmdir ENOENT`、`creat/unlink ENOENT`）。普通已有对象上的
`mkdir EEXIST` 仍然不主动构造。HMDFS 的失败表保持不变。

`causalC2ComboLive` 加入新 variant；`readlink` 仅在 `root=symlink` 时加入。
`directedVariantCallsSeq` 的 link/symlink 生成 target + fresh linkpath；
readlink 用 `plan.BaseB`（C1 root 的 `RootPaths.LinkPath`）。

#### 8.6.4 State effect / authority / HardlinkGroup

```text
planStateEffect:
  write/pwrite64/truncate Success -> Size
  creat Success 且非 FreshB      -> Size
  unlink/rmdir/rename Success    -> Destroy
  mkdir                           -> Create
  link/symlink/setxattr           -> Observer
  readlink/getxattr               -> Observer
  其余                             -> Observer
```

HardlinkGroupKey：

- 只对 `link` 生效；`symlink` 不建组；
- v1 不允许嵌套链接，group key = link target 的 canonical 测试根路径；
  target 可由 anchor 关系或 baseline 选出，key 从当前 Link call 重算；
- 同一 target 的多个 link 共享 group；
- 只影响 directed/C2 batch，不进 FileTree/DCT/cellKey/checker；
- 不做 linkpath 递归继承；
- planned linkpath 可以作为 `write/truncate` 的调用参数（canonical binding）；
  FUSE `inode_path` 在 q0/h1 间任选的 dentry 不是调用参数，DAG 不能把
  事件 path 当 canonical。

group-touched 调用：

```text
write, pwrite64, truncate, creat, read, pread64
```

`open` 在 v1 不生成 O_TRUNC，暂不加入；以后加 O_TRUNC 再补。
`stat/chmod/fsync/getxattr/setxattr/link/symlink/unlink/rename/readlink`
不 touches group。

`planTouchedPaths`：

```text
BaseB
rename 成功再加 BaseB2
group-touched 调用再加 "\x00group:"+HardlinkGroupKey
```

`filterBaseBAuthority`/`batchBaseBAuthorityOK` 算法不变，只多一个 group key；
`FreshSame` 继续豁免。

允许：

```text
unlink(L1) + write(T)
stat(L1) + write(T)
symlink(T,L1) + write(T)
```

冲突：

```text
write(T) + write(L1)
truncate(T) + pwrite64(L1)
read(L1) + write(T)
```

#### 8.6.5 RetB / 时序 / 可见性

- C1：anchor → root HB；root 插在 anchor 之后，`refTime=anchor.Etime`；
  link/symlink/setxattr root 只规划 RetB=Success。
- C2：root 是 modifier 时 CC+HB，否则只 CC；link/symlink root 的 target-role
  走 CC+HB，linkpath-role 只走 HB（CC pair 的 A 侧只看 target）。
- readlink：directed 只规划 HB Success，不规划 CC/ENOENT；路径为 root
  `LinkPath`；跨节点实际不可见时 ENOENT 只作为自然观测，不做 failure cell。
- getxattr/setxattr：setxattr modifier，getxattr observer；HB/CC 按 root
  modifier；path=target；失败只做 FailurePolicy 的 ENODATA/EEXIST。
- rmdir/rename：失败只做 FailurePolicy 的 ENOTEMPTY（RetFailure）。
- destructive Same root 下，link/symlink/getxattr/setxattr 的 HB Success
  不规划；CC 仍按竞态保留 Success；不生成 ENOENT failure cell。readlink
  只有 HB，若 root destructive same LinkPath 则该组合不规划。
- `DualPathCausalEnabled=false` 的 A/B 配置下 link/symlink/readlink 的 causal
  插入不生成；GlusterFS 默认 true。

#### 8.6.6 Coverage cell

- `comboKey` 不加入 role/RelKind/HardlinkGroupKey；`cellKey` 加入 `Role`
  （target/普通 cell 用零值，linkpath 用显式 role）；
- C1 root=link/symlink：B 侧只用 target；linkpath 不进 C1 cell；
- C2 link/symlink variant：B 侧用 target，linkpath fresh 不进 variant cell；
- C2 root 是 link/symlink 时，普通 variant 的 A 侧：target-role 的 CC/HB
  都用 target，linkpath-role 只 HB 用 linkpath；readlink 的 A 侧强制 root
  `LinkPath`；
- linkpath role 只计 HB：`computeProducibleLocked`/`computePlannableLocked`
  按 role 存 `roleCellCounts`，linkpath CC 不进入 model/plannable 势能；
- plannable 额外做 role/relation 过滤：linkpath 是 fresh file/symlink entry，
  非 readlink 只允许 `Parent`/`Sibling`，readlink 只允许 `Same`；model 保留
  自然可达上界，不应用这个过滤；
- target-role 与 linkpath-role 共享 `comboKey`，但 `cellKey.Role` 不同；
  `directedCell.Role` 与 `RootRelationPath` 对齐，pending 按 role 匹配；
- C1 link/symlink anchor 也按 role 枚举：target role 用 target node，
  linkpath role 用 `causalRootPathsByPath(linkpath)`；C1 cell 的 `Role` 与
  anchor role 一致；
- rename 不受 `DualPathCausalEnabled` 影响；C1/C2、normal directed 和 PathSet
  的 rename 仍按 source/destination 处理，rename cell 的 `Role` 始终为
  `cellRoleNone`；只有 link/symlink A 侧 anchor/root 才产生 linkpath role；
- C2 readlink：B 侧用 root `LinkPath`，Temporal 只 HB，relation 只 Same，
  TypeB=false；
- C2 rename variant：B 侧用 src，dst 只用于 materialize/RetB；
- C1 rename root：B 侧用 src，dst 只用于 materialize/RetB；
- `cellKeyOf` 的 `TypeB` 用 `pairPathIsDir(p.B, p.BPathType)`，与
  `featuresOfPairPath` 共用 role path type override；不能直接用
  `DAGVertex.IsDir`，否则 link/symlink target/linkpath role 的 TypeB
  与 planner/features 不一致；
- getxattr/setxattr：B 侧用 target，TypeB=file/dir；
- close 不进入 C1/C2 producible；文件 close 只作为非 modifier 的 B/CC
  pair 做自然观测，不生成 close-root cell；目录 close 无事件；
- link/symlink 双 role pair 仍映射到 `CallVariant{CallName, PathRelation}`，
  同 cell 由 `cellKey` 去重，不同 relation 分别记录；
- `DagPairToVariant` 不改；readlink 非 symlink root 由 producible/cellInModel
  过滤。
- `comboKey.Root` 是 A 侧 call 名，不区分 root/anchor：在 root-directed
  planner 里它是 root，在 group/PathSet 的 directed pair 里它可能是 anchor。
  因此 `Plannable*` 是 generic directed-pair 层；`RootPlannableTarget` 才是
  root-directed 子集。
- `rmdir-root` 是 model-only：DCT root、C2 variant、C1 anchor 都支持；
  `rootDirectedComboEnabled` 根据 `DirectedRootCalls` 精确匹配排除它，但不把
  `DirectedPairPotential`/`DirectedPairDepthPots` 等 anchor 视图一起清零。

model（`producible`）表：

```text
producibleRets:
  link/symlink/readlink -> Success
  getxattr              -> Success, RetFailure（ENODATA）
  setxattr              -> Success, RetEEXIST（XATTR_CREATE）
  rmdir/rename          -> Success, RetFailure（ENOTEMPTY）
  unlink                -> Success
  mkdir                 -> Success, RetEEXIST
  creat                 -> Success, RetFailure
  chmod/stat/truncate/open -> Success, RetENOENT
  write/pwrite64        -> Success, RetFailure
  getdents64            -> Success, RetENOENT
  read/pread64/fsync/fdatasync -> Success

producibleTypes:
  link     -> {file}
  symlink  -> {file, dir}
  readlink -> {false}
  getxattr/setxattr -> {file, dir}
  rmdir    -> {dir}
  rename   -> {file, dir}
  mkdir    -> {dir}
  creat/read/write/pread64/pwrite64/fsync/fdatasync/truncate/unlink -> {file}
  open/stat/chmod -> {file, dir}
  getdents64 -> {dir}

producibleOffBuckets:
  offset calls: zero/mid/tail/beyond；RetENOENT -> NA
  其他: NA
```

`producible` 是**自然可达上界**，所以包含 `mkdir EEXIST`、`open/stat/truncate
ENOENT`、`write/pwrite64 Failure` 等自然失败形态。`plannable` 只保留
`directedVariantKinds` + fresh-fresh race forms；FailurePolicy 只主动构造
`rmdir/rename ENOTEMPTY`、`getxattr ENODATA`、`setxattr EEXIST`。因此
`ModelOnly = ModelFound - PlannableFound` 是预期结果，不是 planner 漏规划。

fresh-fresh 竞赛例外：对 `Rel=Same` 且 root 是 `mkdir`/`creat` 的 combo，
model/plannable 额外采用完整 `freshSameForms` 的 (root, variant, ret, type)
形态。因此 `mkdir/creat Same Failure`、`creat/mkdir Same EEXIST`、
`mkdir/mkdir Same EEXIST`、`mkdir/rmdir Same ENOENT`、`creat/unlink Same
ENOENT` 等 race cell 在这些 combo 内是 model/plannable；普通已有对象上的
同名返回仍不进入 plannable。HMDFS 的 producible 表不变。

实现约束：

- coverage board 初始化不能再用全局 `FsopsSubCalls`；必须用 profile 的
  root/variant 列表；
- `computeProducibleLocked`/`computePlannableLocked` 对 linkpath role 只计
  HB；readlink 是单 role，也只计 HB。target role 仍计 CC+HB，否则 directed
  会把 linkpath CC 当成可构造 cell；
- `directedVariantCalls`/`causalC2ComboLive`/`DirectedPairPotential` 共用同一
  role-aware temporal 规则。

#### 8.6.7 Causal 验收清单

C1：

- anchor 可以是 link/symlink/setxattr；readlink/getxattr 不做 anchor；
- anchor link/symlink 的 Path=linkpath、Source=target、Dest=linkpath；
- anchorNodes 按 role 枚举：target role 用 target FileTree node；linkpath role
  用 `causalRootPathsByPath(linkpath)` 的 Parent/Sibling，不把 linkpath 当
  FileTree node；
- root=link/symlink target 从测试根内已有文件/目录选（anchor 关系或 baseline），
  linkpath fresh；symlink payload 按 linkpath parent 物化；
- C1 link/symlink root 的 B 侧只用 target；C1 link/symlink anchor 的 A 侧按
  target/linkpath role 生成独立 cell，`Cell.Role` 与 anchor role 对齐；
  linkpath-role 只 HB，且 plannable 只允许 Parent/Sibling（readlink Same）；
- 禁止嵌套 link/symlink。

C2：

- link/symlink B 路径是 target，linkpath fresh；RetB=Success；root modifier
  时 CC+HB；
- readlink 只挂 symlink root，只 HB Success，路径=root LinkPath；
- rmdir/rename RetB=Success/RetFailure（ENOTEMPTY）；
- getxattr RetB=Success/RetFailure（ENODATA）；
- setxattr RetB=Success/RetEEXIST（XATTR_CREATE）；
- 其他单操作失败（已有对象的 mkdir EEXIST、unlink/open/stat/... ENOENT、
  link/symlink/readlink 失败、readlink EINVAL、link EMLINK、getxattr ERANGE、
  setxattr EPERM/EINVAL）不进入 directed/plannable；model 保留自然可达上界，
  因此它们可能出现在 `ModelOnly`；
- fresh-fresh 竞赛在 `Rel=Same` 且 root 是 `mkdir`/`creat` 的 combo 内使用
  `freshSameForms` 的 Failure/EEXIST/ENOENT 形态；这是独立通道，不受上面的
  单操作失败限制；
- HardlinkGroupKey 只影响 size/offset 预测；
- `DualPathCausalEnabled=false` 的 A/B 配置下 causal C1 和 normal dual root 的
  link/symlink 插入都不生成；GlusterFS 默认 true。

Coverage：

- `comboKey` 不含 role/group/RelKind；`cellKey` 含 `Role`
  （none/linkpath/symlink-linkpath）。role 是 cell 身份，不是 DCT combo 维度；
- link/symlink root 的 target role 用 `cellRoleNone`，linkpath role 用显式
  role；target role 计 CC+HB，linkpath role 只计 HB；
- C1 link/symlink anchor 也按 target/linkpath role 枚举；linkpath-role HB 与
  C2 link/symlink root 的 linkpath-role HB 是同一个 cell，二者 plan 可共享；
- readlink 是单 role，用 `cellRoleSymlinkLinkPath`，只产生 HB self cell；
- `producible`/`plannable`/`found`/`plannableFound` 都是
  `map[comboKey]roleCellCounts`，按 role 存计数；linkpath CC 不进入
  model/plannable 势能；
- plannable 对 linkpath role 做 relation 过滤：非 readlink 只允许
  `Parent`/`Sibling`，readlink 只允许 `Same`；model 保持自然可达上界；
- producibleRets/Types/OffBuckets 按上表；creation-root Same combo 另加
  `freshSameForms` 的 race 形态（含 model race ENOENT 扩展）；
- `DagPairToVariant` 不改。

HMDFS：

- 现有 C1/C2、cellKey、same-parent rename 行为不变。

### 8.7 PathSet（GlusterFS v1）

PathSet 是 mutation 的路径分量迁移，不是 DCT/定向 cell 规划。现有
`pathMember.Paths []pathSpec`、`pathSpec.ArgIdx` 和 `applyPathSetPlan` 已经
支持一个成员多个路径（`rename` 已这样用）。v1 对 link/symlink 只迁移
target；linkpath 是 program-local fresh binding，不进入 PathSet。

#### 8.7.1 支持范围

| 调用 | PathSet broad | PathSet directed target arm |
|---|---|---|
| `rename(src,dst)` | 支持；src/dst 都迁移，走 `RenamePolicy` | 不参与；双路径成员整体排除 |
| `link(target,linkpath)` | 支持；v1 只迁移 target；linkpath 保持 fresh binding | 不参与；call-name gate |
| `symlink(target,linkpath)` | 支持；v1 只迁移 canonical target；linkpath 保持 fresh binding，物化时重算 payload | 不参与；call-name gate |
| `readlink(linkpath,...)` | 跟随 symlink linkpath，不独立迁移 | 不参与；call-name gate 显式排除 |
| `open(O_CREAT)` | 单路径，补 `pathRoleCreat` | 可单路径 target |

- PathSet directed target arm v1 的判定是 **整个成员 `len(Paths)==1`**，不是“从双路径成员里选一个 spec”。`pickPathSetTarget`/`pathSetSinglePath` 保持这个语义；target-only 后 link/symlink 也是单路径，因此另加 call-name gate，link/symlink/readlink 一律不参与 directed target arm。`readlink` 同样由 call-name gate 排除。
- `PathSet` 内部只保存 canonical 路径（相对测试根）；不保存 symlink payload。
- `link/symlink` 的 target **不是 fixed baseline**：它和 `rename` source 一样属于
  路径分量，按 anchor 关系派生并参与映射；target 可以是 baseline existing，也可以是同一程序/plan 中更早 creator 产生、执行时存在的 fresh 路径；映射后必须重新校验存在性/类型。fresh target 用 `freshPathKindsBefore` 判定类型，用 `freshLinkPathsBefore` 排除更早 link/symlink 的 linkpath（包括 rename 后的 linkpath），继续禁止嵌套链接。
- `linkpath` 可以在任意已有父目录下；fresh 且不得与 target 相同。v1 只把
  linkpath 作为调用参数/readlink follower 使用，不把它加入 PathSet 迁移。
- `symlink` 的 target 必须是测试根内文件/目录；payload 由
  `dirname(linkpath)` 到 canonical target 计算，允许 `..`，但解析后不得逃出
  测试根。
- `link` 的 target 是普通路径参数，按 cwd/test root 解析，不做 payload 转换。
- 后续若实现 linkpath 迁移，`readlink` 的 linkpath 才需要和对应 symlink 的
  linkpath 同步；v1 不实现该迁移。

#### 8.7.2 映射与校验

- v1 的 `buildPathMembers` 为 link/symlink 只生成一个 target `pathSpec`：
  ```text
  link:     arg0 target (existing/fresh file); arg1 linkpath 不迁移
  symlink:  arg0 canonical target (existing/fresh file/dir); arg1 linkpath 不迁移
  readlink: arg0 linkpath (symlink binding follower，不独立迁移)
  ```
- 后续 linkpath 迁移 feature 才需要为 link/symlink 生成第二个 `pathSpec`
  （linkpath：fresh file / fresh symlink），并增加 target/linkpath/readlink
  的 role 区分；v1 不这样做。
- `symlink` 的 arg0 是 payload，不是 canonical target。新增共享 helper
  `symlinkCanonicalTarget(linkpath, payload)`（解析后必须在测试根内），并在
  以下位置使用：
  - `path_set.go::buildPathMembers` 的 symlink target spec；
  - `seed_derivation.go::collectSeedAnchors` 的 symlink target anchor；
  - `mutation.go`/`dctBaseCandidatesFor` 的 DCT mutation target；
  - `dag.go::BuildVertices`/`determinePathRel` 的事件 `Path2`；
  - `applyPathSetToCall` 的 payload 重算；
  - checker 的 `SymlinkPath` 归一化。
  `link` 的 arg0 是普通路径，不需要这个 helper。
- v1 `pathSetSpecValidWithParent`：
  - link target 必须是普通文件；
  - symlink target 必须是文件/目录；
  - target 可以是 baseline 已有路径，或同一 plan 中更早的 creator 产生的
    fresh 路径；fresh target 要求 parent 有效；映射后必须仍是测试根内路径；
  - `PlanProfile.PathStateTracking=true` 时按 in-program
    unlink/rmdir/rename 的 current state 判定：成功删除/改名的路径有
    tombstone，fresh creator kind 优先于 baseline；fresh target 允许落在被
    creator 重建的 baseline 路径上（仍受 creator binding 约束）。
    HMDFS 保持 `false`，继续 baseline-first；GlusterFS 和未来支持 link 的
    DFS 设 `true`；
  - `PathStateTracking=true` 时目录 rename 的 prefix remap 覆盖：
    - `freshPathKindsBefore`：`dst` 根继承 `src` 的 kind，`src` 子树 kind
      移到 `dst` 子树；
    - `freshLinkPathsBefore`：program-local linkpath binding 移到 `dst`，
      binding 的 `Target` 也在 `src` 子树内时一起 remap；这里指嵌套链接
      拒绝用的 binding，不是 PathSet linkpath 迁移；
    - `freshPathAbsentBefore`：`src` 根标记 tombstone，`src` 子树 tombstone
      移到 `dst`，并清除 `dst` 子树旧 tombstone；目录 tombstone 按 prefix
      覆盖后代（`pathSetAbsentIn`）；
    - `freshPathCreatorBefore`：候选 creator 路径先经过其后的 rename 事件
      映射，再与 target 比较；
  - prefix remap 只覆盖 fresh maps；baseline FileTree 中未记录的后代不
    remap。例如 baseline `dirA/f` 被 `rename dirA dirB` 移走后，
    `link dirB/f` 仍是已知 false negative，不做完整 baseline overlay；
  - HMDFS/`PathStateTracking=false` 保持 exact src/dst，不启用 prefix remap；
  - fresh link/symlink target 在 `pathSpec` 中记录
    `CreatorPos`/`CreatorArgIdx` 和 `CurrentKind`；`buildPathPlan`/
    `buildTargetPathPlan` 映射后把 target 对齐到 creator 的 mapped path；
    `validatePathSetPlan` 拒绝 creator 缺失/分叉，以及目录类 creator 的
    link target；
  - target 不能是 `freshLinkPathsBefore` 中的 linkpath（含 rename 后的
    linkpath），不能形成嵌套链接；
  - target != linkpath。
- 后续 linkpath 迁移 feature 才需要：
  - `pathSetTargetValid`/`freshKinds`/`freshLinks` 的完整 target 校验；
  - linkpath fresh、parent 是 baseline 目录或同一 plan 中更早的 fresh `mkdir`；
  - `pathSetMemberValid` 检查跨参数约束；
  - `applyPathSetPlan` 对 symlink 两阶段：先写 linkpath，再用新 canonical
    target + 新 linkpath 计算 payload，最后写 target；
  - readlink 的 linkpath 与对应 symlink linkpath 同步；
  - `pathSpec` role/kind 区分 linkpath（fresh file / fresh symlink）。
- `pathSetTopRoot`/`pathSetTopPath`/`mapPathSetPath`/`pathSetTargetLevel` 改为
  prefix-aware，用 `FileTree.Prefix`/`RootPrefix`，不能硬编码 `MergeViewPrefix`。
- `pathSetCommonPrefix`/`pathSetRoots` 使用全部可迁移 spec；v1 中 link/symlink
  的可迁移 spec 只有 target。
- `open(O_CREAT)` 在 `buildPathMembers` 中按 `openCallCreatesFile` 标成
  `pathRoleCreat`，fresh 路径走 `pathFresh`。
- v1 `pathSpec.Kind`/`TypeConstraint` 只需按 target role 填：link target
  （existing/fresh file）、symlink target（existing/fresh file/dir）；不能全用
  `relTargetAny`。

#### 8.7.3 RenamePolicy 在迁移阶段调用

不能先用 generic `mapPathSetPath` 算出 dst 再验证；迁移阶段就要按 policy 选：

```text
1. 映射 source 和其他成员
2. 对 rename 成员构造 RenameContext（mapped source、anchor、baseline FileTree）
3. 候选顺序：
   a. generic mapping 得到的 mapped existing dst
   b. 其他能保持原路径关系的 existing dst
   c. policy 允许的 fresh dst（parent 为已有目录或同一 plan 中更早的 mkdir）
   d. reject
4. policy.Validate(ctx, source, dst)
5. pathSetSpecValid + pathSetMemberValid + pathSetConnected
6. apply
```

- HMDFS 的 policy 只返回 same-parent 候选，现有行为不变。
- GlusterFS 允许 cross-parent、fresh、existing replace。
- existing replace 必须检查：目标类型匹配、目录为空、不是 source 后代、没有被
  同一 PathSet 的其他成员依赖。依赖定义为：
  ```text
  任一其他成员的 pathSpec.Path == dst
  或任一 fd-only 成员的 resolveFdTarget owner 路径 == dst
  ```
  命中任一条就拒绝 existing，改用 fresh；不要用模糊的“语义依赖”判断。
- fresh dst 的 fallback 是 total：source parent 在 plan 中有效时，fresh sibling
  总能构造；找不到 existing dst 不应导致 rename 无法生成。
- `pathSetFallbackPath` 对 rename dst 不能绕过 policy；没有候选就失败。
- `pathSetTarget` 的 directed arm v1 仍要求整个成员单路径；双路径只作为 broad
  relocation 成员。以后要支持 dual-path target，再给 `pathTarget` 增加
  `AnchorSpec`/`MemberSpec`，不改变本节的映射规则。

#### 8.7.4 Legacy path fallback

`MutateGroupPathDynamic` 的 legacy fallback（`mutateGroupPathDirected` /
`findGroupCalls` / `updateCallPathInProg`）也必须 profile-aware：

- `rename`：保留在 legacy fallback 中，但 dst 必须走 `RenamePolicy`，不能再
  硬编码 HMDFS same-parent `._renamed_`。HMDFS policy 复现现有 same-parent
  suffix 行为；GlusterFS policy 可选 cross-parent/fresh/replace。
- `link/symlink/readlink`：不进入 legacy fallback。它们只走 PathSet broad、
  DCT mutation、derived、directed；legacy `updateCallPathInProg` 只改 arg0，
  会破坏 linkpath/payload/readlink binding。
- `pickAnchor`/`findGroupCalls`/owners 更新要加 `legacyPathMutableCall` gate；
  不能只在 apply 阶段跳过，否则 anchor/member 已经被选中。
- `updateCallPathInProg` 需要接收 `lcs`/`RenamePolicy` 上下文；rename 先算 dst
  再写 source/dst，policy 失败则不修改调用。
- PathSet target arm 仍然不选 rename；legacy fallback 的 rename 是 path mutation，
  不是 target arm。
- 2026-11-18 已落地：`pickAnchor` 和两个 path-mutation 入口按
  `legacyPathMutableCall` 过滤；`updateCallPathInProg` 再防御一次，并接收
  `lcs` 走 RenamePolicy。HMDFS 仍保持 fresh same-parent `._renamed_` suffix。

### 8.8 Fresh / 跨节点边界（GlusterFS v1）

`docs/cross-node-fresh-path-notes.md` 的限制对 GlusterFS v1 继续有效：

- 不实现 C2/DCT 的 **fresh root + fresh child**（root 创建父目录，variant 在
  另一节点创建其子路径）；
- `FreshSame` 只保留现有 `creat`/`mkdir` 固定形态；
- `FreshB` 只允许已有 parent；fresh sibling 的父目录必须是 baseline 已存在目录；
- C1 both-fresh Child 只支持同一 p0/C1+C2 本地链路；
- 只支持 1 层 fresh：fresh anchor/root 的 parent 必须在 FileTree 中已存在；
  nested fresh parent 不生成 fresh-root/both-fresh Child 相关 plan；
- `open(O_CREAT)` 可以作为 anchor path 的 fresh file creator，但不新增
  fresh-root + fresh-child 的 C2/DCT 形态。

GlusterFS 是共享 volume，但不能因此默认 fresh child 跨节点安全：`HB` 只表达
时间意图，不保证目录已跨 client 可见。v1 保持保守规则，等跨节点可见性模型和
runtime 测试完成后再放开。

PathSet 不改变上述 fresh 语义；它迁移 fresh path 后仍按同一
`freshParentNodeEligible`/`pathSetPlanParentValid` 规则校验 parent。

## 9. 一致性检查

### 9.1 A 类强字段（v1 硬判）

- 路径集合
- 完整 `S_IFMT` 类型
- 普通文件内容 checksum
- 普通文件 size
- symlink target
- mode 权限位
- `user.*` xattr，双向比较，过滤内部 key

### 9.2 B 类诊断字段

- `Nlink`
- `Mtime`
- uid/gid

“只诊断”指不进入一致性失败列表、不导致 `ConcFSCheck` 返回 false，但必须
记录日志/计数器，便于后续判断 GlusterFS 的真实 quiescence 行为。不能静默忽略。

代码落点：

- `compareFileMeta`：GlusterFS 只把 A 类差异 append 到 `ics`；B 类差异记录到
  diagnostic 输出/计数器，不 append。
- `symsc/checker.py::check_meta`：GlusterFS 的 `Nlink` 差异只打印/计数，不返回
  失败；不能沿用当前 `not is_hmdfs` 的硬检查。
- uid/gid 当前是注释状态；v1 保持不硬判。

### 9.3 当前实现必须修

- `xattrCmp` 单向 → 双向：先比较 key 集合/数量，再比较 value；比较前过滤
  `trusted.gfid`、`trusted.glusterfs.*`、`trusted.afr.*`、`glusterfs.*` 等内部 key，
  只对 user xattr 做双向比较（GlusterFS 已实现 `xattrCmpUser`）。
- `compareFileMeta` 增加 `SymlinkPath`（GlusterFS 已实现）。
- `SymlinkPath` 是 fsMd 里的原始 target payload；A 类比较同一 program 的 client
  间 payload，不把它当 FileTree 路径。需要 canonical target 时用 linkpath +
  payload 解析；不能在 checker 里对 payload 做普通路径 normalize。
- GlusterFS 用完整类型；HMDFS 保留目录位（已实现）。
- `initTreeSubset` 按 profile namespace root：只取 syscall 的 path 参数，
  `ft.FindNode` 命中才加入；fresh path 跳过，但 fresh path 的最近 baseline
  祖先目录要加入；symlink target 先按 linkpath canonical 解析（已实现）。
- GlusterFS CSAN B 类（Nlink/Mtime）只写专用
  `/home/user/dfsfuzz/glusterfs-csan.log`（可由 `GLUSTERFS_CSAN_LOG`
  覆盖），不进 fuzzer 通用日志；Go 和 Python symsc 的 B 类诊断走同一文件。
- server fsMd 永不进入 client checker。
- symsc 保留完整绝对 program 数组，只过滤 fsMd；最终状态用 root client。
- `filter_failure_sync_calls` 改为 profile supported/ignored，不因
  `fcntl/flock/sendfile/...` 整体跳过 CSAN。
- GlusterFS `Nlink`/`Mtime` 为 B 类：`compareFileMeta` 和
  `symsc/checker.py::check_meta` 只诊断，不进入失败列表（已实现）。

### 9.4 symsc 支持边界

- 当前和 `Monarch-master-bak` 原版 symsc 已支持：
  `link/symlink/readlink/getxattr/setxattr/removexattr/listxattr`、fallocate、lseek。
- 当前和原版都不支持：`mknod/mkfifo`、`fcntl/flock`、`statfs`、`access`、`fsyncdir`。
- v1 不加 mknod，因此 v1 不为 mknod 扩 symsc。
- v2 若加锁/statfs/access/fsyncdir，再扩 symsc 或保持 ignored。

## 10. Executor runtime

### 10.1 DfsRuntimeOps

不散写 `if (dfs_name == ...)`；在 `src/executor/` 增加一个小钩子表：

```cpp
struct DfsRuntimeOps {
    const char *name;

    bool (*trace_enabled_for)(bool is_client);
    void (*init_trace)(bool is_client, const char *trace_root);
    void (*start_trace)();
    void (*stop_trace)();

    bool (*restore_initial)(bool is_restarting, bool is_client,
                            bool is_reset_client);
    bool (*needs_startup_barrier)();
    bool (*needs_restore_barrier)();

    const char *(*client_cwd)();
    const char *(*server_cwd)();
    const char *(*metadata_root)(bool is_client);

    bool (*client_metadata_enabled)();
    bool (*server_metadata_enabled)();

    std::string (*config_command)(const char *init_ip,
                                  const char *setup_params,
                                  uint64 index);
    std::string (*node_up_command)(const char *init_ip,
                                   const char *setup_params,
                                   uint64 index);
    std::string (*node_down_command)(uint64 index);
};
```

`trace_enabled_for` 表示该角色是否产生 trace 输出：HMDFS client=true；
GlusterFS client/server=true（server 只写 `count=0` 头）。`trace_root` 只对
GlusterFS client 非空（`eifuzz_root`），HMDFS/server 传 `nullptr`。

实现点：

- trace 统一走 `rt->trace_enabled_for(is_client)` 门控：
  - `init_hmdfs_trace` → `rt->init_trace(is_client, trace_root)`
  - `start_hmdfs_trace` → `rt->start_trace()`
  - `stop_collect_hmdfs_trace` → `rt->stop_trace()`
- B2 只填 trace 字段；D 阶段已补 restore/cwd/metadata；config/node-up/down
  仍走现有命令路径（E1/E5 已修正参数和错误传播），未新增 runtime command builder。
- `restore_initial_file_tree` →
  `rt->restore_initial(is_restarting, is_client, is_reset_client)`
- cwd/metadata 已走 runtime；config/node-up/down 仍走现有命令路径。

### 10.2 GlusterFS hooks

- trace：attach 是 client-only；`trace_enabled_for` 对 client/server 都为 true，
  server 只写 `count=0` 头。`init_trace(server)` 不 attach，`stop_trace` 仍写头。
- cwd：
  - client `/root/glusterfs-client/eifuzz_root`
  - server `/root`
  - 不使用 legacy `/root/<dfs>-client/dfs-<pid>-<iter>` 目录；不执行每轮
    `remove_dir`/`make_test_dir`，cwd 固定测试根，树由 restore 管理。
- `in_userspace`：GlusterFS 使用 `executor.cc:719` 的默认 userspace 分支
  （`in_userspace=1`）；HMDFS/NFS/cephfs client 的 `0` 不走 GlusterFS。该值
  固定为 profile 属性，不能由 `dfs_name` 散写。
- `common_linux.h:263-270` crash recovery `test_dir`：GlusterFS 当前分支是
  `/root/glusterfs-client/eifuzz_root`；v1 failure/crash 关闭，普通 restore 不走这里。
- metadata：
  - client 固定测试根；
  - server 跳过，server fsMd 不进 checker；
  - 去掉 GlusterFS `stat` workaround 的静默空结果；根不存在应报 restore/config 错误。
- restore：
  - `restore_initial_file_tree()` 在每次 `receive_execute` 前执行，包括
    triage/DAG replay/light round；不只是普通 fuzz round；
  - client-only reset；设计指定 reset client `executor_index == server_num`；
  - `rm -rf /root/glusterfs-client/eifuzz_root`；
  - `timeout 60s rsync ...`（不加 `--timeout=300`）+ sync；
  - 其他 client 等 restore barrier；server 只参与 barrier，不执行 rm/rsync；
  - **normal restore 不重启 server/volume**；
  - 具体 barrier/命令超时见 §10.4。
- FileTree：
  - 每轮 execute 前 restore 后，`FileTree` 始终是 baseline；
  - `SyncFileTreeFromFsMd` 停用（死代码保留）；`fsMd` 只用于 CSAN/DAG，
    不回写 FileTree；
  - 不能在 GlusterFS 下重新启用每轮 fsMd sync，否则 FileTree 会记录下一轮
    不存在的路径。
- `node_down`：
  - 仅 failure/crash 使用；normal restore 不调用。
- `node_up`：
  - client umount/remount；
  - server restart glusterd；
  - mount 带 `attribute-timeout=0,entry-timeout=0`。
- init config：
  - executor 0 执行 `glusterfs-config.sh`；
  - 增加 client mount barrier，确认所有 client 挂载可用后再执行。
- trace 停止在 `write_metadata` 之前，保持 `[program][trace][fsMd]` 输出顺序。

### 10.3 角色相关迁移

- `GeneralFailPos`/`GeneralBarrierPos` 不挂 `ps[0]`；GlusterFS failure 关闭。
- `splice` 校验 batch 长度/角色。
- reservoir/group removal/repro 只处理 client view，保留 server slots；
  minimize 已全局停用，不再为 GlusterFS 增加 client-view minimize 分支。
- `deserializeInput` 校验 batch 长度、server 空、client 非空、Cids 数量、seed family。
- GlusterFS 拒绝 `IsStash`/`IsDCache`/`C2test`/`HasCrashFail`/`HasNetFail`；
  HMDFS 继续接受。
- Persistence 不是 seed family：GlusterFS 不加载 `large_dir.info`，不按
  `IsPersistence` 拒绝旧语料。

### 10.4 Runtime TimingProfile

等待/超时不能继续用 HMDFS 硬编码；放入 `DfsRuntimeOps` 的 timing 表。
HMDFS 的值保持现状；GlusterFS 单独给。

```cpp
struct RuntimeTiming {
    uint64 metadata_delay_ms;
    uint64 metadata_wait_grace_ms;
    uint64 agent_ready_ms;        // 0 = no-op
    uint64 peer_ready_checks;     // 0 = no-op
    uint64 peer_ready_interval_ms;
    uint64 startup_barrier_ms;
    uint64 settle_ms;
    uint64 restore_start_barrier_ms;
    uint64 restore_finish_barrier_ms;
    uint64 restore_cleanup_timeout_ms; // rm -rf / umount / mkdir
    uint64 rsync_timeout_ms;      // 外层 timeout 的总超时
    uint64 node_up_timeout_ms;
    uint64 node_down_timeout_ms;
};
```

| 项 | HMDFS | GlusterFS v1 |
|---|---|---|
| `metadata_delay_ms` | 30000 | **10000** |
| `metadata_wait_grace_ms` | 0 | 0（等待上限 = `program_timeout_ms + grace`） |
| `agent_ready_ms` | 30000 | 0（no-op） |
| `peer_ready_checks` × `peer_ready_interval_ms` | 3 × 3000ms | 0（改用 mount barrier） |
| `startup_barrier_ms` | `(TotalNum-1)*3000 + 30000` | 60000 |
| `settle_ms` | 3000 | 0 |
| `restore_start_barrier_ms` | 30000 | 30000 |
| `restore_finish_barrier_ms` | 90000 | 90000 |
| `restore_cleanup_timeout_ms` | 0（HMDFS 不使用） | 30000（`rm -rf` / `mkdir` / umount） |
| `rsync_timeout_ms` | 60000 | **60000** |
| `node_up_timeout_ms` | 30000 | 30000（仅 failure 模式） |
| `node_down_timeout_ms` | 15000 | 15000（仅 failure 模式） |

规则：

- `MetadataDelayMs` flag 默认改为“未设置”哨兵；由 profile 解析：
  - HMDFS 30000ms；
  - GlusterFS 10000ms；
  - 显式 fuzzer flag 覆盖优先（已实现；manager config 通道待补）。
- `write_metadata` 等所有 client 的 `executionsFinish` 超时用
  `program_timeout_ms + metadata_wait_grace_ms`。
- HMDFS 的 agent/peer/settle/startup barrier 行为不变；GlusterFS 的
  agent/peer 为 no-op，改用 client mount barrier。
- GlusterFS restore 的 rsync **不再加 `--timeout=300`**；使用
  `timeout 60s rsync ...` 总超时，finish barrier 90s。
- `restore_initial_file_tree` 必须先按 `DfsRuntimeOps` dispatch，不能直接调用
  HMDFS restore。
- GlusterFS `metadata_delay=10s` 是保守起点；Phase 0 只有在无 CSAN 误报时才
  逐步降到 5s/3s。HMDFS 30s 不动。

### 10.5 Triage / DAG direct

新 triage/minimize 设计本身是 DFS 无关的：minimize 已停用，triage 只执行一次，
DAG direct 用 `Call="dag"`。GlusterFS 只补 profile/gate，不另写算法：

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
- triage 的 `WorkTriage` target list 仍然按同一侧聚合；client/server 不混在
  一个 list 里。

实现状态（2026-11-17）：GlusterFS 已接入完整 DAG feedback（board learning +
DAG corpus/triage），不是只喂 coverage board。corpus 侧继续使用
`addInputToCorpus` hash 去重、`MaxDagCorpus` 硬 gate、`minimizeCorpus` 修剪、
`saturatedCalls["dag"]` 和固定 `smashIterations`。`MaxDagCorpus` 默认 0
（unlimited），长跑前按规模配置；D2/D3 观察 `dagCorpusEntries`、
`dagCorpusCount`、`dag` saturation 与 smash 预算。

### 10.6 QEMU / SSH 与 config 脚本复用

GlusterFS v1 复用原 Monarch 镜像和原 QEMU 启动路径，不复用 HMDFS 的镜像/agent
专有逻辑：

- `src/vm/qemu/qemu.go:602` 的按 VM index 拼 `hmdfs<index>.img` 只对
  `DFSName=="hmdfs"` 生效；GlusterFS 走原镜像路径。
- `qemu.go:708` 的 `ip=...:::::eth0:off` 只对 HMDFS 生效；GlusterFS 走默认
  `ip=`。
- GlusterFS server 的 `data.img` 分支已存在（`qemu.go:634/671`）；
  v1 不 mkfs/mount，brick 在 rootfs 的 `/root/glusterfs-server`。
- fuzzer 启动使用 SSH key：`syz-manager` 的 `SSHUser` 默认 root，
  `SSHKey` 指向镜像的 `focal.id_rsa`；不实现密码登录。
- root 密码 123456 只作为配置脚本内部 `sshpass` 的兼容依赖，不进入
  QEMU/fuzzer 启动参数。

`glusterfs-config.sh` v1 直接复用，不改脚本。复用前提：

```text
executor config 阶段 cwd = /root
镜像预建 /root/glusterfs-client（空挂载点）
镜像预建 /root/glusterfs-server
镜像存在 /root/dfs-fuzzing/libucov.so
sshpass 可用
root key 或 123456 至少一个可用
```

配置脚本的 mount 使用相对 `glusterfs-client`；v1 不修改脚本，但必须保证
cwd `/root` 和挂载点存在。`eifuzz_root` 不在镜像中预置；volume 挂载后由
首轮 `restore_initial` 创建。

## 11. 初始树、config 与脚本

详细命令和清单见 `docs/glusterfs-config-and-scripts.md`。

### 11.1 初始树加载

- GlusterFS config 不传 cid。
- `InitialDir` 仍要读，但按 profile：
  - HMDFS：按 cid 读 `<cid>.file/.dir/.tmpdir`，前缀 `merge_view/`；
  - GlusterFS：只读 `shared.file/.dir/.tmpdir`、`empty_dirs.info`，前缀为空。
- GlusterFS 忽略 `large_dir.info`；`Persistence_dir=""`；
  `Node_idx_of_persistence=-1`。
- `FileTree` 使用 `PathOwnerCid="shared"`；`Cids` 重复 shared 仅用于长度兼容。
- `FileTree` 只加载普通文件/目录/空目录/TmpDir，不加载也不建模
  symlink/hardlink；`link/symlink` 的 linkpath 只在 program-local
  `RootLinkPath`/`freshLinks`。
- `generate_test_files.py` 用唯一 `shared` 作为 node id；
  `--num-large-files 0` 时跳过整个 large-dir 块，不生成 `large_dir.info`。
- 可选 `--shared` 校验，防止多个 cid。
- baseline 只复制 `shared/` 的内容，不要把 `shared/` 这层目录带进 baseline。
- 清单文件放 `InitialDir` 指向的目录。
- 镜像目录前置条件：
  - `/root/glusterfs-client`：空挂载点，必须预建；
  - `/root/glusterfs-server`：server brick 根；
  - `/root/dfs-fuzzing/libucov.so`：LD_PRELOAD；
  - `/root/glusterfs-client/eifuzz_root` 不在镜像预置，挂载 volume 后由
    首轮 `restore_initial` 创建。
- 当前没有独立 `prefill.sh`：首轮 restore 由 reset client 执行
  `rm -rf eifuzz_root` + rsync baseline + sync，等价于 prefill；如果后续需要
  独立 prologue，再拆出脚本。

### 11.2 3-3 config

```json
{
  "server_num": 3,
  "fuzzing_vms": 6,
  "dfs_setup_params": "3 3 'replica 3'",
  "enable_csan": true,
  "enable_c2san": false,
  "net_failure": false,
  "node_crash": false,
  "vm": {
    "count": 6,
    "cpu": 2,
    "mem": 10240
  }
}
```

- `enable_syscalls` 必须使用 §1.1 的 22 个；不要把 `readv/writev/preadv/pwritev`
  或 at 变体加回来。
- 完整配置文件见 `docs/glusterfs-config-and-scripts.md` §1。

### 11.3 脚本

- `glusterfs-config.sh`：4 参数 `start_ip srv_cnt client_cnt mode`；
  volume create/start；client mount 带 timeout 选项；首轮 restore 负责初始化
  `eifuzz_root`，不单独调用 prefill 脚本。
- `glusterfs-node-up.sh`：5 参数
  `start_ip srv_cnt client_cnt mode cur_idx`；
  server 重启 glusterd；client umount/remount 带 timeout 选项。
- `glusterfs-node-down.sh`：client umount；server 仅 failure 场景。
- 当前 executor 对 HMDFS 保留 quoted params，对 GlusterFS/NFS 恢复原版
  unquoted params；`glusterfs-node-up.sh` 仍按 5 参数契约接收。
- prefill/restore：
  - `rm -rf eifuzz_root`
  - `timeout 60s rsync -a --numeric-ids --delete /home/baseline/glusterfs/ eifuzz_root/`
  - `sync`
  - 不使用 `rsync --timeout=300`；总超时与 barrier 见 §10.4。

## 12. 实施阶段

### Phase 0：Profile/角色契约

- 文档冻结：把 P1 的 FileTree/AccessMode/rename/22-call matcher/
  `PathSameInode`、DAG 路径来源和 LOOKUP 决策写入本文件、config/spec 文档。
- `DFSProfile`、`DFSConfig` alias、client view/wrap、Cids shared。
- HMDFS `ServNum=0` 行为不变。
- 启动校验、batch 校验；`enable_syscalls` 只接受 22 个。
- timing 契约：
  - `RuntimeTiming` 进 `DfsRuntimeOps`；HMDFS/GlusterFS 两套默认值；
  - `MetadataDelayMs` 改为“未设置”哨兵，HMDFS 30s / GlusterFS 10s；
  - GlusterFS agent/peer/settle 为 no-op，client mount barrier 60s；
  - restore dispatch 按 profile 选择，start/finish barrier 30s/90s，
    rsync 总超时 60s。
- fuzzer 入口契约：
  - `GenerateProgsForDfsFsops`、`MutateDfs`、`smashInput` 按 profile 分派；
  - `lcsFsops`/coverage board/DAG feedback 对 GlusterFS 初始化；
  - `ps[0]`/`seedPS[0]` 的 client 语义改为 client view；
  - `executeRaw` 对两个 profile 校验 batch 布局。
- PathSet 契约：
  - canonical 路径相对测试根；symlink target 不绝对化；
  - broad 支持 link/symlink/readlink/rename；
  - prefix-aware；rename policy 在迁移阶段调用。
- Phase 0 trace smoke（源码已确认路径行为，不再作为设计阻塞）：
  - `symlink(T,L1); stat(L1); readlink(L1)`：
    FUSE GETATTR path=`T`，READLINK path=`L1`；DAG `stat` 顶点 path=`L1`
    （调用参数），`readlink` 顶点 path=`L1`。
  - `link(T,L1); stat(L1); write(L1); stat(T)`：
    FUSE `inode_path` 可能给 `T` 或 `L1`；DAG `stat/write` 顶点分别按调用参数/
    fd provenance 取 `L1`/`T`，不取事件 path。
  - `link`/`symlink` 事件 `Path/Path2` 分别为 linkpath/target。
  - 不引入 `entry_ino`、全局 event path canonicalization 或 `PathSameInode`；
    symlink target 的 linkpath+payload 解析是局部的 target 归一化，不是事件
    路径 canonicalization。

### Phase 1：GlusterFS Trace/Event

- 新 `glusterfs_trace.bpf.c/.cc/.h`；trace 分发走 `DfsRuntimeOps` 的
  `init_trace/start_trace/stop_trace`。
- 事件布局、parser、FUSE opcode/path/filter；packed 1228B/1204B。
- 先映射现有 0–14 + 16–20 + 28/44；P0 验证 link/symlink/readlink/xattr。
- 更新旧 spec 中 RENAME2/COPY_FILE_RANGE/0–14 的错误。
- v1 不写 `entry_ino`、不做 `PathSameInode`。
- BPF 构建按 per-DFS vmlinux/include：HMDFS `tools/vmlinux.h` +
  `-Itools`，GlusterFS `tools/glusterfs/vmlinux.h` + `-Itools/glusterfs`；
  两个 skeleton 同时生成。uprobe attach 用 libelf `.symtab` + PT_LOAD
  file offset（本 build 的 `bpf_uprobe_opts` 没有 `func_name`）。

### Phase 2：双路径角色模型

- `DualPathKind`/`RootPaths`/`RenamePolicy`/`symlinkCanonicalTarget`。
- rename 特判迁移到 profile；HMDFS same-parent 保持不变。
- link/symlink 的 target/linkpath 用 canonical 测试根路径；symlink payload 只在
  调用物化时生成；readlink 作为 linkpath follower；共享
  `symlinkCanonicalTarget(linkpath, payload)`。
- GlusterFS v1 不做 `PathSameInode`；HMDFS 保持现有分类并折叠。
- `HardlinkGroupKey` 只做 directed/C2 batch 的 program-local authority，
  不进入 FileTree/DCT/cellKey/checker。

### Phase 3：GlusterFS v1 生成/DAG/覆盖

- 五个新调用生成器与参数策略；22-call 表与 matcher 落地。
- `GenerateProgsForDfsFsops`：GlusterFS 100% fsops、client-only、`IsFsops=true`；
  `MutateDfs`/`smashInput` 使用 client view + `MutateFsopsWithDCT`。
- `link/symlink` target 保存 canonical 测试根路径；symlink payload 按
  linkpath parent 物化；readlink 按 linkpath + payload 还原 canonical target。
- client-only generation + empty server wrap。
- **不做 FileTree 链接建模**；link/symlink 路径走 program-local
  `RootLinkPath`/`freshLinks`，authority 走 `HardlinkGroupKey`（仅
  directed/C2 batch，见 §8.6.4）。
- DAG 双路径、linkpath access/type、A 类 checker；不新增 SameInode/RelKind。
- DAG 路径规则：非 LOOKUP 顶点用调用参数/fd provenance，`ev.Path` fallback；
  GlusterFS LOOKUP 直接用事件 `Path`；所有匹配事件建顶点，不做同调用过滤。
- 不引入 `entry_ino`/canonicalization；hardlink 的 inode_path 任意 alias
  只影响事件 path，不影响 DAG 顶点路径。
- 状态（2026-11-18）：coverage 三层（observed/model/plannable）、
  combo-aware FreshSame race、role-aware linkpath cell、GlusterFS 完整 DAG
  feedback 已实现。

### Phase 4：DCT/定向/restore

- profile root/variant/causal；`BuildDefaultDCT(profile)`。
- GlusterFS root 19 / variant 21；`readlink` 只挂 symlink root。
- DCT seed：link/symlink anchor=target，readlink 特例 linkpath；rename anchor=src，
  dst 走 `RenamePolicy`。
- DCT mutation：已执行 link/symlink 的 target/linkpath 随机，variants 跟随 base；
  rename 已执行用 dst、未执行用 src；readlink 只用 linkpath。
- derived DCT 与 causal root 共用 `DualPathKind`/`RootPaths`；causal 的
  link/symlink 由 `DualPathCausalEnabled` gate 控制，GlusterFS 默认 true。
- C1：link/symlink/setxattr anchor/root；C1 root link/symlink 用 target 做
  cell、linkpath 只生成；读路径不做嵌套链接。
- C2：link/symlink/readlink/getxattr/setxattr；readlink 只挂 symlink root，
  只 HB Success；link/symlink RetB=Success；getxattr RetFailure（ENODATA）；
  setxattr RetEEXIST（XATTR_CREATE）；rmdir/rename RetFailure（ENOTEMPTY）。
- FailurePolicy 只主动构造 rmdir ENOTEMPTY、rename ENOTEMPTY、getxattr
  ENODATA、setxattr EEXIST；其他单操作失败不进 directed/plannable，model
  仍保留自然可达上界。
- fresh-fresh 竞赛例外：`Rel=Same` 且 root 是 `mkdir`/`creat` 的 combo 使用
  完整 `freshSameForms`；这些 Failure/EEXIST/ENOENT race 形态进 plannable，
  并在 model 中为对应 combo 补充单操作表未列出的 race 形态。
- normal dual root 已实现：`planDirectedCellsScan` dual pass +
  `causalRootNormal`；GlusterFS 默认 `DualPathCausalEnabled=true`，正常生成；
  gate=false 只作为 A/B 对照。
- `HardlinkGroupKey` 只做 directed/C2 authority；`planStateEffect` 中
  link/symlink/setxattr 为 observer。
- coverage：`comboKey` 不含 role/group；`cellKey` 含 role，linkpath CC=0；
  readlink 单 role；coverage board 使用 profile root/variant 列表，不再用
  全局 `FsopsSubCalls`。
- 跨目录 rename；link/symlink A/HB 随机 role。
- PathSet broad：link/symlink 只迁移 canonical target，linkpath 保持
  program-local binding；readlink 不独立迁移；rename src/dst 都迁移，dst 按
  `RenamePolicy` 选（existing 优先、fresh 兜底）；symlink payload 在 target
  迁移后重算；directed target arm v1 只接受 `len(Paths)==1` 的成员。
- PathSet target creator：link/symlink target 支持 baseline existing 或更早
  creator 的 fresh 路径；`symlinkCanonicalTarget` 共享。
- legacy fallback：rename 走 `RenamePolicy`，HMDFS same-parent 行为不变；
  link/symlink/readlink 不进入 legacy fallback。
- PathSet prefix-aware：`normalizePathSetPath`/`pathSetTopRoot`/
  `pathSetTopPath`/`mapPathSetPath`/`pathSetTargetLevel` 不硬编码
  `MergeViewPrefix`。
- `callNameConstraint`/`basePathIsFile`/`resolveFdTarget`/
  `ownerPathConstraint` profile 化；`open(O_CREAT)` 进 `pathRoleCreat`。
- DCT seed 无 Ret 规划；跨节点可见性按 `CheckInfo.Stime/Etime` 门控；
  DCT mutation HB 可用 `refTime`。
- 不实现 C2/DCT fresh root + fresh child；C1 both-fresh 只做本地 1 层；
  nested fresh parent 不生成。
- client-only restore；先 trace 后 planner。

### Phase 5：端到端

- 3-3 验证；HMDFS 回归；文档更新。
- 状态（2026-11-18）：P2 review #7–#11 已收口，PathSet current state /
  creator binding / rename prefix remap 文档已同步；GlusterFS dual-path causal
  默认开启，coverage 增加 role 维度（comboKey 保持 role-free）；D1 BPF
  standalone smoke 已通过。Ubuntu `go test ./prog ./syz-fuzzer`、
  `make executor`、D2/D3/D4 仍待执行。

## 13. 验收

- HMDFS 现有 DAG/coverage/rename 测试通过；call set/coverage/same-parent 不变。
- GlusterFS：
  - executor 0–2 server 空程序；3–5 client 调用。
  - link 双路径、symlink target、readlink linkpath、xattr key、跨目录 rename 可观测。
  - FUSE 事件 path 已 strip `/eifuzz_root`；mount root `/` 和 `.glusterfs`
    内部路径不进入 DAG。tree root 是 `""`，不是 mount root。
  - `ia_type`：file=1/dir=2/symlink=3；`link` 用 `loc2.inode+92`，
    `rename` 用 `loc.inode+92`；`symlink/mkdir/create` 新 inode 为 0 走默认。
  - `truncate` 事件 `off=新大小`、`size=0`；read/write、readdir、xattr 的
    off/size 按 tracer spec §6.5，不混用。
  - 文件 `close` 有 D/S、无 R；产生 FuncRelease 顶点并可作为 HB/CC 的 B 侧；
    目录 `close` 的 RELEASEDIR 29 被过滤，无顶点/cell。
  - 内部 GETXATTR `-61/-95` 被 `user.*` 过滤；无 user R 的 S 不产生 final event。
  - ctl/path 双 ring 按 ts 合并后再聚合；S 先于 R 被 drain 时仍能拿到 path。
  - **不要求 `PathSameInode` 与 `PathSame` 区分**；HMDFS 现有
    `PathSameInode` 折叠测试保持通过。
  - readlink directed/coverage 只产生 HB B self cell；DAG 实际若出现 CC，
    只记录反馈，不进入 directed/coverage。
  - link/symlink 的 linkpath 不出现在 FileTree；DCT/directed 不调用
    `FindNode(linkpath)`。
  - DCT root 19 / variant 21；`readlink` 只能作为 `root=symlink` 的 variant。
  - DCT seed：link/symlink anchor=target，readlink 用 linkpath；rename anchor=src，
    dst 由 `RenamePolicy` 生成。
  - DCT mutation：已执行 link/symlink 的 target/linkpath 随机，variants 跟随
    base；rename 已执行用 dst、未执行用 src；readlink 只用 linkpath。
  - derived DCT 与 causal root 共用 `RootPaths`；causal 的 link/symlink 插入
    由 `DualPathCausalEnabled` gate 控制，GlusterFS 默认 true。
  - C1：link/symlink/setxattr 可做 anchor/root；link/symlink root 的
    RootPath=target、RootDest=linkpath；readlink/getxattr 不做 anchor/root。
  - C2：link/symlink/readlink/getxattr/setxattr 可做 variant；readlink 只挂
    symlink root、只 HB Success；link/symlink RetB=Success；
    getxattr RetFailure（ENODATA）；setxattr RetEEXIST（XATTR_CREATE）；
    rmdir/rename RetFailure（ENOTEMPTY）。
  - FailurePolicy 主动构造只有 rmdir ENOTEMPTY、rename ENOTEMPTY、
    getxattr ENODATA、setxattr EEXIST；其他失败不进 directed/plannable，
    model 仍保留自然可达上界。
  - HardlinkGroupKey 只影响 directed/C2 的 size/offset authority；C1/C2
    link/symlink/setxattr 在 `planStateEffect` 中是 observer。
  - coverage：`comboKey` 不含 role/group/RelKind；`cellKey` 含 `Role`。
    link/symlink target/linkpath 是不同 cell，linkpath role 只产生 HB；
    readlink 单 role，只产生 HB self cell，CC=0；coverage board 使用
    profile root/variant 列表。
  - 不生成嵌套 link/symlink；`readlink` 只读一跳。
  - `CallVariant`/`Weights`/`comboKey` 不引入 role/RelKind；`cellKey` 只加
    `Role`，不加 RelKind/group。target/linkpath 共享 combo，但 cell 身份分开。
  - link/symlink 事件 `Path/Path2` 分别等于 linkpath/target；symlink 的 Path2
    是 payload，DAG/checker 能按 linkpath 还原 canonical target。
  - symlink follow 的 FUSE GETATTR 事件 path 是 target `T`，但 DAG `stat`
    顶点 path 是调用参数 `L1`；hardlink 的 FUSE path 可能是 target/alias
    （P0 `truncate("q0")` 事件显示 `h1`），但 DAG 顶点按调用参数/fd
    provenance 取路径；planned linkpath 可作为 truncate/write 的调用参数，
    内核随机 alias 不是。两者都不依赖 `entry_ino`。
  - GlusterFS LOOKUP 顶点直接使用事件 `Path`；HMDFS 的 `lookupPath` 行为不变。
  - A 类 checker 通过；server fsMd 不参与。
  - stash/dcache/failure/C2san/NetFailure/NodeCrash seed 不生成，旧语料拒绝；
    persistence 不作为 seed family，GlusterFS 仅关闭 `large_dir.info` 和
    persist 维度。
  - restore 后 baseline 无 xattr/hardlink/symlink 残留。
  - restore 在每次 execute（含 triage/light）前执行；server 只参与 barrier；
    FileTree 保持 baseline，`SyncFileTreeFromFsMd` 停用。
  - symlink target 在所有内部结构中都是相对测试根的 canonical 路径；
    调用 payload 相对 linkpath parent；`readlink`/DAG/checker 能还原 canonical
    target；任何 target/payload 都不能逃出测试根。
  - PathSet broad 支持 link/symlink/readlink/rename：v1 只迁移 link/symlink
    的 target，linkpath 保持 program-local fresh binding；readlink 不独立迁移；
    symlink payload 在 target 迁移后重算；rename dst 按 `RenamePolicy` 选，
    existing 优先、fresh 兜底；fresh sibling fallback 在 source parent 有效时
    总可构造。
  - PathSet directed target arm 只接受 `len(Paths)==1` 的成员；rename/link/
    symlink 整体排除，readlink 由 call-name gate 排除。
  - PathSet link/symlink target 支持 baseline existing 或更早 creator 的 fresh
    路径；`symlinkCanonicalTarget` 在 PathSet/DCT/DAG/checker 共用；
    symlink payload 两阶段 apply。
  - legacy fallback：rename 走 `RenamePolicy`，HMDFS same-parent 行为不变；
    link/symlink/readlink 不进入 legacy fallback。
  - PathSet 的 `MergeViewPrefix` 硬编码清除；空前缀下可构建、迁移、target arm。
  - `filenameImpl`/通用 fallback 生成 `file0` 而不是 `./file0`；`RootPath()`/
    `JoinRoot()` 在 HMDFS/GlusterFS 下分别为 `merge_view`/`""`；
    `in_userspace=1`、crash recovery test_dir 记录为 profile 行为。
  - 镜像预建 `/root/glusterfs-client`、`/root/glusterfs-server`、
    `/root/dfs-fuzzing/libucov.so`；`eifuzz_root` 由首轮 restore 创建。
  - 镜像包含 `rsync`/`sshpass`/`glusterd`；当前不单独调用 `prefill.sh`，
    首轮 restore 由 reset client 执行，后续如需独立 prologue 再拆脚本。
  - QEMU/SSH：原 Monarch 镜像、默认 `ip=`、key 登录；不把 123456 密码登录
    接入 fuzzer。`glusterfs-config.sh` 复用，cwd `/root`、sshpass/密码或 key
    可用、挂载点存在。
  - fuzzer 入口：GlusterFS 实际走到 `GenerateProgsForDfsFsops` /
    `MutateFsopsWithDCT` / `insertDerivedCallFromDCT`；`ps[0]` 不再被当成
    client；DAG feedback/DAG build 对 GlusterFS 启用。
  - 不生成 C2/DCT fresh root + fresh child；C1 both-fresh 只支持本地 1 层；
    nested fresh parent 不生成。
  - 短 batch/角色错误在 executor 前拒绝。
  - 3-3 config/脚本/mount options/cwd/metadata 全部一致。
  - timing：GlusterFS metadata delay 10s、rsync 总超时 60s、restore
    start/finish barrier 30s/90s；HMDFS 30s/60s/30s/90s 不变；
    `MetadataDelayMs` 未显式设置时按 profile 取默认值。
  - trace 时间对齐复用 `tsc_ns_to_global`/`tsc_offset`/`g_prog_end_raw`/
    ratio/gap；server executor 不 attach，但写 `count=0` 的 trace 头；
    client 的 `trace_root=eifuzz_root`，路径 strip 与该值一致。
  - GlusterFS 不出现 HMDFS agent/peer/settle 等待；client mount barrier 能
    覆盖全部 client 挂载完成。

### 13.1 实现状态与已知边界（2026-11-18）

```text
coverage 三层                        已实现
FreshSame combo-aware plannable      已实现
normal dual root                     已实现，GlusterFS DualPathCausalEnabled=true
GlusterFS DAG feedback               已启用
GlusterFS executor cwd/restore/metadata 已实现
MetadataDelayMs profile default      已实现（HMDFS 30000 / GlusterFS 10000）
GlusterFS CSAN/symsc A/B             已实现
GlusterFS dual-path causal          默认启用（DualPathCausalEnabled=true）
role-aware coverage                 已实现，runtime 待验证
directed diagnostics (noPlan/dcMiss split) 已实现，runtime 待验证
P2 review #7–#11                     已修复，cross-compile/vet 通过
PathSet current state/rename remap   已实现，runtime 待验证
D1 BPF standalone smoke              已通过
D2 executor integration              待执行
D3 short/medium run                  待执行
D4 long run / experiments            待执行
```

D1 BPF standalone 证据：

```text
attach: 3 uprobes + 1 uretprobe
LOOKUP positive mode: 0x4000 (dir) / 0x8000 (file) / 0xA000 (symlink)
negative LOOKUP: ret=-ENOENT, mode=0
path: 已 strip 成 lk/f、lk、lk/l，不带 eifuzz_root/ 前缀
lookup check: total>0, positive_mode_zero=0, negative_mode_nonzero=0
missing_d=0, missing_s=0, dropped=0, unmapped=0
```

D1 同时发现并修复了两个真实问题：

- LOOKUP S 事件原来从 `iov_out[0]`（`fuse_out_header`）读 mode；实际 payload
  在 `iov_out[1]`，`entry_out.attr.mode` 位于 payload + 100；
- standalone 的 `--trace-root` 必须是相对 mount root 的 `eifuzz_root`，
  不能传 `/root/glusterfs-client/eifuzz_root`。

已知边界：

- 非 creation-root 的上下文矛盾仍可能保留在 plannable envelope 中，例如
  `open/mkdir Same Success`、`open/rmdir Same Success`、`open/rename Same
  Success`、`open/getxattr Same Success`、`open/setxattr Same EEXIST`，以及
  readlink 默认 role。这些是 plannable（variant-form 静态上界）和 planner
  构造能力的差异；后续 construction matrix 再收，当前不作为阻塞项。
- potential 目前仍读 plannable：`directedPairPotential` 不减
  `cellSuppressed`，`rootDirectedPotentials` 减；Direction 3 和 2b 的势能
  口径不完全一致。当前不修，D3 用 `noPlan`/`dcMiss` 指标量化。
- normal 2b 和 causal 2b 的 admission 不完全一致：causal `addC2` 不做
  `shapeCovered`；C1 只选 root，不进 pending；DCT/2a 可产生 model-only cell。
  当前作为实现细节保留。
- 只有 link/symlink combo live 时，`planDirectedCellsScan` 在
  `rootsByVariantRel==0` 处提前返回，normal dual pass 不执行；D3 会看到
  `noPlanEmpty`，不再被误报成 `noPlanCombo`。
- `noPlan*` 只归因 normal 2b picker；causal C1/C2 失败和 DCT 不算进去。
  `dcMiss*` 只归因 C2 pending；C1 和 DCT 不产生 `dcMiss`。
- normal dual root 默认进入 plannable/planner；gate=false 的 A/B 配置下
  link 系在 plannable 与 planner 同步排除。
- `missing_r` 当前包含 root GETATTR 的无 path R 事件（D/S 有、R 无），
  后续可单独分类；不影响 LOOKUP mode 验证。
- `dcMisses` 因 fresh-fresh race 构造上升是预期，需要与错误计划区分。诊断行
  现在把它拆成 `dcMissFresh`（`FreshSame` pending）与 `dcMissOther`；拆分只
  做归因，不改变 pending、命中/抑制或反馈口径。
- `MaxDagCorpus` 默认 unlimited；长跑按规模配置并观察 `dag` saturation。
- role-aware cell 会改变 `ObservedCells`、`PlannableTarget` 和
  `RootPlannableTarget` 的分母；D3 与旧日志对比时按新口径解释。

### 13.2 D 验证阶段

D 不是“只跑测试”，而是**验证驱动开发**：先跑最小闭环，用指标决定是否还要
改 trace/parser/DAG/coverage/planner。D1 已经通过 smoke 发现并修复了 BPF
LOOKUP 和 trace-root 两个代码问题。

#### D1：BPF standalone smoke（已通过）

工具：

```text
src/executor/glusterfs_trace_test.cc
make glusterfs-trace-test
```

运行条件：

```text
--fuse-so /usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so
--trace-root eifuzz_root
```

覆盖：

- attach 成功；
- ctl D/S + path R 合并；
- MKDIR/CREATE/OPEN/WRITE/READ/GETATTR/SETATTR/RENAME；
- link/symlink/readlink 的 path/path2；
- setxattr/getxattr user.*；
- LOOKUP 正文件/正目录/symlink/负 lookup；
- 错误 ret/errno。

通过标准：

```text
LOOKUP 正 mode = 0x8000/0x4000/0xA000
负 LOOKUP mode = 0
path 已 strip
missing_d/s = 0
dropped/unmapped = 0
lookup check positive_mode_zero=0, negative_mode_nonzero=0
```

#### D2：executor 集成 smoke（下一步）

目标：

```text
executor attach -> trace 输出 -> Go parser -> DAG -> coverage -> directed
```

步骤：

1. `make executor`；确认 skeleton 已从修复后的 BPF 重建；
2. 3-3 环境挂载 `/root/glusterfs-client`，测试根
   `/root/glusterfs-client/eifuzz_root`；确认 executor 日志的
   `current dir` 是测试根，trace `[path]` 不带 `dfs-<pid>-<iter>/` 前缀；
3. 短跑（10–20 分钟；GlusterFS metadata delay 默认 10s，按实际轮数看曲线）；
4. 先看 executor attach，再看 trace/DAG/coverage/directed。

通过标准：

```text
executor attach 成功
executor cwd = /root/glusterfs-client/eifuzz_root
trace path 无 dfs- 前缀
trace events > 0
fsMds 非空（enable_csan=true 时）
[symsc] init_tree loaded: N DENTRY entries（N > 1）
GlusterFS B 类诊断只出现在 glusterfs-csan.log
无 GlusterFS Nlink/Mtime 误报
DAG vertices > 0
hbPairs 或 ccPairs > 0
coverage obs/model/plan 都有增长
directed dcPlans > 0
dcHits > 0
cfgErr 不增长
无 executor parse/Serialize 错误
MaxDagCorpus/saturatedCalls 未失控
```

失败定位顺序：

```text
1. executor attach
2. trace events / matchFail / progIdxBad
3. DAG vertices/pairs / PathRel / typeFunc / inoFunc
4. coverage obs/model/plan
5. directed plans/hits/noPlan/insertFail
```

#### D3：短/中跑与 noPlan 决策

D3 先看指标，不预设阈值：

- `dcNoPlans`、`dcMisses`、`dcMissFresh`、`dcMissOther`、`dcBatches`；
- `noPlanCombo` / `noPlanCell` / `noPlanEmpty` / `noPlanTree`；
- `obs/outModel/modelOnly/model/plan`、`cellUnreach`/`unreach`；
- `dagCorpusEntries`、`MaxDagCorpus`、`dag` saturation；
- `cfgErr`、`insertFail`、fallback 频率。

`noPlan` 原因计数器已经实现，D3 直接看四类：

- `noPlanCombo` 高：饱和；
- `noPlanCell` 高：covered/suppressed/domain/shape 饱和；
- `noPlanEmpty` 高且集中在非 creation-root combo：plannable/planner 构造缺口，
  考虑 construction matrix；
- `noPlanTree` 高：tree 太小。

`dcMissFresh` 高是 fresh-fresh race 预期；`dcMissOther` 高才重点看错误计划。
只有数据表明 plannable 与 planner 枚举不一致时，才进入代码修复：

- construction matrix / `comboStaticallyUnreachableFor`；
- 收紧 `plannableFormsFor` / `computePlannableLocked`；
- 调整 FreshSame race/model 表；
- 若需要，再拆 `plannerAvailable`/`PlannerPotential`（见 §13.1 已知边界）。

#### D4：长跑与对照实验

D2/D3 通过后再做：

- 小时级/长跑；
- 覆盖曲线、bug 数、首现时间；
- DAG 反馈开/关对照；
- 覆盖率、corpus、smash 预算分析。

### 13.3 noPlan 分类

`directCell=plans/hits/misses/noPlan/insertFail` 里的 `noPlan` 是聚合计数，
不能直接判定为 bug。至少要区分：

| 类别 | 诊断字段 | 含义 | 是否通常需要改代码 |
|---|---|---|---|
| no live combo | `noPlanCombo` | `plannableTarget=0`、全部 discovered、combo unreachable | 否，饱和 |
| no pickable cell | `noPlanCell` | live combo，但候选 cell 全被饱和过滤（`cellUnreachable`/domain/covered/shape） | 否，先看抑制原因 |
| enumeration empty | `noPlanEmpty` | potential > 0、tree 非空，但枚举没有产出可 admission 的候选；包括枚举出 plannable 层之外的 cell（`OutOfPlannable>0`） | 可能是，construction matrix/plannable 过宽 |
| tree too small | `noPlanTree` | live combo 存在但 planning view 为空（缺可构造节点） | 否，环境/预算 |
| cfgErr | `cfgErr` | 配置/前置条件异常 | 是/查配置，不算 noPlan |

分类由 `planDirectedCellsScan` 的 `LiveCombos`/`BNodes`/`Filtered`/
`OutOfPlannable` 和 pick 阶段的 `pickable` 状态决定，只写入诊断计数器，不改变
planner 选择。`noPlan` 仍是四类原因之和；`NoteDirectedNoPlan()` 保留为无原因
兼容入口。

归因边界：

- `noPlan*` 只归因 normal 2b picker；causal C1/C2 失败和 DCT/2a 不算进去；
- `dcMiss*` 只归因 C2 pending；C1 只选 root，DCT/2a 不产生 `dcMiss`；
- 计数器是累计值，D3 判断趋势时用相邻日志的 delta，不看绝对值。

link/symlink root 的 role 已进入 `cellKey`：linkpath-role 只计 HB，linkpath CC
不再进 plannable；plannable 还对 linkpath role 过滤掉不可构造的
`Child`/非 readlink `Same`。因此 `dcNoPlans` 不再被 role-free CC 或这些
relation 虚高。若 link/symlink root 的 noPlan 仍高，按 construction
matrix/关系可行性排查，而不是 role 丢失。

D2 先看 trace/DAG/coverage 是否健康；D3 直接用 `noPlanCombo`/`noPlanCell`/
`noPlanEmpty`/`noPlanTree` 判断 `dcNoPlans` 卡在哪一层。只有 `noPlanEmpty`
高且集中在 construction 不可达的 combo 时，才上 construction matrix。

当前不写死 `dcNoPlans` 阈值；阈值应由 D2/D3 的实际执行率、树规模和
batch 预算决定。

## 14. 文件落点

- `src/prog/dfs_profile.go`：profile、registry、validation。
- `src/prog/hmdfs_config.go`：`DFSConfig` 别名、角色字段。
- `src/prog/dag.go`：事件/角色/path relation/DAG。
- `src/prog/directed_plan.go`：角色 plan/state effect/authority；normal dual
  root pass、`RootLinkPath` grouping、plannable 入池闸门。
- `src/prog/distributed_choice.go`：call table/DCT/path resolution。
- `src/prog/generation.go`、`rand.go`、`mutation.go`：client-only generators/mutators；
  `updateCallPathInProg` 的 legacy rename 走 `RenamePolicy`，link/symlink/readlink
  不进入 legacy fallback。
- `src/prog/path_set.go`：link/symlink target-only broad、prefix-aware、
  rename policy、symlink canonical/payload 映射。
- `src/prog/coverage_board.go`：observed/model/plannable 三层、role-aware
  `roleCellCounts`、combo-aware fresh-fresh race forms、Direction-3 potential/
  enumeration、`cellAdmission`、noPlan/dcMiss breakdown。
- `src/prog/directed_plan.go`：`planDirectedCellsScan`、FreshA/FreshA2/
  FreshSame/normal dual、`variantKindAllowed`、pending 与 batch materialize。
- `src/prog/glusterfs_coverage_test.go`、`glusterfs_plan_test.go`：三层口径、
  race plannable、normal dual root/insert、causal C1/C2 回归测试。
- `src/prog/coverage_role_test.go`、`rename_role_test.go`：role-aware cell/
  plannable relation、rename role-free 与跨路径回归测试。
- `src/prog/causal_root.go`、`seed_derivation.go`、`anchor_path.go`：
  profile planner、`RootPaths`、AnchorPathKind/creator 分类。
- `src/pkg/ipc/ipc.go`：GlusterFS parser、server/client parse split。
- `src/checker/conc-fs.go`、`src/checker/symsc`：A 类检查、profile 过滤。
- `src/syz-fuzzer/proc.go`、`fuzzer.go`：角色 view、seed/corpus 校验；
  GlusterFS DAG feedback（board + corpus/triage）与三层 coverage 日志；
  `dcMissFresh/dcMissOther`、`noPlanCombo/Cell/Empty/Tree` 诊断字段。
- `src/prog/rand.go`、`generation.go`、`mutation.go`：`RootPath()`/`JoinRoot()`、
  `filenameImpl` 空前缀、client-only generators/mutators。
- `src/executor/executor.cc`、`common_linux.h`、`glusterfs_trace.*`、
  `dfs_runtime.*`：runtime/tracer、`in_userspace`、crash recovery test_dir。
- `src/executor/glusterfs_trace_test.cc`、`src/Makefile` 的
  `glusterfs-trace-test`：不依赖 executor/fuzzer 的 standalone BPF
  smoke harness（复用生产 trace 合并逻辑，执行器源文件不变）。
- `src/vm/qemu/qemu.go`：原镜像启动、GlusterFS server data.img、HMDFS gate；
  key 登录与 SSH 配置。
- `src/dfs-config/glusterfs/*`：config/node-up/node-down；config 脚本
  复用，sshpass/key/cwd/挂载点前提；prefill 由首轮 restore 承担。
- `HorcruxSourceCode/generate_test_files.py`：shared 清单。
- `docs/glusterfs-tracer-implementation-spec.md`：BPF raw 细节；冲突以本文为准。
