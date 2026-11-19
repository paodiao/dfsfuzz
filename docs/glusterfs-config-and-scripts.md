# GlusterFS 配置、初始树与脚本

> 状态：config/scripts/restore 已实现；D1 BPF standalone smoke 已通过，
> D2 executor integration 待执行
> 日期：2026-11-15（2026-11-18 更新实现状态与 D1 结果）
> 范围：GlusterFS 3-3 replica 3 的 config、InitialDir 加载、generate_test_files、
> prefill/restore、node-up/node-down、mount 选项
> 相关：`docs/glusterfs-adaptation-plan.md`、`docs/executor-file-tree-restore.md`

## 0. 结论摘要

- `enable_syscalls` 固定为 22 个（见 §1）；不启用
  `readv/writev/preadv/pwritev`，也不做 alias。
- GlusterFS config **不传 cid**；profile 固定路径所有者为 `shared`。
- `InitialDir` 仍然要读，但 GlusterFS 只读 `shared.file/.dir/.tmpdir` 和
  `empty_dirs.info`，不读 `large_dir.info`。
- `Cids` 由 profile 生成：重复 `"shared"` 到 `ClientNum`；只用于长度兼容，
  不参与 client identity。
- `generate_test_files.py` 用唯一 `shared` 作为 node id，输出 `shared.*` 清单；
  `--num-large-files 0` 时应跳过整个 large-dir 块。
- baseline 只复制 `shared/` 的内容到 `/home/baseline/glusterfs/`，不要带
  `shared/` 这层目录。
- `glusterfs-config.sh` 负责 volume create/start 和 client mount；
  `glusterfs-node-up.sh` 负责 server glusterd/client remount；
  `glusterfs-node-down.sh` 只在 failure/crash 场景使用。
- normal restore 只做 client 侧 `rm -rf + rsync`，不重启 server/volume。

### 0.1 上线前检查清单

```text
[ ] /root/glusterfs-client          空目录，Volume 挂载点
[ ] /root/glusterfs-server          空目录，server brick
[ ] /root/dfs-fuzzing/libucov.so    LD_PRELOAD
[ ] /home/baseline/glusterfs/       client baseline
[ ] rsync                           可执行
[ ] sshpass                         可执行
[ ] gluster / glusterd              可执行
[ ] /root/.ssh/authorized_keys      focal.id_rsa.pub
[ ] ssh -i focal.id_rsa root@VM     可登录
[ ] eifuzz_root                     不预置，由首轮 restore 创建
```

当前没有独立 `prefill.sh`；首轮 `receive_execute` 的 `restore_initial_file_tree()`
由 reset client 执行，等价于 prefill。不再依赖 manager 复制 prefill 脚本。

## 1. 3-3 config

新增 GlusterFS 3-3 normal 配置，例如：

```json
{
  "target": "linux/amd64",
  "workdir": "ROOTDIR/fuzzing-dir/",
  "image": "ROOTDIR/dimage-kernel/glusterfs/focal.qcow2",
  "sshkey": "ROOTDIR/dimage-kernel/glusterfs/focal.id_rsa",
  "monarch": "ROOTDIR/src/",
  "enable_syscalls": [
    "open", "read", "write", "close", "stat", "pread64", "pwrite64",
    "fsync", "fdatasync", "truncate", "getdents64",
    "rename", "mkdir", "rmdir", "creat", "unlink", "chmod",
    "link", "symlink", "readlink", "getxattr", "setxattr"
  ],
  "disable_syscalls": [
    "bpf$*", "mount", "ioctl$*", "fcntl$setlease", "openat$*", "write$P9*"
  ],
  "server_num": 3,
  "fuzzing_vms": 6,
  "dfs_name": "glusterfs",
  "dfs_setup_params": "3 3 'replica 3'",
  "kernel_server": false,
  "kernel_client": false,
  "net_failure": false,
  "node_crash": false,
  "enable_csan": true,
  "enable_c2san": false,
  "enable_server_feedback": false,
  "enable_client_feedback": true,
  "vm": {
    "cmdline": "net.ifnames=0",
    "count": 6,
    "cpu": 2,
    "mem": 10240,
    "kernel": "ROOTDIR/dimage-kernel/glusterfs/kernel-5.15-glusterfs-bzImage"
  }
}
```

