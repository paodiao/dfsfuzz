# GlusterFS FUSE Tracer 实现规格（B 方案）

> 状态：BPF raw 细节参考；v1 总决策已被 `docs/glusterfs-adaptation-plan.md` 取代；
> tracer/parser 已实现；D1 BPF standalone smoke 已通过（2026-11-17）。
> coverage 三层、combo-aware fresh-fresh race、
> normal dual root 的最终语义见总计划 §8.2.1–§8.2.4。
> 日期：2026-11-14（2026-11-15 修订 v1 调用/事件/角色/checker 决策；
> 2026-11-17 补 coverage/dual-root 交叉引用、D1 smoke 结果）
> 范围：GlusterFS 客户端 FUSE tracer、IPC 解析、DAG 路径接入、FileTree 空前缀、GlusterFS 文件树恢复
> 相关：`docs/glusterfs-adaptation-plan.md`、`docs/glusterfs-config-and-scripts.md`、`docs/glusterfs-nfs-adaptation-plan.md`、`docs/executor-file-tree-restore.md`、`docs/kernel-build-glusterfs-bpf.md`
>
> **权威关系：v1 的调用集、事件集、双路径规则、角色模型、executor/config/checker
> 以 `docs/glusterfs-adaptation-plan.md` 为准。** 本文保留 BPF 原始事件、探针和
> offset 参考；若本文旧段落与总计划冲突，以总计划为准。

## 0. 文档目的

本文是 BPF raw event、探针和 offset 的实现参考。实现时：
- BPF raw event、探针、offset 以本文为准；
- v1 的调用集、事件集、双路径、角色模型、executor/config/checker 以
  `docs/glusterfs-adaptation-plan.md` 为准。

如果实现需要偏离文档，先改对应文档，再改代码。

### 0.1 文档分工

| 内容 | 权威文档 |
|---|---|
| wire event 布局、BPF raw event、ring、探针/offset、userspace 聚合、opcode 映射、IPC parser、构建细节 | **本文** |
| v1 调用集、事件语义、Call→Event matcher、双路径 role、FileTree、DAG/覆盖板、DCT/定向、executor runtime、config/scripts、checker、restore 生命周期、验收 | `docs/glusterfs-adaptation-plan.md` |
| coverage 三层、combo-aware fresh-fresh race、normal dual root | `docs/glusterfs-adaptation-plan.md` §8.2.1–§8.2.4 |
| config 22-call 清单、InitialDir、prefill/restore 命令、node-up/down 参数 | `docs/glusterfs-config-and-scripts.md` |
| restore barrier/生命周期细节 | `docs/executor-file-tree-restore.md` |

本文 §1 是 tracer 视角摘要，不是 v1 策略的重复定义；§8–§16 是 tracer/parser
的实现落点和索引。任何策略冲突以总计划为准，本文只保留 BPF/parser 需要的
事实和检查点。

本文只覆盖 GlusterFS。HMDFS 的 tracer 逻辑保持不变，NFS 暂不实现。

### 0.2 2026-11-15 修订摘要

以下决策以 `docs/glusterfs-adaptation-plan.md` 为准：

- v1 调用集固定 22 个（见总计划 §1.1）：新增 `link/symlink/readlink/getxattr/
  setxattr`；`readv/writev/preadv/pwritev` 不启用、不做 alias；不做 `mknod`。
- GlusterFS 新 FuncID：16 LINK、17 SYMLINK、18 READLINK、19 GETXATTR、20 SETXATTR。
- GlusterFS final wire event 增加 `xattr_name_len/xattr_name[128]`，总大小 1228B；
  raw path event 增加同名 xattr 字段，总大小 1204B。
- READDIR 28 和 READDIRPLUS 44 都映射 ITERATE。
- RENAME2 45 在当前 build 不可达，不再映射；COPY_FILE_RANGE 47 未编译。
- `LINK` 的 `Path`=新链接、`Path2`=源；`SYMLINK` 的 `Path`=linkpath、
  `Path2`=target；xattr 的 `XattrName` 独立字段。
- `SYMLINK Path2` 是原始 payload，不是 canonical FileTree 路径。DAG/定向/
  checker 需要时用 `linkpath + Path2` 解析回测试根内 canonical target；
  `RootPaths`/DCT/PathSet 只保存 canonical target。
- link/symlink/readlink 使用双路径关系规则；**GlusterFS v1 不做
  `PathSameInode`，不新增 `RelKind`，不改 `cellKey`/`comboKey`**；HMDFS
  保留现有分类并继续折叠为 `PathSame`。
- `entry_ino` 不进入 v1 wire event。源码已确认 LINK/SYMLINK/UNLINK/RMDIR/
  RENAME 的 `loc.inode` 在 `fuse_fop_resume` 可用，但 v1 不需要它：
  hardlink 的 inode/fd 事件 path 由 `inode_path` 选任意 dentry，DAG 非
  LOOKUP 顶点改用调用参数/fd provenance，`ev.Path` 只做 fallback/校验。
- client/server 角色、server 空程序、client-only seed/mutation、
  executor runtime、config/scripts 和 A 类 checker 见总计划。
- linkpath AccessMode、跨目录 `RenamePolicy`、DCT seed `readlink` 规则和
  跨节点可见性见总计划 §6.2、§6.5、§8.4。

### 0.3 2026-11-16 P0 复核修订摘要

以下为 5.15 单 VM、GlusterFS 11dev `fuse.so` 的实测结论；实现数值以本节和
§18 为准。

- **D 探针**：`fuse_async_t` 为 iobuf+0 / finh+8 / this+16 / msg+24 /
  async+32；`finh = *(u64 *)(arg0 - 24)`。`fuse_in_header` 的
  opcode/unique/nodeid/pid 在 +4/+8/+16/+32。`pid` 是发起 syscall 的进程，
  不是 glusterfs worker。
- **R 探针**：`fuse_state_t` 的 loc=+32 / loc2=+96 / finh=+160 / off=+176 /
  size=+200 / name=+248；`loc.inode=+16`、`loc2.inode=+112`。
- **mode**：`inode_t` 没有 `i_mode`，只有 `ia_type`（+92）；1/2/3 =
  file/dir/symlink，userspace 映射为 `S_IFMT`。`link` 读 `loc2.inode+92`；
  `rename` 读 `loc.inode+92`；`LOOKUP` 的 R 时 `loc.inode` 是父目录，由 S 的
  reply `fuse_entry_out.attr.mode` 解析；`symlink/mkdir/create` 的新 inode 在
  R 时可能为 0，用函数默认。
- **测试根前缀**：FUSE 路径相对挂载根，实际是 `/eifuzz_root/...`；tracer
  必须 strip `eifuzz_root`。rename/link 的 path2 同样 strip；symlink 的
  path2 是 raw payload，不 strip。
- **truncate**：`off = fsi->size`（新大小），`size = 0`；FUSE SETATTR 不提供
  旧文件大小。read/write 的 off/size 是请求 offset/长度；readdir 的 off/size
  是目录 offset/缓冲区；xattr 的 size 是请求缓冲区。
- **RELEASE/RELEASEDIR**：两者都直接 `send_fuse_err`，没有
  `fuse_fop_resume`，所以只有 D/S、没有 R。v1 保留 RELEASE 18（HMDFS 也
  trace `hmdfs_file_release_local`），过滤 RELEASEDIR 29（HMDFS 不 trace
  目录 release）。
- **hardlink alias**：`inode_path` 对 hardlink 返回任意 dentry；`truncate("q0")`
  的事件 path 可能是 `h1`。DAG 非 LOOKUP 用调用参数/fd provenance；
  planned linkpath 可以作为 truncate/write 的调用参数，内核 alias 不是。
- **xattr**：内部 GETXATTR 会出现 `-61/-95`；D/R 只保留 `user.*`，没有 user R
  的 S-only 内部 xattr 在 userspace 丢弃。
- **符号/构建**：`fuse_dispatch` / `fuse_fop_resume` / `send_fuse_iov` 是
  LOCAL `.symtab`；`send_fuse_iov` 在 P0 build 有 4 个调用点、未被内联；
  P0 host 的 `bpf_uprobe_opts` 没有 `func_name`，统一用 `.symtab` +
  PT_LOAD file offset + `bpf_program__attach_uprobe`。
- **时间对齐**：直接复用 `tsc_ns_to_global` / `tsc_offset` / `g_prog_end_raw` /
  ratio/gap；ctl/path 两个 ring 必须按 ts 合并。

## 1. 已定决策

> 本节是 tracer 视角摘要；v1 权威定义见 `glusterfs-adaptation-plan.md`
> §1、§4、§6。

| 项目 | 决定 |
|---|---|
| GlusterFS tracer 方案 | B：独立 wire event，独立 FUSE tracer，共用 Go/DAG 接口 |
| HMDFS wire event | 不变，仍为 112 字节 |
| HMDFS ring | 不变，256 KB |
| GlusterFS path/path2 | 512 字节 |
| GlusterFS ctl ring | 1 MB |
| GlusterFS path ring | 4 MB |
| `MAX_GLUSTERFS_TRACE_EVENTS` | 8192 |
| executor 输出缓冲区 | 沿用 16 MB |
| 路径根 | 空前缀，根 = 固定测试根 |
| GlusterFS restore | client-side logical restore |
| FileTree 空前缀 | 与 tracer 同一波实现 |
| HMDFS 回归要求 | HMDFS wire、探针、行为不变 |
| 拓扑 | 3 server + 3 client，replica 3 |
| data.img | 不启用，brick 在 server rootfs |
| cid | `"shared"` |
| baseline | `/home/baseline/glusterfs/`，烤进 client 镜像 |
| prefill | client1 挂载后 rsync baseline 到固定测试根 |
| reset client | `executor_index == server_num`，3-3 下为 index 3 |
| CSAN | `enable_csan=true`，`enable_c2san=false` |
| testdirIno | 固定测试根 `/root/glusterfs-client/eifuzz_root` |
| v1 调用集 | 22 个：`open/read/write/close/stat/pread64/pwrite64/fsync/fdatasync/truncate/getdents64/rename/mkdir/rmdir/creat/unlink/chmod/link/symlink/readlink/getxattr/setxattr` |
| v1 不做 | `readv/writev/preadv/pwritev`、at 变体、`mknod`、stash、dcache、failure/C2san/NetFailure/NodeCrash；persistence 只关闭配置/覆盖维度（不是 seed family） |
| 角色模型 | client-local view + empty server progs；absolute executor index |
| GlusterFS checker | 只硬判 A 类强字段；B 类字段仅诊断 |
| 双路径 | link/symlink/readlink 规则见总计划 §6 |

