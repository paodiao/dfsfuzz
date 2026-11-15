# GlusterFS / BPF 内核构建记录

> 状态：已验证；当前 GlusterFS P0 内核可启动，BTF/uprobe 可用
> 日期：2026-11-14
> 范围：为 GlusterFS FUSE tracer 构建 5.15 内核的配置、构建问题和运行验证
> 相关：`docs/glusterfs-tracer-implementation-spec.md`、`docs/glusterfs-nfs-adaptation-plan.md`

## 0. 结论摘要

- 当前测试内核：`Linux 5.15.0++ #2`，由 GCC 11 构建。
- `pahole 1.25` 需要 `--skip_encoding_btf_enum64`，否则 v5.15 旧 libbpf 无法加载 BTF。
- GCC 11 可避开 v5.15 `tools/lib/subcmd/subcmd-util.h` 的 `realloc` 误报。
- 当前内核保留了 KASAN、SLUB、SELinux。
- IMA/EVM 已关闭；否则会出现 `integrity_inode_get: lsm=integrity required` panic。
- 当前 LSM 为 `lockdown,capability,selinux`，旧内核为 `capability,selinux`。
- `CONFIG_FUNCTION_TRACER` 不是 uprobe 的前置条件；`available_filter_functions` 缺失不影响 FUSE P0。
- `CONFIG_DEVMEM=n`、`CONFIG_UIO=n` 不影响 ivshmem 的 PCI resource mmap。
- 网络和共享内存所需的 guest 配置已满足；host 侧需要 `br0` 和 `/dev/shm`。

## 1. 已验证环境

```text
Kernel:  Linux 5.15.0++ #2
Compiler: GCC 11
pahole:  1.25
KASAN:   enabled
SLUB:    enabled
BTF:     /sys/kernel/btf/vmlinux 存在
bpftrace: BEGIN 可执行
LSM:     lockdown,capability,selinux
```

旧内核基线：

```text
LSM: capability,selinux
```

说明：

- `lockdown` 来自 `CONFIG_SECURITY_LOCKDOWN_LSM`。
- 当前 `/sys/kernel/security/lockdown` 为 `[none]` 时不影响 P0。
- 若要求完全对齐旧内核，可关闭 `CONFIG_SECURITY_LOCKDOWN_LSM` 后重编；不要在没有确认的情况下关闭。

## 2. 必需配置

