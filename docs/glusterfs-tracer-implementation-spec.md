# GlusterFS FUSE Tracer 实现规格（B 方案）

> 状态：BPF raw 细节参考；v1 总决策已被 `docs/glusterfs-adaptation-plan.md` 取代；代码未实施
> 日期：2026-11-14（2026-11-15 修订 v1 调用/事件/角色/checker 决策）
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
- client1 挂载后运行 `prefill.sh`：`rm -rf` 固定测试根 + rsync baseline + `sync`；其他 client 只等待 barrier。
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
| `timestamp` | `send_fuse_iov` 结束时间，作为事件结束时间 |
| `entry_ts` | `fuse_dispatch` 开始时间；缺失时为 0 |
| `func_id` | 0–14 通用 + 16–20 GlusterFS；15 仅 HMDFS |
| `ret` | `fuse_out_header.error`；0 成功，负数 errno |
| `nodeid` | FUSE nodeid；GlusterFS 中是 `inode_t *` |
| `unique` | FUSE 请求唯一号，仅用于调试/关联 |
| `off` | 读/写/truncate 的 offset；未知为 0 |
| `size` | 读/写/truncate 的 size；未知为 0 |
| `mode` | `loc.inode->i_mode`；未知为 0 |
| `path_len` | `path` 实际字节数，不含 NUL |
| `path2_len` | `path2` 实际字节数，不含 NUL |
| `xattr_name_len` | `xattr_name` 实际字节数，不含 NUL；上限 128 |
| `truncated` | 位图：bit0=path 截断，bit1=path2 截断，bit2=xattr 截断 |
| `path` | 归一化路径，不带前导 `/`；根为空 |
| `path2` | 第二路径：rename 目标 / link 源 / symlink target |
| `xattr_name` | xattr key；非 xattr 事件为空 |

### 4.2 序列化

输出格式与 HMDFS 相同：

```text
ratio  u64
offset u64
gap    u64
count  u32
events[count]
```

每个 event 固定 1228 字节。`path/path2/xattr_name` 按对应 length 截取，不依赖 NUL。

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
    u32 pid;      /* 32 */
    s32 error;    /* 36 */
};                /* 40 */
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
- `path` ring 4 MB 约可容纳 3900 条 R 事件。
- BPF 只采集，不做路径聚合。

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

`fuse_fop_resume`：

```text
fuse_state_t.finh      = state + 160
fuse_state_t.loc       = state + 32
fuse_state_t.loc2      = state + 96
loc_t.path             = loc + 0
fuse_state_t.off       = state + 176
fuse_state_t.size      = state + 200
fuse_state_t.name      = state + 248   /* symlink target / xattr key；以 offsetof 复核 */
```

`send_fuse_iov`：

```text
finh    = arg1
iov_out = arg2
fuse_out_header = iov_out[0].iov_base
error   = *(s32 *)(fuse_out_header + 4)
```

### 5.3 用户态内存读取

这些地址都在用户态：

- `arg0` 指向 `gf_async_t`
- `fuse_in_header_t *finh`
- `fuse_state_t *state`
- `loc.path` / `loc2.path`
- `iov_out[0].iov_base`

因此 BPF 必须使用用户态读取：

```text
bpf_probe_read_user
bpf_probe_read_user_str
```

路径拷贝直接写入 ringbuf 预留区，避免在 BPF 栈上同时放 `path[512] + path2[512]`。

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
`inode-invalidate`。

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

`FUSE_RELEASE` 暂时保留；`FUSE_RELEASEDIR` 第一版过滤。
`FUSE_RENAME2 45` 当前 build 不可达，不映射。

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
D:
  建立 pending；记录 start_ts/opcode/nodeid/pid

R:
  填 path/path2/off/size/mode/truncated

S:
  生成最终 glusterfs_trace_event；删除 pending
