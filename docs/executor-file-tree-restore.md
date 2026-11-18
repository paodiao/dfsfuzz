# Executor 文件树恢复设计

> 状态：HMDFS/GlusterFS restore 路径已实现；D1 BPF standalone smoke 已通过，
> D2 executor integration 待执行；GlusterFS 恢复/角色细节见总计划
> 日期：2026-11-08（2026-11-18 更新实现状态与 D1 结果）
> 范围：executor 每次执行前的文件树恢复；triage/minimize 简化；corpus 时间线保留  
> 相关：`docs/glusterfs-adaptation-plan.md`、`docs/glusterfs-config-and-scripts.md`、`docs/glusterfs-tracer-implementation-spec.md`、`docs/seed-optimization-plan.md`
>
> 2026-11-15 修订：GlusterFS normal restore 只做 client 侧 `rm -rf + rsync`；
> 不重启 server/volume；`glusterfs-node-down.sh` 只在 failure/crash 使用；
> executor 使用 `DfsRuntimeOps`，cwd 为 `/root/glusterfs-client/eifuzz_root`，
> server 跳过 metadata/trace；初始树加载使用 shared 清单，不读 `large_dir.info`。
> `restore_initial_file_tree` 在每次 `receive_execute` 前执行（含 triage/light），
> 不只是普通 fuzz round；server 只参与 barrier，FileTree 始终是 baseline。

## 0. 摘要

- 恢复点放在 **executor 的每次 `receive_execute()`**，不是 Go 侧 triage/minimize。
- 每次执行前恢复到 baseline；不区分普通 fuzz、triage、coverage 采集或其它执行类型。
- HMDFS 每个 VM 的 baseline 内容可以不同，但 VM 内路径相同。
- HMDFS baseline 映射：`/home/baseline/non_account/` -> `/data/service/el2/100/non_account/`。
- GlusterFS 使用所有 client 共享的一份 baseline：`/home/baseline/glusterfs/`。
- HMDFS 采用离线 unmount/remount + rsync，不使用 QEMU/qcow2 快照。
- GlusterFS 不使用本文件的 HMDFS brick 恢复方案；采用 client-side logical restore：固定测试根、单个 reset client、baseline `rm -rf + rsync`，不恢复 server brick。详见 `glusterfs-nfs-adaptation-plan.md` §2.9 和 §4.4。
- 多节点 restore 使用独立 `restoreStart` / `restoreState` barrier，不复用 `srvSetupBit`。
- `is_restarting` 的 crash recovery 路径跳过整个 restore；该路径恢复同一个 testcase，不能把崩溃现场恢复到 baseline。
- `barrierCount` / `syz_failure_barrier()` 属于 guest 程序级同步，不属于 restore 生命周期；当前不注入该伪调用，本轮不改。
- minimize 整段注释停用，不删除代码；triage 从两组 3 次执行压成 **一次执行**。
- corpus 的时间线继续使用“最近一次实际执行”的 `Call.CheckInfo`，由 `ProgTimings()` 在发送 corpus 前提取。
- `FileTree` 保持 baseline：停用 fuzzer 侧 `SyncFileTreeFromFsMd`；执行后的 `fsMd` 只用于 CSAN 和 DAG，不回写 `FileTree`。

## 1. 背景与目标

### 1.1 为什么要恢复

当前 triage/minimize 的覆盖率等价判断依赖文件系统状态：

- triage 在前几次执行之间取 `newSignal` 交集；
- minimize 的每个候选都按覆盖率/signal 是否等价决定是否提交；
- 如果执行之间不恢复文件树，同一个程序会在不同状态上运行，`Signal`/`Cover` 不可比较。

因此恢复不是 triage/minimize 的局部优化，而是覆盖率反馈能否复现的前提。

### 1.2 目标模型

```text
每个 execute 请求
  -> 恢复到该节点自己的 baseline
  -> 执行该 program
  -> 收集结果
```

所有执行使用同一个恢复路径。Go 侧不再判断“这次是 triage 还是 minimize，要不要恢复”。

### 1.3 非目标

- 本文只记录 HMDFS 的恢复实现；GlusterFS/NFS 的具体恢复见 `glusterfs-nfs-adaptation-plan.md` §2.9；
- 本文不修改 `prog.Minimize` 的实现源码；
- 本文不改变 DAG、FileTree、覆盖板的语义。