B 的准确含义：

```text
GlusterFS 在 executor/IPC 层使用独立 wire event
HMDFS 的 C 结构、BPF ring、112 字节 wire event 完全不动
Go 层给 HmdfsTraceEvent 增加可选 Path/Path2/NodeID/Truncated 字段
DAG 共用
```

### 1.1 内核与运行环境前置条件

当前实现依赖已构建的 5.15 内核：

```text
BTF 可用
uprobe/UPROBE_EVENTS 可用
BPF_SYSCALL/BPF_EVENTS/BPF_JIT 可用
FUSE_FS 可用
KASAN/SLUB/SELinux 保持
IMA/EVM/INTEGRITY 关闭
```

已知构建问题、完整配置和验证命令见：

```text
docs/kernel-build-glusterfs-bpf.md
```

### 1.2 部署、prefill 与 CSAN

- 3-3 + replica 3；6 VM，每台 2C/10G；资源不足时只调 `vm.mem`。
- `data.img` 不启用；QEMU 仍会挂给 server，但 mkfs/mount 保持注释，brick 在 rootfs 的 `/root/glusterfs-server`。
- host 用 `generate_test_files.py shared ...` 生成一棵树；树本体复制到 client 镜像 `/home/baseline/glusterfs/`，清单为 `shared.file/.dir/.tmpdir` 和 `empty_dirs.info`。
- GlusterFS 不使用 `large_dir.info`；生成时 `--num-large-files 0`。
- client1 挂载后由首轮 restore 执行 `rm -rf` 固定测试根 + rsync baseline +
  `sync`；其他 client 只等待 barrier。当前没有独立 `prefill.sh`。
- 每轮 restore 复用同一份 baseline；reset client 固定 `executor_index == server_num`。
- CSAN：
  - `enable_csan=true`，`enable_c2san=false`。
  - `fsMds` 只保留 client 固定测试根下路径；server fsMd 不参与 pairwise。
  - `initTreeSubset`、symsc 路径归一化改为空前缀。
  - 旧 `dfs-*`、`lost+found`、server brick 路径不进入 CSAN。
  - `testdirIno` 固定为固定测试根 inode。

## 2. P0 实测结论

P0 环境：单 VM，GlusterFS 单节点 volume，FUSE 挂载成功，内核 BTF/uprobe 可用。

已验证：

```text
send_fuse_iov / fuse_dispatch / fuse_fop_resume 均可 attach
D/R/S 的 unique 可配对
rename 同时有 Path 和 Path2
READDIRPLUS/READ 的 Path 为空，但 nodeid 可映射
P0 时 GETXATTR/FLUSH/p=0/FORGET 属于需过滤或特殊处理的请求；v1 修订后 GETXATTR 只保留 user.*（见 §5.4）
```

关键事实：

```text
finh->nodeid 是 inode_t *，不是 st_ino
```

源码依据：

```text
xlators/mount/fuse/src/fuse-helpers.c
  fuse_ino_to_inode():  inode = (inode_t *)(unsigned long)ino
  inode_to_fuse_nodeid(): return (unsigned long)inode
  根 inode 特判为 1
```

因此：

- `nodeid` 不能直接作为 `fsMd.StatMd.Ino`。
- `nodeid` 可作为挂载生命周期内的映射键。
- 路径优先使用 `fuse_fop_resume` 的 `loc.path`。
- fd 类操作通过 `nodeid -> path` 补齐。

P0 样例：

```text
D o=12 u=680 n=1
R o=12 u=680 p=/q0
R2 /q1
S o=12 u=680
```

```text
D o=44 u=604 n=1
R o=44 u=604 p=
S o=44 u=604
```

```text
D o=15 u=638 n=106721347771096
R o=15 u=638 p=
S o=15 u=638
```

## 3. 总体架构

HMDFS 路径不变：

```text
kprobe/kretprobe
  -> hmdfs_trace.bpf.c
  -> 112B merge_trace_event
  -> hmdfs_trace.cc
  -> parseHmdfsTraceEvents
  -> BuildVertices
```

GlusterFS 路径：

```text
uprobe/uretprobe
  -> glusterfs_trace.bpf.c
  -> raw ctl/path events
  -> glusterfs_trace.cc
  -> glusterfs_trace_event (1228B)
  -> parseGlusterfsTraceEvents
  -> prog.HmdfsTraceEvent（Go 层带 Path/Path2/XattrName）
  -> BuildVertices
```

约束：

- 不修改 `hmdfs_trace.bpf.c` 的 wire 结构。
- 不修改 `parseHmdfsTraceEvents` 的读取布局。
- 不修改 HMDFS 的 ring 大小。
- 只给 Go 的 `HmdfsTraceEvent` 增加可选字段。
- DAG 不复制。

## 4. GlusterFS wire event

### 4.1 结构

新增 `src/executor/glusterfs_trace.h`：

```c
#define GLUSTERFS_TRACE_PATH_MAX 512
#define GLUSTERFS_TRACE_XATTR_MAX 128
#define MAX_GLUSTERFS_TRACE_EVENTS 8192

struct glusterfs_trace_event {
    uint64_t timestamp;      /* 0 */
    uint64_t entry_ts;       /* 8 */
    uint32_t func_id;        /* 16 */
    int32_t  ret;            /* 20 */
    uint64_t nodeid;         /* 24 */
    uint64_t unique;         /* 32 */
    uint64_t off;            /* 40 */
    uint64_t size;           /* 48 */
    uint32_t mode;           /* 56 */
    uint32_t path_len;       /* 60 */
    uint32_t path2_len;      /* 64 */
    uint32_t xattr_name_len; /* 68 */
    uint32_t truncated;      /* 72 */
    char     path[512];      /* 76 */
    char     path2[512];     /* 588 */
    char     xattr_name[128];/* 1100 */
};                           /* 1228 */
```

**对齐要求：上面是 packed 布局。C 结构必须 `__attribute__((packed))` 或
手写序列化；默认 8 字节对齐时 `sizeof` 是 1232B。C 端 `sizeof`、executor
输出长度和 Go parser 必须使用同一个尺寸。**

v1 不加 `entry_ino`。源码已确认 `loc.inode` 在 LINK/SYMLINK/UNLINK/RMDIR/
RENAME 的 `fuse_fop_resume` 可用，但 hardlink 的 inode/fd 事件 path 是
`inode_path` 任意 dentry；v1 的 DAG 路径改用调用参数/fd provenance，不再
需要这个字段。若以后要做 canonicalization，再扩展 raw/final event 并同步
本文和 parser。

字段语义：

| 字段 | 语义 |
|---|---|
| `timestamp` | `send_fuse_iov` 返回时间（uretprobe），作为事件结束时间 |
| `entry_ts` | `fuse_dispatch` 开始时间；缺失时为 0 |
| `func_id` | 0–14 通用 + 16–20 GlusterFS；15 仅 HMDFS |
| `ret` | `fuse_out_header.error`；0 成功，负数 errno |
| `nodeid` | FUSE nodeid；GlusterFS 中是 `inode_t *`；LOOKUP 时是父目录 |
| `unique` | FUSE 请求唯一号，D/R/S 关联键 |
| `off` | read/write 的 offset；truncate 的新大小；readdir 的目录 offset；未知为 0 |
| `size` | read/write 的请求长度；truncate 为 0；readdir 的缓冲区大小；xattr 的请求缓冲区；未知为 0 |
| `mode` | `loc.inode->ia_type` 映射成 `S_IFMT`；link 用 `loc2.inode`；LOOKUP 用 S reply 的 `fuse_entry_out.attr.mode`；symlink/mkdir/create 未知为 0 |
| `path_len` | `path` 实际字节数，不含 NUL |
| `path2_len` | `path2` 实际字节数，不含 NUL |
| `xattr_name_len` | `xattr_name` 实际字节数，不含 NUL；上限 128 |
| `truncated` | 位图：bit0=path 截断，bit1=path2 截断，bit2=xattr 截断 |
| `path` | 归一化到**测试根**的路径，不带前导 `/`；测试根为空 |
| `path2` | rename 目标 / link 源：同样归一化到测试根；symlink target：原始 payload |
| `xattr_name` | xattr key；非 xattr 事件为空 |

`mode` 的来源不是 `i_mode`：

```text
inode_t.ia_type = inode + 92
IA_IFREG 1 -> S_IFREG
IA_IFDIR 2 -> S_IFDIR
IA_IFLNK 3 -> S_IFLNK
其他/0     -> 0（DAG 用函数默认/fsMd 回退）
```

- `rename`：源 inode 用 `loc.inode + 92`；
- `link`：源 inode 用 `loc2.inode + 92`（R 发生在 `fuse_link_resume` 之前，
  `loc.inode` 还是空的）；
- `lookup`：R 时 `loc.inode` 是父目录，不能用于 entry 类型；由 S 的
  `send_fuse_iov` reply 解析 `fuse_entry_out.attr.mode`：
  `iov_out[0]` 是 16B `fuse_out_header`，`iov_out[1].iov_base + 100` 才是
  entry_out.attr.mode；`send_fuse_err` 没有 payload，negative lookup 为 0；
- `symlink/mkdir/create`：新 inode 在 R 时 `ia_type=0`，用函数默认；
- `symlink` 的 `path2` 是 payload，不能 strip；`rename/link` 的 `path2` 是
  挂载内路径，必须 strip。

### 4.2 序列化

输出格式与 HMDFS 相同：

```text
ratio  u64
offset u64
gap    u64
count  u32
events[count]
```

每个 event 固定 1228 字节。parser 必须按固定 stride 读完整 512/512/128
字节数组，再按对应 length 截取，不依赖 NUL；不要把 wire event 写成变长
（§8 的读取顺序必须与本节一致）。C 结构必须 packed，并加
`_Static_assert(sizeof(struct glusterfs_trace_event) == 1228)`。

