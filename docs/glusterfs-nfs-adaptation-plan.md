# GlusterFS / NFS 适配记录

> 状态：历史适配记录；GlusterFS v1 总决策以 `docs/glusterfs-adaptation-plan.md` 为准
> 日期：2026-10-21（2026-11-15 修订 v1 调用/角色/checker/executor/config）
> 范围：EIFuzz 在 GlusterFS / NFS 上的适配；HMDFS 逻辑保持不变
> 相关：`docs/glusterfs-adaptation-plan.md`、`docs/glusterfs-config-and-scripts.md`、`docs/glusterfs-tracer-implementation-spec.md`
>
> 修订说明：本文的 0–14 事件、共享 cid、DAG gate、restore/cwd 等旧结论只作历史参考。
> v1 实际实现以 `docs/glusterfs-adaptation-plan.md` 为准：新增
> `link/symlink/readlink/getxattr/setxattr`，不做 mknod/stash/failure，使用
> client-local view + empty server progs，A 类强字段检查，3-3 replica 3。
> 2026-11-15 P1 进一步收敛：v1 config 为 22 个调用；FileTree 不建模
> link/symlink；GlusterFS 不做 `PathSameInode`；rename 支持跨目录；DCT seed
> 的 readlink 只做 symlink variant。

## 0. 结论摘要

- GlusterFS 为首选第二目标，NFS 为对照/备选；两者都用 Monarch 镜像。
- 非 HMDFS 执行器 cwd 改为固定测试根 `/root/<dfs>-client/eifuzz_root`，不做每轮 `dfs-*` 目录隔离；不使用 symlink/copy。
- GlusterFS 文件树恢复采用 **client-side logical restore**：固定测试根、单个 reset client、`rm -rf` + baseline rsync；不恢复 server brick，不做 volume stop/start。详见 §2.9。
- `merge_view` 前缀解耦已定：HMDFS/默认测试保留 `merge_view/`，非 HMDFS 去掉该前缀，采用**空前缀**（根 = 挂载根）。
- 非 HMDFS 使用共享单 cid；InitialDir、FileTree、覆盖板、DAG、sync 对所有 DFS 放开。
- GlusterFS / NFS 各写独立 tracer，不复用 HMDFS 的 BPF 探针；GlusterFS 采用 B 方案，复用 Go/DAG 接口，不修改 HMDFS wire event。
- GlusterFS 定为 3-3 + replica 3；客户端 FUSE handler P0 已通过，VFS kprobe 不再作为第一版方案，见 §3.3.1、§7 与 `docs/glusterfs-tracer-implementation-spec.md`。

## 1. 目标与范围

- 第二 DFS：**GlusterFS**；NFS 作为对照/备选。
- 使用 Monarch 镜像：
  - GlusterFS：`focal.qcow2` + `kernel-5.15-glusterfs-bzImage`；
  - NFS：`stretch.qcow2` + `kernel-5.15-nfs-bzImage`。
- 不追求与 Monarch 基线在初始状态/隔离方式上完全一致，论文中不展开该差异。
- 目标：让 EIFuzz 的 FileTree、覆盖板、DAG、定向插入在 GlusterFS / NFS 上跑通。

## 2. 已确定决策

### 2.1 执行模型

- 非 HMDFS 执行器 cwd 直接改为固定测试根 `/root/<dfs>-client/eifuzz_root`，不再创建/删除 `dfs-<pid>-<exec>`。
- 每次 execute 前由 executor 统一恢复文件树。HMDFS 使用离线 unmount/remount + rsync，baseline 为 `/home/baseline/non_account/` -> `/data/service/el2/100/non_account/`；GlusterFS 使用 client-side logical restore，固定测试根 + reset client + baseline rsync，不恢复 server brick。详见 §2.9 与 [`executor-file-tree-restore.md`](executor-file-tree-restore.md)。
- **不使用 symlink/copy**；`merge_view` 前缀解耦，仅 HMDFS/默认测试保留（详见 §3.6）。
- 非 HMDFS 预填树直接建在固定测试根（空前缀）。
- 保留 `tmpDirEstablished` 同步屏障；client 只置标志，其他 executor 等待后 chdir。
- `executor.cc`：
  - `cwdbuf` 改为固定测试根（`:1691-1693`）；
  - 跳过 `make_test_dir` / `remove_dir`（`:1696-1712`）；
  - 保留 `chdir(cwdbuf)`（`:1730-1745`）。

### 2.2 cid 与 FileTree

- 非 HMDFS 使用**共享单 cid**：共享命名空间下，节点不拥有私有文件。
- `Cids` 中重复 `ClientNum` 份 `"shared"`，满足 client view 的 `len(Cids) == len(client ps)` 检查；server 不进入 Cids（2026-11-15 修订）。
- `Init_file / Init_dir / Init_tmpdir / Init_empty_dir / FileSize` 只挂这个 cid。
- `InitFromHmdfsConfig`（`distributed_choice.go:212`）对所有 DFS 生效。
- `NewLayeredChoiceStrategy`、`InitCoverageBoard`、`len(Executor) == Node_num` 校验对所有 DFS 放开。
- `large_dir.info` persistence 保持 HMDFS-only。
- GlusterFS 的 cid 固定为 `"shared"`；`generate_test_files.py` 的 `node_id`、`InitialDir` 清单名、`Cids` 都使用该值。

### 2.3 Sync 与 DAG 门控

- 启用每轮文件树恢复的 DFS，`FileTree` 保持 baseline，不再调用 `SyncFileTreeFromFsMd`；`fsMd` 仍用于过滤、CSAN 和 DAG。
- 只有在关闭 restore 的调试模式下，才重新启用 `FileTree` sync。
- 若启用 sync，`fsMd` 先按 `FileTree` 根前缀过滤：
  - HMDFS：只保留 `merge_view/...`；
  - 非 HMDFS（空前缀）：保留固定测试根下路径，丢掉旧 `dfs-*`、`lost+found` 等；
  - raw key `./...` 先 `TrimPrefix("./")`；
  - 非 HMDFS 不做 `CanonicalizeFsMd`（冲突后缀是 HMDFS 语义）。
- CSAN 的 `fsMds` 同样只保留 client 固定测试根下路径；server fsMd 不参与 pairwise，`initTreeSubset`/symsc 使用空前缀。
- `SyncFileTreeFromFsMd` 内部加 guard（仅 sync 启用时）：不在当前根前缀下的路径跳过，避免 `NormalizeMergeViewPath`（`distributed_choice.go:2131-2137`）把 `dfs-*` 变成 `merge_view/dfs-*`；空前缀时直接用相对路径。
- `proc.go:1513/1520` 的 DAG hmdfs gate 去掉；DAG 使用过滤后的 fsMds。
- `hmcfg.Cids` 只按 client view 保持长度；server 不进入 Cids（2026-11-15 修订）。

