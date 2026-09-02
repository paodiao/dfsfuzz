# HMDFS 一致性问题归档：2026-08 运行批次（64 个 csan dump / 17 个有效 diff）

## 元信息

| 项目 | 内容 |
|---|---|
| 数据来源 | `hmdfsfuzz/hmdfs-bugs/`（64 个 csan dump：diff.txt/output.txt/prog.txt），对应 `hmdfsfuzz/crashes/` 的 22 个 bucket |
| 运行时间 | 2026-08-30 21:31 ~ 09-01 08:26（约 36h，3 节点 hmdfs merge_view） |
| 有效 diff | 17/64（全部 ≥ 08/31 09:11，此前 dump 均为空——**FSCFG 修复部署生效的实证**） |
| 比对机制 | fuzzer 侧 CSAN 全局树比对：`MdCmp`（src/checker/conc-fs.go:248）以 globalFsMd 为基准，与各节点收集的 FileMetadata 逐字段比较 |
| 状态 | 分类与根因分析确认，归档 |

## 分类总览

| 子类 | 数量 | 判定 | 根因族 |
|---|---|---|---|
| 路径存在性分歧（missing） | 15 | 真实不一致 | 并发命名空间竞争（同族：concurrent-rmdir-mkdir-divergence） |
| 同路径类型冲突（dirtype） | 2 | 真实不一致，**最尖锐** | 同族，type 翻转的持久化表现 |
| （非 bug）幽灵 stat 行 | — | 诊断噪声 | 见"字段比对设计确认" |

## 子类 1：路径存在性分歧（15 例）

### 模式

- `file count: N vs M`（差 1~3）+ `<path>: missing from clientX`
- 缺失规模：单缺失 13、六缺失 1（csan-20260831-095120）、**十七缺失 1**（csan-20260901-041506，diff 19.5KB）
- 方向：**双向**（基准侧缺 17 行、对比侧缺 19 行）——非单节点问题，普遍的对账缺口

### 操作序列证据（prog.txt）

- 041506（17 缺失）：node0 **rmdir+mkdir** / node1 mkdir+mkdir / node2 stat+mkdir —— 并发 rmdir/mkdir 同路径族
- 214445（1 缺失）：node0 mkdir+stat / node1 mkdir+**rename** / node2 mkdir+open+write+close
- 060409（1 缺失）：node0 **creat** / node1 open+read+close / node2 **creat** —— 双 creat 竞争

### 根因（同族引用）

与 `hmdfs-bug-concurrent-rmdir-mkdir-divergence.md` 同根因族：并发命名空间操作（mkdir/rmdir/creat 同路径族）无跨节点仲裁，各节点本地成功后对账无法合并冲突，产生持久的存在性分歧。本轮数据为该 bug 的 15 个新实例，新增观察：

1. **creat 竞争变体**（双 creat 同路径 → 一侧条目丢失），缺失文件多为 `._creat_xxx.txt` 后缀（creat 变体产物）；
2. **方向双向统计**（17/19 行），确认非固定节点缺陷。

### 判定

不修（同族根因已归档；hmdfs 并发命名空间设计问题）。本轮为实例扩充归档。

## 子类 2：同路径类型冲突（dirtype，2 例）

### 现象

```
csan-20260831-132327.998:
  ...Eris_ofmyehonzz_33107412366.uas: dirtype 0 vs 40000        （基准=文件，对比节点=目录）
csan-20260901-080706.421:
  ...Eris_woeokfsdmg_718021252744.odp: dirtype 40000 vs 0        （反向）
```

同一 merge_view 路径在两个节点分别是**文件**和**目录**——dirtype 是 compareFileMeta 保留比对的最强跨节点不一致信号（见下文字段设计），两个方向各一例。

### 持久性证据

- 冲突路径（`ofmyehonzz_33107412366.uas` / `woeokfsdmg_718021252744.odp`）在 **19 小时内持续存在**（132327 08/31 13:23 → 080706 09/01 08:07），不自愈；
- 同路径出现在 **9 个 crash bucket** 的比对输出中（17e6acbc/1f49816d/45c63e8e/49a94e7e/57d5b1cd/6512620c/86580a31/94d3c7ba/cb3ffa70），佐证该分歧长期参与全局树比对。

### 代码级机制（hmdfs 内核源码，`Monarch-master/hmdfs/`）

merge_view 的创建路径**没有任何跨节点类型/存在性仲裁**：

