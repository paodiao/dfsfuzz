# HMDFS 真 Bug：并发 rmdir/mkdir 同路径竞争导致跨节点命名空间持久分歧

## 概述

| 项 | 内容 |
| --- | --- |
| **缺陷类型** | 命名空间收敛性违反（并发冲突目录操作后，跨节点视图持久分歧） |
| **触发条件** | 两个节点对**同一目录路径**并发执行 `rmdir` 与 `mkdir`（无故障注入，纯并发元数据竞争） |
| **现象** | 60s 后：节点 A 视图缺目录 D（1700 条目），节点 B/C 视图有 D（1701 条目）——分歧不收敛 |
| **发现者** | Monarch CSAN（ConsistencySan 跨节点元数据一致性检查） |
| **证据存档** | `hmdfs-bugs/csan-20260826-154633.458/`（prog.txt / diff.txt），`log0` :41728-41750 |
| **复现环境** | 3 节点 hmdfs（Linux 6.6.0-dirty，CONFIG_HMDFS_FS_PERMISSION=y），Monarch fuzzer 生成 inode_ops 程序 |
| **根因状态** | 机制事实已静态确认；H1/H2/H3 候选根因待 VM trace 实证（见"实证验证方案"） |

## 复现程序

`prog.txt`（三个节点并发执行，同一父目录 `.../Eris_ipmnvrbegd_137256574840.d`）：

```
node 0: rmdir('merge_view/Eris_mgfkesggwh_256325716405.d/Eris_zidqosaowu_500510908088.d/
                Eris_uqcwdekhmy_626041715472.d/Eris_pjcozffirt_298134617228.d/
                Eris_nlpnqjeukn_512743242184.d/Eris_vzfzlrxrim_63646094875.d/
                Eris_rbynylnvpi_496206591506.d/Eris_ipmnvrbegd_137256574840.d/
                Eris_kxaqrwhciy_949868521164.d\x00')
node 1: mkdir('…/Eris_ipmnvrbegd_137256574840.d/Eris_kxaqrwhciy_949868521164.d\x00', 0x1ed)
node 2: unlink('…/Eris_ipmnvrbegd_137256574840.d/Eris_skyzgdgkud_770954058441.d\x00')
```

- D = `Eris_kxaqrwhciy_949868521164.d`（9 层深的目录，位于初始树中）
- node 0 删除 D，node 1 创建**同名** D，node 2 删除同父目录的另一条目（与 D 无直接依赖，仅增加父目录并发扰动）
- prog-meta：`is_inode_ops=true`，`is_stash=false`，`is_dcache=false`，`is_file_ops=false`

## 分歧快照（diff.txt）

```
file count: 1700 vs 1701
./Eris_…/Eris_ipmnvrbegd_137256574840.d/Eris_kxaqrwhciy_949868521164.d: missing from client1

node 1: {Ino:9223372036854791006 Nlink:2 Mode:16889 Size:4096 Blksize:1
         Atim:Mtim:Ctim=1787730331.404739171}
node 2: {Ino:9223372048946898392 Nlink:3 Mode:16889 Size:0 Blksize:1
         Atim:0 Mtim:1787730331.404739171 Ctim:0}
```

- mdCmp 对（node0, node1）：2 个差异 = **file count（1700 vs 1701）** + **D 从 node 0 视图缺失**（"missing from client1" = 在 node 1 视图有、node 0 视图无）
- mdCmp 对（node1, node2）：0 差异（存在性一致，都"有 D"）
- 收集发生在 MetadataDelayMs=60s 之后——**分歧持续 ≥60s 未收敛**
- 三节点 Checksum 一致（D 为目录条目，Checksum 幂等）→ 数据通道无损坏，缺陷在命名空间/元数据层
- `HasNetFail=false`：无网络故障注入参与

## 违反的语义

并发冲突操作（rmdir ∥ mkdir 同一路径）无论内部如何排序，最终必须**全局收敛到同一个串行顺序的结果**：

- 「先删后建」→ 所有节点最终都应有 D
- 「先建后删」→ 所有节点最终都不应有 D