### 4.3 大小与容量

```text
final event: 1228 B
8192 events: 10,059,776 B ≈ 9.6 MiB
16 MB 输出缓冲区: 足够
```

## 5. BPF raw event 与探针

### 5.1 两个 ring

新增 `src/executor/glusterfs_trace.bpf.c`。

```c
struct glusterfs_ctl_event {
    u64 ts;       /* 0 */
    u64 unique;   /* 8 */
    u32 phase;    /* 16: D=0, S=1 */
    u32 opcode;   /* 20 */
    u64 nodeid;   /* 24 */
    u32 pid;      /* 32: finh->pid（发起 syscall 的进程） */
    s32 error;    /* 36: 仅 S 有值；D/R 为 0 */
    u32 mode;     /* 40: 仅 S 的 LOOKUP reply entry type */
};                /* 44 */
```

```c
struct glusterfs_path_event {
    u64 ts;             /* 0 */
    u64 unique;         /* 8 */
    u64 off;            /* 16 */
    u64 size;           /* 24 */
    u32 mode;           /* 32 */
    u32 path_len;       /* 36 */
    u32 path2_len;      /* 40 */
    u32 xattr_name_len; /* 44 */
    u32 truncated;      /* 48 */
    char path[512];     /* 52 */
    char path2[512];    /* 564 */
    char xattr_name[128];/* 1076 */
};                      /* 1204 */
```

raw path event 也必须是 packed/手写序列化；默认 8 字节对齐时是 1208B。

ring：

```c
ctl_events:  max_entries = 1 * 1024 * 1024
path_events: max_entries = 4 * 1024 * 1024
```

说明：

- `ctl` 只放 D/S，避免路径字段浪费。
- `path` 只放 R，携带路径。
- `path` ring 4 MB / 1204B ≈ 3480 条 R 事件（含 ring 头会略少）。
- BPF 只采集，不做路径聚合。
- **两个 ring 是独立消费的，跨 ring 不保证 D/R/S 顺序**。userspace 必须把
  ctl 和 path 全部 drain 后按 `ts` 合并（同 ts 用 D < R < S），再做
  pending 聚合；不能先消费一个 ring 就按 S 生成最终事件。
- 每个请求自身的 D→R→S 因果顺序仍然成立；D/R/S 可以在不同 worker 线程，
  userspace 只用 `unique` 配对，不按线程/时间片假设连续性。

### 5.2 探针与偏移

源码：

```text
xlators/mount/fuse/src/fuse-bridge.c
  send_fuse_iov    :352
  fuse_fop_resume  :1149
  fuse_dispatch    :6068
```

`fuse_dispatch`：

```c
typedef struct _fuse_async {
    struct iobuf *iobuf;      /* 0 */
    fuse_in_header_t *finh;   /* 8 */
    xlator_t *this;           /* 16 */
    void *msg;                /* 24 */
    gf_async_t async;         /* 32 */
} fuse_async_t;
```

```text
finh = *(u64 *)(arg0 - 32 + 8) = *(u64 *)(arg0 - 24)
```

`fuse_in_header`（UAPI，40B）：

```text
len    +0
opcode +4
unique +8
nodeid +16
uid    +24
gid    +28
pid    +32
```

P0 原始内存已验证：`arg0` 是 `&fasync->async`，`fasync = arg0 - 32`；
`fasync->iobuf=+0`、`finh=+8`、`this=+16`、`msg=+24`；`finh` 指向输入
缓冲区 base，`msg = finh + sizeof(fuse_in_header)`。

`fuse_fop_resume`：

```text
fuse_state_t.finh       = state + 160
fuse_state_t.loc        = state + 32
fuse_state_t.loc2       = state + 96
loc_t.path              = loc + 0
loc_t.inode             = loc + 16
loc_t.name              = loc + 8
fuse_state_t.off        = state + 176
fuse_state_t.size       = state + 200
fuse_state_t.name       = state + 248   /* symlink target / xattr key */
fuse_state_t.loc.inode  = state + 48
fuse_state_t.loc2.inode = state + 112
inode_t.ia_type         = inode + 92
```

`ia_type` 在 P0 build 已验证为 +92（+132 是垃圾值）；`link` 的源 inode 在
`loc2.inode`，`rename` 的源 inode 在 `loc.inode`。

`send_fuse_iov`：

```text
finh    = arg1
iov_out = arg2
fuse_out_header = iov_out[0].iov_base
error   = *(s32 *)(fuse_out_header + 4)
```

**S 必须是 uretprobe，且 error 在入口捕获**：

```text
entry uprobe:
  send_state[pid_tgid] = {finh, unique, opcode, pid, nodeid, error}
  error = *(s32 *)(iov_out[0].iov_base + 4)
return uretprobe:
  val = send_state[pid_tgid]; 若缺失则跳过
  S.timestamp = bpf_ktime_get_ns()
  写 ctl S（用保存的 unique/opcode/pid/nodeid/error）
  删除 send_state[pid_tgid]
```

不能在 uretprobe 读 `arg1/arg2`：HMDFS 的 retprobe 教训同样适用；而且
`send_fuse_err` 是 `return send_fuse_iov(...)`，-O2 可能尾调用，返回时
caller 栈帧可能已失效。入口捕获 error 后，返回只取时间。

`fuse_release` / `fuse_releasedir` 直接 `send_fuse_err`，不经过
`fuse_fop_resume`，因此：

```text
FUSE_RELEASE 18     -> D + S，无 R
FUSE_RELEASEDIR 29  -> D + S，无 R
```

v1 保留 18、过滤 29（见 §5.4）；18 的 path 只能从 nodeid 映射或 fd
provenance 来，不计 `missing_r`。

**符号与 attach**：

```text
P0 image fuse.so（LOCAL .symtab）：
  fuse_dispatch    = 0x5e940, 583B
  fuse_fop_resume  = 0x68be0, 338B
  send_fuse_iov    = 0x216a0, 832B
send_fuse_iov 在 P0 build 有 4 个 callq（394/986/2780/3015），未被内联。
```

- P0 host 的 `bpf_uprobe_opts` **没有 `func_name`**
  （`bpf_uprobe_opts__last_field=retprobe`），不能按函数名 attach。
- 统一用
  `bpf_program__attach_uprobe(prog, retprobe, -1, fuse_so, file_offset)`；
  `send_fuse_iov` 的 `retprobe=true`，`fuse_dispatch` / `fuse_fop_resume`
  为 false。
- libelf 解析 `.symtab`（三个符号都是 LOCAL），按符号所在 PT_LOAD 计算：
  ```text
  file_offset = st_value - p_vaddr + p_offset
  ```
  不直接把 `st_value` 当文件偏移；多数 shared lib 两者相同，但实现不能赌。
- P0 的 `fuse.so` 有 `.symtab`；如果镜像重建/strip，符号和 offset 都要
  重新确认。

### 5.3 用户态内存读取

这些地址都在用户态：

- `arg0` 指向 `gf_async_t`
- `fuse_in_header_t *finh`
- `fuse_state_t *state`
- `state->loc.inode` / `loc2.inode`（读 `ia_type`）
- `loc.path` / `loc2.path` / `state->name`
- `iov_out[0].iov_base`

因此 BPF 必须使用用户态读取：

```text
bpf_probe_read_user
bpf_probe_read_user_str
```

路径拷贝直接写入 ringbuf 预留区，避免在 BPF 栈上同时放 `path[512] + path2[512]`。
字符串读取失败时 length=0，不视为路径；超过 512/128 时设置对应
`truncated` 位。raw event 用 packed/手写序列化，并加
`_Static_assert(sizeof(struct glusterfs_path_event) == 1204)`。

5.15 verifier 已确认该写法合法：

```text
bpf_probe_read_user_str.arg1 = ARG_PTR_TO_UNINIT_MEM
verifier mem_types 包含 PTR_TO_MEM
bpf_ringbuf_reserve.ret_type = RET_PTR_TO_ALLOC_MEM_OR_NULL
  -> NULL 检查后 PTR_TO_MEM，mem_size = sizeof(glusterfs_path_event) = 1204
path +52/512、path2 +564/512、xattr +1076/128 均在 [0, 1204) 内。
```

因此 path/path2/xattr 直接写 ringbuf 预留区不需要 per-CPU scratch map。

**bpftrace 验证注意**：旧版 bpftrace 对 `$var = arg0 - 24` 这类“变量 + 负
偏移”可能读 0；P0 用嵌套直接表达式
`*(uint32 *)(*(uint64 *)(arg0 - 24) + 4)` 验证成功。正式 executor 的 BPF
用 `PT_REGS_PARM1 - 24`，不受该工具问题影响。

### 5.4 过滤

在 BPF 侧过滤：

```text
finh->pid == 0
FUSE_FORGET        2
FUSE_FLUSH         25
FUSE_INIT          26
FUSE_RELEASEDIR    29
FUSE_INTERRUPT     36
FUSE_DESTROY       38
FUSE_BATCH_FORGET  42
```

`FUSE_SETXATTR`/`FUSE_GETXATTR` 不整体过滤；只保留 `user.*`，过滤
`security.*`、`trusted.*`、`system.*`、虚拟 `glusterfs.*`/GFID 和
`inode-invalidate`。内部 GETXATTR 在 P0 出现 `-61 (ENODATA)` /
`-95 (EOPNOTSUPP)`；没有 user R 的 xattr S 在 userspace 丢弃，不能生成
无 name/path 的最终事件。

保留：

```text
1  LOOKUP
3  GETATTR
4  SETATTR
5  READLINK
6  SYMLINK
9  MKDIR
10 UNLINK
11 RMDIR
12 RENAME
13 LINK
14 OPEN
15 READ
16 WRITE
18 RELEASE
20 FSYNC
21 SETXATTR（仅 user.*）
22 GETXATTR（仅 user.*）
27 OPENDIR
28 READDIR
35 CREATE
44 READDIRPLUS
```

`FUSE_RELEASE` 保留：HMDFS 也 trace `hmdfs_file_release_local`，它的
`FuncRelease` 会匹配 `close`，作为 DAG B/CC 侧。`FUSE_RELEASEDIR` 过滤：
HMDFS 不 trace 目录 release（`hmdfs_dir_release_merge` 没有对应探针）。
`FUSE_RENAME2 45` 当前 build 不可达，不映射。