### 2.1 BPF / uprobe / BTF

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
CONFIG_DEBUG_INFO=y
CONFIG_DEBUG_INFO_DWARF4=y
CONFIG_DEBUG_INFO_REDUCED=n
CONFIG_DEBUG_INFO_BTF=y
CONFIG_KALLSYMS=y
CONFIG_KALLSYMS_ALL=y
```

`CONFIG_BPF_JIT_ALWAYS_ON=y` 可选，建议启用。

### 2.2 GlusterFS / QEMU 启动

```text
CONFIG_FUSE_FS=y
CONFIG_VIRTIO=y
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_NET=y
CONFIG_PCI=y
CONFIG_SYSFS=y
CONFIG_PROC_FS=y
CONFIG_EXT4_FS=y
CONFIG_DEVTMPFS=y
CONFIG_DEVTMPFS_MOUNT=y
CONFIG_NET=y
CONFIG_INET=y
CONFIG_IP_PNP=y
```

`CONFIG_VIRTIO_NET`、`CONFIG_VIRTIO_PCI`、`CONFIG_FUSE_FS`、`CONFIG_EXT4_FS` 当前为 built-in；如果改成模块，必须把模块装进 rootfs。

### 2.3 共享内存 / ucov

```text
CONFIG_SYSVIPC=y
CONFIG_SYSVIPC_SYSCTL=y
CONFIG_SHMEM=y
CONFIG_TMPFS=y
CONFIG_IPC_NS=y
```

说明：

- ucov 使用 System V 共享内存（`shmget/shmat/shmdt`）和 FIFO，不使用 KCOV。
- ivshmem 的测试数据通道走 PCI resource mmap，不走 SysV shm。
- `CONFIG_SYSVIPC` 是 ucov 必需；`CONFIG_PCI`/`CONFIG_SYSFS` 是 ivshmem 必需。

### 2.4 保持当前基线

```text
CONFIG_KASAN=y
CONFIG_SLUB=y
CONFIG_SLAB=n
CONFIG_SECURITY_SELINUX=y
CONFIG_LSM="selinux"
CONFIG_IMA=n
CONFIG_EVM=n
CONFIG_INTEGRITY=n
```

不要为了省事关闭 KASAN 或改成 SLAB；当前内核基线是 KASAN + SLUB + SELinux。

## 3. 可选配置

```text
CONFIG_KPROBES=y
CONFIG_KPROBE_EVENTS=y
CONFIG_FUNCTION_TRACER=y
CONFIG_DYNAMIC_FTRACE=y
CONFIG_IKHEADERS=y
```

说明：

- GlusterFS FUSE P0 只需要 uprobe，不需要 kprobe。
- `CONFIG_KPROBES` / `CONFIG_KPROBE_EVENTS` 供后续 VFS kprobe fallback 或 NFS 使用。
- `CONFIG_FUNCTION_TRACER` / `CONFIG_DYNAMIC_FTRACE` 只影响 ftrace 函数追踪和 `available_filter_functions`，不是 kprobe/uprobe 的前置条件。
- `CONFIG_IKHEADERS` 在 BTF 可用时不是必需；如果启用，建议 `=y`，避免 rootfs 模块问题。
- `CONFIG_IKHEADERS=m` 需要 `CONFIG_MODULES=y` 并把模块装进 rootfs。

## 4. 已知编译问题

### 4.1 GCC 12+ 的 `subcmd-util.h` 错误

错误：

```text
subcmd-util.h:56:23: error: pointer may be used after ‘realloc’
[-Werror=use-after-free]
```

原因：

- v5.15 的 `tools/lib/subcmd/subcmd-util.h` 旧 `xrealloc()` 代码。
- GCC 12+ 新增 `-Wuse-after-free`，被 `-Werror` 放大。

当前采用：

```text
使用 GCC 11
```

构建命令：

```bash
cd ~/linux
make clean
make -j"$(nproc)" \
  CC=gcc-11 HOSTCC=gcc-11 \
  CXX=g++-11 HOSTCXX=g++-11 \
  bzImage
```

备选：

- 应用上游 `libsubcmd: Fix use-after-free for realloc(..., 0)` 补丁。
- 或临时 `WERROR=0`；不推荐作为长期方案。

不要同时依赖 GCC 11 和 `WERROR=0`；当前基线是 GCC 11。

### 4.2 pahole 1.24+ 的 ENUM64 错误

错误：

```text
BTFIDS  vmlinux
FAILED: load BTF from vmlinux: Invalid argument
```

原因：

- `pahole 1.25` 默认生成 `BTF_KIND_ENUM64`。
- v5.15.0 自带旧 libbpf/resolve_btfids 不支持该类型。

修复：

在 `scripts/link-vmlinux.sh` 的 `gen_btf()` 中，在 `--btf_gen_floats` 判断后加入：

```sh
if [ "${pahole_ver}" -ge "124" ]; then
    extra_paholeopt="${extra_paholeopt} --skip_encoding_btf_enum64"
