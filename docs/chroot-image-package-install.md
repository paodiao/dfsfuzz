# 通过 chroot 在 qcow2 镜像中安装软件

> 状态：流程已验证到 apt 安装阶段；适用于不启动 VM、直接在主机侧修改镜像的场景
> 日期：2026-11-13
> 范围：qcow2 镜像的挂载、chroot、apt 安装、清理；不涉及 DFS 运行时配置

## 0. 适用场景

- 需要在 VM 镜像里预装工具，但不想启动整套虚拟机。
- 典型例子：给 `focal.qcow2` 安装 `bpftrace`。
- 原则：在镜像**副本**上操作，不污染后续实验使用的 golden image。

## 1. 前置检查

- 确认镜像是 qcow2，不能直接 `mount -o loop`。
- 确认主机有 `qemu-nbd`、`partprobe`、`lsblk`。
- 用 `lsblk` 确认镜像内分区，通常是 `/dev/nbd0p1`；若镜像没有分区表，则直接挂 `/dev/nbd0`。

```bash
IMG=/path/to/focal.qcow2
MNT=/home/user/mntimg
NBD=/dev/nbd0

sudo mkdir -p "$MNT"
```

## 2. 挂载 qcow2

```bash
sudo modprobe nbd max_part=16
sudo qemu-nbd --connect="$NBD" --format=qcow2 "$IMG"
sudo partprobe "$NBD"
sudo lsblk -f "$NBD"
```

确认分区后挂载：

```bash
sudo mount "${NBD}p1" "$MNT"
```

如果 `lsblk` 显示没有分区表：

```bash
sudo mount "$NBD" "$MNT"
```

## 3. chroot 准备

```bash
sudo mount --bind /proc "$MNT/proc"
sudo mount --bind /sys  "$MNT/sys"
sudo mount --bind /dev  "$MNT/dev"
sudo mount --bind /dev/pts "$MNT/dev/pts"
sudo mkdir -p "$MNT/run"
sudo mount --bind /run  "$MNT/run"
```

说明：

- `/proc`、`/sys`、`/dev`、`/dev/pts` 是 apt 和安装脚本的常规依赖。
- `/run` 不是所有包都需要，但部分服务/安装脚本会读取，建议一并 bind。
- chroot 共享主机内核，不代表 VM 内核。

## 4. DNS 与 apt 源

备份并临时替换镜像内的 `resolv.conf`：

```bash
sudo cp -a "$MNT/etc/resolv.conf" "$MNT/etc/resolv.conf.bak"
sudo rm -f "$MNT/etc/resolv.conf"
sudo cp /etc/resolv.conf "$MNT/etc/resolv.conf"
```

先只读检查 apt 源：

```bash
sudo chroot "$MNT" /bin/bash -c \
  "grep -R '^deb' /etc/apt/sources.list /etc/apt/sources.list.d 2>/dev/null; \
   apt-cache policy bpftrace"
```

Ubuntu focal 的 `bpftrace` 位于 `universe`。如果源只有 `main`，在**镜像内**的 `/etc/apt/sources.list` 中补 `universe`：

```text
deb http://archive.ubuntu.com/ubuntu focal main universe
```

可以从主机侧直接改镜像文件：

```bash
sudo sed -i \
  's/^deb http:\/\/archive.ubuntu.com\/ubuntu focal main$/deb http:\/\/archive.ubuntu.com\/ubuntu focal main universe/' \
  "$MNT/etc/apt/sources.list"
```

不要修改主机的 `/etc/apt/sources.list`。`apt.llvm.org/bionic` 等第三方源与 `bpftrace` 无关，不要误改。

## 5. 在 chroot 中安装

```bash
sudo chroot "$MNT" apt-get update
sudo chroot "$MNT" /bin/bash -c \
  'DEBIAN_FRONTEND=noninteractive apt-get install -y bpftrace'
```

检查二进制是否存在：

```bash
sudo chroot "$MNT" /bin/bash -c 'command -v bpftrace'
```

注意：