## 2. 统一恢复模型

### 2.1 恢复生命周期

恢复属于 executor 的执行生命周期：

```text
receive_execute
  -> 等待 testcase
  -> 读取并校验 execute_req
  -> 解析 env/exec flags、timeouts、prog offset/size
  -> restore start barrier
  -> restore local baseline
  -> restore finish barrier
  -> peer readiness
  -> calibrate_tsc
  -> 创建/清理本轮测试目录
  -> chdir
  -> execute_one
  -> reply_execute
```

### 2.2 恢复点

建议位置：`src/executor/executor.cc:1802` 的 `receive_execute()`。

恢复应放在：

- 读取并校验 `execute_req` 之后；
- `calibrate_tsc()` 之前；
- 创建/清理测试目录 `:1900-1941` 之前；
- `chdir(cwdbuf)` `:1943-1959` 之前。

这样本轮测试目录、挂载点和 cwd 都在恢复后的 base 上重新建立。

### 2.3 config 与恢复分离

- `receive_handshake()` 的 `config.sh` 保持一次性；
- 不在 handshake 中做每轮恢复；
- `is_restarting` 的 recovery 路径直接跳过 restore，不读写 `restoreStart` / `restoreState`，也不恢复 baseline；
- 非 restarting 的正常轮在 `reconfigure_dfs()` 之后，下一次 `receive_execute()` 仍然先恢复；
- restore 命令本身失败（unmount/rsync/mount）时，本节点先设置 `restoreState[executor_index] = 3`，仍进入 restore finish barrier；所有节点到达或 barrier 超时后统一失败，不能让程序在未恢复的状态上继续跑；agent/peer readiness 超时按 3.4 的 soft-timeout 策略处理。

### 2.4 Go 侧不再判断执行类型

恢复由 executor 无条件执行，因此：

- `StatTriage` / `StatMinimize` 不再参与 restore 决策；
- Go 侧不需要新增 `restoreBeforeTriage` / `restoreBeforeMinimize` 之类的分支；
- triage/minimize 的简化只影响执行次数和 coverage 收集，不影响恢复路径。

## 3. 多节点 barrier

### 3.1 为什么需要

GlusterFS/HMDFS 是多节点文件系统。若每个 executor 收到请求后各自恢复并立即执行，可能出现：

- 节点 A 已恢复并开始跑；
- 节点 B 仍在恢复；
- 客户端访问到 B 的旧状态。

这会让各节点在同一轮看到不同的文件树状态。

### 3.2 要求

每轮执行前：

```text
所有节点收到本轮请求
  -> 所有节点完成 restore
  -> barrier
  -> 所有节点开始 execute
```

- 使用 `restoreStart[64]`（0 not ready / 1 ready）和 `restoreState[64]`（0 pending / 2 done / 3 failed），不要复用 `srvSetupBit`；
- 所有 server/client 节点都参与；
- 先做 restore start barrier：所有节点 `restoreStart == 1` 后才开始 unmount/rsync；
- restore 完成后做 finish barrier：成功置 2，失败置 3，等全员到达后统一决定；
- start/finish 使用独立字段，避免先完成的节点把 start 状态改成 done 后卡住慢节点；
- restore 命令失败时先置 3，等全员到达或 barrier 超时后所有节点统一失败；agent/peer readiness 超时只记录日志，不直接失败（见 3.4）；
- 不能让部分节点先执行。

### 3.3 实现建议

- C++ 侧在 `executeControl` 中增加 `volatile unsigned char restoreState[SUPPORT_VMS]` 和 `volatile unsigned char restoreStart[SUPPORT_VMS]`；
- Go 侧在 `executeControl` 中同步增加 `restoreState [64]byte` 和 `restoreStart [64]byte`；
- start barrier：每个节点置 `restoreStart=1`，等待所有节点都等于 `1`；
- finish barrier：每个节点置 `restoreState=2` 或 `3`，等待所有节点都 `>=2`；
- start barrier 超时 **30 秒**，finish barrier 超时 **90 秒**；超时让本轮失败；
- readiness 超时记录日志和耗时；默认继续，strict 模式才失败（见 3.4）；
- barrier 通过后再创建测试目录/`chdir`。