### 2.4 拓扑与镜像

- GlusterFS 定稿为 **3-3**（3 server + 3 client，6 VMs），卷模式为 **replica 3**。
  - 脚本参数化支持：`glusterfs-config.sh` 的 `srv_cnt=$2`、`client_cnt=$3`。
  - 库存没有 3-3 配置；复制 `3-2` 改：
    - `server_num = 3`
    - `fuzzing_vms = 6`
    - `dfs_setup_params = "3 3 'replica 3'"`
    - `vm.count = 6`
  - 资源开销：6 × 2C / 10G；资源不足时只调 `vm.mem`，不改拓扑。
  - `data.img` 不启用：server VM 仍会被 QEMU 挂上 data.img，但 `glusterfs-config.sh` 中 mkfs/mount 保持注释，brick 继续使用 rootfs 的 `/root/glusterfs-server`。
- NFS 使用 **1-2**（1 server + 2 client，3 VMs）。
- 最终配置片段：

```json
{
  "server_num": 3,
  "fuzzing_vms": 6,
  "dfs_setup_params": "3 3 'replica 3'",
  "enable_csan": true,
  "enable_c2san": false,
  "vm": {
    "count": 6,
    "cpu": 2,
    "mem": 10240
  }
}
```

> 2026-11-15 历史标记：§2.5–§2.9 中的 0–14 事件、旧 DAG gate、
> 旧 restore/executor 描述仅作历史记录；实现以
> `docs/glusterfs-adaptation-plan.md` 和 `docs/glusterfs-config-and-scripts.md` 为准。
> 下面保留的 prefill/baseline 结论仍有效。

### 2.5 预填（已定）

- **baseline 烤进 client 镜像**，不直接烤 server brick：
  - host 用 `HorcruxSourceCode/generate_test_files.py` 生成一棵 `shared` 树；
  - 树本体复制到 client 镜像的 `/home/baseline/glusterfs/`；
  - 清单为 `shared.file`、`shared.dir`、`shared.tmpdir`、`empty_dirs.info`；
  - GlusterFS 不使用 `large_dir.info`，生成时 `--num-large-files 0`。
- client1 挂载 volume 后运行幂等 `prefill.sh`：
  - `rm -rf /root/glusterfs-client/eifuzz_root`；
  - `timeout 60s rsync -a --numeric-ids --delete /home/baseline/glusterfs/ /root/glusterfs-client/eifuzz_root/`；
  - `sync`。
- prefill 和每轮 restore 共用同一份 baseline，避免树漂移。
- 不在 server brick 中预放文件：GlusterFS 需要正确的 GFID/xattr/replica 元数据，直接写 brick 不可靠。
- baseline 保留空目录，不含 xattr / symlink / hardlink。

### 2.6 专用 tracer（核心变更）

- **GlusterFS、NFS 各写一套专用 tracer，不复用 HMDFS 探针**；HMDFS tracer 只服务 HMDFS。
- 新增：
  - `src/executor/glusterfs_trace.bpf.c` / `.cc` / `.h`
  - `src/executor/nfs_trace.bpf.c` / `.cc` / `.h`
  - `src/executor/dfs_trace.h` / `.cc`（按 `dfs_name` 分发 init/start/stop）
  - `src/executor/trace_common.h` / `.cc`（事件格式、TSC 校准、输出序列化、路径前缀；只共享工具）
- `src/Makefile`：新增两套 BPF 源/对象/skeleton，executor 目标链接两个 `.cc`，clean 加产物。
- executor 调用点：
  - `init_hmdfs_trace()` → `init_dfs_trace(dfs_name)`（`:711`）
  - `start_hmdfs_trace()` → `start_dfs_trace()`（`:2068`）
  - `stop_collect_hmdfs_trace()` → `stop_collect_dfs_trace()`（`:1944`）
- 事件契约的 v1 修订：HMDFS 保持 0–15；GlusterFS 新增 16 LINK、17 SYMLINK、18 READLINK、19 GETXATTR、20 SETXATTR，READDIR 28/44 都映射 ITERATE。详见 `glusterfs-adaptation-plan.md` §4。
- GlusterFS 采用 B 方案：独立 wire event，不修改 HMDFS 的 112 字节事件；`pkg/ipc` 增加 `parseGlusterfsTraceEvents`，Go 层给 `HmdfsTraceEvent` 增加 `NodeID/Path/Path2/XattrName/Truncated` 可选字段，`prog.BuildVertices` 增加 Path 路径。
- 路径统一为相对 FileTree 根：HMDFS 为 `merge_view/...`；非 HMDFS 为固定测试根下相对路径（空前缀）；剥掉 `/root/glusterfs-client/eifuzz_root/` 或 `/root/nfs-client/eifuzz_root/`。
- GlusterFS 的 FUSE uprobe 不依赖 sb magic；通过 opcode/pid 过滤，具体见实现规格。

### 2.7 构建与验证

- `gofmt` / `go vet` / `GOOS=linux go test -c`；Ubuntu 编 executor + BPF。
- GlusterFS 3-3 冒烟：
  - tracer attach 日志；
  - events > 0；
  - 路径与 FileTree 根前缀一致（HMDFS `merge_view/...`；非 HMDFS 空前缀相对路径）；
  - 无 ext4/proc 事件；
  - DAG `events/pairs`、matchFail、board cell 正常；
  - sync 正常。
- NFS 1-2 同样验证。
- 抽几个 mkdir/write/rename 与手动操作对照。

### 2.8 GlusterFS 版本与源码基线（已确认）

- VM 内 `glusterfs --version` 输出 `glusterfs 11dev`；`11dev` 是开发版，不对应某个 release tag。
- 源码 commit：`79154ae538f4539ccf69272b2b4736d891262781`
- commit 日期：`2022-06-01 11:01:31 +0200`
- commit message：`configure.ac: run autoupdate on it to update to a newer autoconf`
- upstream：`https://github.com/gluster/glusterfs`
- 精确源码下载：
  - `https://github.com/gluster/glusterfs/archive/79154ae538f4539ccf69272b2b4736d891262781.tar.gz`
  - 或 `git clone https://github.com/gluster/glusterfs.git && git -C glusterfs checkout 79154ae538f4539ccf69272b2b4736d891262781`
