# HMDFS 元数据弱一致机制——代码级分析（stat 缓存、更新点与检查器适配）

## 1. 问题背景

Monarch 的 CSAN 在 hmdfs 上做跨节点元数据一致性检查时，最初把**跨节点 stat 字段差异**全部当作 bug 上报（案例：`hmdfs-bugs/csan-20260825-085426.070`——size/mtime/nlink/mode 跨节点不一致被误报为数据损坏）。深入 hmdfs 内核源码后确认：这些差异绝大多数是 **hmdfs 元数据弱一致设计**的预期产物，而非缺陷。本文档给出 stat 数据源、缓存更新点、nlink 视图语义的代码级依据，以及检查器为此做的适配清单。

## 2. stat 数据源分层

merge_view 上对条目的 `stat` 调用链：

```
stat(merge_view/<path>)
  → hmdfs_getattr_merge（inode_merge.c:753-772）
      → vfs_getattr_nosec(lower_path)   // lower_path = hmdfs_get_fst_lo_d(dentry)
                                        //   = 该 merge dentry 第一个 comrade 的 lo_d
                                        //   = device_view/<cid>/<path> 下的 inode
```

即 **merge_view 的 stat 透传 lower（device_view）inode 的 getattr**，返回的字段取决于该 device_view inode 的类型：

| device_view inode 类型 | stat 字段来源 |
| --- | --- |
| **本地条目**（local lower） | ext4（lower fs）真实值，经 `fill_inode_local`（inode_local.c:75）镜像 |
| **远端条目**（comrade） | `hmdfs_get_cached_attr_remote`（inode_remote.c:940-957）——**纯本地缓存** |

`get_cached_attr_remote` 只填 **6 个字段**：`ino / size(getattr_isize) / mtime / mode / uid / gid`；**不填**：nlink / blksize / blocks / atime / ctime / rdev（保持 0/默认值）。

## 3. nlink 视图相关语义（跨节点天然不同）

| 位置 | 语义 |
| --- | --- |
| inode_merge.c:152 | merge_view inode：`set_nlink(inode, get_num_comrades(child_dentry) + 2)`——comrade 数按**每节点本地 comrade_list** 计数（inode_merge.c:85-96），同一逻辑目录在不同节点的 nlink **天然不同** |
| inode_remote.c:380,384 | device_view 远端 inode：file 固定 `nlink=1`、dir 固定 `nlink=2`——**非真实值** |
| inode_local.c:368 | device_view 本地 inode：`set_nlink(dir, lower_inode->i_nlink)`——镜像 lower 真实值 |
| hmdfs_server.c:1720 | 协议响应 `resp->nlink = ks->nlink`——服务端自身视图的 nlink（同样视图相关） |

结论：**同一目录跨节点的 nlink 没有可比性**（持有节点=真实值、远程节点=固定值或 comrade 计数值、陈旧缓存=历史值）。检查器必须跳过 nlink 比较。

## 4. 缓存更新点（getattr_isize / i_mtime 何时刷新）

`get_cached_attr_remote` 返回的缓存值只在以下 4 个场景被更新：

1. **open（远端）**——唯一真正向对端拉取实时状态的操作：server 回传 lower 的 `file_size` → `i_size_write(file_size)` 且 `getattr_isize = STALE`（file_remote.c:148-149，注释原文见 §6）
2. **本节点远端写**：`write_end_remote` 里 `i_size_write(pos + copied)` + `getattr_isize = STALE`（file_remote.c:865 附近）
3. **device_view lookup（仅当 dentry 失效时）**：对端响应带 `i_size / i_mtime` → `fill_inode_remote` 对缓存命中的 inode 也会经 `hmdfs_update_inode` 更新（inode_remote.c:351 附近）——**但 merge_view 的 lookup 不触发这条路径**（见 §5）
4. **merge_view 占位 inode 重建（I_NEW）**：`update_inode_attr`（inode_merge.c:56 附近）从 device_view inode 同步——但 merge_view 的 stat 不读占位 inode 的这些字段（透传 device_view，见 §2）