约束：

- `server_num + 3 == fuzzing_vms == vm.count == len(Executor)`；
- `DfsSetupParams` 的第一个数是 server 数，第二个数是 client 数，v1 必须为 `3`；
- GlusterFS v1 固定 3 个 client（`MAXCLT=3`）；1/2 client 暂不支持，
  seed/legacy 生成器只按 2–3 个 slot 验证过，1 client 会触发 over-budget/截断；
- `enable_syscalls` 必须与上面的 22 个逐字一致；profile 启动校验遇到
  `readv/writev/preadv/pwritev`、at 变体或其它列表外调用时报告
  unsupported/ignored，不能静默保留；
- v1 不启用 mknod、stash、dcache、persistence、failure/C2san；
  C2san 代码未完成，HMDFS 当前也未启用；GlusterFS 不把它作为能力开关；
- `data.img` 仍只挂 server，但当前不使用；brick 在 server rootfs；
- `initial_dir` 指向 manifest 目录，而不是 baseline 树目录。

推荐的 manifest 目录布局：

```text
<InitialDir>/
  shared.file
  shared.dir
  shared.tmpdir
  empty_dirs.info
```

`large_dir.info` 不放在 GlusterFS manifest 目录；即使存在也由 profile 忽略。

## 2. generate_test_files

### 2.1 命令

在 host 上生成一棵共享树：

```bash
python3 HorcruxSourceCode/generate_test_files.py shared \
  --num-large-files 0 \
  --seed 1
```

可选参数按需要调整：

```text
--num-files
--num-dirs
--max-depth
--num-empty-dirs
```

GlusterFS 不使用：

```text
large_dir.info
Persistence_dir
Node_idx_of_persistence
```

### 2.2 输出

脚本当前输出：

```text
HorcruxSourceCode/
  shared/                 # 实际文件树
  shared.file             # 相对路径 + 大小
  shared.dir              # 普通目录
  shared.tmpdir           # 中间目录
  empty_dirs.info         # node_id: shared + 空目录列表
  large_dir.info          # 当前会生成，GlusterFS 应跳过
```

`shared.file` 的路径相对于 `shared/`，例如：

```text
Eris_xxx_123.txt 1024
dirA/Eris_yyy_456.txt 4096
```

`shared.dir` / `shared.tmpdir` / `empty_dirs.info` 也是相对路径。

### 2.3 脚本需要的小改动

1. **`num_large_files <= 0` 跳过整个 large-dir 块**  
   当前即使 `--num-large-files 0` 也会创建空 large directory 并写
   `large_dir.info`。GlusterFS 不需要这一块。

2. **可选 `--shared` 校验**  
   设置 `--shared` 时强制：

   ```python
   node_ids = ["shared"]
   ```

   并跳过 large-dir。这样调用方不会误传多个 cid。不是文件名映射必需；
   文件名本来就会按唯一 node id 生成。

3. **保持 `shared.file/.dir/.tmpdir` 文件名**  
   不需要在脚本里硬编码 `shared` 的特殊分支；唯一 node id 自然得到统一文件名。

### 2.4 baseline 复制

脚本输出 `shared/` 树和清单。复制到 client 镜像时：

```bash
# 正确：复制 shared/ 的内容
cp -a HorcruxSourceCode/shared/. /path/to/client-image/home/baseline/glusterfs/

# 错误：会把 shared/ 这层目录带进去，manifest 相对路径会错位
cp -a HorcruxSourceCode/shared /path/to/client-image/home/baseline/glusterfs/
```

清单文件放在 `InitialDir` 指向的 host 目录，供 manager/fuzzer 加载。

baseline 保留空目录，但不含 xattr/symlink/hardlink；运行期创建的链接由每轮
restore 清掉，不写回 FileTree。

镜像目录前置条件：

```text
/root/glusterfs-client        空目录，GlusterFS 挂载点
/root/glusterfs-server        server brick 根
/root/dfs-fuzzing/libucov.so  LD_PRELOAD
```

- `/root/glusterfs-client` 必须预建；`glusterfs-config.sh` 的 mount 依赖它。
- `/root/glusterfs-client/eifuzz_root` 不在镜像预置；volume 挂载后由首轮 restore
  创建。
- 不要把这些目录和 baseline 内容混在一起。