实测：node 0 缺 D，node 1/2 有 D——两个节点**持久停留在不同的命名空间状态**，60s 未收敛，违反 eventual consistency。这正是 Monarch 论文中 CSAN 要抓取的缺陷类别。

## 非异常观察（排除项，不是 bug 证据）

node 1 与 node 2 对 D 的属性缓存差异（Nlink 2 vs 3、Size 4096 vs 0、Atim/Ctim 为 0）**是 hmdfs 设计行为**，不是本 bug 的证据：

1. **nlink 视图相关**（详见 `hmdfs-metadata-weak-consistency.md` §3）：
   - merge_view inode：`nlink = get_num_comrades(child_dentry) + 2`（inode_merge.c:152），comrade 数按**每节点本地 comrade_list** 计数（inode_merge.c:85-96）→ 同一目录在不同节点 nlink 天然不同
   - device_view 远端 inode：目录 nlink 固定 2（inode_remote.c:384）
   - device_view 本地 inode：nlink 镜像 lower 真实值（inode_local.c:368）
   - merge_view stat 透传 lower（device_view）inode 的 `vfs_getattr`（inode_merge.c:753-772）→ 返回 nlink 取决于该节点 device_view 中 inode 的类型与状态
2. **Size/Atim/Ctim**：弱一致缓存产物（cached getattr 只填 6 字段，见弱一致文档 §2）——node 2 的 Size=0 即未回填状态

因此本 bug 的**唯一真实证据是路径集分歧**（D 存在性：node 0 无 vs node 1/2 有），属性差异一律不作为证据。

## 源码级根因分析

### 机制事实（已静态确认）

| # | 事实 | 位置 |
| --- | --- | --- |
| 1 | **mkdir 无跨节点广播**：`mkdir_merge` 经 `create_lo_d_child` → `hmdfs_create_lower_dentry`，在**本节点 local 设备** `kern_path_create` + `vfs_mkdir` 创建真实目录，`link_comrade_unlocked` 只入**本节点** comrade_list；无任何跨节点主动通知 | inode_merge.c:1057-1082, 908-971, 1007-1044 |
| 2 | **rmdir 只删本节点 comrade_list**：`do_rmdir_merge` 遍历本节点 comrade_list，对每个 comrade 的 lo_d 逐个 `vfs_rmdir`，任一失败即 break；成功后 `d_drop(dentry)`；**不清理 comrade_list、不向其他节点广播删除** | inode_merge.c:1107-1153 |
| 3 | **lookup 实时广播**：`lookup_merge_normal` 先查 local，再对 `sbi->connections.node_list`（所有在线 peer）发起异步 `merge_lookup_async`；**任一 work 成功（found）即提前唤醒**（work_count 与 found 的或条件），comrade_list 非空即返回成功 | inode_merge.c:498-563, 395-442 |
| 4 | **发现路径穿透本地 dcache**：`merge_lookup_comrade` 用 `vfs_path_lookup(root, name)`（name = `device_view/<cid>/<ppath>/<rname>`）走**本节点 dcache**；device_view 的 `d_revalidate` 在线恒返回 1（`hmdfs_dev_d_revalidate`）→ positive dentry 永不失效 | inode_merge.c:345-374, dentry.c:74 |
| 5 | **comrade 有效性判定不阻止目录恢复**：目录场景 `is_valid_comrade` 只校验 `mdi->type == DT_DIR && S_ISDIR(mode)` → 目录 lookup 响应有效即放行 | inode_merge.c:376-393 |

### 完整调用链

```
mkdir(D) @node1:  hmdfs_mkdir_merge (inode_merge.c:1057)
  → create_lo_d_child (:1007) → hmdfs_create_lower_dentry (:908)
      → kern_path_create(real_dst 本地路径) → vfs_mkdir（本节点 local 创建真实目录）
      → link_comrade_unlocked (d_child, comrade(local)) (:960)

rmdir(D) @node0:  hmdfs_rmdir_merge (:1133)
  → do_rmdir_merge (:1107)
      → 遍历本节点 comrade_list：vfs_rmdir(lo_i_dir, lo_d)（remote lo_d → F_RMDIR 发给对端）
      → 任一失败 break
  → d_drop(dentry) (:1149)

lookup(D) @nodeX:  hmdfs_lookup_merge (:678)
  → lookup_merge_normal (:498)
      → local: merge_lookup_async(device_view/local/...)
      → 每 peer: merge_lookup_async(device_view/<cid>/...)
  → merge_lookup_work_func (:395)
      → merge_lookup_comrade (:345): vfs_path_lookup(本地 dcache 路径) → F_LOOKUP 穿透
      → is_valid_comrade (:376) → link_comrade (:422)
  → 任一 found 即唤醒返回 (:427)
```