- 18 只有 D/S、没有 R；path 由 OPEN 的 nodeid map 或 fd provenance 提供，
  不计 `missing_r`。
- 29 直接过滤，不产生 raw/final event；不要为了目录 close 去补 R。
- D/R/S 使用同一张 opcode 分类表：18 保留，29 过滤；不能只在某一个探针
  里放过/丢掉，否则会破坏 unique 配对计数。

## 6. Userspace 聚合与路径映射

### 6.1 状态机

`glusterfs_trace.cc` 维护：

```text
pending: unique -> {
  start_ts, opcode, nodeid, pid,
  path, path2, xattr_name, off, size, mode, truncated
}
```

流程：

```text
stop_collect（父进程）:
  1. 分别 drain ctl ring（D/S）和 path ring（R）到内存
  2. 按 ts 合并两路事件（同 ts：D < R < S）
  3. 按 unique 建 pending；D/R/S 可以以任意跨请求顺序到达
  4. 每个 unique 最终以 S 生成 glusterfs_trace_event
  5. 删除 pending；清空 nodeid map（本轮结束）
```

- D/R/S 可以在不同 worker 线程；同一 unique 的因果顺序 D→R→S 成立，
  但跨 unique 会交错，不能假设连续。
- `pid` 用 `finh->pid`（发起 syscall 的进程），不是 glusterfs worker；
  pending 只按 `unique` 配对，一个 client VM 只允许一个 mount。
- 不能在“收到 S”时立即 finalize：ctl/path 两个 ring 独立 drain，S 可能
  先于 R 被读到；必须先合并/收集完本轮事件。
- `start_glusterfs_trace()` 在子进程里只做 ring drain（丢弃 stale 事件）；
  pending/nodeid map 的清理放在父进程 `stop_collect` 末尾，避免 fork COW
  子进程清了个副本、父进程仍保留上一轮状态。

异常：

| 情况 | 处理 |
|---|---|
| S 无 D | 生成事件，`entry_ts=0`，计数 `missing_d` |
| S 无 R | path 为空，尝试 nodeid 映射；非 xattr、非 18 计数 `missing_r` |
| S 无 R 且是 xattr | 丢弃（没有 name/path，不能生成最终事件） |
| S 无 R 且 opcode=18 | 正常；path 走 fd provenance/nodeid，不计 `missing_r` |
| R 无 D | 保留 pending，等 S；不能直接丢弃 |
| R 无 S | 丢弃，计数 `missing_s` |
| D 无 S | stop 时丢弃，计数 `missing_s` |
| path 截断 | 设置 `truncated` 位；优先用 nodeid 映射补全；仍无完整路径则 DAG 丢弃 |
| ring 溢出 | 记录 drop 计数，不静默 |

`no_r_by_design` 只有 `FUSE_RELEASE 18`；`FUSE_RELEASEDIR 29` 在 §5.4
直接过滤，不进入本状态机。

### 6.2 路径归一化

在 `glusterfs_trace.cc` 中完成。GlusterFS 的 FUSE 路径相对**挂载根**
`/root/glusterfs-client`，而 FileTree/调用参数相对**测试根**
`/root/glusterfs-client/eifuzz_root`；tracer 必须去掉测试根这一层：

```text
/eifuzz_root          -> ""
/eifuzz_root/q0       -> "q0"
/eifuzz_root/a/b      -> "a/b"
/                     -> out-of-root，丢弃
/.glusterfs/...       -> out-of-root，丢弃
```

规则：

- 先去掉前导 `/`，再 strip `trace_root`（GlusterFS 固定 `eifuzz_root`；
  可通过 init 参数覆盖）。
- 测试根本身归一化为空字符串。
- 不在 trace root 下的路径标记 out-of-root，最终事件丢弃并计数；不能把
  mount root 或 `.glusterfs` 内部路径带进 DAG。
- 不做 `merge_view/` 前缀。
- `path2`：rename/link 是挂载内路径，同样 strip；symlink 的 `path2` 是原始
  payload，不 strip。
- 超过 512 字节：截断并设置 `truncated` 位。
- `nodeid -> path` 映射值也使用同一归一化规则。

### 6.3 `nodeid -> path`

只对“`nodeid` 指向操作目标 inode”的事件建立映射：

```text
GETATTR 3
SETATTR 4
SETXATTR 21
GETXATTR 22
OPEN 14
OPENDIR 27
```

不能建立目标映射的事件：

```text
LOOKUP 1
MKDIR 9
UNLINK 10
RMDIR 11
RENAME 12
CREATE 35
```

原因：

- `LOOKUP/MKDIR/CREATE/UNLINK/RMDIR/RENAME` 的 `nodeid` 是父目录。
- 用它们把父目录映射到目标路径会造成错误映射。

补路径：

```text
READ 15
WRITE 16
RELEASE 18
READDIR 28
READDIRPLUS 44
部分 GETATTR 3
```

- 只有存在 R 且 D 的 opcode 属于上表“目标 inode”集合时，才写入映射；
  LOOKUP/MKDIR/CREATE/UNLINK/RMDIR/RENAME 的 `nodeid` 是父目录，不能写。
- `RELEASE 18` 没有 R，不能从自己的 R 建立映射；它的 path 来自同程序
  OPEN 建立的映射或 fd provenance。
- `nodeid` 指针可能在同一轮内被 inode 释放/复用；映射只做 fallback，
  DAG 非 LOOKUP 仍以调用参数/fd provenance 为准。

特殊：

```text
nodeid == 1 -> mount root "/"，out-of-root，不映射
测试根 /eifuzz_root -> strip 后 ""
```

不要把 `nodeid == 1` 映射成测试根 `""`：1 是 FUSE 挂载根
`/root/glusterfs-client`，不是 `/root/glusterfs-client/eifuzz_root`。

hardlink alias：GETATTR/OPEN/WRITE 的 `nodeid` 是目标 inode，事件 path 由
`inode_path` 决定，可能是 target，也可能是某个 alias。源码依据：

```text
fuse-helpers.c: fuse_loc_fill -> inode_path(inode, NULL)
fuse-resolve.c: loc_touchup -> inode_path(loc->inode, 0)
inode.c: inode_path -> __dentry_search_arbit
inode.c: inode_link -> list_add(head)
```

因此 v1 不把事件 path 当 hardlink 的权威路径，也不做 canonicalization：
DAG 非 LOOKUP 顶点用调用参数/fd provenance（§9），`ev.Path` 只做 fallback/
校验；不据 `inode_path` 做 `PathSameInode`。

每轮在父进程 `stop_collect` 末尾清空 `nodeid -> path`，避免 inode 指针复用；
`start_glusterfs_trace()` 在 fork 子进程里只 drain ring，不能承担清理职责
（COW 只改子进程副本）。

P0 实测 `truncate("q0")` 的事件 path 可能是 hardlink alias `h1`，因为
`inode_path` 在 q0/h1 间任选 dentry。事件 path 不是 canonical 调用参数；
DAG 必须用调用参数/fd provenance，`ev.Path` 只做 fallback/校验。

### 6.4 双路径操作

`RENAME 12`：

- `path` = 旧路径
- `path2` = 新路径
- 两者都做 trace-root strip；`path2` 为空时表示未捕获，不要用 `path` 代替。
- mode 来自源 inode：`loc.inode + 92`（P0 目录 rename 已验证 `ia_type=2`）。
- `path2`/`loc2.inode` 只做 rename 目标校验；DAG 用调用参数 `src/dst`。
- `RENAME2 45` 在当前 FUSE 7.24 build 无 handler，不映射、不生成。

`LINK 13`：

- `path` = 新链接路径
- `path2` = 源 inode 路径（可能是 `inode_path` 的任意 alias）
- R 发生在 `fuse_link_resume` 之前，`loc.inode` 还是空的；**源 inode 的
  `ia_type`/mode 必须读 `loc2.inode + 92`**，不能读 `loc.inode`。
- v1 不写 `EntryIno`。DAG `link` 顶点用调用参数 `new/old` 作为
  `Path/Path2`，不取事件 `loc2.path`。

`SYMLINK 6`：

- `path` = 新链接路径
- `path2` = target 字符串（原始 payload）
- `path2` 不是 canonical FileTree 路径；它按 linkpath 的父目录解析。DAG/定向
  使用 target 做路径关系时，必须先用 `linkpath + path2` 还原 canonical target，
  再和 FileTree/调用路径比较；不要直接把 payload 当 FileTree 节点。
- `path2` 不做 trace-root strip（它是 payload，不是挂载路径）。
- R 时新 symlink inode 的 `ia_type` 可能是 0；mode 用 FuncSymlink 默认
  （S_IFLNK/非目录）。
- v1 不写 `EntryIno`。`loc.inode` 是新 symlink inode；linkpath/target 都由
  syscall 参数精确给出。symlink follow 的底层访问由 VFS 解析到 target 路径，
  DAG `stat(L1)` 仍用调用参数 `L1`，不取 FUSE GETATTR 的 target path。

`READLINK 5`：

- `path` = 链接路径
- 不记录 target；readlink 的 role 只用 linkpath。若 checker/symsc 需要 target，
  用 syscall 输出或 fsMd 的 `SymlinkPath`，并按 linkpath 解析回 canonical target。

`SETXATTR 21` / `GETXATTR 22`：

- `path` = 目标路径
- `xattr_name` = key
- `size` 是请求缓冲区/值长度，不是文件大小；`getxattr(size=0)` 是合法的
  size 查询。
- 只保留 `user.*`；虚拟 `glusterfs.*`/GFID 不进入事件。内部 GETXATTR 的
  `-61/-95` 证实了 filter 的必要性；没有 user R 的 S 在 userspace 丢弃。
- `xattr_name` 不做 trace-root strip。

### 6.5 mode / off / size

`mode`：

- 来自 `inode_t.ia_type`（+92），映射成 `S_IFMT`；不是 `i_mode`，也没有
  `i_mode` 字段。
- `LOOKUP` 例外：R 时 `loc.inode` 是父目录，entry 类型由 S reply 的
  `fuse_entry_out.attr.mode` 解析；negative lookup 为 0。