- VM 内源码路径：`/root/glusterfs`（见 `src/dfs-config/glusterfs/readme.md`）。
- 本地 patch 已确认（`git status --short` + `git diff`）：

  | 文件 | 本地改动 | 是否影响 DFS 语义 |
  |---|---|---|
  | `xlators/mount/fuse/src/fuse-bridge.c` | `fuse_dispatch` 加 `__ucov_start(finh->pid)` / `__ucov_stop()` | 否；覆盖率采集，可能带来少量时序/性能开销 |
  | `xlators/mount/fuse/src/fuse-bridge.c` | `notify` 移动日志，并用 `event_graph` 决定是否打印 graph id | 否；只改日志 |
  | `xlators/performance/io-threads/src/io-threads.c` | `iot_worker` 围绕 `call_resume` 加 `__ucov_start/__ucov_stop`，并对 `stub->frame/root` 判空 | 否；覆盖率采集 |
  | `xlators/mount/fuse/src/fuse-bridge-2.h` | 未跟踪；`grep` 无任何引用 | 否；未参与构建，可忽略 |
  | `test.c`、`liburing-dev_*.deb` | 未跟踪的临时文件 | 否；不参与构建 |

- 因此源码分析基线为：**上游 `79154ae...` + 上述本地 patch**。分析时以上游代码为准，跳过 `__ucov_start` / `__ucov_stop` / `event_graph` 日志改动。
- `src/bugs/...818f935...` 这类 40 位哈希是 ASan 崩溃标识，不是 GlusterFS 源码 commit。

### 2.9 GlusterFS 文件树恢复（已定）

GlusterFS 不使用 HMDFS 的 server-side brick 恢复，也不执行 `gluster volume stop/start`。恢复采用 **client-side logical restore**：一个 reset client 清空固定测试根并从本地 baseline 重新复制，其他 client 通过 barrier 等待。

**范围与假设**

- 固定测试根：`/root/glusterfs-client/eifuzz_root`；不对 volume 根目录做恢复。
- baseline：reset client 本地 `/home/baseline/glusterfs/`。
- baseline 内容：普通文件、目录、空目录；不含 xattr、symlink、hardlink。
- GlusterFS 没有 HMDFS 的 `dcache_threshold=1000` 语义；`Persistence_dir` 保持为空，`is_persist` 恒为 false。baseline 文件数按覆盖需求设计，不按 1000 阈值设计。
- 不恢复 server brick，不按 replica/distributed/dispersed 写不同恢复逻辑。
- designated reset client 固定为 `executor_index == server_num`，3-3 下即 index 3。
- prefill 和 restore 使用同一份 `/home/baseline/glusterfs/`；prefill 只在 volume 挂载后执行一次，restore 每轮执行。
- 旧 `dfs-*`、`lost+found` 和 server brick 路径不进入 FileTree/CSAN；固定测试根下只保留 `eifuzz_root`。

**每轮流程**

1. 所有 executor 结束上一轮执行，并 `chdir("/")`，确保没有 cwd/fd 停留在测试根内。
2. restore start barrier：所有节点到达后，reset client 才开始恢复。
3. reset client（固定第一个 client，即 `executor_index == server_num`）执行：
   ```bash
   rm -rf /root/glusterfs-client/eifuzz_root
   mkdir -p /root/glusterfs-client/eifuzz_root
   timeout 60s rsync -a --numeric-ids --delete \
     /home/baseline/glusterfs/ \
     /root/glusterfs-client/eifuzz_root/
   sync
   ```
   说明：`--delete` 已启用，不需要 `--force`；测试根已清空，rsync 只负责复制 baseline。
4. reset 完成后进入 restore finish barrier；任一节点失败则本轮统一失败。
5. barrier 通过后，各 client 进入测试根执行；若跨 client 仍观察到旧 dentry/attr，再执行 `drop_caches`，必要时 remount 作为回退。

**为什么采用“清空 + 复制”**

- `rsync --delete` 只删除目标中多出来的路径，不能保证清除已有路径上的额外 xattr。
- `-H` 只保证源中的 hardlink 关系，不保证打断目标中已有的、源中不存在的 hardlink。
- `-X` 在 GlusterFS FUSE 下的可见性和行为需要额外验证，不能作为绝对保证。
- 清空后重新复制会创建新 inode/dentry，能靠构造保证 baseline 没有 xattr/hardlink/symlink 残留。
- 代价是每轮完整复制 baseline；baseline 较小时可接受，后续若成本过高再评估增量 rsync + 属性清理验证。

**实现接入**

- `executor.cc` 的 `restore_initial_file_tree()` 按 `dfs_name` 分派：HMDFS 走现有实现，GlusterFS 走 `restore_glusterfs_file_tree()`，其他 DFS 暂不处理。
- GlusterFS server 节点不执行恢复，只参与 barrier；client 节点中仅 designated reset client 执行 `rm/rsync`。
- `is_restarting` 的 crash recovery 路径跳过整个 restore，保留崩溃现场。
- 复用现有 `restoreStart` / `restoreState` barrier；不新增 `srvSetupBit` 语义。
- reset 命令失败（rm/mkdir/rsync/sync）时先置失败状态，进入 finish barrier 后统一失败，不能继续在未恢复状态上执行。
- timing：rsync 外层 `timeout 60s`，restore start/finish barrier 30s/90s；
  不使用 `rsync --timeout=300`。GlusterFS baseline 约 100 文件，超时不应
  比 HMDFS 的 1000 文件大目录更长。完整 timing 表见
  `glusterfs-adaptation-plan.md` §10.4。

**验证**

- 同一 program 连续执行多次，比较客户端可见文件树、路径、类型、大小。
- 检查测试根无上一轮多出的路径、xattr、hardlink、symlink。
- 检查其他 client 在 barrier 后是否立即看到恢复后的树。
- 统计 `rm -rf + rsync + sync + barrier` 的单轮耗时。
- 若出现 server 端残留状态导致不可复现，再单独评估 brick-level restore；第一版不实现。

## 3. 专用 tracer 设计

### 3.1 模块拆分

- 每个 DFS 一套独立的 BPF 程序 + 用户态加载/收集逻辑。
- `dfs_trace` 只做分发，不含具体探针。
- `trace_common` 只放公共工具，不引用 HMDFS 内部实现。

> 2026-11-15 历史标记：§3.2–§3.3 的 0–14 事件契约、旧 opcode 映射和旧路径规则
> 已被 `glusterfs-adaptation-plan.md` §4 取代；保留本节仅用于追溯。

### 3.2 事件契约

- 复用 `prog.HmdfsTraceEvent` 的字段布局（`prog.go:68`）和 `ipc.go:1059` 的解析格式。
- 每个 tracer 输出 `[ratio][offset][gap][count][events]`；`ProgIdx` 仍由 IPC 层按 executor 设置。
- 0–14 func_id 视为通用操作类别，不绑定 HMDFS 函数名。
- 可选后续重命名为 `DfsTraceEvent` / `parseTraceEvents`，但会多动 DAG/IPC，暂不作为第一阶段。

### 3.3 GlusterFS