### 3.4 readiness 超时策略

当前保留现有 soft-timeout 行为，不把 agent/peer 等待超时改成硬失败：

- `wait_hmdfs_agent_ready()`：30 秒超时后记录耗时/ready 状态，默认继续；
- `wait_hmdfs_peers_ready()`：3 轮检查后记录缺失 CID/耗时，默认继续；
- HMDFS mount/agent 重启属于 restore 流程，命令失败仍按 restore 失败处理；
- 每轮记录：restore 开始/结束、agent ready 耗时、peer 缺失列表、总等待时间；
- peer readiness 放在 finish barrier 之后执行，等所有节点完成 mount/agent 启动后再统一检查；
- 若统计显示每轮恢复经常超时，再启用 strict 模式：超时让本轮失败，可重试一轮；
- strict 模式作为可选开关，不改变当前运行语义。
- 这些等待只属于 HMDFS：GlusterFS 下 agent/peer/settle 全部 no-op，
  不执行 `wait_hmdfs_agent_ready()` / `wait_hmdfs_peers_ready()`，改用
  client mount barrier；timing 由 profile 决定，见总计划 §10.4。

## 4. 恢复方式：离线 unmount/remount + rsync

已确定采用方案 B。QEMU/qcow2 快照保留为未采用备选。

### 4.1 HMDFS 已选方案：离线 unmount/remount + rsync

baseline 映射：

```text
/home/baseline/non_account/ -> /data/service/el2/100/non_account/
```

每个 VM 的 baseline 内容不同，但 VM 内路径相同。`/data/service/el2/100/non_account` 是 HMDFS 的 mount source，也是本轮要恢复的底层文件树。

恢复流程：

```text
restore start barrier
sync
chdir("/")
timeout 15s /root/hmdfs-node-down.sh
wait_hmdfs_agent_exit()
umount2(mnt_dir, MNT_DETACH) 校验
sync
timeout 60s rsync -a --numeric-ids --delete --one-file-system --checksum \
      /home/baseline/non_account/ \
      /data/service/el2/100/non_account/
sync
echo 3 > /proc/sys/vm/drop_caches
timeout 30s /root/hmdfs-node-up.sh <init_ip> "<dfs_setup_params>" <executor_index>
wait_hmdfs_agent_ready()
if (executor_index == vm_count - 1)
    usleep(HMDFS_SETTLE_MS * 1000)
restore finish barrier
wait_hmdfs_peers_ready()
```

要求：

- 卸载前必须 `chdir("/")`，避免 executor 的 cwd 仍在 HMDFS 挂载点内导致 EBUSY；
- 复用 `/root/hmdfs-node-down.sh` 和 `/root/hmdfs-node-up.sh`，不要重复 mount 参数；
- node-down 后等待旧 agent 退出（最多 5 秒），避免旧 agent 与新 agent/rsync 竞争；
- node-down 后必须用 `umount2` 或 mount 状态检查确认挂载点已卸载；`umount -l` 失败不能静默继续；
- restore 命令加 timeout，避免 rsync/mount 卡死；当前为 node-down 15s、rsync 60s、node-up 30s，start/finish barrier 分别为 30s/90s；
- HMDFS baseline 不含 xattr、ACL、hardlink、symlink，且 fuzz 调用面未启用相关 syscall，因此 rsync 不需要 `-H/-A/-X`；`-a` 已包含权限、时间、owner/group 和空目录。
- 第一版保留 `--checksum` 作为数据正确性基线；若实测发现其开销明显，再评估去掉。`--delete` 已启用时不需要 `--force`。
- peer readiness 在 finish barrier 之后执行；
- `is_restarting` 的 recovery 路径跳过 restore，保留 crash 现场；
- `barrierCount` / `syz_failure_barrier()` 不纳入 restore 生命周期；当前不注入该伪调用；
- HMDFS restore 已实现；GlusterFS 按 §4.4 做 client-side logical restore；
  NFS 暂不实现；
- 非 HMDFS 使用 `/home/baseline/<dfs-specific-dir>/` 作为 baseline；
- `restore_initial_file_tree`、cwd、metadata、trace、config、node-up/down 统一走
  `DfsRuntimeOps`（见 `glusterfs-adaptation-plan.md` §10），不要保留 HMDFS-only 分支；