```

异常：

| 情况 | 处理 |
|---|---|
| S 无 D | 生成事件，`entry_ts=0`，计数 `missing_d` |
| S 无 R | path 为空，尝试 nodeid 映射，计数 `missing_r` |
| R 无 S | 丢弃，计数 `missing_s` |
| D 无 S | stop 时丢弃，计数 `missing_s` |
| path 截断 | 设置 `truncated` 位；优先用 nodeid 映射补全；仍无完整路径则 DAG 丢弃 |
| ring 溢出 | 记录 drop 计数，不静默 |

### 6.2 路径归一化

在 `glusterfs_trace.cc` 中完成：

```text
"/"          -> ""
"/q0"        -> "q0"
"/a/b"       -> "a/b"
```

规则：

- 去掉前导 `/`。
- 根 `/` 归一化为空字符串。
- 不做 `merge_view/` 前缀。
- `path2` 同样处理。
- 超过 512 字节：截断并设置 `truncated` 位。

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

特殊：

```text
nodeid == 1 -> 根路径 ""
```

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

每轮 `start_glusterfs_trace()` 清空 `nodeid -> path`，避免 inode 指针复用。

### 6.4 双路径操作

`RENAME 12`：

- `path` = 旧路径
- `path2` = 新路径
- `path2` 为空时表示未捕获；不要用 `path` 代替新路径。
- `RENAME2 45` 在当前 FUSE 7.24 build 无 handler，不映射、不生成。

`LINK 13`：

- `path` = 新链接路径
- `path2` = 源 inode 路径
- v1 不写 `EntryIno`。源码确认 `loc.inode` 是源 inode（`inode_ref(loc2.inode)`），
  但 v1 不需要它；`loc2.path` 可能来自 `inode_path` 的任意源 alias。
  DAG `link` 顶点用调用参数 `new/old` 作为 `Path/Path2`，不取事件 `loc2.path`。

`SYMLINK 6`：

- `path` = 新链接路径
- `path2` = target 字符串（原始 payload）
- `path2` 不是 canonical FileTree 路径；它按 linkpath 的父目录解析。DAG/定向
  使用 target 做路径关系时，必须先用 `linkpath + path2` 还原 canonical target，
  再和 FileTree/调用路径比较；不要直接把 payload 当 FileTree 节点。
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
- 只保留 `user.*`；虚拟 `glusterfs.*`/GFID 不进入事件。

### 6.5 mode / off / size

- `mode` 来自 `loc.inode->i_mode`；失败则 0。
- `off/size` 来自 `fuse_state_t`；失败则 0。
- DAG 的 `eventIsDir` 在 `mode=0` 时使用 `funcDefaultIsDir` 或 fsMd 回退。

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
FUSE_RELEASE      18 -> release
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

每个 event：

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
path           path_len bytes
path2          path2_len bytes
xattr_name     xattr_name_len bytes
```

严格检查：

- `path_len <= 512`
- `path2_len <= 512`
- `xattr_name_len <= 128`
- `count` 不超过剩余长度可容纳的事件数
- 不足时停止解析并保留已解析事件

`parseOutputs` 的两个 executor 解析路径都要按：

```go
env.config.DFSName == "glusterfs"
```

选择 GlusterFS parser；HMDFS 继续用 `parseHmdfsTraceEvents`。

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
- `Ino` 对 GlusterFS 只作诊断/特征，不用于路径解析。
- GlusterFS v1 不填 `EntryIno`；`SupportsSameInode=false`，
  `determinePathRel` 不比较 `Ino`，只做路径相等/父子/同父/None。
- 所有匹配到的事件都建顶点；LOOKUP 也建；v1 不做同调用过滤。
- hardlink 的 FUSE path 可能是 target/alias，但 DAG 非 LOOKUP 顶点按调用侧
  路径，不据此新增 `PathSameInode`。

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
- `proc.go` 的 `InitialDir` 使用 `hc.RootPrefix`。
- `executor.cc` 的 cwd 和 `write_metadata` 对非 HMDFS 使用固定测试根。
- `path_set.go` 的 `normalizePathSetPath`、`pathSetTopRoot`、`pathSetTopPath`、`mapPathSetPath` 去掉硬编码 `MergeViewPrefix`。
- `checker/conc-fs.go` 的 `TrimPrefix` 和硬编码 `merge_view` 改为当前 prefix。
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

新增：

```text
src/executor/dfs_trace.h
src/executor/dfs_trace.cc
```

接口：

```c
void init_dfs_trace(const char *dfs_name);
void start_dfs_trace(void);
void stop_collect_dfs_trace(void);
```

分发：

```text
hmdfs     -> hmdfs_trace 的 init/start/stop
glusterfs -> glusterfs_trace 的 init/start/stop
其他      -> no-op
```

v1 实现不只在 trace 上分发；cwd/restore/metadata/config/node-up/down 统一走
`DfsRuntimeOps`，见 `docs/glusterfs-adaptation-plan.md` §10。

`executor.cc` 调用点：

```text
init_hmdfs_trace()         -> init_dfs_trace(dfs_name)
start_hmdfs_trace()        -> start_dfs_trace()
stop_collect_hmdfs_trace() -> stop_collect_dfs_trace()
```

调用时机保持不变：

- `init`：启动时，`calibrate_tsc()` 前。
- `start`：执行前。
- `stop_collect`：执行结束、`write_metadata()` 前。

## 12. 构建

`src/Makefile` 增加：

```make
GLUSTERFS_BPF_SRC   = executor/glusterfs_trace.bpf.c
GLUSTERFS_BPF_OBJ   = executor/glusterfs_trace.bpf.o
GLUSTERFS_BPF_SKEL  = executor/glusterfs_trace.skel.h
GLUSTERFS_TRACE_SRC = executor/glusterfs_trace.cc
```