- `rename` 用 `loc.inode`；`link` 用 `loc2.inode`；`symlink/mkdir/create`
  新 inode 在 R 时可能为 0，用函数默认；`symlink` 默认非目录，其 target
  类型由 DAG 的调用链近似解析。
- `funcDefaultIsDir`：`link/symlink/readlink` 默认 false；
  `getattr/setattr/rename/xattr` 未知，走 mode/fsMd 回退。
- DAG 的 `eventIsDir` 在 `mode=0` 时使用 `funcDefaultIsDir` 或 fsMd 回退。

`off/size`：

- read/write：`off` = 请求 offset，`size` = 请求长度；
- truncate：`off` = `fsi->size`（新大小），`size` = 0；FUSE SETATTR 请求
  不提供旧文件大小；
- chmod：`off=size=0`（DAG 用 call-name gate 排除，不当成 truncate to 0）；
- readdir/readdirplus：`off` = 目录 offset，`size` = 缓冲区；
- getxattr/setxattr：`size` = 请求缓冲区/值长度；
- 失败/未知：0。

## 7. Opcode 映射

```text
FUSE_LOOKUP       1  -> lookup
FUSE_GETATTR      3  -> getattr
FUSE_SETATTR      4  -> setattr
FUSE_READLINK     5  -> readlink
FUSE_SYMLINK      6  -> symlink
FUSE_MKDIR        9  -> mkdir
FUSE_UNLINK       10 -> unlink
FUSE_RMDIR        11 -> rmdir
FUSE_RENAME       12 -> rename
FUSE_LINK         13 -> link
FUSE_OPEN         14 -> open
FUSE_READ         15 -> read
FUSE_WRITE        16 -> write
FUSE_RELEASE      18 -> release      （D/S，无 R；保留）
FUSE_RELEASEDIR   29 -> 过滤          （无 R；HMDFS 不 trace 目录 release）
FUSE_FSYNC        20 -> fsync
FUSE_SETXATTR     21 -> setxattr
FUSE_GETXATTR     22 -> getxattr
FUSE_OPENDIR      27 -> dir_open
FUSE_READDIR      28 -> iterate
FUSE_CREATE       35 -> create
FUSE_READDIRPLUS  44 -> iterate
```

不映射：

```text
FUSE_RENAME2 45  当前 build 无 handler，ENOSYS
FUSE_COPY_FILE_RANGE 47  未编译
FUSE_SYNCFS/TMPFILE/STATX  当前 header 不存在
```

事件契约：

```text
0 MKDIR
1 CREATE
2 RMDIR
3 UNLINK
4 RENAME
5 WRITE
6 READ
7 FILE_OPEN
8 DIR_OPEN
9 GETATTR
10 SETATTR
11 FSYNC
12 RELEASE
13 LOOKUP
14 ITERATE
15 WRITEPAGE_CB（仅 HMDFS）
16 LINK
17 SYMLINK
18 READLINK
19 GETXATTR
20 SETXATTR
```

GlusterFS 不发送 15；`FuncIDCount=21`；`DagDiag` 数组扩到 21 或动态。

`FuncRelease 12` 在 v1 是**文件 close**：HMDFS 也 trace
`hmdfs_file_release_local`，并映射到 `close` 调用；close 不是 modifier，但
可以作为 HB/CC 的 B 侧，DAG pair 保留。`FUSE_RELEASEDIR 29` 不产生事件，
所以目录 close 没有顶点；这与 HMDFS（只 trace 文件 release）一致。

## 8. IPC 解析

> 本节只保留 wire→Go parser 的实现细节；v1 角色/DAG/coverage 策略见
> `glusterfs-adaptation-plan.md`。

`src/pkg/ipc/ipc.go` 新增：

```go
func parseGlusterfsTraceEvents(data *[]byte) ([]prog.HmdfsTraceEvent, float64, int64)
```

读取顺序：

```text
ratio  u64
offset u64
gap    u64
count  u32
events[count]
```

每个 event 固定 1228 字节（不是变长）：

```text
timestamp      u64
entry_ts       u64
func_id        u32
ret            s32
nodeid         u64
unique         u64
off            u64
size           u64
mode           u32
path_len       u32
path2_len      u32
xattr_name_len u32
truncated      u32
path           char[512]     /* 读满 512，再按 path_len 截取 */
path2          char[512]     /* 读满 512，再按 path2_len 截取 */
xattr_name     char[128]     /* 读满 128，再按 xattr_name_len 截取 */
```

严格检查：

- `path_len <= 512`
- `path2_len <= 512`
- `xattr_name_len <= 128`
- 每个 event 按 1228B stride 前进；`count` 不超过剩余长度可容纳的事件数
- 不足时停止解析并保留已解析事件；不能按 length 变长读取

parser 填 Go 字段：

```text
Ino        = 0          /* 不要填 nodeid；v1 不做 PathSameInode */
NodeID     = nodeid
Path       = path
Path2      = path2
XattrName  = xattr_name
Truncated  = truncated
Mode       = mode
```

`parseOutputs` 的两个 executor 解析路径都要按：

```go
env.config.DFSName == "glusterfs"
```

选择 GlusterFS parser；HMDFS 继续用 `parseHmdfsTraceEvents`。server executor
即使不采集 trace，也必须写 `count=0` 的 28B 头，否则流顺序会错。

## 9. Go 事件与 DAG

> 本节只说明 Go 字段和路径接入；DAG path relation/role 策略见
> `glusterfs-adaptation-plan.md` §5.2、§6。

`src/prog/prog.go` 只给 Go struct 增加字段：

```go
type HmdfsTraceEvent struct {
    ...
    NodeID    uint64
    Path      string
    Path2     string
    XattrName string
    Truncated uint32
}
```

HMDFS parser 不填这些字段，保持零值。GlusterFS parser 直接把 wire event 填进 `HmdfsTraceEvent`，不再引入额外的 Go 事件类型。

`src/prog/dag.go` 的路径规则：

- HMDFS 完全保持现状：`Path` 为空，走旧 `Ino -> path`；LOOKUP 继续用
  `lookupPath` 的父/子 inode 反查和 rename 候选逻辑。
- GlusterFS **LOOKUP 顶点**：`Path = ev.Path`（FUSE parent+name 的精确分量），
  正/负由 `Ret` 区分；rename 的源/目标用调用参数和事件 `Path/Path2` 区分。
- GlusterFS **非 LOOKUP 顶点**：
  1. 路径型调用用 `extractPathFromCall` / 专用参数提取；
  2. fd 类调用用 `resolveFdToPath` 从同程序的 `open/creat` 回溯；
  3. fd 路径按 open 之后的 rename 时间线修正；
  4. 以上都没有时用 `ev.Path` fallback。
- `link/symlink` 用 syscall 参数：
  - `link(old,new)`：`Path=new`，`Path2=old`；
  - `symlink(target,linkpath)`：`Path=linkpath`，`Path2=target`；
  - `readlink(linkpath)`：`Path=linkpath`。
- `symlink` 的 `Path2` 是原始 payload，不是 canonical FileTree 路径。DAG 做
  path relation/cell 时先用 `linkpath + target` 解析回测试根内 canonical
  target；`RootPaths`/DCT/PathSet 只保存 canonical target。payload 解析后逃出
  测试根的事件丢弃并计数。
- `readlink` 只用 linkpath；不根据事件猜 target。
- `rename`：`OldPath=src`，`Path=dst`；不要用事件 `loc2.path` 覆盖调用参数。
- `ev.Path` 与调用参数/fd provenance 不一致时，以调用侧为准，并计入
  path-mismatch 诊断；LOOKUP/无调用事件才以 `ev.Path` 为权威。
- `Truncated != 0` 时优先尝试 nodeid 映射；仍失败则丢弃并计数。
- GlusterFS parser 固定 `Ino=0`，`NodeID` 单独保存；不要把 `nodeid` 填进
  `Ino`，否则 `determinePathRel` 的 same-inode 分支和 delete timeline 会误用
  inode 指针。
- GlusterFS v1 不填 `EntryIno`；`SupportsSameInode=false`，
  `determinePathRel` 不比较 `Ino`，只做路径相等/父子/同父/None。
- 所有匹配到的事件都建顶点；LOOKUP 也建；v1 不做同调用过滤。
- 文件 `RELEASE 18` 建 `FuncRelease` 顶点；它不是 modifier，但可以作为
  HB/CC 的 B 侧。目录 `RELEASEDIR 29` 已被过滤，不建顶点。
- hardlink 的 FUSE path 可能是 target/alias（P0 `truncate("q0")` 显示 `h1`），
  但 DAG 非 LOOKUP 顶点按调用侧路径/fd provenance，不据此新增
  `PathSameInode`。planned linkpath 可作为 truncate/write 的**调用参数**；
  内核选出的 alias 不是调用参数。
- 路径进入 DAG 前必须已 strip `eifuzz_root`；测试根为 `""`，mount root
  和 `.glusterfs` 内部路径不能进入顶点。

rename 时间线：

- HMDFS 继续用 `extractRenamePaths(call)`。
- GlusterFS 的 rename 调用参数是精确的 old/new；fd provenance 路径仍用
  rename 时间线修正。事件 `Path/Path2` 只做 fallback/校验。

## 10. FileTree 空前缀

> 本节只说明空前缀 FileTree 的实现落点；链接建模/绑定策略见
> `glusterfs-adaptation-plan.md` §1.4、§6。

### 10.1 数据模型

`src/prog/distributed_choice.go`：

```go
type FileTree struct {
    ...
    Prefix string
}
```

- `NewFileTree()` 默认 `merge_view/`，现有测试不变。
- `NewFileTreeWithPrefix(prefix)` 显式设置。
- 空前缀 Root：`Name=""`、`FullPath=""`。
- HMDFS Root：`merge_view`。
- **FileTree 不建模 link/symlink**：v1 baseline 无链接，每轮 restore 回
  baseline；`linkpath` 是 program-local binding，不调用 `FindNode(linkpath)`。
  `SyncFileTreeFromFsMd` 在每轮 restore 模式下停用；如临时开启，遇到
  symlink/hardlink 只记录/跳过，不回写 FileTree。