### 核心缺陷定性

**hmdfs 对跨节点并发冲突目录操作（rmdir ∥ mkdir 同路径）没有任何全局串行化/仲裁机制**：

- node 0 的 rmdir 与 node 1 的 mkdir 各自在**本节点状态**上演进（机制 1/2）
- 删除**不广播**（机制 2）——其他节点对 D 的"已删除"认知只能靠自己的 lookup 发现
- 创建**不广播**（机制 1）——其他节点对 D 的"已存在"认知只能靠自己的 lookup 发现
- 最终一致性完全依赖机制 3 的"lookup 实时发现链"

本案例中 node 1/2 通过发现链看到了 node 1 的新 D（存在性收敛于"有"），**唯独 node 0 在 ≥60s 内没有发现 node 1 的新 D** → 发现链在 node 0 一侧存在断裂点。这是文档要实证定性的核心问题。

### 候选根因（H1/H2/H3，待实证）

- **H1：node 0 的 lookup 广播未发生/未覆盖 node 1**——rmdir 后 merge dentry 状态异常（如 negative dentry 的 revalidate/lookup 路径特殊处理），或 node_list 中 node 1 缺失
- **H2：lookup 发生，但 `vfs_path_lookup(device_view/<cid1>/.../D)` 未穿透**——node 0 的 device_view 层 positive/负面缓存拦截（`hmdfs_dev_d_revalidate` 恒 1 导致陈旧 dentry 不失效），F_LOOKUP 实际未发出
- **H3：F_LOOKUP 发出且 node 1 响应"存在"，但 comrade 重建失败**——响应处理/`is_valid_comrade` 拒绝/异步 work 竞态（注意机制 3 的 :427：**第一个 found 即唤醒**，后续 work 结果可能被静默丢弃或覆盖）

### 实证验证方案（VM 上执行后回填结论）

1. 复现：3 节点执行精简程序（node0 `rmdir(D)` ∥ node1 `mkdir(D)`），开启跟踪：
   - `trace-cmd record -e hmdfs:*`（hmdfs_trace.h 全部 tracepoint：`hmdfs_mkdir_merge` / `hmdfs_rmdir_merge` / `hmdfs_lookup_merge` / `hmdfs_merge_lookup_work_enter/exit` / `hmdfs_show_comrade`）
   - `dmesg | grep hmdfs`（hmdfs_err 输出）
2. 观测点：
   - node 0 `hmdfs_rmdir_merge`：comrade_list 组成（`hmdfs_show_comrade`）、删除目标节点、返回值
   - node 1 `hmdfs_mkdir_merge`：创建目标、comrade link
   - 60s 窗口内 node 0 对 D 的任何 `hmdfs_lookup_merge` + `merge_lookup_work_enter/exit`：work 数量（覆盖几个 peer）、各 work 的 found 值
   - node 1 server 端（若 F_LOOKUP 到达）的响应与 inodeinfo 回传
3. 判定矩阵：
   - work 未创建/未到 node 1 → **H1**
   - work 到 node 1 但 server 无响应或 ENOENT → **H2**（并检查 device_view 层是否发出 F_LOOKUP）
   - work found=true 但最终视图无 D → **H3**（检查 comrade 是否被 `is_valid_comrade` 拒绝或 work 竞态丢失）

### 结论（待回填）

> 实证后填写：H1/H2/H3 判定、对应的内核函数与行号、缺陷的直接机制描述。

## 复现要点

- inode_ops 类型程序（rmdir/mkdir/unlink 属于 inode_ops 集合）
- 同父目录下并发 rmdir+mkdir 同名目录（路径取自生成树的深层目录）
- 多轮 fuzz 概率触发（依赖 rmdir 与 mkdir 的跨节点交错窗口）