- 客户端挂载是 FUSE（`mount -t glusterfs`）；在客户端 VM 上追踪。
- 边界层候选：
  - VFS：`vfs_open`、`vfs_getattr`、`vfs_read`、`vfs_write`、`vfs_fsync`、`vfs_unlink`、`vfs_mkdir`、`vfs_rmdir`、`vfs_rename`；
  - 缺失的用 kprobe 补：`vfs_create`、`iterate_dir`、`setattr`、`lookup`；
  - FUSE 内核侧 `fuse_request_send` / `fuse_request_end` 仅作为备选；已确定走用户态 FUSE handler，见 §3.3.1。
- 实现层候选：glusterfs 客户端/brick 的 FOP uprobe；多进程、符号、异步，成本高，暂不作为首选。
- 过滤：FUSE sb magic `0x65735546`。
- 起止：已确定采用 **FUSE handler 边界**（见 §3.3.1）；VFS kprobe 保留为备选。

#### 3.3.1 FUSE handler 事件边界（已确定）

GlusterFS 客户端 tracer 采用用户态 `fuse-bridge` 的 FUSE handler 作为事件边界，不追踪 xlator fops。VFS kprobe 保留为回退方案。

**接口定位：**

- 源码：`xlators/mount/fuse/src/fuse-bridge.c`。
- 编译产物：`fuse.so`，本地 build 路径为
  `/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so`。
- `fuse_std_ops[]`（`fuse-bridge.c:6001`）是 FUSE opcode 到 handler 的总表。
- `fuse_dispatch`（`fuse-bridge.c:6068`）按 `finh->opcode` 分发。
- handler 通过 `FUSE_FOP` / `STACK_WIND` 进入 `xlator_fops`；第一阶段不追这一层。

**探针点：**

```text
开始：uprobe:/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so:fuse_dispatch
路径：uprobe:/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so:fuse_fop_resume
结束：uretprobe:/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so:send_fuse_iov
关联键：finh->unique
```

- `fuse_dispatch` 入口记录 `start_time`、`opcode`、`pid`、`nodeid`。
- `fuse_fop_resume` 在路径解析完成后调用，记录 `state->loc.path`；rename 等双路径操作使用 `state->loc2.path`，fd 类操作使用 `state->fd`。
- `send_fuse_iov` 返回时 reply 已写入 `/dev/fuse`，作为最接近用户 syscall 完成的结束点。
- `send_fuse_err` 也走 `send_fuse_iov`，因此错误路径能正常收尾。

**事件窗口语义：**

- `fuse_*` handler 返回不代表请求完成，用户 syscall 仍在等 FUSE reply。
- 只建模客户端可见的 FUSE 请求窗口，不建模 GlusterFS 后端异步写回或 brick 完成。
- 这一点与 HMDFS 的现有设计口径一致，保持跨 DFS 可比性。

**异步边界：**

- GlusterFS 的 `kernel-writeback-cache` 默认关闭，保持不变。
- FUSE init 默认声明 `FUSE_ASYNC_READ`，readahead 可能产生额外 read 请求。
- readahead/writeback 产生的请求有独立 `finh->unique`，会进入事件流；第一版接受或按 opcode/`FUSE_WRITE_CACHE` 过滤，不改变 FUSE 请求窗口定义。

**符号验证：**

- 本地 build 的 `fuse.so` 为 `not stripped`，带 debug info。
- `fuse_dispatch`、`fuse_fop_resume`、`send_fuse_iov` 均为可用的 local symbol。
- 其它 build 若 stripped，需要 debuginfo、重编译带符号版本，或改源码 tracepoint；不要假设总有用户态符号。

**过滤与兜底：**

- 过滤 `FUSE_FORGET`、`FUSE_INTERRUPT`、`FUSE_INIT`、`FUSE_DESTROY`、`FUSE_BATCH_FORGET` 等非文件操作。
- `fuse_fop_resume` 不保证覆盖所有早期解析失败；无路径时用 opcode / nodeid 兜底。
- `send_fuse_iov` 可能被非请求回复调用；用 pending `unique` map 过滤。
- 一个 `unique` 只应结束一次；输出事件后删除 map 项。

**操作映射：**

v1 覆盖的 FUSE opcode（含 link/symlink/readlink/xattr、READDIR 28/44）以 `glusterfs-adaptation-plan.md` §3/§4 为准；下面是早期 0–14 记录：

```text
FUSE_LOOKUP       -> lookup
FUSE_GETATTR      -> getattr
FUSE_SETATTR      -> setattr / truncate
FUSE_MKDIR        -> mkdir
FUSE_CREATE       -> create
FUSE_UNLINK       -> unlink
FUSE_RMDIR        -> rmdir
FUSE_RENAME       -> rename
FUSE_OPEN         -> open
FUSE_READ         -> read
FUSE_WRITE        -> write
FUSE_OPENDIR      -> dir_open
FUSE_READDIR      -> iterate
FUSE_FSYNC        -> fsync
FUSE_RELEASE      -> release
```

`mknod`、`symlink`、`link`、`flush`、xattr、lock 暂不进入第一版事件契约。

### 3.4 NFS

- 客户端为内核 NFS；在客户端 VM 上追踪。
- 边界层候选：
  - VFS：同 GlusterFS 的 VFS 集合；
  - NFS：`nfs_file_*`、`nfs_*_done`、`rpc_task_*` 等，用于 RPC 完成点。
- 实现层候选：`nfs_*` / `nfsd_*` 内核函数 kprobe。
- 过滤：NFS sb magic `0x6969`。
- 服务端 `nfsd` 事件是否纳入未定；默认先做客户端边界。

### 3.5 executor 接入

- `init_dfs_trace(dfs_name)` 在启动时按 DFS 选择模块。
- `start_dfs_trace()` 在程序执行前启动收集。
- `stop_collect_dfs_trace()` 在执行结束、`write_metadata` 前停止、排序、转换时间戳并写输出。
- HMDFS 行为不变；GlusterFS/NFS 不再调用 `init_hmdfs_trace`。

### 3.6 `merge_view` 前缀解耦（已定：空前缀）

目标：`merge_view` 只作为 HMDFS/默认测试的逻辑根；非 HMDFS 不再要求真实 `merge_view` 目录。

决定：非 HMDFS 使用**空前缀**（根 = 固定测试根 `/root/<dfs>-client/eifuzz_root`）。空前缀路径更干净（`file`、`a/b`），但 depth/root/fallback 要 prefix-aware。不采用 `"."` 虚拟根（参考系统也没有这层抽象，见 §3.7）。

#### 3.6.1 数据模型