- 镜像必须包含 `rsync`。`create-image-new.sh` 和 `create-image.sh` 都已加入 `rsync` 和 `iptables`。

### 4.2 未采用备选：QEMU/qcow2 快照回滚

思路：

- 每个 VM 在 base 状态做 snapshot；
- 每次 execute 前回滚到该 snapshot；
- 可同时恢复磁盘、页缓存、挂载状态等。

未采用原因：

- 每次 execute 回滚开销大；
- 多节点需要同时回滚并 barrier；
- 快照可能包含 VM 时钟状态，回滚后需要额外处理 TSC；
- 网络连接和 agent 状态可能需要额外恢复。

### 4.3 共同约束

- 禁止在线 rsync 整棵文件树作为内层恢复；
- 恢复必须覆盖本轮会读写的所有节点；
- 只恢复 client 可见目录不等于恢复分布式 FS 状态；
- restore 命令失败不能静默继续；readiness 超时按 3.4 的 soft/strict 策略处理；
- GlusterFS 第一版采用 client-side logical restore，只重置客户端可见命名空间，不恢复 server brick；详见 `glusterfs-nfs-adaptation-plan.md` §2.9。若出现 server 侧残留导致不可复现，再评估 brick-level restore。
- NFS 要覆盖 client 挂载点和 server 导出状态。

### 4.4 GlusterFS prefill 与 restore

GlusterFS 的 baseline 烤在 client 镜像中：

```text
/home/baseline/glusterfs/
```

prefill 在 volume 挂载后只执行一次，由 client1 执行：

```bash
rm -rf /root/glusterfs-client/eifuzz_root
mkdir -p /root/glusterfs-client/eifuzz_root
timeout 60s rsync -a --numeric-ids --delete \
  /home/baseline/glusterfs/ \
  /root/glusterfs-client/eifuzz_root/
sync
```

每轮 restore 复用同一份 baseline 和同一命令。reset client 固定为：

```text
executor_index == server_num
```

3-3 下即 index 3。其他 client 只参与 barrier，server 不执行 restore。

约束：

- 不把树直接写进 server brick 或 `data.img`；GlusterFS 需要正确的 GFID/xattr/replica 元数据。
- 旧 `dfs-*`、`lost+found` 和 server brick 路径不进入 FileTree/CSAN。
- prefill 和 restore 共用 baseline，避免 prefill 树与恢复树漂移。
- prefill 幂等：先清空固定测试根，再 rsync。
- GlusterFS rsync 总超时 60s，不使用 `--timeout=300`；restore finish
  barrier 90s，start barrier 30s。详见 `glusterfs-adaptation-plan.md` §10.4。
- GlusterFS baseline 约 100 文件，restore 不应比 HMDFS 的 1000 文件大目录
  给更长超时。
- 镜像预建 `/root/glusterfs-client` 空挂载点；`/root/glusterfs-client/eifuzz_root`
  不预置，由 prefill 在挂载 volume 后创建。
- GlusterFS baseline 不含 symlink/hardlink/xattr；运行期创建的链接由
  `rm -rf + rsync` 清掉，FileTree 始终是 baseline，不把 linkpath 写回树。
- normal restore 不重启 server/volume，不调用 `glusterfs-node-down.sh`；
  node-down 只在 failure/crash 场景使用。
- restore 在每次 `receive_execute` 前执行，包括 triage/light/DAG replay；
  server 只参与 restore barrier，不执行 rm/rsync；client 中只有 reset client
  执行 rm/rsync，其他 client 等 barrier。
- 恢复后 FileTree 保持 baseline；`SyncFileTreeFromFsMd` 停用，`fsMd` 只用于
  CSAN/DAG，不回写 FileTree。
- `restore_initial_file_tree`、cwd、metadata、trace、config、node-up/down 统一走
  `DfsRuntimeOps`，不要只保留 HMDFS 分支；见 `glusterfs-adaptation-plan.md` §10。
- InitialDir 对 GlusterFS 只加载 `shared.file/.dir/.tmpdir` 和 `empty_dirs.info`，
  不加载 `large_dir.info`；baseline 只复制 `shared/` 的内容。详见
  `glusterfs-config-and-scripts.md` §2–§3。