`Hmdfs_config.RootPrefix`：

```text
HMDFS     : "merge_view/"
GlusterFS : ""
```

### 10.2 路径原语

新增/修改：

```text
RootPath()
JoinRoot(rel)
NormalizePath(prefix, path)
IsUnderRoot(prefix, path)
DepthBucketOfWithPrefix(path, prefix)
```

规则：

- 空前缀：只去 `./` 和首 `/`，不补前缀。
- 空前缀 Root 的 parent 解析直接返回 Root。
- `NormalizeMergeViewPath` / `IsPathInMergeView` 保留默认 wrapper，供 HMDFS/旧测试使用。

### 10.3 调用点

需要改为 prefix-aware：

```text
src/prog/distributed_choice.go
src/prog/proc.go
src/prog/rand.go
src/prog/generation.go
src/prog/mutation.go
src/prog/dag.go
src/prog/path_set.go
src/checker/conc-fs.go
src/executor/executor.cc
```

关键点：

- `rand.go` / `generation.go` / `mutation.go` 中写死的 `merge_view`、`merge_view/test_dir` 改为 `RootPath()` / `JoinRoot()`。
- `rand.go:389 filenameImpl` 不能再用 `dir="."` 或 HMDFS `merge_view`；GlusterFS 空前缀应生成 `file0`，不能生成 `./file0` 或 `/file0`。该函数服务 `Target.Generate`、标准 `Mutate`、`filename()` BufferType 和 DCT/derived fallback。
- `proc.go` 的 `InitialDir` 使用 `hc.RootPrefix`。
- `executor.cc` 的 cwd 和 `write_metadata` 对非 HMDFS 使用固定测试根。
- `path_set.go` 的 `normalizePathSetPath`、`pathSetTopRoot`、`pathSetTopPath`、`mapPathSetPath` 去掉硬编码 `MergeViewPrefix`。
- `checker/conc-fs.go` 的 `TrimPrefix` 和硬编码 `merge_view` 改为当前 prefix（`initTreeSubset` 已按 profile prefix 与 syscall path 参数实现）。
- `dag.go` 的 `mergeRoot`、`firstComponentUnderRoot` 使用 `ft.Prefix` / `RootPrefix`。

### 10.4 验收

```text
HMDFS 默认构造路径逐字不变
GlusterFS 根为 ""
file 是 depth 1
a/b 是 depth 2
不出现前导 /
PathSet 在空前缀下可构建、可迁移
FileTree 不包含 link/symlink；linkpath 只存在于 bindings
```

GlusterFS InitialDir 只加载 `shared.file/.dir/.tmpdir` 和 `empty_dirs.info`，
不加载 `large_dir.info`；`Cids` 重复 `"shared"` 到 `ClientNum`，server 不进入
FileTree。详见 `glusterfs-config-and-scripts.md` §3。

## 11. executor 接入

> 本节只说明 trace 分发接入；executor runtime/config/checker 策略见
> `glusterfs-adaptation-plan.md` §10。

不新增 `dfs_trace.h/.cc`。在 `src/executor/dfs_runtime.h/.cc` 定义
`DfsRuntimeOps`；trace 子集在 B2 已实现，D 阶段已补
restore/cwd/metadata；config/node-up/down 仍为 `nullptr`。完整字段表见
`docs/glusterfs-adaptation-plan.md` §10.1：

```cpp
struct DfsRuntimeOps {
    const char *name;

    bool (*trace_enabled_for)(bool is_client);
    void (*init_trace)(bool is_client, const char *trace_root);
    void (*start_trace)();
    void (*stop_trace)();
};
```

`trace_root` 是挂载根到测试根的相对路径；GlusterFS 固定 `eifuzz_root`，
HMDFS 不使用（传 `nullptr`）。它只用于 tracer 路径 strip，不写进
FileTree.Prefix。

分发：

```text
dfs_runtime_ops(dfs_name) -> DfsRuntimeOps
hmdfs     -> hmdfs_trace 的 init/start/stop
glusterfs -> glusterfs_trace 的 init/start/stop
其他      -> nullptr/no-op
```

`trace_enabled_for` 表示该角色是否产生 trace 输出：

```text
HMDFS client      -> true
GlusterFS client  -> true
GlusterFS server  -> true（只写 count=0 头）
其他              -> false
```

v1 实现不只在 trace 上分发；cwd/restore/metadata/config/node-up/down 统一走
`DfsRuntimeOps`，见 `docs/glusterfs-adaptation-plan.md` §10。

`executor.cc` 调用点：

```cpp
const DfsRuntimeOps *rt = dfs_runtime_ops(dfs_name);
if (rt && rt->trace_enabled_for(is_dfs_client != 0))
    rt->init_trace(is_dfs_client != 0, trace_root);

// fork 子进程、执行前
if (rt && rt->trace_enabled_for(is_dfs_client != 0))
    rt->start_trace();

// 父进程，write_metadata 前
if (rt && rt->trace_enabled_for(is_dfs_client != 0))
    rt->stop_trace();
```

`trace_root` 只对 GlusterFS client 设置：

```cpp
const char *trace_root =
    (is_dfs_client && !strcmp(dfs_name, "glusterfs")) ? "eifuzz_root" : nullptr;
```

调用时机：

- `init`：启动时；当前 `executor.cc` 是先 `calibrate_tsc()` 再
  `init_hmdfs_trace()`，GlusterFS 沿用实际顺序，不依赖校准。
- `start`：fork 子进程、执行前；只做 ring drain（丢弃 stale 事件）。
- `stop_trace`：父进程，`clt_finish()`/`g_prog_end_raw` 之后、
  `write_metadata()` 前。

GlusterFS 时间对齐直接复用现有 HMDFS 代码路径：

```text
calibrate_tsc() / tsc_ns_to_global() / tsc_offset / tsc_ns_ratio /
tsc_best_gap / g_prog_end_raw
```

`stop_trace()` 必须：

1. 在父进程 drain ctl/path 两个 ring；
2. 按 `ts` 合并，再做 D/R/S 聚合；
3. 对 `timestamp` 和 `entry_ts` 调 `tsc_ns_to_global`；
4. 写与 HMDFS 相同的 `[ratio][offset][gap][count]` 头；
5. 在父进程末尾清 `pending`/`nodeid` map（不能在 fork 子进程的 start 里清）。

角色：

- GlusterFS server executor 不 attach fuse uprobe；但 `stop_trace` 仍写
  `count=0` 的 trace 头，保证 `[program output][trace][fsMd]` 流顺序。
- client executor 才 attach；一个 client VM 只允许一个 mount，否则
  `unique` 会跨 mount 冲突。
- `trace_root` 只对 client 设置；server 传 `nullptr`。

## 12. 构建

`src/Makefile` 用 per-DFS vmlinux/include，禁止两个 profile 共用
`-Itools`：

```make
HMDFS_VMLINUX_H     = tools/vmlinux.h
GLUSTERFS_VMLINUX_H = tools/glusterfs/vmlinux.h
HMDFS_BPF_INC       = -Itools
GLUSTERFS_BPF_INC   = -Itools/glusterfs

HMDFS_BPF_SRC    = executor/hmdfs_trace.bpf.c
HMDFS_BPF_OBJ    = executor/hmdfs_trace.bpf.o
HMDFS_BPF_SKEL   = executor/hmdfs_trace.skel.h
HMDFS_TRACE_SRC  = executor/hmdfs_trace.cc

GLUSTERFS_BPF_SRC   = executor/glusterfs_trace.bpf.c
GLUSTERFS_BPF_OBJ   = executor/glusterfs_trace.bpf.o
GLUSTERFS_BPF_SKEL  = executor/glusterfs_trace.skel.h
GLUSTERFS_TRACE_SRC = executor/glusterfs_trace.cc
DFS_RUNTIME_SRC     = executor/dfs_runtime.cc

$(HMDFS_BPF_OBJ): $(HMDFS_BPF_SRC) $(HMDFS_VMLINUX_H)
	clang -g -O2 -target bpf -D__TARGET_ARCH_x86 $(HMDFS_BPF_INC) -c $< -o $@

$(GLUSTERFS_BPF_OBJ): $(GLUSTERFS_BPF_SRC) $(GLUSTERFS_VMLINUX_H)
	clang -g -O2 -target bpf -D__TARGET_ARCH_x86 $(GLUSTERFS_BPF_INC) -c $< -o $@

$(HMDFS_BPF_SKEL): $(HMDFS_BPF_OBJ)
	bpftool gen skeleton $< > $@

$(GLUSTERFS_BPF_SKEL): $(GLUSTERFS_BPF_OBJ)
	bpftool gen skeleton $< > $@
```

- `tools/vmlinux.h`：HMDFS 内核完整 BTF，保持现有。
- `tools/glusterfs/vmlinux.h`：GlusterFS 5.15 内核完整 BTF
  （P0 文件 7,620,032B / 358,698 行），不是手写 minimal 头。
- `executor` 目标同时生成两个 skeleton，并链接
  `executor/executor.cc $(HMDFS_TRACE_SRC) $(GLUSTERFS_TRACE_SRC) $(DFS_RUNTIME_SRC)`；
  `make clean` 同时删除两个 `.bpf.o/.skel.h`。
- `send_fuse_iov`/`fuse_dispatch`/`fuse_fop_resume` 的 attach 按 §5.2：
  libelf `.symtab` + PT_LOAD file offset，不使用 `func_name`。

## 13. GlusterFS 文件树恢复

> 本节是 tracer 文档里的 restore 摘要；完整生命周期以
> `executor-file-tree-restore.md` 和 `glusterfs-adaptation-plan.md` §10 为准。

`executor.cc`：

```text
restore_initial_file_tree()
  hmdfs     -> 现有 HMDFS restore
  glusterfs -> restore_glusterfs_file_tree()
```

固定根：

```text
/root/glusterfs-client/eifuzz_root
```

prefill（volume 挂载后由首轮 restore 执行，client1）：

```bash
rm -rf /root/glusterfs-client/eifuzz_root
timeout 60s rsync -a --numeric-ids --delete \
  /home/baseline/glusterfs/ \
  /root/glusterfs-client/eifuzz_root/
sync
```