- `FileTree.Prefix string`；`NewFileTree()` 默认 `MergeViewPrefix`（`merge_view/`），新增 `NewFileTreeWithPrefix(prefix)`，测试默认不动。
- `Hmdfs_config.RootPrefix string`：HMDFS `merge_view/`，非 HMDFS `""`；`proc.go` 创建 FileTree 时传入。
- 空前缀 Root：`Name=""`、`FullPath=""`；子节点 FullPath 用 `JoinRoot`，避免 `"/file"`。HMDFS Root 仍 `merge_view`。

#### 3.6.2 路径原语

- `RootPath()`、`JoinRoot(rel)`、`NormalizePath(prefix, path)`、`IsUnderRoot(prefix, path)`。
- `addNodeLocked` / `removeNodeLocked` / `RenameNode` / `findNodeLocked` / `parentNodeLocked` / `NormalizeMergeViewPath` / `IsPathInMergeView` / `SyncFileTreeFromFsMd` 全部走 `ft.Prefix`；`NormalizeMergeViewPath` 保留默认 wrapper 给测试/HMDFS。
- 非 HMDFS 空前缀：`NormalizePath` 只去 `./`、首 `/`，不补前缀；Root 的 parent 解析直接返回 Root。

#### 3.6.3 深度桶

- 非 HMDFS 顶层 `file` 在现有 `DepthBucketOf` 下是 bucket 0，`a/b` 是 bucket 1，整体错位；不能只补 `n==0`。
- 新增 `DepthBucketOfWithPrefix(path, prefix)`：先去掉 prefix，再按相对路径组件数分桶；`DepthBucketOf` 保留默认 `merge_view/` wrapper。
- `FileTree.DepthBucketOf`、DAG、覆盖板、directed_plan 用对应前缀；测试默认不改。

#### 3.6.4 硬编码 fallback

- `rand.go` 约 13 处、`generation.go` 1 处、`mutation.go` 5 处写死 `merge_view` / `merge_view/test_dir`；改成 `RootPath()` / `JoinRoot()`，空前缀时避免生成 `/mut_dir`。
- `TransformDevicePathComplete` 当前无调用者；若启用需加 prefix 参数，暂不阻塞。
- `proc.go` InitialDir 的 `"merge_view/" + ...` 改 `hc.RootPrefix`；`Persistence_dir` 保持 HMDFS-only。

#### 3.6.5 DAG / checker / executor

- `dag.go` 的 `mergeRoot`（`:1034`）、`firstComponentUnderRoot`（`:1104-1111`）用 `ft.Prefix` / `RootPrefix`。
- `checker/conc-fs.go`（`:389-425`）的 `TrimPrefix(..., prog.MergeViewPrefix)` 和硬编码 `"merge_view"` 检查改用 `ft.Prefix`。
- `executor.cc:3443-3450` 的 `write_metadata`：非 HMDFS 必须从 `dfs-<pid>-<iter>` 改成固定测试根；HMDFS 才用 `merge_view`。否则 cwd 改固定测试根后 fsMd 为空/错误。
- 专用 tracer 输出路径需按当前 FileTree 根归一化；HMDFS trace 保持 `merge_view/...`。

#### 3.6.6 测试

- 保留 `NewFileTree()` 默认，现有测试大多不动。
- 新增空前缀测试：Root/Join、`file` vs `a/b` 深度桶、parent、Select、Sync、DAG、checker。
- 少数直接断言 `MergeViewPrefix` 的测试改默认 wrapper 或显式 prefix。

#### 3.6.7 影响面

- 非测试代码 40+ 处 `MergeViewPrefix` / `"merge_view"` 引用；测试 100+ 处，但默认构造函数保留后多数无需改。
- 主要风险不是字符串替换，而是：根路径前导斜杠、深度桶错位、fallback 默认路径、`write_metadata` 路径、checker/DAG 归一化。

#### 3.6.8 实施计划（方案已定，代码未实施）

目标：把 `merge_view` 从“所有 DFS 共用的路径前缀”改为“HMDFS/默认测试的逻辑根”，让 GlusterFS/NFS 使用空前缀（根 = 挂载根），同时不改 HMDFS 行为。

原则：

- 默认 `NewFileTree()` 保持 `merge_view/`，现有测试与 HMDFS 零回归；
- 空前缀只通过显式 `RootPrefix`/`NewFileTreeWithPrefix("")` 启用；
- 不引入 `.` 虚拟根；
- 逐段落地，避免一次性全局字符串替换。

**P1：FileTree 数据模型**

- `FileTree.Prefix string`，默认 `merge_view/`；
- 新增 `NewFileTreeWithPrefix(prefix)`，`NewFileTree()` 作为默认 wrapper；
- 新增 `RootPath()` / `JoinRoot(rel)`；
- 空前缀 Root：`Name=""`、`FullPath=""`；HMDFS Root 仍 `merge_view`；
- `Hmdfs_config.RootPrefix`：HMDFS `merge_view/`，非 HMDFS `""`；
- `proc.go` 创建 FileTree 时传入 `RootPrefix`。

验收：

- 默认构造的树路径与当前逐字一致；
- 空前缀下 `file`、`a/b` 不出现前导 `/`；
- 空前缀 Root 的 parent 解析直接返回 Root。

**P2：路径原语 prefix-aware**

- 新增/改造 `NormalizePath(prefix, path)`、`IsUnderRoot(prefix, path)`；
- `addNodeLocked`、`removeNodeLocked`、`RenameNode`、`findNodeLocked`、`parentNodeLocked` 使用 `ft.Prefix`；
- `NormalizeMergeViewPath` / `IsPathInMergeView` 保留默认 wrapper，供 HMDFS/旧测试使用；
- `SyncFileTreeFromFsMd` 先按当前 prefix 过滤，避免把 `dfs-*`、`lost+found` 等拼进 `merge_view/`；
- 新增 `DepthBucketOfWithPrefix(path, prefix)`：先去 prefix，再按相对路径组件数分桶；
- `FileTree.DepthBucketOf`、DAG、覆盖板、directed_plan 改用当前 prefix；`DepthBucketOf` 保留默认 wrapper。

验收：

- 空前缀 `file` 为 depth 1，`a/b` 为 depth 2；
- 不再整体错位到 bucket 0/1；
- HMDFS 默认测试不变。

**P3：路径调用点改造**

