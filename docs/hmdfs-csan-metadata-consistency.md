# HMDFS CSAN 检查器：跨节点元数据一致性设计与字段筛选依据

## 概述

Monarch 的 CSAN（ConsistencySan）对分布式文件系统（hmdfs）做**跨节点元数据一致性**检查：多个 executor（各跑在一个 VM/节点上）在共享目录（`/mnt/hmdfs/100/non_account/merge_view`）上收集元数据，Go 端跨客户端比较，不一致则保存 bug（`saveCsanBug`）。

本文档记录：检查器架构、跨节点元数据一致性分类、字段筛选依据（含 `CONFIG_HMDFS_FS_PERMISSION` 设计说明）、已知缺陷与调整历史。

## 架构

```
executor（每节点）：write_dir_info(merge_view) 遍历共享树 → write_stat 收集
  · filepath（相对路径 ./xxx）
  · xattr（节点视图的 xattr）
  · checksum（仅 REG：get_file_chksum——读文件内容算 CRC）
  · symlink path（readlink）
  · stat（完整 struct stat）
  → 写回 Go 端（fsMd: path → FileMetadata）

Go 端（syz-fuzzer/proc.go:1193）：
  csanPassed, csanDiffs = checker.ConcFSCheck(ps, infos, fsMds, ...)
  → ConcFSCheck（src/checker/conc-fs.go:20）
      → MdCmp（:194）：文件数 + 路径集合（missing from client1/client2）
      → compareFileMeta（:237）：逐文件字段比较
  → 不一致 → saveCsanBug
```

（注：`src/pkg/ipc/ipc.go` 另有旧版比较器 `semanticSanitizers`/`clientMdCmp`，但其调用点已整段注释，当前不活跃。）

## 跨节点元数据一致性分类

远程节点的 stat 来自 hmdfs 的 **cached getattr**（`hmdfs_get_cached_attr_remote`，inode_remote.c:940-957），**只填 6 个字段**（ino/mtime/mode/uid/gid/size），其余（nlink/atim/ctim/dev/blksize/blocks/rdev）**未填（=0）**；mode/uid/gid 在 `CONFIG_HMDFS_FS_PERMISSION=y` 下是简化值。

| 类 | 字段 | 跨节点一致？ | 机制 / 依据 | CSAN 现状 |
| --- | --- | --- | --- | --- |
| **A 必须一致** | Checksum | ✅ | 同一分布式文件内容（CRC） | 比较 ✓ |
| | Size（文件） | ✅ | 协议回传对端 `ks->size` | 比较 ✓ |
| | Mtime（含 nsec） | ✅ | 协议回传对端 mtime（含 nsec） | 比较（只比 sec，保守） |
| | Xattr | ✅ | F_GETXATTR 对端回传 | 比较 ✓ |
| | **目录位 S_IFDIR** | ✅ | 目录/非目录性质必须一致（完整 S_IFMT 会因 hmdfs symlink 类型位缺陷误报，见"已知缺陷"） | **本次改为只比目录位** |
| | 路径集合（存在性） | ✅ | MdCmp | 比较 ✓ |
| **B 设计不一致**（PERMISSION=y） | Uid / Gid | ❌（设计如此） | 远程 = 父目录继承（实测 1008）；本机 = ext4 真实（实测 1000）——`fill_inode_remote` :327-330/:392 `inode->i_uid = dir->i_uid` | **本次注释掉**（保留代码供他 FS 复用） |
| | Mode 权限位 | ❌（设计如此） | 远程 = 硬编码（REG=0660，`fill_inode_remote` :365-374）；本机 = ext4 真实（实测 0664） | **本次只比类型位**（权限位不比较） |
| **C 远程未填（=0）** | Nlink | ❌ | cached getattr 不填 nlink（实测远程 0 vs 本机 1） | 排除 ✓ |
| | Atim / Ctim | ❌ | 同上未填 | 未比较 ✓ |
| | Size（目录） | ❌ | 远程目录 size 未写 | 排除 ✓ |
| | Blksize / Blocks / Rdev | ❌ | 同上未填 | 未比较 ✓ |
| **D 节点本地** | Ino | ❌ | 各节点本地 inode | 排除 ✓ |
| | Dev | ❌ | 本地 getattr 置 0 / 远程未填 | 排除 ✓ |
| | Stime / Etime / Retv / ProcId / Dents | ❌ | executor 本地执行/调试信息 | 未比较 ✓ |

## PERMISSION 设计说明（CONFIG_HMDFS_FS_PERMISSION=y）

