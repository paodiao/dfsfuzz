# HMDFS 一致性问题归档：收集阶段 file_stat ENOENT（fpdzkkvcoj 子树）

## 概述

| 项 | 内容 |
| --- | --- |
| **现象** | 元数据收集阶段（write_dir_info 遍历共享树），节点 2 对初始树子树 `Eris_fpdzkkvcoj_99469205221.d/Eris_vbbzakrnhn_585401139733.d/Eris_jpkzacijpv_344130969263.d/...` 的 `stat()` 连续两次返回 ENOENT，触发 `fail()` → 实例重启存档 |
| **关键矛盾** | 该条目刚被本节点的 `readdir` 列出（遍历到才 stat）——**同一节点上、毫秒级间隔内 readdir 可见而 stat 不可见** |
| **同子树对照** | 同一时段 executor 0/1 对同一子树的收集完全正常（log0 中各 41 次 file_stat 命中，无失败记录） |
| **触发环境** | `net_failure=true`；本轮首轮程序为 `is_stash=true` 类型且含 `syz_failure_net_down`/`net_up` 注入 |
| **定性** | **待区分（a/b）——需 R1/R2/R3 任一实验回填**：瞬态不一致窗口被收集器撞上，还是持久丢失状态。当前证据不足以判定 |
| **证据存档** | `log0`/`report0`（SYZFAIL 现场与 VM DIAGNOSIS）；`initialdir/`（edd5a2a 节点命中该子树：11 目录 + 7 文件） |

## 现场还原

### SYZFAIL 日志摘录（log0）

```
executor 0 /mnt/hmdfs/100/non_account/merge_view/Eris_hisrrykxiv_451072199116.d/...   ← node0 正常收集其他子树
executor 2 access /mnt/hmdfs/100/non_account/merge_view/Eris_fpdzkkvcoj_99469205221.d/Eris_vbbzakrnhn_585401139733.d/Eris_jpkzacijpv_344130969263...
           （access 输出：doesn't exist）
executor 1 file_stat /mnt/hmdfs/100/non_account/merge_view/Eris_hisrrykxiv_451072199116.d/...   ← node1 正常
stat again: /mnt/hmdfs/100/non_account/merge_view/Eris_fpdzkkvcoj_99469205221.d/Eris_vbbzakrnhn_585401139733.d/Eris_jpkzacijpv_344130969263.d/...
            (ENOENT)
SYZFAIL: executor 2: file_stat /mnt/hmdfs/100/non_account/merge_view/Eris_fpdzkkvcoj_99469205221.d/...
```

- `file_stat` 内部的两次尝试均 ENOENT：原调用失败后执行 `access(F_OK)`（不存在）与二次 `stat`（仍 ENOENT）——见 `getmetadata.h:11-39`
- 失败点为目录路径，走 `write_dir_info` 的 :163 入口（递归子目录的 file_stat）

### 调用链（代码级）

```
executor.cc write_metadata()
  → getmetadata.h write_dir_info(dn, dent)                      (:151)
      → opendir/readdir 遍历                                     (:175/:195)
      → 对每个条目：
          目录 → write_dir_info(sub_fn, sub_ent) 递归 (:212-213)
                 └→ file_stat(DT_DIR, dn) == 0 ? 写记录 : fail()  (:163-170)
          文件 → file_stat(d_type, sub_fn) == 0 ? 写记录 : fail()  (:216-224)
```

- 失败即 `fail()`（getmetadata.h:169/:223）：整个实例退出 → fuzzer 判定执行异常 → 重启存档

## 机制候选（并列，未判定）

### (a) readdir → stat 间隙的并发消失传播（瞬态窗口竞争）

hmdfs 弱一致模型下，跨节点的删除/重命名事件在 net_down 恢复期以延迟/乱序方式传播：

- 节点 2 的 `readdir` 结果来自某时刻的视图（列出该条目）
- 与 `stat()` 之间的间隙内，删除/失效传播生效（comrade dentry 失效、lookup 返回 ENOENT）
- 属于弱一致环境的正常瞬态——**被收集器的"任何失败即终止"策略撞上**

支持因素：首轮程序正是 stash + net_down/net_up 类型，失败紧邻网络恢复期（comrade/dentryfile 失效重建高频窗口）。

### (b) 持久丢失状态（hmdfs 缺陷候选）

若恢复完成后该路径在节点 2 的视图中长期 ENOENT（不复自愈），则与 csan-20260826-204842.777（.ggy 路径的"node0 目录 / node1/2 文件"类型分歧，见关联文档）可能同源：net_failure 场景下 hmdfs 对 edd5a2a local 子树的元数据维护存在缺陷。

## 为何当前无法区分

1. `fail()` 即刻终止实例并触发 VM 重启，`-snapshot` 回滚现场
2. "恢复后该路径是否可见"这一决定性观测从未采集
3. 收集器无重试/跳过逻辑，一次 ENOENT 直接终局

## 区分实验方案（R1/R2/R3 任一即可判定）

| # | 方案 | 改动点 | 判定 |
| --- | --- | --- | --- |
| R1 | `file_stat` 失败改为**记录 + 跳过**（不 fail），照常收齐 fsMd 交 mdCmp | getmetadata.h:216-224 与 :163-170：fail() → 记日志 + continue | 若 mdCmp 通过且后续轮次该路径重新可见 → (a)；持续缺失 → (b) |
| R2 | `file_stat` 失败后**短退避重试**（2–3 次 × ~100ms），仍失败才 fail | getmetadata.h file_stat 内加重试环 | 重试成功 → 坐实瞬态窗口 |
| R3 | 记录失败时刻与最近 `net_up` 的相对时间偏移 | executor 侧记录 net_up 时间戳，SYZFAIL 时打印差值 | 差值小（秒级内）强指向 (a) 恢复期竞争 |

建议优先 R1+R2 组合（同为收集器健壮性改动，一处修改同时完成区分实验与降级处理）。

## 关联的工程问题（单列，不属于 bug 定性）

collect 阶段的"任何 stat/xattr 失败 → 整轮终止 + VM 重启"策略对 hmdfs 弱一致语义过于激进：瞬态（候选 a）与真实缺陷同样触发重启，重启成本高且毁掉现场。是否改为"记录失败并继续收集 + 以 mdCmp 作为一致性最终判据"，属于 Monarch 设计取舍，需单独决策。

## 关联文档

- `docs/hmdfs-bug-concurrent-rmdir-mkdir-divergence.md`：命名空间收敛性违反（同类 net-free 场景的真实分歧）
- `docs/hmdfs-bug-merge-view-missing-comrade.md`：merge_view 缺失 comrade 类问题
- `docs/hmdfs-metadata-weak-consistency.md`：元数据弱一致机制基线（判断瞬态/持久的参照系）
- 本例与 csan-20260826-204842.777（.ggy 类型分歧）疑似同根因不同暴露面：两者都发生在 net_failure 场景下的 edd5a2a local 子树；前者以类型分歧暴露于 mdCmp，本例以收集期 ENOENT 暴露于遍历器