- `rand.go` 约 13 处、`generation.go` 1 处、`mutation.go` 5 处写死的 `merge_view` / `merge_view/test_dir` 改为 `RootPath()` / `JoinRoot()`；
- `proc.go` InitialDir 的 `"merge_view/" + ...` 改为 `hc.RootPrefix`；`Persistence_dir` 保持 HMDFS-only；
- `executor.cc` 的 `write_metadata`：非 HMDFS 从 `dfs-<pid>-<iter>` 改为固定测试根；HMDFS 才用 `merge_view`；
- `checker/conc-fs.go` 的 `TrimPrefix(..., prog.MergeViewPrefix)` 和硬编码 `"merge_view"` 检查改用 `ft.Prefix`；
- `dag.go` 的 `mergeRoot`、`firstComponentUnderRoot` 使用 `ft.Prefix` / `RootPrefix`；
- 专用 tracer 输出路径按当前 FileTree 根归一化；HMDFS trace 保持 `merge_view/...`；
- `path_set.go`：`normalizePathSetPath`、`pathSetTopRoot`、`pathSetTopPath`、`mapPathSetPath` 去掉硬编码 `MergeViewPrefix`，改为接受 prefix/FileTree；空前缀时 top root 从相对路径第一段取。
- PathSet 的 dual-path 规则以 `docs/glusterfs-adaptation-plan.md` §8.7 和
  `docs/seed-construction-causal-chain-notes.md` §1.5 为准：link/symlink/
  readlink/rename 的 canonical target/linkpath 一起迁移，symlink payload 在
  调用物化时重算，rename dst 由 RenamePolicy 选；prefix 改造不能只改普通
  单路径成员，否则 link/symlink 会在空前缀下错位。
- PathSet directed target arm 的成员要求是 `len(Paths)==1`；rename/link/
  symlink 整体排除，readlink 由 call-name gate 排除。双路径只走 broad。
- `symlinkCanonicalTarget(linkpath, payload)` 是共享 helper；PathSet/DCT/DAG/
  checker 都必须用它，不能把 payload 当 FileTree 路径。

验收：

- cwd 改固定测试根后 `write_metadata` 不再扫 `dfs-*`；
- checker/DAG 对空前缀路径匹配正确；
- 非 HMDFS 不需要真实 `merge_view` 目录；
- PathSet 在空前缀下可构建、可迁移。

**P4：测试**

- 保留所有 `NewFileTree()` 默认测试；
- 新增空前缀测试：`Root/Join/Normalize/Parent/Select/Sync`；
- 新增空前缀深度桶测试；
- 新增空前缀 DAG/checker 测试；
- 新增空前缀 PathSet 构建、迁移与 target arm 测试；
- 新增空前缀 dual-path link/symlink/readlink PathSet 测试（canonical target 迁移、
  symlink payload 重算、readlink follower）；
- GlusterFS/NFS 冒烟路径：`file`、`a/b`。

**P5：验收与回退**

验收：

- HMDFS 全量测试通过；
- 非 HMDFS 路径不再出现 `merge_view/`；
- PathSet 在空前缀下可构建、可迁移；
- executor/checker/DAG 路径一致；
- GlusterFS/NFS 冒烟通过。

回退：

- 默认构造函数保持旧行为；
- 新逻辑只在显式 `RootPrefix` 下启用；
- 可按 P1–P5 逐段回退，不需要整体回滚。

**暂不纳入**

- NFS tracer 层级与异步完成点；
- NFS 镜像/版本；
- 事件重命名（第一版继续使用 `HmdfsTraceEvent`）；
- CSAN 误报的进一步调优。

**当前硬编码清单（待 prefix 层统一处理）**

- `src/prog/path_set.go`：
  - `normalizePathSetPath` 固定调用 `NormalizeMergeViewPath`；
  - `pathSetTopRoot` / `pathSetTopPath` 固定 `MergeViewPrefix`；
  - `mapPathSetPath` 固定按 `MergeViewPrefix` 去掉顶层路径；
  - `pathSetTargetLevel` 使用默认 `DepthBucketOf`，未按 FileTree prefix 分桶；
- 因此当前 PathSet 只适用于 HMDFS，不能直接用于 GlusterFS/NFS；prefix 层落地后统一改，不做零散替换。

**主要风险**

- 根路径前导斜杠；
- 深度桶错位；
- fallback 默认路径；
- `write_metadata` 路径；
- checker/DAG 归一化；
- PathSet 当前硬编码 `MergeViewPrefix`，必须等 prefix 层落地后统一改，不能零散替换。

### 3.7 参考系统调研：Monarch / Horcrux 的根路径

调研对象：Monarch main（本仓库）、Monarch back（`../Monarch-master-back`）、Horcrux（`HorcruxSourceCode/`）。

结论：两者都没有通用“虚拟根”；根就是实际挂载点/真实目录。

**Monarch**

- main/back 都没有跨 DFS 的 root prefix。
- HMDFS：根是真实的 `merge_view` 目录；back `executor.cc:1297-1302` 直接 chdir 到 `/mnt/hmdfs/100/non_account/merge_view`，main `:1689-1693` 是 HMDFS 挂载根、非 HMDFS `/root/<dfs>-client/dfs-*`；`write_metadata` 在 `:3443-3450`。
- `merge_view` 是 HMDFS 的真实目录，不是虚拟抽象。

**Horcrux**

- `FileTree(path)` 的 `path` 是实际 mount path（`fileTree.py:10-14`）。
- `dfsFuzzer.py:2283` 硬编码 `clients_mount_places = ["/mnt/mycephfsA/test_dfs_fuzzer/smf/", ...]`；`reset_top(path)` 换的是实际 mount 根。
- `generate_test_files.py:50-61` 用 `node_dir` + `rel_path` 生成相对路径，根级为 `.`；`oracle.py:9` 也硬编码 mount 路径。
- 论文里的 “root operation” 是每轮初始操作，不是路径根。

**参考价值**

- 支持“根 = 实际挂载根”，没有 `.` 虚拟根先例。
- Horcrux 的 `FileTree(path)` / `reset_top(path)` 是“root path 参数化”的先例，对应我们的 `FileTree.Prefix` / `RootPath`。
- Horcrux 的相对路径 + depth-1/depth-2 候选支持空前缀方案；其 mount 路径按 client/DFS 不同而硬编码，我们共享命名空间用一个 `RootPrefix` 即可。

因此：非 HMDFS 用**空前缀**；HMDFS 用真实 `merge_view`；不引入 `.` 虚拟根。

## 4. 已定 / 仍待确认

### 4.1 已定

1. **GlusterFS tracer 层级**：客户端 FUSE handler，P0 已通过；三点为 `fuse_dispatch → fuse_fop_resume → send_fuse_iov`。
2. **GlusterFS 异步完成点**：以 `send_fuse_iov` 为客户端可见完成点，不建模后端异步写回。
3. **拓扑与卷模式**：3-3，`replica 3`；6 × 2C / 10G。
4. **data.img**：不启用；brick 在 server rootfs 的 `/root/glusterfs-server`。
5. **预填**：host 生成一棵 `shared` 树，烤进 client 镜像 `/home/baseline/glusterfs/`；client1 挂载后 rsync 到 volume。
6. **cid**：`"shared"`。
7. **CSAN**：`enable_csan=true`，`enable_c2san=false`；fsMds 只保留 client 固定测试根下路径，server fsMd 不参与 pairwise；`initTreeSubset`/symsc 改为空前缀。
8. **testdirIno**：固定为 `/root/glusterfs-client/eifuzz_root` 的 inode。
9. **旧 `dfs-*` 目录**：不再创建；不进入 FileTree/CSAN；残留由 prefill/镜像重置清理。
10. **GlusterFS restore**：reset client 固定 `executor_index == server_num`；prefill/restore 共用 baseline；不恢复 server brick。