## 5. fuzzer 侧简化

### 5.1 minimize 停用

决定：**注释掉 `prog.Minimize` 整段，不删除。**

原因：

- minimize 会反复执行，放大恢复/执行成本；
- 已有 issue 指出 minimize 可能把程序变空；
- 当前 mutation 路径已有专门控制程序大小的策略，足够使用。

### 5.2 triage 压成一次

当前 triage 有两组 3 次执行：

- 前 3 次 `signalRuns`：计算/收缩 `newSignal`；
- 后 3 次 `signalRuns`：为最小化后的程序重新收集 coverage。

minimize 停用后，两者合并为 **一次执行**：

```text
廉价预过滤（可选，不执行）
  -> executor 内部 restore
  -> 执行一次 triage
  -> 收集目标 subNum + server/client 的 signal/cover
  -> 若目标新 signal 为空则丢弃
  -> 发送 corpus
```

### 5.3 具体改动范围

在 `src/syz-fuzzer/proc.go` 的 `triageInput` 中：

- 注释掉前 3 次 `signalRuns` 交集循环 `:516-541`；
- 注释掉 `prog.Minimize` 整段 `:554-575`；
- 注释掉后 3 次 coverage 循环 `:613-642`；
- 用一次 `executeRaw(execOptsLight, item.ps, StatTriage)` 同时收集所有 server/client 的 signal/cover；
- 保留廉价预过滤：`corpusSignalDiff(item.info.Signal)` 为空则直接返回；
- 非 DAG 情况下，单次执行后新 signal 为空则丢弃；
- DAG triage 继续走 DAG 反馈路径，不因 coverage 为空而丢弃；
- 保留 `totalLen == 0` 兜底；
- 不再需要 `minimizeAttempts`；
- 不再需要 `execOptsLightMinimize`；
- 不再需要 minimize 后面的 stage 2 `preSig`/重新 hash；stage 1 hash 去重保留。

### 5.4 执行次数变化

普通候选从：

```text
3 次 triage 稳定性
+ 3 次 coverage
+ 3 × minimize 候选数
```

降为：

```text
1 次 triage
```

minimize 候选执行数为 0。

### 5.5 停用 fuzzer 侧 SyncFileTreeFromFsMd

前提：`InitialDir` 的 manifest 与每个 VM 的 baseline 完全一致：
HMDFS `/home/baseline/non_account`，GlusterFS `/home/baseline/glusterfs`。

每轮 execute 前恢复文件树后，`FileTree` 应始终保持 baseline：

- `FileTree` 是种子构造、覆盖板和 CSAN 使用的初始树；
- 执行后的 `fsMd` 是临时状态，下一轮 execute 前会被 restore 丢弃；
- 如果继续 `SyncFileTreeFromFsMd`，`FileTree` 会记录下一轮并不存在的路径、大小、类型和空目录状态。

因此：

- 注释掉 `src/syz-fuzzer/proc.go:1486-1500` 的 FileTree sync 块；
- `clientIdx` 只服务于该 sync 块，一并注释；
- 保留 `fsMds` 收集、CSAN 和 DAG；
- 更新 `proc.go:1527` 附近“FileTree sync above”的旧注释；
- `collectCreateCallOwners` 和 `SyncFileTreeFromFsMd` 暂时保留为死代码，确认不需要回退后再删除。

回退条件：

- 如果以后关闭每轮文件树恢复，必须重新启用 `SyncFileTreeFromFsMd`；
- 如果 `InitialDir` manifest 与 baseline 不一致，不能直接停 sync，需要改成启动时从 baseline 做一次 `FileTree` 初始化。

## 6. 注释策略

代码计划采用注释而不是删除，便于后续对照和恢复。

### 6.1 注释块

建议使用明确标记：

```go
// ===== DISABLED: minimize =====
// ...
// ===== END DISABLED: minimize =====
```

```go
// ===== DISABLED: extra triage runs =====
// ...
// ===== END DISABLED: extra triage runs =====
```

```go
// ===== DISABLED: FileTree sync (per-exec restore keeps baseline) =====
// ...
// ===== END DISABLED: FileTree sync =====
```

### 6.2 编译清理

只被注释块使用的局部变量/常量要同步处理，避免 Go 未使用错误：