镜像命令/软件包检查：

```text
rsync        首轮 restore（等价 prefill）+ 每轮 restore 必需
sshpass      glusterfs-config.sh 的 sshpass 依赖
gluster      gluster CLI
glusterd     GlusterFS server 服务
mount        mount.glusterfs
timeout/sync coreutils
```

缺 `rsync` 时必须 chroot 进镜像安装；否则首轮 restore/每轮 restore 会直接失败。

## 3. InitialDir 加载

### 3.1 HMDFS

保持现状：

- 对每个 cid 读 `<cid>.file/.dir/.tmpdir`；
- 路径加 `merge_view/`；
- 读 `empty_dirs.info`；
- 读 `large_dir.info`，初始化 `Persistence_dir`、
  `Node_idx_of_persistence`；
- `FileTree` owner 使用 cid。

### 3.2 GlusterFS

新增 profile 分支：

- 只读 `shared.file/.dir/.tmpdir`；
- 只读 `empty_dirs.info`，其中 `node_id: shared`；
- 不读 `large_dir.info`；
- 路径不加 `merge_view/`，直接使用相对路径；
- `Init_file["shared"]`、`Init_dir["shared"]`、`Init_tmpdir["shared"]`、
  `Init_empty_dir["shared"]`、`FileSize["shared"]`；
- `FileTree` 使用空前缀，owner=`shared`；
- `FileTree` 只加载普通文件/目录/空目录/TmpDir，不加载也不建模
  symlink/hardlink；
- `Cids` 重复 `"shared"` 到 `ClientNum`，但只加载一次 shared 清单；
- `Node_idx_of_persistence=-1`，`Persistence_dir=""`；
- `File_in_persistence_dir` 为空。

### 3.3 加载伪代码

```go
func LoadInitialTree(cfg *DFSConfig, initDir string) error {
    switch cfg.Profile.Name {
    case "hmdfs":
        return loadPerCidInitialTree(cfg, initDir, "merge_view/")
    case "glusterfs":
        return loadSharedInitialTree(cfg, initDir, "")
    default:
        return loadSharedInitialTree(cfg, initDir, "")
    }
}
```

`loadSharedInitialTree` 只处理：

```text
shared.file
shared.dir
shared.tmpdir
empty_dirs.info
```

`InitFromConfig` 对 shared owner 去重，避免 `Cids` 重复导致重复 `addNodeLocked`。

### 3.4 空目录和 large dir

- `empty_dirs.info` 是全局文件名，内容按 `node_id: shared` 分组；
- GlusterFS 只接受 `shared` 组；
- `large_dir.info` 不进入盘；如果存在，profile 直接忽略并记录日志；
- 不初始化 persistence/dcache；persistence 不是 seed family，GlusterFS 只是
  关闭 `Persistence_dir`/`Node_idx_of_persistence` 和 coverage 的 persist 维度。

## 4. prefill 与 restore

### 4.1 prefill（由首轮 restore 承担）

当前没有独立 `prefill.sh`。第一轮 `receive_execute` 的
`restore_initial_file_tree()` 由 designated reset client 执行，等价于 prefill：

```bash
chdir("/root")
rm -rf /root/glusterfs-client/eifuzz_root
timeout 60s rsync -a --numeric-ids --delete \
  /home/baseline/glusterfs/ \
  /root/glusterfs-client/eifuzz_root/
sync
```

要求：

- 由 client1（3-3 下 `executor_index == 3`）执行；
- 其他 client 等 restore finish barrier；
- 首轮 restore 和每轮 restore 使用同一份 baseline；
- 不直接写 server brick；
- 不新增 `src/dfs-config/glusterfs/prefill.sh`；如果后续需要独立 prologue，
  再拆脚本并复用同一命令。

### 4.2 normal restore

每次 `receive_execute` 前（含普通 fuzz、triage、light、DAG replay）：

```text
reset client: rm -rf /root/glusterfs-client/eifuzz_root
               timeout 60s rsync baseline -> eifuzz_root
               sync
other clients: wait restore finish barrier
server:        wait barrier only; no restore, no volume stop/start
```

GlusterFS restore barrier：start 30s、finish 90s；metadata delay 10s 起步；
不使用 `rsync --timeout=300`。详见 `glusterfs-adaptation-plan.md` §10.4。