### 4.2 仍待确认

1. **事件命名**：暂时保留 `HmdfsTraceEvent` / `parseHmdfsTraceEvents`，B 方案下只给 Go struct 增加可选字段；是否后续重命名为 `DfsTraceEvent` 未定。
2. **NFS 边界**：只做 client 边界，还是加入服务端 `nfsd` 事件？
3. **NFS 完成点**：RPC completion 如何定义。
4. **NFS 镜像/版本**：Monarch 5.15 镜像还是自有 6.6 + NFS4.2。
5. **CSAN 误报调优**：若空前缀下出现误报，是否临时关闭 CSAN 或缩小 fsMd 范围。
6. **restore 缓存回退**：跨 client 旧 dentry/attr 是否足够；`drop_caches` / remount 的触发阈值待实测。

## 5. 风险与备注

- FUSE handler 边界有意不建模后端异步写回；这与 HMDFS 现有口径一致，但会漏掉后端完成时序。
- GlusterFS 的 readahead 默认开启，可能产生额外 read 请求；第一版接受或过滤，需要保持跨 DFS 口径一致。
- `fuse.so` 在其它 build 中可能被 strip；不能假设 `fuse_dispatch` / `send_fuse_iov` 符号总是可用。
- 早期路径解析失败可能不经过 `fuse_fop_resume`，事件路径需要 opcode/nodeid 兜底。
- GlusterFS 逻辑恢复只重置客户端可见命名空间，不重置 server brick 内部状态；若出现 server 侧残留导致不可复现，需 campaign 重置或升级为 brick-level restore。
- 每轮 `rm -rf + rsync` 的成本取决于 baseline 大小；需要实测单轮耗时。
- `testdirIno` 固定为测试根；GlusterFS 当前 symsc 不消费该值，但必须保持稳定。
- CSAN 在空前缀下必须过滤 fsMd 和 `initTreeSubset`，否则会产生误报或空初始树。
- `data.img` 不启用，brick 在 server rootfs；3-3 replica 下每个 server rootfs 都有一份完整副本，需要监控容量。
- 3-3 资源开销 6 × 2C / 10G；资源不足时只调 `vm.mem`。
- 路径前缀/绝对路径归一化不一致会导致 DAG path 匹配失败。
- `merge_view` 解耦的主要坑：深度桶错位、根路径前导斜杠、fallback 默认路径、`write_metadata` 路径、checker/DAG 归一化。

## 6. 参考代码位置

- `src/executor/executor.cc`：cwd（`:1691-1745`）、trace 初始化（`:711`）、start（`:2068`）、stop（`:1944`）、`write_metadata`（`:3443-3450`）。
- `src/syz-fuzzer/proc.go`：hc 初始化（`:99-304`）、lcs/board（`:310-327`）、InitialDir（`:145-299`）、sync gate（`:1487-1498`）、DAG gate（`:1513-1526`）。
- `src/prog/directed_plan.go`：`len(Cids) == len(ps)`（`:1132`）。
- `src/prog/distributed_choice.go`：`FileTree`/`NewFileTree`（`:181-210`）、`InitFromHmdfsConfig`（`:212`）、`addNodeLocked`（`:261`）、`findNodeLocked`（`:549`）、`parentNodeLocked`（`:578`）、深度选择（`:714/1119`）、`NormalizeMergeViewPath`（`:2131-2137`）、`SyncFileTreeFromFsMd`（`:2385`）。
- `src/prog/dag.go`：`mergeRoot`（`:1034`）、`firstComponentUnderRoot`（`:1101-1111`）、`DepthBucketOf`（`:1414`）。
- `src/prog/rand.go`：`merge_view` fallback（`:342/361/368/371/376/390/1247/1257/1559/1744/4128/4183/4216`）。
- `src/prog/generation.go`：fallback `testDir`（`:274`）。
- `src/prog/mutation.go`：fallback `dir` / `testDir`（`:5664/5711/5762/5797`）。
- `src/checker/conc-fs.go`：`initTreeSubset` 的 `MergeViewPrefix` / `"merge_view"`（`:389-425`）。
- `src/prog/hmdfs_conflict.go`：`CanonicalizeFsMd`（`:63`）。
- `src/prog/prog.go`：`HmdfsTraceEvent`（`:68`）、func_id 名称（`:83`）。
- `src/executor/hmdfs_trace.bpf.c`：func_id 常量（`:115-131`）。
- `src/pkg/ipc/ipc.go`：trace 解析（`:1059`）。
- `fuzz-config/all-config/glusterfs/3-2/...`：3-3 配置的复制基础。
- `fuzz-config/all-config/nfs/1-2/...`：NFS 1-2 配置。
- `src/dfs-config/glusterfs/readme.md`：GlusterFS 源码编译、ucov 插桩与 `mount.glusterfs` 修补说明；VM 源码路径 `/root/glusterfs`。
- `src/dfs-config/glusterfs/glusterfs-config.sh`：`srv_cnt` / `client_cnt` 与挂载流程。
- `src/dfs-config/nfs/nfs-config.sh`：NFS export/mount 流程。

### 6.1 GlusterFS 客户端 FUSE 代码位置

- `glusterfs-79154ae538f4539ccf69272b2b4736d891262781/xlators/mount/fuse/src/fuse-bridge.c`
  - `send_fuse_iov`（`:352`）
  - `fuse_std_ops[]`（`:6001`）
  - `fuse_dispatch`（`:6068`）
  - `fuse_fop_resume`（`:1149`）
  - 各 `fuse_*` handler（如 `fuse_lookup`、`fuse_write`、`fuse_rename`）
- `.../xlators/mount/fuse/src/fuse-bridge.h`
  - `FUSE_FOP` 宏：`STACK_WIND(frame, ret, xl, xl->fops->fop, args)`
- `.../xlators/mount/fuse/src/Makefile.am`
  - `xlator_LTLIBRARIES = fuse.la`
  - `fuse_la_SOURCES` 含 `fuse-bridge.c`
- `.../libglusterfs/src/glusterfs/xlator.h`
  - `struct xlator_fops`（`:545`）