规则与 HMDFS 相同：

```make
$(GLUSTERFS_BPF_OBJ): $(GLUSTERFS_BPF_SRC) $(VMLINUX_H)
	clang -g -O2 -target bpf -D__TARGET_ARCH_x86 -Itools -c $< -o $@

$(GLUSTERFS_BPF_SKEL): $(GLUSTERFS_BPF_OBJ)
	bpftool gen skeleton $< > $@
```

executor 链接：

```make
executor/executor.cc $(HMDFS_TRACE_SRC) $(GLUSTERFS_TRACE_SRC)
```

`make executor` 自动生成 skeleton。

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

prefill（volume 挂载后只执行一次，client1）：

```bash
rm -rf /root/glusterfs-client/eifuzz_root
mkdir -p /root/glusterfs-client/eifuzz_root
timeout 60s rsync -a --numeric-ids --delete \
  /home/baseline/glusterfs/ \
  /root/glusterfs-client/eifuzz_root/
sync
```

prefill 和 restore 共用同一份 baseline；prefill 后其他 client 通过 replica 看到同一棵树。

每次 `receive_execute` 前的 restore 流程（含 triage/light）：

```text
restore start barrier（所有节点）
chdir("/")
reset client: rm -rf /root/glusterfs-client/eifuzz_root
              mkdir -p /root/glusterfs-client/eifuzz_root
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
- cwd/metadata/trace/config/node-up/down 统一由 `DfsRuntimeOps` 分发，见总计划 §10；
  不要只在这里保留一个 `restore_initial_file_tree` 分支。

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

### 14.3 单 VM 冒烟

```bash
ls /root/glusterfs-client
touch /root/glusterfs-client/a
cat /root/glusterfs-client/a
mkdir /root/glusterfs-client/d
mv /root/glusterfs-client/a /root/glusterfs-client/b
rm /root/glusterfs-client/b
rmdir /root/glusterfs-client/d
touch /root/glusterfs-client/a
ln /root/glusterfs-client/a /root/glusterfs-client/hard_a
ln -s a /root/glusterfs-client/sym_a
stat /root/glusterfs-client/hard_a
stat /root/glusterfs-client/sym_a
readlink /root/glusterfs-client/sym_a
rm /root/glusterfs-client/hard_a /root/glusterfs-client/sym_a
```

验收：

```text
Path/Path2 正确
LINK Path=新路径 / Path2=源；SYMLINK Path=linkpath / Path2=target
READLINK Path=linkpath
LOOKUP Path=父目录+name 的精确分量
SETXATTR/GETXATTR Path=目标 / XattrName=user.*
READDIR 28 / READDIRPLUS 44 都映射 ITERATE
READ/WRITE fd provenance 路径正确
FLUSH/p=0 已过滤；GETXATTR 不再整体过滤
事件数不超过 8192
DAG vertices/pairs/matchFail 正常
每轮 restore 后树一致

hardlink：FUSE stat/write 事件 path 可能是 target 或 alias；
          DAG 顶点 Path 必须来自调用参数/fd provenance
symlink：FUSE stat GETATTR 事件 path 可能是 target T；
         DAG stat 顶点 Path 是调用参数 L1；readlink Path=L1
```

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
| nodeid 指针复用 | 每轮 start 清空映射 |
| hardlink alias 的事件 path 不确定（target/alias） | 源码已确认 `inode_path` 任意 dentry；v1 DAG 用调用参数/fd provenance，`ev.Path` 只 fallback/校验；不依赖 `PathSameInode`/`entry_ino` |
| path ring 溢出 | 记录 drop 计数；必要时增大 ring |
| 事件超过 8192 | 截断并记录；必要时增大上限和输出缓冲区 |
| HMDFS 回归 | HMDFS wire/C 不变；只新增 Go 字段 |
| prefix 改造回归 | 默认 `NewFileTree()` 保持 `merge_view/`；新增测试 |
| restore 只恢复客户端命名空间 | 若 server 侧残留导致不可复现，再评估 brick-level restore |
| `GETXATTR/FLUSH` 误过滤 | `FLUSH` 继续过滤；`GETXATTR/SETXATTR` 只保留 `user.*`，虚拟/内部 key 过滤 |

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
```

偏移：

```text
fuse_dispatch:       finh = *(u64 *)(arg0 - 24)
fuse_fop_resume:     finh = state + 160
                     path = state + 32
                     path2 = state + 96
                     off = state + 176
                     size = state + 200
send_fuse_iov:       finh = arg1
                     error = *(s32 *)(iov_out[0].iov_base + 4)
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