fi
```

如果编模块 BTF，`scripts/Makefile.modfinal` 也要加：

```diff
- LLVM_OBJCOPY="$(OBJCOPY)" $(PAHOLE) -J --btf_base vmlinux $@; \
+ LLVM_OBJCOPY="$(OBJCOPY)" $(PAHOLE) -J --skip_encoding_btf_enum64 --btf_base vmlinux $@; \
```

不要关闭 `CONFIG_DEBUG_INFO_BTF`，否则 bpftrace/BTF 不可用。

### 4.3 IMA/EVM panic

错误：

```text
Kernel panic - not syncing: integrity_inode_get: lsm=integrity required.
```

原因：

- base config 启用了 IMA/EVM。
- `CONFIG_LSM="selinux"` 没有 `integrity`。
- IMA 在 exec 时调用 `integrity_inode_get()`，发现 integrity LSM 未启用。

当前处理：

```text
CONFIG_IMA=n
CONFIG_EVM=n
CONFIG_INTEGRITY=n
```

如果必须保留 IMA：

```text
CONFIG_LSM="selinux,integrity"
```

但运行时 LSM 会变成 `capability,selinux,integrity`，与旧内核不一致，且需要 IMA 策略。当前 GlusterFS P0 不需要 IMA。

### 4.4 LSM 差异

新内核：

```text
lockdown,capability,selinux
```

旧内核：

```text
capability,selinux
```

说明：

- `lockdown` 来自 `CONFIG_SECURITY_LOCKDOWN_LSM`。
- 当前 `lockdown` 状态为 `[none]` 时不影响 P0。
- 若要求完全对齐旧内核，关闭 `CONFIG_SECURITY_LOCKDOWN_LSM` 后重编。
- 不要为了对齐旧内核而关闭 SELinux。

## 5. 已知非问题

### 5.1 `available_filter_functions` 缺失

现象：

```text
No such file or directory:
/sys/kernel/debug/tracing/available_filter_functions
```

原因：

- 未启用 `CONFIG_FUNCTION_TRACER`。
- 该文件只用于 ftrace 函数追踪和 `bpftrace -l` 的 kprobe 枚举。

影响：

- 不影响 uprobe。
- 不影响已知符号的 kprobe 直接 attach。
- `bpftrace -l` 可能报错，但 FUSE P0 不需要它。

tracefs 实际挂载点：

```text
/sys/kernel/tracing
```

旧版 bpftrace 可能仍找 `/sys/kernel/debug/tracing`；这是兼容路径问题，不是内核 BPF 故障。

### 5.2 `CONFIG_DEVMEM=n` / `CONFIG_UIO=n`

ivshmem 使用：

```text
/sys/bus/pci/devices/0000:00:10.0/resource2
```

mmap 依赖：

```text
CONFIG_PCI=y
CONFIG_SYSFS=y
```

不需要：

```text
CONFIG_DEVMEM
CONFIG_UIO
```

当前 `CONFIG_DEVMEM=n`、`CONFIG_UIO=n` 是正常的，不要为了 ivshmem 盲目打开 `/dev/mem` 或 UIO。

### 5.3 `CONFIG_BRIDGE` / `CONFIG_TUN`

- host 侧 QEMU bridge 需要 bridge/TUN。
- guest 侧不是必需；当前启用了也无害。
- 不要为了 guest 精简去关闭，除非已验证 QEMU 网络路径。

## 6. 构建流程

### 6.1 主机依赖

```bash
sudo apt-get install -y build-essential bc bison flex libssl-dev libelf-dev \
  dwarves rsync gcc-11 g++-11
pahole --version
```

### 6.2 源码和配置

```bash
cd ~/linux
git status --short
make clean
```

复制 `upstream-selinux-kasan.config` 作为基底，再按第 2、3 节调整。

### 6.3 配置检查

```bash
grep -E '^CONFIG_(BPF|BPF_SYSCALL|BPF_JIT|BPF_EVENTS|PERF_EVENTS|UPROBES|UPROBE_EVENTS|KPROBES|KPROBE_EVENTS|DEBUG_FS|FTRACE|DEBUG_INFO_BTF|FUSE_FS|VIRTIO|VIRTIO_PCI|VIRTIO_NET|PCI|SYSFS|PROC_FS|SYSVIPC|SYSVIPC_SYSCTL|SHMEM|TMPFS|IPC_NS|IP_PNP|INET|NET|EXT4_FS|DEVTMPFS|SLUB|SLAB|KASAN|SECURITY_SELINUX|LSM|IMA|EVM|INTEGRITY)=' .config
```

重点确认：

```text
CONFIG_DEBUG_INFO_BTF=y
CONFIG_BPF_SYSCALL=y
CONFIG_UPROBES=y
CONFIG_UPROBE_EVENTS=y
CONFIG_FUSE_FS=y
CONFIG_KASAN=y
CONFIG_SLUB=y
CONFIG_SLAB is not set
CONFIG_IMA is not set
CONFIG_EVM is not set
CONFIG_INTEGRITY is not set
CONFIG_LSM="selinux"
```

### 6.4 编译

```bash
make -j"$(nproc)" \
  CC=gcc-11 HOSTCC=gcc-11 \
  CXX=g++-11 HOSTCXX=g++-11 \
  bzImage