prefill 和 restore 共用同一份 baseline；首轮 restore 完成后其他 client 通过 replica 看到同一棵树。

每次 `receive_execute` 前的 restore 流程（含 triage/light）：

```text
restore start barrier（所有节点）
chdir("/root")
reset client: rm -rf /root/glusterfs-client/eifuzz_root
              timeout 60s rsync -a --numeric-ids --delete \
                /home/baseline/glusterfs/ \
                /root/glusterfs-client/eifuzz_root/
sync
其他 client / server: 只等 barrier
restore finish barrier
```

约束：

- 仅 designated reset client 执行恢复；固定为 `executor_index == server_num`，3-3 下为 index 3。
- 其他 client 只参与 barrier。
- server 节点不执行恢复。
- 不恢复 server brick。
- 不执行 volume stop/start。
- 任一命令失败：本节点置失败状态，finish barrier 后本轮统一失败。
- rsync 外层总超时 60s；start/finish barrier 30s/90s；不使用
  `rsync --timeout=300`。timing 表见总计划 §10.4。
- `is_restarting`：跳过 restore，保留崩溃现场。
- normal restore 不重启 server/volume，不调用 `glusterfs-node-down.sh`。
- `glusterfs-node-down.sh` 只在 failure/crash 场景使用；当前 v1 不启用 failure。
- 恢复后其他 client 若看到旧 dentry/attr，再评估 `drop_caches` / remount 回退。
- cwd/metadata/trace/restore 已由 `DfsRuntimeOps` 分发；config/node-up/down
  仍待实现，见总计划 §10；不要只在这里保留一个 `restore_initial_file_tree` 分支。

## 14. 测试与验收

> 本节测试项只覆盖 tracer/parser；端到端角色/config/checker 验收见
> `glusterfs-adaptation-plan.md` §13。

### 14.1 单元测试

```bash
go test ./prog/... ./pkg/ipc/...
```

覆盖：

- GlusterFS event parser 正常/截断/不足数据。
- HMDFS parser 不受影响。
- 新增事件：LINK/Path2、SYMLINK target、READLINK linkpath、xattr key。
- 空前缀 FileTree：Root/Join/Parent/DepthBucket。
- DAG 使用 `Path`/`Path2`；GlusterFS v1 **不区分** `PathSameInode` 与
  `PathSame`；HMDFS 现有折叠测试保持通过。
- link/symlink HB A 双路径 pair；readlink HB B self。
- symlink `Path2` payload 与 canonical target：linkpath 在测试根/子目录两种情形；
  payload 解析后不逃出测试根；DAG/cell 使用 canonical target。
- PathSet 空前缀。
- PathSet dual-path link/symlink/readlink：canonical target/linkpath 迁移、
  symlink payload 重算、readlink follower。
- PathSet directed target arm 只接受 `len(Paths)==1` 的成员；rename/link/
  symlink 整体排除，readlink 由 call-name gate 排除；target creator 用
  `freshKinds`/`freshLinks` 校验。
- 路径来源：路径型调用取调用参数；fd 类调用从 open/creat 回溯；两者都缺失
  才用 `ev.Path`。
- GlusterFS LOOKUP 顶点取 `ev.Path`；HMDFS `lookupPath` 行为不变。
- symlink follow：FUSE GETATTR 事件可以指向 target，但 DAG `stat` 顶点路径
  必须来自调用参数；hardlink 事件 path 可以是 target/alias，DAG 顶点路径
  仍来自调用参数/fd provenance。断言不依赖 `EntryIno`/`PathSameInode`。
- `ia_type`：file=1/dir=2/symlink=3；`link` 用 `loc2.inode+92`，
  `rename` 用 `loc.inode+92`，`symlink/mkdir/create` 新 inode 为 0 时走
  函数默认。
- 路径：`/eifuzz_root` -> `""`；`/eifuzz_root/q0` -> `q0`；mount root
  `/` 和 `.glusterfs` 内部路径被丢弃；rename/link path2 strip，symlink
  payload 不 strip。
- `truncate`：`off=新大小`、`size=0`；read/write 的 off/size、readdir 的
  off/size、xattr 的 size 不混淆。
- `RELEASE 18`：D+S、无 R；path 走 fd provenance；文件 close 产生
  `FuncRelease` 顶点，可作为 HB/CC 的 B 侧。
- `RELEASEDIR 29`：过滤，不产生 D/R/S/final event；目录 close 没有顶点。
- 内部 GETXATTR `-61/-95` 被 `user.*` 过滤；没有 user R 的 S 丢弃。
- ctl/path 双 ring 合并：构造 S 先于 R drain 的场景，最终事件仍能拿到 path。
- parser 固定 1228B stride：构造两个连续 event，第二个不会被第一个的
  `path_len` 错位。
- checker 空前缀路径匹配、xattr 双向、symlink target。
- CSAN 只保留 client 固定测试根下 fsMd；server fsMd 不参与 pairwise。
- `testdirIno` 固定为 `/root/glusterfs-client/eifuzz_root`。
- 角色：server 空程序、client view、client-only seed/mutation。

### 14.2 构建

```bash
make executor
```

检查：

```text
glusterfs_trace.skel.h 生成
executor 链接成功
```

构建机前置条件（点 2）：

```text
clang 支持 -target bpf
bpftool 能 gen skeleton
libbpf.h / libbpf.so 可链接
libelf / zlib 可链接
tools/vmlinux.h 来自 HMDFS 内核
tools/glusterfs/vmlinux.h 来自 GlusterFS 5.15 内核完整 BTF
```

`fuse_dispatch` / `fuse_fop_resume` / `send_fuse_iov` 是 LOCAL `.symtab`；
P0 host 的 `bpf_uprobe_opts` 没有 `func_name`，统一用 libelf 解析
`.symtab` + PT_LOAD `file_offset = st_value - p_vaddr + p_offset`，再调用
`bpf_program__attach_uprobe`。P0 build 的 `send_fuse_iov` 有 4 个 callq，
没有内联；如果重建后 call 数变化，先检查 noinline。

### 14.3 单 VM 冒烟

```bash
ROOT=/root/glusterfs-client/eifuzz_root
mkdir -p "$ROOT"
cd "$ROOT"
rm -rf ./*

touch q0
cat q0
mkdir d0
mv d0 d1
stat d1
ln q0 h1
ln -s q0 s1
readlink s1
stat h1
stat s1

# path truncate（用 syscall，不用 shell 的 ftruncate）
cat >/tmp/tr.c <<'EOF'
#include <unistd.h>
#include <sys/syscall.h>
int main(void) { return syscall(SYS_truncate, "q0", 12); }
EOF
gcc -o /tmp/tr /tmp/tr.c
/tmp/tr

chmod 600 q0
setfattr -n user.foo -v bar q0
getfattr -n user.foo q0
ls -la
rm -f h1 s1
```

验收：

```text
Path/Path2 正确，且 path 已 strip /eifuzz_root
LINK Path=新路径 / Path2=源；SYMLINK Path=linkpath / Path2=target
READLINK Path=linkpath
LOOKUP Path=父目录+name 的精确分量
SETXATTR/GETXATTR Path=目标 / XattrName=user.*
READDIR 28 / READDIRPLUS 44 都映射 ITERATE
READ/WRITE fd provenance 路径正确
RELEASE 18：D+S、无 R；文件 close 有 FuncRelease 顶点
RELEASEDIR 29：无 D/R/S/final event；目录 close 无顶点
FLUSH/p=0 已过滤；GETXATTR 只保留 user.*
内部 GETXATTR -61/-95 无 final event
事件数不超过 8192
DAG vertices/pairs/matchFail 正常
每轮 restore 后树一致

hardlink：truncate("q0") 的事件 path 可能是 h1（P0 实测）；
          DAG 顶点 Path 必须来自调用参数/fd provenance
symlink：FUSE stat GETATTR 事件 path 可能是 target T；
         DAG stat 顶点 Path 是调用参数 L1；readlink Path=L1
dir close：ls 只看到 OPENDIR/READDIRPLUS，看不到 RELEASEDIR final event
```

### 14.3.1 Standalone BPF tracer

不依赖 executor/fuzzer 的独立 smoke 工具。harness 本身只新增 test 源文件，
不改 `glusterfs_trace.cc/.h` 和 executor；D1 期间单独修复了 BPF LOOKUP
payload iovec（见下文“D1 结果”），生产 merge 逻辑未改。

```bash
cd src
make glusterfs-trace-test
# host 上产物：
# bin/linux_amd64/glusterfs-trace-test
```

拷贝到目标 VM 后以 root 运行：

```bash
TRACE_ROOT=/root/glusterfs-client/eifuzz_root

./glusterfs-trace-test \
  --fuse-so /usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so \
  --trace-root eifuzz_root --raw
```

注意 `--trace-root` 是 **相对 mount root** 的路径（executor 也传
`eifuzz_root`）；`normalize_trace_path` 会先去掉 raw `/eifuzz_root/...` 的
leading `/`，所以不要传 `/root/glusterfs-client/eifuzz_root` 或 `/eifuzz_root`。

交互：Enter/c 收集，q 退出。`--raw` 会同时打印：

```text
[ctl D] ... opcode=... error=... mode=...
[ctl S] ... opcode=... error=... mode=...
[path]  ... path=... path2=... xattr=...
[final] func=... ret=... mode=... path=...
```

LOOKUP 必须单独验证。remount 后对新路径执行：

```bash
stat "$TRACE_ROOT/lk/f"       # 正文件：S mode=0x8000
stat "$TRACE_ROOT/lk"         # 正目录：S mode=0x4000
stat "$TRACE_ROOT/lk/l"       # symlink：S mode=0xA000
stat "$TRACE_ROOT/lk/missing" # 负 lookup：error=-ENOENT, mode=0
```

final event 的 `path` 应是 `lk/f`、`lk`、`lk/l`、`lk/missing`，不是
`eifuzz_root/...`。

判定：

- 正 LOOKUP 的 raw S `mode` 和 final `mode` 都不能为 0；
- 负 LOOKUP `mode=0` 正常；
- raw S 正确但 final 错 -> merge 问题；raw S 错 -> iovec[1] payload
  指针/长度或 `entry_out.attr.mode`（payload+100）偏移问题；