- `.../xlators/protocol/client/src/client.c`
  - `struct xlator_fops fops`（`:2852`）
- `.../xlators/storage/posix/src/posix.c`
  - `struct xlator_fops fops`（`:30`）
- VM 内 FUSE 模块路径：
  - `/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so`

## 7. GlusterFS tracer 当前进展（2026-11-14）

### 7.1 已完成

- GlusterFS 文件树恢复方案已定并写入 §2.9：固定测试根、单个 reset client、`rm -rf` + baseline rsync，不恢复 server brick。
- HMDFS restore 超时与 rsync 参数已更新：
  - node-down 15s、rsync 60s、node-up 30s；
  - barrier 拆分为 start 30s / finish 90s；
  - rsync 使用 `-a --numeric-ids --delete --one-file-system --checksum`，不使用 `-H/-A/-X` 和 `--force`。
- 已创建 `docs/chroot-image-package-install.md`，记录 qcow2 + chroot 安装流程。
- GlusterFS 版本与源码基线已确认：`glusterfs 11dev`，源码 commit `79154ae538f4539ccf69272b2b4736d891262781`。
- FUSE 库 `/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so` 为 not stripped、带 debug_info。
- 以下符号已确认存在且为 `LOCAL FUNC`：
  - `fuse_dispatch`
  - `fuse_fop_resume`
  - `send_fuse_iov`
- 已构建带 BTF/uprobe 的 5.15 内核，`/sys/kernel/btf/vmlinux` 存在，`bpftrace BEGIN` 成功。
- 单 VM 最小 GlusterFS volume 已创建并挂载，FUSE P0 已通过。

### 7.2 P0 结论

- `send_fuse_iov`、`fuse_dispatch`、`fuse_fop_resume` 三点均可 attach。
- 同一次运行中 D/R/S 的 `unique` 可配对。
- `rename` 同时有 `Path` 和 `Path2`。
- `READDIRPLUS` / `READ` 的 `loc.path` 为空，但 `nodeid` 可映射。
- `GETXATTR`、`FLUSH`、`p=0`、`FORGET` 需过滤或特殊处理。
- `finh->nodeid` 是 `inode_t *`，不是 `st_ino`；不能直接作为 `fsMd.StatMd.Ino`。
- 客户端 FUSE handler 方案成立，VFS kprobe 不再作为第一版方案。

### 7.3 采用 B 方案

- GlusterFS 使用独立 wire event，不修改 HMDFS 的 112 字节事件。
- GlusterFS 使用独立 FUSE tracer，不修改 HMDFS 的 BPF 探针。
- Go 层给 `HmdfsTraceEvent` 增加可选 `NodeID/Path/Path2/Truncated` 字段，DAG 共用。
- 完整实现规格见 [`glusterfs-tracer-implementation-spec.md`](glusterfs-tracer-implementation-spec.md)。

### 7.4 待实现

- `glusterfs_trace.bpf.c` / `.cc` / `.h`。
- `dfs_trace` 分发。
- `src/Makefile` 的 BPF skeleton 规则。
- `parseGlusterfsTraceEvents` 与 IPC 分支。
- DAG `Path/Path2` 接入。
- FileTree 空前缀改造。
- CSAN fsMd 过滤和 `initTreeSubset`/symsc 空前缀适配。
- GlusterFS prefill/restore 代码。
- 3-3 冒烟。
- NFS 延后；NFS 后续 P0 应优先验证 NFS client 内部 fops，而不是默认 VFS 层。

### 7.5 内核编译与运行环境

当前已验证内核和配置的完整记录见：

```text
docs/kernel-build-glusterfs-bpf.md
```

摘要：

- 当前内核：`Linux 5.15.0++ #2`，GCC 11，`pahole 1.25`。
- 保留 KASAN、SLUB、SELinux。
- 关闭 IMA/EVM/INTEGRITY，避免 `integrity_inode_get: lsm=integrity required` panic。
- 当前 LSM：`lockdown,capability,selinux`；旧内核：`capability,selinux`。
- `CONFIG_FUNCTION_TRACER` 不是 uprobe 前置条件；`available_filter_functions` 缺失不影响 FUSE P0。
- `CONFIG_DEVMEM=n`、`CONFIG_UIO=n` 不影响 ivshmem PCI resource mmap。

**GlusterFS FUSE P0 必需**

```text
CONFIG_BPF=y
CONFIG_BPF_SYSCALL=y
CONFIG_BPF_JIT=y
CONFIG_BPF_EVENTS=y
CONFIG_PERF_EVENTS=y
CONFIG_UPROBES=y
CONFIG_UPROBE_EVENTS=y
CONFIG_DEBUG_FS=y
CONFIG_FTRACE=y
CONFIG_DEBUG_INFO_BTF=y
CONFIG_KALLSYMS=y
CONFIG_KALLSYMS_ALL=y
CONFIG_FUSE_FS=y
CONFIG_PCI=y
CONFIG_SYSFS=y
CONFIG_PROC_FS=y
CONFIG_VIRTIO=y
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_NET=y
CONFIG_EXT4_FS=y
CONFIG_DEVTMPFS=y
CONFIG_DEVTMPFS_MOUNT=y
CONFIG_NET=y
CONFIG_INET=y
CONFIG_IP_PNP=y
CONFIG_SYSVIPC=y
CONFIG_SYSVIPC_SYSCTL=y
CONFIG_SHMEM=y
CONFIG_TMPFS=y
CONFIG_IPC_NS=y
CONFIG_KASAN=y
CONFIG_SLUB=y
CONFIG_SLAB=n
CONFIG_SECURITY_SELINUX=y
CONFIG_LSM="selinux"
CONFIG_IMA=n
CONFIG_EVM=n
CONFIG_INTEGRITY=n
```

**可选**

```text
CONFIG_BPF_JIT_ALWAYS_ON=y
CONFIG_KPROBES=y
CONFIG_KPROBE_EVENTS=y
CONFIG_FUNCTION_TRACER=y
CONFIG_DYNAMIC_FTRACE=y
CONFIG_IKHEADERS=y
```

说明：

- `KPROBES` / `KPROBE_EVENTS` 供 VFS kprobe fallback 或 NFS 使用，不是 GlusterFS FUSE P0 必需。
- `FUNCTION_TRACER` / `DYNAMIC_FTRACE` 只影响 ftrace 函数追踪和 `available_filter_functions`。
- `IKHEADERS` 在 BTF 可用时不是必需；如果启用，建议 `=y`，避免 rootfs 模块问题。
- `ucov` 使用 SysV shm/FIFO，不使用 KCOV。
- 当前内核构建中的 GCC 11、pahole ENUM64、IMA/EVM、LSM、tracefs 等已知问题，均记录在 `docs/kernel-build-glusterfs-bpf.md`。