```

注意：

- 用 `make clean`，不要用 `make mrproper`，否则删 `.config`。
- 不要覆盖旧 `bzImage`，复制到新路径。
- 如果 `CONFIG_WERROR` 导致其他警告失败，再单独处理；不要提前全局关闭。

## 7. 启动验证

```bash
uname -a
ls -l /sys/kernel/btf/vmlinux
cat /sys/kernel/security/lsm
cat /sys/kernel/security/lockdown
bpftrace --version
bpftrace --info
bpftrace -e 'BEGIN { printf("ok\n"); exit(); }'
```

期望：

```text
BTF 存在
LSM 为 lockdown,capability,selinux（或目标配置）
bpftrace BEGIN 成功
```

再验证 FUSE uprobe：

```bash
bpftrace -l \
  'uprobe:/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so:*' |
  grep -E 'fuse_dispatch|fuse_fop_resume|send_fuse_iov'
```

## 8. 网络与共享内存运行时检查

### 8.1 网络

Guest：

```text
CONFIG_IP_PNP=y
CONFIG_VIRTIO_NET=y
CONFIG_INET=y
```

Host：

```text
br0 存在
/etc/qemu/bridge.conf 允许 br0
host 内核有 bridge/TUN
```

Guest 启动日志应出现：

```text
IP-Config: Complete
```

### 8.2 ivshmem 测试数据通道

Host：

```text
/dev/shm 是 tmpfs
QEMU memory-backend-file 正常创建
```

Guest：

```bash
ls -l /sys/bus/pci/devices/0000:00:10.0/resource2
ls -l /sys/bus/pci/devices/0000:00:11.0/resource2
ls -l /sys/bus/pci/devices/0000:00:12.0/resource2
```

### 8.3 ucov SysV shm

```bash
grep -E '^CONFIG_(SYSVIPC|SYSVIPC_SYSCTL|SHMEM|TMPFS|IPC_NS)=' .config
```

## 9. 复现检查清单

```text
[ ] gcc-11 可用
[ ] pahole 1.25
[ ] scripts/link-vmlinux.sh 已加 --skip_encoding_btf_enum64
[ ] .config 有 BTF/BPF_JIT/UPROBES/UPROBE_EVENTS
[ ] IMA/EVM/INTEGRITY 已关闭
[ ] KASAN/SLUB/SELinux 保持
[ ] bzImage 编译成功
[ ] 新内核启动成功
[ ] /sys/kernel/btf/vmlinux 存在
[ ] bpftrace BEGIN 成功
[ ] GlusterFS volume 可挂载
[ ] FUSE uprobe 符号可列出
[ ] ivshmem resource2 存在
[ ] guest IP 配置成功
```

## 10. 回退

- 保留旧 `kernel-5.15-glusterfs-bzImage`，新内核另存路径。
- 新内核无法启动时，QEMU `-kernel` 指回旧内核。
- 不要覆盖 rootfs 的 `/lib/modules`，除非确认新模块已安装。
- 如果只需要 FUSE uprobe，不需要因为 `available_filter_functions` 缺失而重编。
- 如果需要 VFS kprobe fallback，再评估 `CONFIG_FUNCTION_TRACER` / `CONFIG_DYNAMIC_FTRACE`。