- `hmdfs_mkdir_merge`（inode_merge.c:1057）/ `hmdfs_create_merge`（:1084）→ `create_lo_d_child`（:1007）→ `hmdfs_create_lower_dentry`（:908）→ `do_mkdir_merge`（:819，vfs_mkdir）/ `do_create_merge`（:847，vfs_create）——**全部只操作本地底层 replica**（HMDFS_DEVID_LOCAL）；
- 入口的"类型检查"仅是文件名后缀约定（`hmdfs_file_type != HMDFS_TYPE_COMMON`），注释写明 confict_name/类型检查委托 local 层——**本地 EEXIST 之外无远端检查**；
- 跨节点命名空间传播是**懒惰对账**：merge dentry 的 comrade 链表在 lookup/readdir 时才从其他节点拉取条目建本地 comrade（do_rmdir_merge :1119 遍历可见该结构）。

### type 冲突发生链

1. node A `mkdir(X)` 与 node B `creat(X)` 并发：各节点本地底层均无对方条目，**两侧各自创建成功**（A=目录，B=文件）；
2. 后续懒惰对账：A 拉取 B 的文件条目时本地底层 EEXIST（目录已占），拉取失败；B 同理——**各持己见**；
3. 持久化：A 视图 X=目录、B 视图 X=文件，全局树比对命中 `dirtype`，双向存在。

### 判定

- **真实一致性违反**，且是本批中最尖锐的一类（type 冲突使该路径后续所有操作语义混乱：checksum/size/mtime 均不可比）；
- 同族根因（并发竞争无仲裁）已归档，dirtype 为其 type 翻转表现，**独立成节记录**；
- 修复属 hmdfs 并发设计层面（创建时全局类型仲裁或对账冲突解决策略），与本批其他缺失类问题同批评估，不单独行动。

## 字段比对设计确认（核实记录）

`compareFileMeta`（conc-fs.go）对 hmdfs 的比对字段经过源码核实设计：

| 字段 | 处理 | 依据 |
|---|---|---|
| 目录位（S_IFDIR） | **恒比**，dirtype 不匹配短路 | 唯一强类型信号；全量 mode/uid/gid 在 remote view 是简化值（uid/gid 继承父目录、mode 硬编码 0660），symlink 被硬编码为 S_IFREG（fill_inode_remote），故只比目录位 |
| Checksum / Size | 恒比 | 理论必一致 |
| Mtime | 只比 sec（不比 nsec） | 协议丢失远端 mtime nsec |
| Nlink / Mode 全量 / Uid/Gid | **跳过**（fsType=="hmdfs"） | remote-view cached attr 不提供 nlink（恒 0）、mode 硬编码——**intentionally inconsistent** |

**结论**：diff.txt 中 node 行 dump 出现的 `Nlink:0`、`Atim/Ctim:{0,0}`、mode 差异等是**被跳过字段的诊断输出失真**（MdCmp 的 `outputBuf` 打印，conc-fs.go:269），**不触发比对、非 bug**。17 个 diff 的全部命中项（missing / dirtype）均落在保留比对的字段内——**无工具假阳性**。

## 判定与动作汇总

| 子类 | 动作 |
|---|---|
| 缺失类（15） | 归档（同族实例扩充）；后续若需根治，按 concurrent-rmdir-mkdir-divergence 文档的方向 |
| dirtype（2） | 归档（type 翻转新表现）；修复需 hmdfs 创建仲裁/对账冲突解决策略，暂不行动 |
| 幽灵 stat | 非 bug，本文档记录核实结论以防误判 |
| 工具侧 | 无需修改（比对字段设计正确；FSCFG 修复后 diff 正常产出） |

## 附：csan 判定语义备忘

- `MdCmp(fsMd1=基准 globalFsMd, fsMd2=对比节点)`：
  - `file count: N vs M` = 两侧 map 大小差；
  - `missing from client2` = 基准有、对比节点缺；`missing from client1` = **对比节点有、基准缺**（方向语义反直觉但实现一致，conc-fs.go:266/:276）；
  - 字段差异经 `compareFileMeta` 产出（上表设计）。
- dump 的 prog.txt 是**触发比对时的执行程序**，不一定包含根因操作——比对对象是全局树累积快照，冲突可能来自历史轮次的持久化分歧（如 132327 的 prog 仅含 3 个 open，冲突路径 `.uas` 来自历史状态）。