## 5. stat 为什么不拉取（每次 lookup 都发生，但结果不回填）

- `d_revalidate_merge`（dentry.c:283 附近）**恒失效**（注释：每次路径解析都重新 lookup）→ 每次 stat/lookup 确实触发 `lookup_merge_normal`（inode_merge.c:498）→ 向所有在线 peer 广播异步 F_LOOKUP
- **但 lookup 的结果只用于 comrade/dentry 校验**：`hmdfs_dentry_comrade`（merge_view.h:51 附近）不携带元数据字段；`fill_inode_merge` 对**已存在 inode（!I_NEW）直接跳过属性更新**（inode_merge.c:98-162 的 I_NEW 分支逻辑）→ **元数据缓存不随 stat/lookup 更新**

即：网络查询发生了，但 stat 返回值来自本地缓存——**弱一致的代价是"stat 的 size/mtime 可能陈旧"，收益是"避免每次 stat 的网络 RTT"**。

## 6. 设计意图（源码注释原文）

`file_remote.c:148-149` 附近 `getattr_isize` 相关注释（大意）：

> not actual inode size, just a value showed to user... up-to-date after open

即：`getattr_isize` 不是真实 inode size，只是给用户看的值；**open 之后才是新的**。这是 hmdfs 明确的弱一致权衡：stat 走缓存，open 保证准确。

## 7. 对 Monarch 检查器的影响与适配清单

| 检查器位置 | 适配内容 | 原因 |
| --- | --- | --- |
| src/checker/conc-fs.go `compareFileMeta` | hmdfs 跳过 nlink / mode（含权限位）/ mtime / size（非目录）；保留 Checksum / 目录位 S_IFDIR / xattr / 路径集合比较 | §2 未填字段、§3 视图相关、§4 缓存陈旧 |
| src/checker/symsc/checker.py `check_meta` | is_hmdfs 跳过 nlink / mode 权限位 / size | 同上 |
| src/checker/symsc/checker.py `state_check` | stat 分支 hmdfs 恒等通过 | 缓存值不可跨节点比较 |
| src/checker/symsc/syscalls.py | `emulate failure` 允许列表含 hmdfs；stat_base 的 hmdfs 分支；write 后 size 按本地最大值处理 | 避免把弱一致假象当失败/不一致 |
| src/checker/conc-fs.go `initTreeSubset` / 临时文件传参 | 初始树子集灌入 symsc | 见 csan-metadata-consistency.md |
| hmdfs_agent.c / executor（common_linux.h） | netup 重连机制（unix socket 触发离线节点恢复） | hmdfs 无自动重连（agent OFFLINE 后 connector 只连 ONLINE），补位恢复通道 |

（更完整的字段筛选表见 `hmdfs-csan-metadata-consistency.md`。）

## 8. 可选验证实验（drop_caches）

如需在 VM 上直接观察缓存语义：

1. 节点 A open 一个文件（触发 server 回传真实 size）→ 两个节点 stat，确认持有节点与远端节点 size 一致
2. 在持有节点 truncate/写大文件（不经 open）→ 远端节点 stat：**size/mtime 保持旧缓存值**
3. 远端节点重新 open 该文件 → stat 更新为真实值
4. （可选）`echo 2 > /proc/sys/vm/drop_caches` 清 icache + 强制 dentry 失效 → 观察 device_view lookup 路径（§4 场景 3）是否回填

## 9. 相关文档

- `hmdfs-bug-concurrent-rmdir-mkdir-divergence.md`：并发 rmdir/mkdir 真 bug 报告（§3 nlink 语义的实践案例）
- `hmdfs-csan-metadata-consistency.md`：CSAN 检查器设计、字段筛选依据与调整历史