规则：

- normal restore 不调用 `glusterfs-node-down.sh`；
- 不重启 glusterd/volume；
- 不恢复 server brick；
- `is_restarting` 的 crash recovery 路径跳过 restore；
- restore 失败时 barrier 统一失败，不允许在未恢复状态执行；
- 每轮 restore 后 FileTree 保持 baseline；`SyncFileTreeFromFsMd` 停用，fsMd
  只用于 CSAN/DAG。

### 4.3 crash/failure

当前 v1 不启用 failure/C2san/NetFailure/NodeCrash。C2san 代码未完成，
HMDFS 当前也未启用；NodeCrash 在 HMDFS 代码中存在，但当前 config 为 false。

未来 failure 模式才允许：

- client `umount`；
- server `systemctl restart glusterd`；
- `glusterfs-node-down.sh`；
- 重新 `glusterfs-node-up.sh`。

## 5. 脚本参数

### 5.1 glusterfs-config.sh

参数：

```text
$1 start_ip
$2 srv_cnt
$3 client_cnt
$4 mode
```

职责：

- 启动/重启所有 server 的 glusterd；
- peer probe；
- `gluster volume create test-volume <mode> ... force`；
- `gluster volume start test-volume force`；
- volume 性能选项；
- 在每个 client 上挂载：

```bash
umount -f /root/glusterfs-client 2>/dev/null || true
mount -t glusterfs \
  -o attribute-timeout=0,entry-timeout=0 \
  $start_ip:/test-volume /root/glusterfs-client
```

- volume create/start 前用 `volume info/status` 做幂等判断，失败 `exit 1`；
- client mount 后台任务收集 PID，逐个 `wait`，任一失败 `exit 1`；
- executor 对 config 脚本的 `popen/pclose` 检查退出码；HMDFS 保持原行为。
- 挂载完成后增加 client mount barrier；不能只依赖 `popen` 返回。

脚本复用还需：

- 镜像安装 `sshpass`；
- root key 或 123456 密码至少一个可用；
  `sshpass -p "123456"` 是脚本内部依赖，不是 fuzzer 登录；
- `/root/glusterfs-server`、`/root/dfs-fuzzing/libucov.so` 存在；
- 3-3 参数为 `start_ip 3 3 'replica 3'`。

### 5.2 glusterfs-node-up.sh

统一参数：

```text
$1 start_ip
$2 srv_cnt
$3 client_cnt
$4 mode
$5 cur_idx
```

server：

```bash
systemctl restart glusterd
```

client：

```bash
umount -f /root/glusterfs-client
mkdir -p /root/glusterfs-client
mount -t glusterfs \
  -o attribute-timeout=0,entry-timeout=0 \
  $start_ip:/test-volume /root/glusterfs-client
```

当前 executor 的 `node-up` 调用按 DFS 分支：

```text
HMDFS:       node-up.sh start_ip "<dfs_setup_params>" cur_idx
GlusterFS:   node-up.sh start_ip srv_cnt client_cnt "mode" cur_idx
```

GlusterFS/NFS 使用原版 unquoted 参数，shell 会把 canonical
`3 3 'replica 3'` 拆成脚本需要的 5 个参数。`glusterfs-node-up.sh`
server 分支检查 glusterd 退出码，client 分支先 `umount -f` 再 `mount`，
mount 失败 `exit 1`；executor 检查 `pclose` 退出码。

### 5.3 glusterfs-node-down.sh

当前脚本只 kill gluster 进程；需要区分角色：

- client：`umount -f /root/glusterfs-client`；
- server：仅 failure/crash 模式重启 glusterd 或 kill；
- normal restore 不调用。

### 5.4 executor cwd/metadata

- client cwd：`/root/glusterfs-client/eifuzz_root`
- server cwd：`/root`
- client metadata root：`/root/glusterfs-client/eifuzz_root`
- server metadata：跳过
- `testdirIno`：client cwd 的 `st_ino`
- trace：client-only
- trace root：`eifuzz_root`（相对挂载根 `/root/glusterfs-client`）；FUSE
  事件 path 是 `/eifuzz_root/...`，tracer 必须 strip 成测试根相对路径；
  `FileTree.Prefix` 仍为 `""`，不要把 `eifuzz_root` 写进 FileTree。