- 远程 inode 的 uid/gid **继承父目录**（inode_remote.c:327-330/:392）——权限由 hmdfs 的 `HMDFS_PERM_XATTR` 模型管理，真实 uid/gid 无意义；
- 远程 inode 的 mode **按类型硬编码**（inode_remote.c:365-374：REG=0660、DIR=0751、LNK=0760）；
- 因此**同一文件在持有节点（本机）与远程节点的 stat 中 uid/gid/mode 权限位不一致是设计行为**，不是一致性 bug；
- 实测：节点 1（持有）= 1000/1000/0664（ext4 真实）；节点 2（远程）= 1008/1008/0660（dir 继承 + 硬编码）。

## 已知缺陷（记录不修复）

**远程 stat 的 nlink=0**：`hmdfs_get_cached_attr_remote` 未填 `stat->nlink`，远程节点 stat 的 nlink 恒为 0（本机为真实值）。属 cached getattr 字段遗漏（轻微缺陷），不影响 CSAN（nlink 已排除）。若未来修复：在 `hmdfs_get_cached_attr_remote` 补 `stat->nlink = inode->i_nlink`（协议 `getattr_response` 本身支持回传 nlink）。

**远程 symlink inode 类型位 = S_IFREG**：`fill_inode_remote` 的 LNK 分支（inode_remote.c:369-370）将链接 inode 的 `i_mode` 类型位硬编码为 `S_IFREG`（而非 `S_IFLNK`）——远程 stat 链接的类型位与本机（ext4 真实 `S_IFLNK`）不一致。影响：完整 `S_IFMT` 比较会在 symlink 场景误报（`S_IFLNK` vs `S_IFREG`）。CSAN 已规避（目录位比较）；记录不修复。

## 字段筛选调整记录（2026-08-24）

背景：CSAN 的字段级比较（`compareFileMeta`）此前从未真正执行——`MdCmp` 在文件数不一致时直接返回（此前 CSAN 一直报 `file count: 400 vs 1501`，即 merge_view 遍历缺陷导致的"三方互缺"）。merge_view 迭代器修复后文件数一致，字段级比较首次执行，暴露出：

- **Mode 权限位**：本机 0664 vs 远程 0660 → 报差异；
- **Uid/Gid**：本机 1000 vs 远程 1008 → 报差异。

均属 PERMISSION 设计不一致（见上），故调整：

1. **Mode 比较改为只比目录位**（`m1.StatMd.Mode & syscall.S_IFDIR != m2.StatMd.Mode & syscall.S_IFDIR`）——保留"目录/非目录性质跨节点一致"检测；不用完整 `S_IFMT`（hmdfs symlink 类型位缺陷会误报，见"已知缺陷"）；
2. **Uid/Gid 比较注释掉**（保留代码——未来其他文件系统测试可能需要严格 uid/gid 比较）；
3. 保留：Checksum / Size（非目录）/ Mtime(sec) / Xattr。

历史对照：`src/pkg/ipc/ipc.go` 旧版 `clientMdCmp` 中 Uid/Gid、Atim/Mtim/Ctim 的比较也被注释（两套比较器的演进——同样因跨节点不一致误报而停用）。

## 验证

- 改动为 Go 侧（`src/checker/conc-fs.go`），无需内核重编译；
- 重跑 CSAN（3 节点，内核已部署 merge_view 迭代器修复）预期：
  - file count 一致（三方互缺消失）；
  - 字段级通过（目录位 / checksum / size(文件) / mtime / xattr）；
- 观察项：mtime 弱一致窗口（远程 cached mtime，lookup 后过期——metadata_delay 60s 缓解）；PERMISSION 下 xattr（HMDFS_PERM_XATTR）跨节点一致性。

## 相关文件

| 文件 | 说明 |
| --- | --- |
| `src/checker/conc-fs.go` | `ConcFSCheck`/`MdCmp`/`compareFileMeta`/`xattrCmp` |
| `src/pkg/ipc/ipc.go` | 旧版 `semanticSanitizers`/`clientMdCmp`（调用已注释，不活跃） |
| `src/syz-fuzzer/proc.go` | :1193-1200 ConcFSCheck 调用 + saveCsanBug |
| `src/prog/prog.go` | `FileMetadata` 结构 |
| `src/executor/executor.cc` | `write_stat`/`write_dir_info`（元数据收集） |
| `hmdfs/inode_remote.c` | `fill_inode_remote`（uid/gid/mode 简化）、`hmdfs_get_cached_attr_remote`（nlink 未填） |