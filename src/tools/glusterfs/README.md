# GlusterFS vmlinux.h

- 来源：GlusterFS 5.15 测试内核（`kernel-5.15-glusterfs`）的
  `/sys/kernel/btf/vmlinux`
- 导出日期：2026-11-16
- 用途：仅用于 `executor/glusterfs_trace.bpf.c`
- 大小：7,620,032 字节 / 358,698 行
- SHA256：`d6fcdbef99b873b7daef7574fda4a427939d85ea4d8cc24e7e11f62a24bb2e27`

导出命令（`bpftool` 在 host 上执行；VM 不需要安装）。以下命令从
`<repo>/src` 目录执行：

```bash
scp root@<glusterfs-vm>:/sys/kernel/btf/vmlinux /tmp/glusterfs-5.15.btf
bpftool btf dump file /tmp/glusterfs-5.15.btf format c \
  > tools/glusterfs/vmlinux.h
```

不要覆盖 `tools/vmlinux.h`：那是 HMDFS 测试内核的完整 BTF。
两个文件通过 Makefile 的 per-DFS include 目录隔离：

```text
HMDFS:     -Itools              -> tools/vmlinux.h
GlusterFS: -Itools/glusterfs    -> tools/glusterfs/vmlinux.h
```