- `missing_d/missing_s` 不应异常增长。

#### D1 结果（2026-11-17，单 VM）

状态：**通过**。测试环境为单 client + 单 GlusterFS 挂载，测试根
`/root/glusterfs-client/eifuzz_root`。

修复记录：

- `send_fuse_iov` 的 `iov_out[0]` 是 16B `fuse_out_header`，payload 在
  `iov_out[1]`；旧代码把 `iov_base` 当 payload 读 `+116`，导致正 LOOKUP
  mode 全是 `0x6000`（其实是 header 后内存）。BPF 现在按 `count >= 2`
  抓 `iov_out[1].iov_base/len`，从 payload + 100 读
  `fuse_entry_out.attr.mode`；`send_fuse_err`（count=1）没有 payload，
  负 LOOKUP `mode=0`。
- `--trace-root` 必须是 `eifuzz_root`（相对 mount root）。传
  `/eifuzz_root` 会让 `normalize_trace_path` 匹配失败、final path 带前缀。

观测结果：

```text
[ctl S] opcode=1 error=0 mode=0x4000   -> final LOOKUP path=""
[ctl S] opcode=1 error=0 mode=0x4000   -> final LOOKUP path="lk"
[ctl S] opcode=1 error=0 mode=0x8000   -> final LOOKUP path="lk/f"
[ctl S] opcode=1 error=0 mode=0xA000   -> final LOOKUP path="lk/l"
[ctl S] opcode=1 error=-2 mode=0       -> final LOOKUP ret=-ENOENT path="lk/missing"

[lookup check] total=26 positive_mode_zero=0 negative_mode_nonzero=0
missing_d=0 missing_s=0 drop_xattr=0 out_root=0 path2_out_root=0
unmapped=0 dropped=0
```

同时确认了 GETATTR/SETATTR、FILE_OPEN、READLINK、MKDIR、CREATE、
SYMLINK（`path2="f"`）、SETXATTR/GETXATTR、WRITE 的 off/size 字段。

已知统计口径：`missing_r` 会包含 root/无路径 GETATTR（D/S 有、path R 无），
例如 `[final] GETATTR ret=0 mode=0x0 path=""`。这不是 LOOKUP mode
回归；后续可单独统计 `missing_r_root_getattr` 或把 root GETATTR 从
`missing_r` 排除。D2 集成 smoke 以 `make executor` + 3-3 短跑为准。

### 14.4 3-3 + replica 3

单 VM 通过后验证：

- 6 VM 启动与 volume 挂载。
- 多 client 路径一致性。
- restore barrier 和 reset client index 3。
- DAG 跨节点 pairs。
- CSAN 只比较 client 固定测试根下 fsMd。
- 覆盖板 cell。

## 15. 风险与回退

| 风险 | 处理 |
|---|---|
| path 截断 | `truncated` 位；nodeid 映射回退；仍失败则 DAG 丢弃并计数 |
| nodeid 指针复用 | 父进程 `stop_collect` 末尾清空；不是 start 子进程 |
| hardlink alias 的事件 path 不确定（target/alias） | 源码已确认 `inode_path` 任意 dentry；v1 DAG 用调用参数/fd provenance，`ev.Path` 只 fallback/校验；不依赖 `PathSameInode`/`entry_ino` |
| path ring 溢出 | 记录 drop 计数；必要时增大 ring |
| 事件超过 8192 | 截断并记录；必要时增大上限和输出缓冲区 |
| HMDFS 回归 | HMDFS wire/C 不变；只新增 Go 字段 |
| prefix 改造回归 | 默认 `NewFileTree()` 保持 `merge_view/`；新增测试 |
| restore 只恢复客户端命名空间 | 若 server 侧残留导致不可复现，再评估 brick-level restore |
| `GETXATTR/FLUSH` 误过滤 | `FLUSH` 继续过滤；`GETXATTR/SETXATTR` 只保留 `user.*`，虚拟/内部 key 过滤；内部 xattr 的 S-only 事件丢弃 |
| `RELEASE/RELEASEDIR` 无 R | 18 保留 D/S，path 走 fd provenance，不计 missing_r；29 过滤，不产生 final event |
| ctl/path 双 ring 顺序 | 两个 ring 全 drain 后按 ts 合并；不能按单 ring 顺序 finalize |
| 构建机 libbpf/bpftool | 先找 HMDFS 既有构建环境/kernel tree；本 build 的 `bpf_uprobe_opts` 无 `func_name`，统一 `.symtab` + PT_LOAD file offset |

回退：

- HMDFS 走原 tracer 和原 wire。
- GlusterFS tracer 可单独关闭，回退到 no-op。
- FileTree 默认 prefix 保持旧行为。

## 16. 实现顺序与检查点

v1 的完整阶段与角色/executor/config 检查点见 `glusterfs-adaptation-plan.md` §12。
本文只保留 tracer 内部检查顺序：

1. wire event + Go parser + 单元测试。
2. BPF skeleton + executor 编译。
3. 单 VM 事件冒烟。
4. prefix/DAG/restore 冒烟。

## 17. 源码参考

| 内容 | 位置 |
|---|---|
| FUSE 三点 | `glusterfs-.../xlators/mount/fuse/src/fuse-bridge.c:352,1149,6068` |
| nodeid 转换 | `.../fuse-helpers.c:422,440` |
| HMDFS event | `src/executor/hmdfs_trace.bpf.c`、`hmdfs_trace.cc:34` |
| HMDFS parser | `src/pkg/ipc/ipc.go:1059` |
| Go event | `src/prog/prog.go:68` |
| DAG | `src/prog/dag.go:183-215` |
| executor trace 调用 | `src/executor/executor.cc:712,2158,2282` |
| restore | `src/executor/executor.cc:1428` |
| cwd | `src/executor/executor.cc:1730` |
| write_metadata | `src/executor/executor.cc:3612` |
| FileTree | `src/prog/distributed_choice.go` |
| PathSet | `src/prog/path_set.go` |
| checker | `src/checker/conc-fs.go` |

## 18. P0 复现附录

探针符号：

```text
uprobe:/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so:fuse_dispatch
uprobe:/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so:fuse_fop_resume
uprobe:/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so:send_fuse_iov
uretprobe:/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so:send_fuse_iov
```

偏移：

```text
fuse_dispatch:       finh = *(u64 *)(arg0 - 24)
fuse_fop_resume:     finh = state + 160
                     path = state + 32
                     path2 = state + 96
                     off = state + 176
                     size = state + 200
                     name = state + 248
send_fuse_iov:       entry: finh = arg1, iov_out = arg2
                     error = *(s32 *)(iov_out[0].iov_base + 4)
                     uretprobe: 用入口保存的 finh/error + 当前 ktime
```

观测：

```text
D o=12 u=680 n=1
R o=12 u=680 p=/q0
R2 /q1
S o=12 u=680
```

```text
D o=44 u=604 n=1
R o=44 u=604 p=
S o=44 u=604
```

```text
D o=15 u=638 n=106721347771096
R o=15 u=638 p=
S o=15 u=638
```

### 18.1 2026-11-16 P0 复核记录

探针符号（5.15 VM，`/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so`）：

```text
nm -C:
  000000000005e940 t fuse_dispatch      583B
  0000000000068be0 t fuse_fop_resume    338B
  00000000000216a0 t send_fuse_iov      832B

send_fuse_iov 的 callq = 4（源码 394/986/2780/3015），无内联。
```

实测布局：

```text
fuse_async_t: iobuf=+0 finh=+8 this=+16 msg=+24 async=+32
D:            finh = arg0 - 24
finh:         opcode=+4 unique=+8 nodeid=+16 pid=+32
fuse_state_t: loc=+32 loc2=+96 finh=+160 off=+176 size=+200 name=+248
loc.inode=+16 loc2.inode=+112
inode_t.ia_type=+92（+132 为垃圾值）
```

实测语义：

```text
路径（mount root=/root/glusterfs-client, cwd=eifuzz_root）：
  stat q0              -> /eifuzz_root/q0
  mv d0 d1             -> p=/eifuzz_root/d0 p2=/eifuzz_root/d1
  ln q0 h1             -> p=/eifuzz_root/h1 p2=/eifuzz_root/h0(alias)
                         ia1=0 ia2=1（link 源在 loc2.inode）
  ln -s q0 s1          -> p=/eifuzz_root/s1 name=q0 ia1=0
  readlink s1          -> p=/eifuzz_root/s1 ia1=3
  truncate syscall q0  -> p=/eifuzz_root/h1(alias) off=12 size=0
  chmod q0             -> p=/eifuzz_root/h1(alias) off=0 size=0
  setxattr q0 user.foo -> p=/eifuzz_root/h1(alias) name=user.foo size=3
  getxattr q0 user.foo -> name=user.foo size=0/256
  ls -la               -> op=27 p=/eifuzz_root; op=44 p=<null>;
                          op=29 无 R，且 v1 过滤
  cat q0               -> op=14/15/18；18 无 R
内部 GETXATTR S: -61(ENODATA) / -95(EOPNOTSUPP)
```

bpftrace 注意：

```text
旧版 bpftrace 对 $var = arg0 - 24 可能读 0；
用嵌套直接表达式：
  *(uint32 *)(*(uint64 *)(arg0 - 24) + 4)
正式 executor 的 BPF 用 PT_REGS_PARM1，不受影响。
```

host libbpf：

```text
bpf_uprobe_opts 没有 func_name 字段；
bpf_uprobe_opts__last_field = retprobe。
```

因此 attach 固定走：

```text
libelf .symtab -> st_value
PT_LOAD: file_offset = st_value - p_vaddr + p_offset
bpf_program__attach_uprobe(prog, ret, -1, fuse_so, file_offset)
```

`send_fuse_iov` 用 ret=true；另两个 ret=false。

2026-11-16 在 client 镜像上复核（`fuse.so` BuildID
`e752dad97c4d6048387bd91117df351afb83c0c1`，`not stripped`）：

```text
.symtab 存在；三个符号均 LOCAL，Ndx=14
LOAD1: p_offset=0x15000 p_vaddr=0x15000
=> file_offset == st_value
   send_fuse_iov   0x216a0
   fuse_dispatch   0x5e940
   fuse_fop_resume 0x68be0
```