- `minimizeAttempts`；
- `execOptsLightMinimize` 及其初始化；
- `notexecuted`；
- `preSig`；
- 只用于旧循环的 `signalRuns` / `newSignal` 变量。

不能为了保留代码而留下未使用变量。

### 6.3 不能一起注释的代码

以下逻辑必须保持活跃：

- `sendInputToManager`；
- `addInputToCorpus`；
- `Timings: prog.ProgTimings(item.ps)`；
- `parseClientOutput` 中对 `Call.CheckInfo` 的写入；
- `ProgTimings()` / `ApplyProgTimings()`；
- manager 的 timeline 刷新；
- fuzzer 重启时的 timeline 恢复；
- DAG 中所有 `CheckInfo`、`Stime`、`Etime` 使用；
- `fsMds` 收集、CSAN `ConcFSCheck`、DAG `BuildVertices`；
- `FileTree.InitFromHmdfsConfig` 的 baseline 初始化。

## 7. 时间线保留

### 7.1 语义

corpus 保存的是**最近一次实际执行**的时间线，不区分普通 fuzz、triage 还是 coverage 采集。

因此：

- 谁最后执行，就保存谁的时间线；
- minimize 停用且 triage 压成一次后，corpus 的时间线来自这次 triage；
- 如果以后再加 coverage 重跑，最后一次 coverage 执行会成为时间线来源；
- 不需要额外选择，也不需要单独保存“发现时”的时间线。

### 7.2 数据链

```text
executor reply stime/etime
  -> pkg/ipc parseClientOutput: Call.CheckInfo
  -> prog.ProgTimings(item.ps)
  -> RPCInput.Timings
  -> manager.newInput 刷新 corpus timeline
  -> fuzzer.addInputFromAnotherFuzzer -> ApplyProgTimings
  -> DAG / seed construction
```

### 7.3 代码位置

- `src/executor/executor.cc`：call 回复中的时间字段；
- `src/pkg/ipc/ipc.go:1341-1348`：把 `reply.stime/etime` 写入 `p.Calls[reply.index].CheckInfo`；
- `src/prog/prog.go:160`：`Call.CheckInfo *FileMetadata`；
- `src/prog/mutation.go:4920`：`ProgTimings()`；
- `src/prog/mutation.go:4946`：`ApplyProgTimings()`；
- `src/syz-fuzzer/proc.go:654`：triage 发送 `Timings: prog.ProgTimings(item.ps)`；
- `src/syz-fuzzer/proc.go:1188`：failure triage 的 `Timings`；
- `src/syz-manager/manager.go:1553-1557`：刷新 corpus 中已有 seed 的 timeline；
- `src/syz-fuzzer/fuzzer.go:701-706`：fuzzer 重启后恢复 timeline；
- `src/prog/dag.go:363`、`:897`、`:948`：DAG 使用 `CheckInfo` 的 `Stime/Etime`。

### 7.4 实现约束

- `Timings: prog.ProgTimings(item.ps)` 必须放在最后一次执行之后、发送 corpus 之前；
- 不能提前提取或缓存；
- 不能把 `Timings`、`CheckInfo`、`ProgTimings`/`ApplyProgTimings`、manager 刷新一起注释掉；
- restore 后统一重新 `calibrate_tsc()`；离线恢复不改变 VM 时钟状态，但重新校准可以限制 restore 期间的时钟漂移；
- 不要让未执行的空时间线覆盖已有 corpus 时间线。

## 8. 验证计划

### 8.1 恢复正确性

- 同一 program 恢复后连续执行多次，比较 `Signal`/`Cover` 是否一致；
- 多节点检查 restore start barrier 前没有节点提前 unmount/rsync；
- restore finish barrier 前没有节点提前 execute；
- node-down 后旧 agent 是否在 rsync 前退出；
- node-down 后 mount 状态检查是否通过；
- restore 命令失败是否让本轮干净失败；agent/peer readiness 超时是否被记录、计数，strict 模式是否按预期失败。

### 8.2 triage/minimize

- triage 是否只执行一次；
- 单次 triage 的 coverage 收益是否可接受；
- 空程序率是否下降；
- minimize 执行次数是否为 0；
- `totalLen == 0` 兜底是否仍保留。

### 8.3 时间线