- 路径模型：所有调用参数是相对测试根的 canonical 路径；`symlink` target 在
  调用物化时转换为相对 linkpath 父目录的 payload；target 必须在测试根内。
  PathSet/DCT/`RootPaths` 只保存 canonical target，不保存 payload。

### 5.5 executor timing

timing 由 profile 解析，HMDFS 现有值不变：

| 项 | HMDFS | GlusterFS v1 |
|---|---|---|
| metadata delay | 30s | **10s** |
| agent ready | 30s | no-op |
| peer/settle | 3×3s / 3s | no-op / 0 |
| startup barrier | stagger + 30s | client mount barrier 60s |
| restore start/finish barrier | 30s / 90s | 30s / 90s |
| rsync 总超时 | 60s | 60s |
| node-down/node-up | 15s / 30s | normal restore 不调用 |

- `MetadataDelayMs` config 未显式设置时取 profile 默认值，显式值优先；
- GlusterFS CSAN B 类（Nlink/Mtime）只写
  `/home/user/dfsfuzz/glusterfs-csan.log`，可用 `GLUSTERFS_CSAN_LOG`
  覆盖；Go 与 Python symsc 共用该文件，不写 fuzzer 通用日志；
- GlusterFS rsync 命令为 `timeout 60s rsync -a --numeric-ids --delete ...`；
- 完整字段表见 `glusterfs-adaptation-plan.md` §10.4。

### 5.6 QEMU / SSH

- GlusterFS 使用原 Monarch 镜像和原 QEMU 启动路径；
- 不复用 HMDFS 的 `hmdfs<index>.img` 命名和 `ip=...eth0` 特殊参数；
- QEMU 会给 server 挂 `data.img`，但 v1 不 mkfs/mount；
- fuzzer 用 SSH key，`ssh_user` 默认 root；不把 123456 密码登录写进
  QEMU/fuzzer 启动；
- `glusterfs-config.sh` 内部仍使用 `sshpass -p "123456"`，这是脚本依赖，
  不是 fuzzer 登录方式；
- 启动前用 `ssh -i <focal.id_rsa> root@<ip>` 验证 key 可用。

## 6. 验收

- 3-3 config 启动后：
  - `enable_syscalls` 与 §1 的 22 个一致，没有 `readv/writev/preadv/pwritev`；
  - `close` 可执行；文件 close 产生 RELEASE D/S（无 R），目录 close 的
    RELEASEDIR 29 被 tracer 过滤；两者都不作为 DCT/定向 root/variant；
  - executor 0–2 没有 fuzz 程序；
  - executor 3–5 cwd 指向 `eifuzz_root`；
  - `mount` 选项包含 `attribute-timeout=0,entry-timeout=0`；
  - `InitialDir` 只加载 shared 清单；
  - FileTree 中没有 server cid，也没有 link/symlink 节点；linkpath 只在
    program-local `RootLinkPath`/`freshLinks`；所有调用路径是相对测试根
    canonical 路径，symlink target 在调用物化时转 payload 且不逃出测试根；
  - `large_dir.info` 未被读取；
  - 镜像预建 `/root/glusterfs-client`（空挂载点）、`/root/glusterfs-server`、
    `/root/dfs-fuzzing/libucov.so`；`eifuzz_root` 由首轮 restore 创建；
  - 镜像包含 `rsync`、`sshpass`、`gluster`/`glusterd`；缺 `rsync` 时首轮/
    每轮 restore 直接失败；
  - `glusterfs-config.sh` 复用，cwd `/root`，sshpass/key 可用；
  - 当前无独立 `prefill.sh`；首轮 restore 由 reset client 执行，等价 prefill；
  - 首轮 restore 后 baseline 内容在 volume 根；
  - 每次 execute 前 restore 后树回到 baseline（含 triage/light）；server 只
    参与 barrier；FileTree 保持 baseline，`SyncFileTreeFromFsMd` 停用；
  - metadata delay 未显式配置时取 GlusterFS profile 的 10s，rsync 总超时 60s，
    restore barrier 30s/90s；
  - server fsMd 不进入 CSAN；
  - A 类字段检查通过。
- HMDFS config 回归：
  - 仍按 cid 加载；
  - `merge_view` 前缀不变；
  - `large_dir.info`/persistence 仍只对 HMDFS 生效。