- `apt-get update/install` 必须在 chroot 内执行，否则会操作主机系统。
- 不要在 chroot 里用主机的 `uname -r` 安装 kernel headers；那不是 VM 内核。
- chroot 里的 `bpftrace --info` 使用主机内核，不能代表 VM；必须在启动 VM 后验证。

## 6. 启动 VM 后验证

```bash
bpftrace --version
bpftrace --info
ls -l /sys/kernel/btf/vmlinux
```

如果 `bpftrace` 报 BTF 或 headers 问题，先看 `bpftrace --info`，再决定是否安装匹配 VM 内核的 headers。

## 7. 清理

先恢复 `resolv.conf`：

```bash
sudo mv "$MNT/etc/resolv.conf.bak" "$MNT/etc/resolv.conf"
```

再按顺序卸载：

```bash
sudo umount "$MNT/dev/pts"
sudo umount "$MNT/dev"
sudo umount "$MNT/run"
sudo umount "$MNT/sys"
sudo umount "$MNT/proc"
sudo umount "$MNT"
sudo qemu-nbd --disconnect "$NBD"
```

如果 apt 失败，也要先恢复 `resolv.conf`，再执行卸载，避免留下半挂载状态。

## 8. 常见坑

- qcow2 不能直接 `mount -o loop`；必须用 `qemu-nbd` 或 `guestmount`。
- 分区号写错：先 `lsblk`，不要盲挂 `nbd0p1`。
- 忘记 bind `/run`：部分安装脚本会失败或行为异常。
- 忘记恢复 `resolv.conf`：镜像后续 DNS 配置被污染。
- 在主机上执行 apt：会修改主机，不是镜像。
- 用主机 `uname -r` 给 VM 装 headers：版本不匹配。
- 把调试工具装进 golden image：后续实验镜像不再干净。
- 卸载前没有退出 chroot/终止进程：`umount` 可能 busy。

## 9. 完整示例：给 focal.qcow2 安装 bpftrace

```bash
IMG=/path/to/focal.qcow2
MNT=/home/user/mntimg
NBD=/dev/nbd0

sudo mkdir -p "$MNT"
sudo modprobe nbd max_part=16
sudo qemu-nbd --connect="$NBD" --format=qcow2 "$IMG"
sudo partprobe "$NBD"
sudo lsblk -f "$NBD"
sudo mount "${NBD}p1" "$MNT"

sudo mount --bind /proc "$MNT/proc"
sudo mount --bind /sys  "$MNT/sys"
sudo mount --bind /dev  "$MNT/dev"
sudo mount --bind /dev/pts "$MNT/dev/pts"
sudo mkdir -p "$MNT/run"
sudo mount --bind /run  "$MNT/run"

sudo cp -a "$MNT/etc/resolv.conf" "$MNT/etc/resolv.conf.bak"
sudo rm -f "$MNT/etc/resolv.conf"
sudo cp /etc/resolv.conf "$MNT/etc/resolv.conf"

sudo sed -i \
  's/^deb http:\/\/archive.ubuntu.com\/ubuntu focal main$/deb http:\/\/archive.ubuntu.com\/ubuntu focal main universe/' \
  "$MNT/etc/apt/sources.list"

sudo chroot "$MNT" apt-get update
sudo chroot "$MNT" /bin/bash -c \
  'DEBIAN_FRONTEND=noninteractive apt-get install -y bpftrace'
sudo chroot "$MNT" /bin/bash -c 'command -v bpftrace'

sudo mv "$MNT/etc/resolv.conf.bak" "$MNT/etc/resolv.conf"

sudo umount "$MNT/dev/pts"
sudo umount "$MNT/dev"
sudo umount "$MNT/run"
sudo umount "$MNT/sys"
sudo umount "$MNT/proc"
sudo umount "$MNT"
sudo qemu-nbd --disconnect "$NBD"
```

## 10. 变更记录

- 2026-11-13：记录 qcow2 + qemu-nbd + chroot 安装流程；补充 focal `universe`、`/run` bind、DNS 恢复、卸载和常见坑。