- corpus 中的 `Timings` 是否来自最后一次执行；
- fuzzer 重启后 `timingsApplied/timingsTotal` 是否正常；
- DAG `MatchFailNoCI` 是否升高；
- `timingAlignMissCount` 是否异常；
- 恢复后时间窗口是否仍能匹配 call-event。

### 8.4 性能

- 单次 restore 总耗时；
- unmount / rsync / remount / agent ready / peer ready 分项耗时；
- 每次 execute 的平均耗时；
- 多节点 barrier 的额外开销。

### 8.5 FileTree 稳定性

- 连续执行多个 program 后，`FileTree` 不随执行结果漂移；
- 生成器选择的路径始终存在于恢复后的 baseline；
- CSAN 的 `initTreeSubset` 使用 baseline，而不是上一次执行后的临时状态；
- `fsMds` 仍正常用于 CSAN 和 DAG。

## 9. 待实测与后续

1. 实测 restore + barrier 总耗时，确认是否落在 10–20 秒预算内；
2. 在 3 节点 HMDFS 上验证 barrier、agent ready 和 peer ready；
3. 单次 triage 的 coverage 收益是否足够；不足时是否回退到“3 次干净执行”；
4. GlusterFS 按 `glusterfs-nfs-adaptation-plan.md` §2.9 实现 client-side logical restore；NFS 的 baseline 映射和恢复脚本后续再定。

## 10. 变更记录

- 2026-11-04：记录统一 executor 恢复模型、minimize 注释停用、triage 压成一次、时间线保留约束；恢复实现保持两种可选方案（已由 2026-11-08 决策取代）。
- 2026-11-04：更新 readiness 超时策略为 soft-timeout 默认 + 耗时/缺失观测，strict fail 作为可选模式。
- 2026-11-08：确定 HMDFS 离线 rsync 恢复方案、baseline 路径映射、`restoreState` barrier 语义；qcow2 快照改为未采用备选。
- 2026-11-08：记录 FileTree 保持 baseline，停用 fuzzer 侧 `SyncFileTreeFromFsMd`；`fsMd` 仍保留给 CSAN/DAG。
- 2026-11-08：写入 executor restore、`restoreState` barrier 和 fuzzer 侧 FileTree sync 停用代码。
- 2026-11-08：增加 restore start/finish 两阶段 barrier、node-down 后 mount 校验、restore 命令 timeout，并将 peer readiness 移到 finish barrier 之后。
- 2026-11-08：拆分 `restoreStart` 与 `restoreState`，修复 start/finish 共用状态导致的死锁；增加旧 agent 退出等待。
- 2026-11-08：`is_restarting` recovery 路径跳过 restore，保留 crash 现场；`barrierCount` 不纳入 restore 生命周期，本轮不改。
- 2026-11-13：补充 GlusterFS 边界：采用 client-side logical restore，不恢复 server brick；固定测试根 + reset client + baseline `rm -rf + rsync`，详见 `glusterfs-nfs-adaptation-plan.md` §2.9。
- 2026-11-13：HMDFS rsync 去掉 `-H/-A/-X`（baseline 与 fuzz 调用面均无 xattr/ACL/hardlink/symlink），保留 `-a --checksum`；`--delete` 已启用，不使用 `--force`。
- 2026-11-13：收紧 restore 超时：node-down 15s、rsync 60s、node-up 30s；barrier 拆分为 start 30s / finish 90s。
- 2026-11-14：补充 GlusterFS 3-3 replica 部署决定：baseline 烤 client 镜像、client1 prefill、reset client 固定 index 3、prefill/restore 共用 baseline、data.img 不启用。
- 2026-11-15：明确 GlusterFS baseline 不含 symlink/hardlink/xattr；FileTree 不建模链接，link/symlink 的 linkpath 是 program-local binding，restore 后不写回树。
- 2026-11-15：收敛 GlusterFS timing：rsync 总超时 60s、start/finish barrier 30s/90s，不用 `--timeout=300`；metadata delay 10s 起步，详见总计划 §10.4。
- 2026-11-15：明确 restore 在每次 `receive_execute`（含 triage/light/DAG replay）前执行；server 只参与 barrier；FileTree 始终 baseline，`SyncFileTreeFromFsMd` 停用。
