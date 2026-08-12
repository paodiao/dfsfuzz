# HMDFS Stash功能模糊测试设计文档

## 文档概述

本文档记录了针对hmdfs分布式文件系统stash功能的模糊测试设计方案，包括bug类型分析、故障注入方法设计、状态感知机制等核心内容。本文档旨在为后续的模糊测试实现提供完整的设计参考。

---

## 一、Stash功能Bug类型分析

### 1.1 错误类型优先级排序

根据对hmdfs stash功能代码的分析（主要参考`hmdfs/stash.c`），错误类型按出现概率从高到低排序：

#### 第一优先级：并发错误（最容易出现）

**核心问题**：stash功能涉及大量的并发操作，包括多节点同时访问、状态转换、锁管理等。

**具体类型**：

##### 1.1.1 竞态条件（Race Conditions）

**关键代码位置**：
- stash状态转换：`HMDFS_REMOTE_INODE_NONE` → `STASHING` → `RESTORING` → `NONE`
  - 参见代码：[stash.c:626-629](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L626-L629)
  ```c
  spin_lock(&info->stash_lock);
  info->cache = cache;
  info->stash_status = HMDFS_REMOTE_INODE_STASHING;
  spin_unlock(&info->stash_lock);
  ```

- 多个客户端同时访问同一文件时的竞态
  - 参见代码：[stash.c:724-751](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L724-L751)
  ```c
  spin_lock(&conn->wr_opened_inode_lock);
  list_for_each_entry(info, &conn->wr_opened_inode_list, wr_opened_node) {
      int status = smp_load_acquire(&info->stash_status);
      if (status == HMDFS_REMOTE_INODE_NONE) {
          list_add_tail(&info->stash_node, list);
          hmdfs_remote_add_wr_opened_inode_nolock(conn, info);
          ihold(&info->vfs_inode);
      }
  }
  spin_unlock(&conn->wr_opened_inode_lock);
  ```

- 节点离线/上线事件与文件操作的并发
  - 参见代码：[stash.c:1071-1082](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L1071-L1082)

**诱发场景**：
- 多个客户端同时对同一文件进行写操作时节点离线
- 节点在stash过程中又上线
- 恢复过程中节点再次离线

##### 1.1.2 死锁（Deadlocks）

**关键代码位置**：
- 多个锁的获取顺序问题
  - 涉及的锁：`stash_lock`、`wr_opened_inode_lock`、`stashed_inode_lock`、`offline_cb_lock`、`seq_lock`
  - 参见代码：[stash.c:673-680](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L673-L680)
  ```c
  spin_lock(&info->stash_lock);
  info->cache = NULL;
  smp_store_release(&info->stash_status, status);
  spin_unlock(&info->stash_lock);
  ```

**诱发场景**：
- 不同代码路径以不同顺序获取多个锁
- 异常路径下锁释放顺序不一致
- 中断处理与正常操作的锁竞争

##### 1.1.3 数据竞争（Data Races）

**关键代码位置**：
- `cache`指针的并发访问和释放
  - 参见代码：[stash.c:538-546](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L538-L546)
  ```c
  static void hmdfs_del_file_cache(struct hmdfs_cache_info *cache)
  {
      if (!cache)
          return;
      fput(cache->cache_file);
      kfree(cache->path_buf);
      kfree(cache);
  }
  ```

- `stash_status`状态的并发读写（使用了`smp_load_acquire`/`smp_store_release`，但仍可能有遗漏）
  - 参见代码：[stash.c:734-735](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L734-L735)
  ```c
  status = smp_load_acquire(&info->stash_status);
  if (status == HMDFS_REMOTE_INODE_NONE) {
  ```

- `written_pgs`和`to_write_pgs`原子计数器的并发更新

**诱发场景**：
- 多个线程同时更新cache指针
- 状态检查与状态更新之间的时间窗口
- 原子操作与普通操作的并发访问

#### 第二优先级：内存错误（次容易出现）

**核心问题**：涉及复杂的内存管理，包括缓存结构、inode引用等。

**具体类型**：

##### 1.2.1 Use-after-free

**关键代码位置**：
- `cache`结构体的生命周期管理
  - 参见代码：[stash.c:538-546](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L538-L546)

- inode引用计数管理问题（`ihold`/`iput`）
  - 参见代码：[stash.c:743](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L743)
  ```c
  ihold(&info->vfs_inode);
  ```
  - 参见代码：[stash.c:819](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L819)
  ```c
  iput(&info->vfs_inode);
  ```

**诱发场景**：
- 异常路径下cache被提前释放
- inode引用计数管理错误
- 多个代码路径释放同一资源

##### 1.2.2 内存泄漏（Memory Leaks）

**关键代码位置**：
- 异常路径下资源未释放
  - 参见代码：[stash.c:562-564](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L562-L564)
  ```c
  cache->path_buf = kmalloc(PATH_MAX, GFP_KERNEL);
  if (!cache->path_buf) {
      err = -ENOMEM;
      goto free_path;
  }
  ```

- `path_buf`、`page`等临时缓冲区的泄漏
  - 参见代码：[stash.c:1538-1542](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L1538-L1542)
  ```c
  dst = kmalloc(PATH_MAX, GFP_KERNEL);
  if (!dst) {
      err = -ENOMEM;
      goto put_path;
  }
  ```

**诱发场景**：
- 错误处理路径中资源未释放
- 中断处理时清理不完整
- 循环中的内存分配未配对释放

##### 1.2.3 Double-free

**关键代码位置**：
- 多个代码路径可能释放同一资源
  - 参见代码：[stash.c:1982-1996](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L1982-L1996)
  ```c
  spin_lock(&info->stash_lock);
  cache = info->cache;
  info->cache = NULL;
  info->stash_status = HMDFS_REMOTE_INODE_NONE;
  spin_unlock(&info->stash_lock);
  hmdfs_remote_del_wr_opened_inode(conn, info);
  hmdfs_del_file_cache(cache);
  iput(&info->vfs_inode);
  ```

**诱发场景**：
- 正常路径和异常路径都释放同一资源
- 错误恢复时重复释放
- 引用计数管理错误导致多次释放

##### 1.2.4 空指针解引用（Null Pointer Dereference）

**关键代码位置**：
- `cache`可能为NULL但未检查
  - 参见代码：[stash.c:2093-2095](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L2093-L2095)
  ```c
  cache = info->cache;
  if (!cache)
      return -EIO;
  ```

- `conn`、`info`等指针的空指针检查

**诱发场景**：
- 初始化失败导致指针为NULL
- 并发访问导致指针被置NULL
- 错误传播路径中指针检查遗漏

#### 第三优先级：语义错误（相对较少）

**核心问题**：涉及数据一致性、状态机正确性等逻辑错误。

**具体类型**：

##### 1.3.1 数据不一致

**关键代码位置**：
- stash文件元数据与实际数据不匹配
  - 参见代码：[stash.c:1084-1168](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L1084-L1168)
  ```c
  static int hmdfs_verify_restore_file_head(struct hmdfs_file_restore_ctx *ctx,
                                      const struct hmdfs_cache_file_head *head)
  {
      if (le32_to_cpu(head->magic) != HMDFS_STASH_FILE_HEAD_MAGIC) {
          err = -EUCLEAN;
          hmdfs_err("peer 0x%x:0x%llx ino 0x%llx invalid magic: got 0x%x, exp 0x%x",
                    conn->owner, conn->device_id, ctx->inum,
                    le32_to_cpu(head->magic),
                    HMDFS_STASH_FILE_HEAD_MAGIC);
          goto out;
      }
      // ... 其他验证
  }
  ```

- CRC校验失败但未正确处理

**诱发场景**：
- 节点崩溃导致数据写入不完整
- 网络分区导致元数据和数据不同步
- 并发写入导致数据损坏

##### 1.3.2 状态机错误

**关键代码位置**：
- stash状态转换逻辑错误
  - 参见代码：[stash.c:671-680](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L671-L680)
  ```c
  status = err > 0 ? HMDFS_REMOTE_INODE_RESTORING :
             HMDFS_REMOTE_INODE_NONE;
  spin_lock(&info->stash_lock);
  info->cache = NULL;
  smp_store_release(&info->stash_status, status);
  spin_unlock(&info->stash_lock);
  ```

- 节点状态与inode状态不一致

**诱发场景**：
- 异常事件导致状态转换错误
- 并发操作导致状态不一致
- 状态转换顺序错误

##### 1.3.3 恢复失败

**关键代码位置**：
- 恢复时文件已存在或路径错误
  - 参见代码：[stash.c:1243-1266](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L1243-L1266)
  ```c
  dst = file_open_root(&ctx->dst_root_path,
                       ctx->dst, O_LARGEFILE | rw_flag, 0);
  if (IS_ERR(dst)) {
      err = PTR_ERR(dst);
      hmdfs_err("open remote file ino 0x%llx err %d", ctx->inum, err);
      if (hmdfs_is_node_offlined(conn, ctx->seq))
          err = -ESHUTDOWN;
      goto out;
  }
  ```

- 数据恢复不完整

**诱发场景**：
- 节点上线时路径配置错误
- 恢复过程中节点再次离线
- 磁盘空间不足导致恢复失败

### 1.2 最容易诱发bug的节点状态和集群状态

#### 1.2.1 节点状态组合

**高危险场景**：

##### 节点频繁上下线
- 节点在stash过程中又上线
- 节点在恢复过程中又离线
- 参见代码：[stash.c:1071-1082](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L1071-L1082)
  ```c
  static inline bool hmdfs_is_node_offlined(const struct hmdfs_peer *conn,
                                         unsigned int seq)
  {
      smp_mb__before_atomic();
      return hmdfs_node_evt_seq(conn) != seq;
  }
  ```

**诱发原因**：
- 状态转换过程中节点状态变化
- 恢复操作被中断
- 缓存文件与实际状态不一致

##### 部分节点离线
- 多副本场景下部分节点离线
- 导致数据不一致和恢复失败

##### 节点崩溃
- 在stash过程中崩溃
- 在恢复过程中崩溃
- 导致缓存文件损坏

#### 1.2.2 集群状态

**高危险场景**：

##### 网络分区
- 客户端与服务器网络中断
- 服务器之间网络中断
- 导致stash和恢复操作失败

##### 高并发访问
- 多个客户端同时访问同一文件
- 多个客户端同时触发stash操作
- 参见代码：[stash.c:724-751](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L724-L751)

**诱发原因**：
- 锁竞争加剧
- 状态转换冲突
- 资源争用

##### 资源受限
- 磁盘空间不足
- 内存不足
- 导致stash操作失败

##### 长时间运行
- 大量文件被stash
- 长时间离线后恢复
- 导致缓存文件累积和恢复延迟

#### 1.2.3 文件状态

**高危险场景**：

##### 文件正在写入
- 节点离线时文件正在写入
- 导致数据不一致

##### 大文件
- 大文件的stash和恢复耗时较长
- 更容易在过程中遇到节点状态变化

##### 硬链接文件
- 参见代码：[stash.c:567-587](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L567-L587)
  ```c
  stash_dentry = d_find_any_alias(&info->vfs_inode);
  if (stash_dentry) {
      cache->path = dentry_path_raw(stash_dentry, cache->path_buf,
                                    PATH_MAX);
      dput(stash_dentry);
  }
  ```

**诱发原因**：
- 路径解析复杂
- 多个dentry引用同一inode
- 缓存管理复杂

### 1.3 提升测试效率的策略建议

根据以上分析，提升测试效率的策略按优先级排序：

#### 第一优先级：设计并实现新的故障注入方法

**当前Monarch的故障注入能力**：
- `syz_failure_crash_client`：客户端崩溃
- `syz_failure_crash_server`：服务器崩溃
- `syz_failure_sync`：同步点
- `syz_failure_send/recv`：消息同步
- 参见代码：[common_linux.h:114-259](file:///d:\科研\博士复现\原版备份\Monarch-master\src\executor\common_linux.h#L114-L259)

**建议增强**：

a) **基于并发操作的故障注入**
```c
// 通过生成并发操作测试用例来触发竞态条件
// 多个客户端同时访问同一文件，在关键时刻注入节点/网络故障
syz_failure_node_offline(node_id, mode)  // mode: graceful/abrupt
syz_failure_node_online(node_id, delay)
syz_failure_network_partition(node_group1, node_group2, duration)
```

b) **时机感知的故障注入**
```c
// 通过监控节点上下线频率，在stash/restore操作的关键时机注入故障
syz_failure_inject_at(node_id, timing, fault_type)
// 例如：在检测到频繁节点上下线时，在stash操作进行中注入网络分区
// timing: "during_stash", "during_restore", "node_frequently_offline"
```

c) **资源限制故障**
```c
// 通过限制系统资源来触发资源管理错误
syz_failure_disk_full(node_id, threshold)  // 模拟磁盘满
syz_failure_memory_pressure(node_id, level)  // 模拟内存压力
syz_failure_file_handle_exhaust(node_id, limit)  // 模拟文件描述符耗尽
```

d) **并发操作测试用例生成**
```c
// 生成并发操作测试用例，而不是直接注入并发故障
// 通过测试用例生成来触发并发场景
generate_concurrent_stash_test(file_path, num_clients, pattern)
generate_concurrent_restore_test(file_path, num_clients, pattern)
```

#### 第二优先级：优化种子生成和突变

a) **针对stash功能的种子生成**
- 生成大量文件打开/写入/关闭的序列
- 在关键位置插入节点离线/上线操作
- 生成并发访问同一文件的场景

b) **语义感知的突变**
- 保留系统调用序列的语义完整性
- 重点突变故障注入的时机和类型
- 突变文件操作的参数（文件大小、写入模式等）

#### 第三优先级：设计新的适应度指标

虽然应该从整体考虑，但针对stash功能可以设计：

a) **状态覆盖指标**
- 覆盖所有stash状态转换路径
- 覆盖所有锁的获取/释放组合

b) **并发场景覆盖**
- 记录并发访问的文件数量
- 记录并发执行的stash/restore操作数量

c) **故障场景覆盖**
- 覆盖不同的故障注入时机
- 覆盖不同的故障类型组合

#### 第四优先级：优化种子调度和优先级

- 优先调度触发stash状态的种子
- 优先调度在故障场景下成功的种子
- 优先调度覆盖新代码路径的种子

### 1.4 关键测试场景

基于以上分析，重点测试以下场景：

1. **节点在stash过程中崩溃**
2. **节点在恢复过程中崩溃**
3. **多个客户端同时访问同一文件时节点离线**
4. **节点频繁上下线**
5. **网络分区下的stash和恢复**
6. **大文件的stash和恢复**
7. **资源受限情况下的stash操作**
8. **长时间离线后的恢复**

---

## 二、针对hmdfs的故障注入方法设计

### 2.1 当前Monarch故障注入实现分析

#### 2.1.1 架构设计

当前Monarch采用了**严格的客户端-服务器分离架构**：

```go
// 从proc.go:152-156行可以看到
for idx := 0; idx < proc.fuzzer.config.ServNum; idx++ {
    // 为服务器生成空的子测试用例
    p, _ := proc.fuzzer.target.Generate(proc.rnd, 0, nil, nil, true, ...)
    ps = append(ps, p)
}
// 为客户端生成实际的测试用例
subTsNum := proc.fuzzer.config.FuzzingVMs - proc.fuzzer.config.ServNum
```

**关键数据结构**：
```go
// 从prog.go:15-18行
type Conn struct {
    From int  // 源节点
    To   int  // 目标节点
}

// 从types.go:26行（推断）
type SrvFailInfo struct {
    Srv       int    // 服务器节点ID
    PartNodes []int  // 被分区隔离的节点
}
```

#### 2.1.2 故障枚举策略

**节点崩溃枚举**：
```go
// 从proc.go:466-481行
func genNodeCombs(srvNum int) (combs [][]prog.SrvFailInfo) {
    for sub := 1; sub <= 1; sub++ {  // 目前只枚举1个服务器崩溃
        idxCombs := combin.Combinations(srvNum, sub)
        for _, c := range idxCombs {
            comb := make([]prog.SrvFailInfo, 0)
            for _, i := range c {
                comb = append(comb, prog.SrvFailInfo{i, nil})
            }
            combs = append(combs, comb)
        }
    }
    return combs
}
```

**网络分区枚举**：
```go
// 从proc.go:483-510行
func genEdgeCombs(srvNum int, cltNum int) (combs [][]prog.SrvFailInfo) {
    conns := make([]prog.Conn, 0)
    // 生成边：从服务器到其他节点
    for i := 0; i < srvNum; i++ {
        for j := i + 1; j < srvNum+cltNum; j++ {
            conns = append(conns, prog.Conn{i, j})
        }
    }

    // 随机选择1条边进行分区
    for sub := 1; sub <= 1; sub++ {
        for _, c := range combin.Combinations(len(conns), sub) {
            comb := make([]prog.SrvFailInfo, 0)
            for _, i := range c {
                if conns[i].From <= srvNum {
                    comb = updateComb(comb, conns[i].From, conns[i].To)
                } else if conns[i].To <= srvNum {
                    comb = updateComb(comb, conns[i].To, conns[i].From)
                }
            }
            combs = append(combs, comb)
        }
    }
    return combs
}
```

#### 2.1.3 故障注入实现

**网络命令生成**：
```go
// 从mutation.go:1066-1080行
func (ctx *mutator) genNetCmd(failInfo SrvFailInfo) []byte {
    bytes := strings.Split(ctx.initIp, ".")
    lastByte, _ := strconv.Atoi(bytes[3])
    inputChanStr := ""
    outputChanStr := ""

    // 使用iptables实现网络分区
    for _, node := range failInfo.PartNodes {
        inputChanStr += fmt.Sprintf("iptables -A INPUT -s %s.%s.%s.%d -j DROP;",
            bytes[0], bytes[1], bytes[2], lastByte+node)
        outputChanStr += fmt.Sprintf("iptables -A OUTPUT -d %s.%s.%s.%d -j DROP;",
            bytes[0], bytes[1], bytes[2], lastByte+node)
    }
    return []byte(inputChanStr + outputChanStr)
}
```

### 2.2 去中心化架构适配

**问题**：hmdfs是去中心化的，每个节点都可以同时充当服务器和客户端角色。

**解决方案**：重新设计节点角色模型

```go
// 新的节点角色定义
type NodeRole int

const (
    RoleUnknown NodeRole = iota
    RoleServer      // 服务器角色
    RoleClient      // 客户端角色
    RoleHybrid      // 混合角色（hmdfs节点）
)

type NodeInfo struct {
    ID          int
    Role        NodeRole
    Connections []int  // 连接的节点列表
    IsOnline    bool
    StashStatus StashState  // stash相关状态
}

type StashState int

const (
    StashNone StashState = iota
    Stashing
    Restoring
    StashComplete
)

// 新的拓扑结构
type ClusterTopology struct {
    Nodes       []NodeInfo
    Connections [][]Conn  // 全连接图
    IsDynamic   bool  // 是否动态拓扑
}
```

### 2.3 基于stash状态的定制化故障注入

**核心思想**：根据stash功能的状态机设计故障注入策略，但故障注入仍然在节点/网络级别

```go
// stash状态感知的故障注入器
type StashAwareFailureInjector struct {
    topology        *ClusterTopology
    stashStates     map[int]StashState
    failureHistory  []FailureRecord
    priorityWeights map[FailureType]float64
}

type FailureType int

const (
    FailureNodeCrash FailureType = iota
    FailureNetworkPartition
    FailureNetworkDelay
    FailureNodePause
    FailureDiskFull
    FailureMemoryPressure
)

type FailureRecord struct {
    Type        FailureType
    Nodes       []int
    Timestamp   time.Time
    StashStates map[int]StashState
    Result      FailureResult
}

type FailureResult int

const (
    ResultBugFound FailureResult = iota
    ResultCoverageIncrease
    ResultNoEffect
    ResultSystemUnstable
)

// 基于stash状态生成故障策略
func (inj *StashAwareFailureInjector) GenerateFailureStrategies() []FailureStrategy {
    strategies := make([]FailureStrategy, 0)

    // 策略1：在stash过程中注入节点崩溃
    for nodeID, state := range inj.stashStates {
        if state == Stashing {
            strategies = append(strategies, FailureStrategy{
                Type: FailureNodeCrash,
                Nodes: []int{nodeID},
                Timing: "during_stash",
                Priority: 0.9,  // 高优先级
                Description: "节点在stash过程中崩溃，触发文件状态不一致",
            })
        }
    }

    // 策略2：在restore过程中注入网络分区
    for nodeID, state := range inj.stashStates {
        if state == Restoring {
            connectedNodes := inj.topology.Nodes[nodeID].Connections
            if len(connectedNodes) > 1 {
                strategies = append(strategies, FailureStrategy{
                    Type: FailureNetworkPartition,
                    Nodes: []int{nodeID, connectedNodes[0]},
                    Timing: "during_restore",
                    Priority: 0.85,
                    Description: "节点在restore过程中与部分节点网络分区，触发恢复失败",
                })
            }
        }
    }

    // 策略3：多节点并发stash时注入故障
    stashingNodes := make([]int, 0)
    for nodeID, state := range inj.stashStates {
        if state == Stashing {
            stashingNodes = append(stashingNodes, nodeID)
        }
    }
    if len(stashingNodes) >= 2 {
        strategies = append(strategies, FailureStrategy{
            Type: FailureNetworkPartition,
            Nodes: stashingNodes[:2],  // 选择前两个正在stash的节点
            Timing: "concurrent_stash",
            Priority: 0.8,
            Description: "多个节点并发stash时网络分区，触发并发竞态条件",
        })
    }

    // 策略4：在stash过程中注入磁盘满
    for nodeID, state := range inj.stashStates {
        if state == Stashing {
            strategies = append(strategies, FailureStrategy{
                Type: FailureDiskFull,
                Nodes: []int{nodeID},
                Timing: "during_stash",
                Priority: 0.75,
                Description: "节点在stash过程中磁盘满，触发资源管理错误",
            })
        }
    }

    return strategies
}
```

### 2.4 动态拓扑感知的网络分区

**问题**：当前的网络分区是随机的，没有考虑实际的连接拓扑。

**解决方案**：基于实际连接关系生成网络分区

```go
// 拓扑感知的网络分区生成器
type TopologyAwarePartitionGenerator struct {
    topology *ClusterTopology
}

func (gen *TopologyAwarePartitionGenerator) GeneratePartitions() []PartitionStrategy {
    strategies := make([]PartitionStrategy, 0)

    // 策略1：割点分区（识别关键节点）
    articulationPoints := gen.findArticulationPoints()
    for _, node := range articulationPoints {
        strategies = append(strategies, PartitionStrategy{
            Type: "articulation_point",
            IsolatedNodes: []int{node},
            AffectedNodes: gen.getAffectedNodes(node),
            Description: fmt.Sprintf("隔离关键节点 %d", node),
            Priority: 0.9,
        })
    }

    // 策略2：边割分区（识别关键连接）
    bridges := gen.findBridges()
    for _, bridge := range bridges {
        strategies = append(strategies, PartitionStrategy{
            Type: "bridge_edge",
            IsolatedNodes: []int{bridge.From, bridge.To},
            AffectedNodes: gen.getAffectedNodesByEdge(bridge),
            Description: fmt.Sprintf("切断关键连接 %d-%d", bridge.From, bridge.To),
            Priority: 0.85,
        })
    }

    // 策略3：社区检测分区（隔离紧密连接的节点组）
    communities := gen.detectCommunities()
    for i := 0; i < len(communities); i++ {
        for j := i + 1; j < len(communities); j++ {
            strategies = append(strategies, PartitionStrategy{
                Type: "community_partition",
                IsolatedNodes: communities[i],
                AffectedNodes: append(communities[i], communities[j]...),
                Description: fmt.Sprintf("隔离社区 %d 和 %d", i, j),
                Priority: 0.7,
            })
        }
    }

    return strategies
}
```

### 2.5 时机感知的故障注入

**核心思想**：在stash操作的关键时机注入故障

```go
// 时机感知的故障注入器
type TimingAwareFailureInjector struct {
    stashMonitor *StashOperationMonitor
}

type StashOperationMonitor struct {
    operations map[int]*StashOperation  // 节点ID -> 操作
}

type StashOperation struct {
    NodeID      int
    Phase       StashPhase
    StartTime   time.Time
    FileCount   int
    Progress    float64  // 0.0 - 1.0
}

type StashPhase int

const (
    PhasePrepare StashPhase = iota
    PhaseFlushData
    PhaseFlushMetadata
    PhaseCloseFile
    PhaseComplete
)

// 在关键时机注入故障
func (inj *TimingAwareFailureInjector) InjectAtCriticalTiming() []FailureInjection {
    injections := make([]FailureInjection, 0)

    for nodeID, op := range inj.stashMonitor.operations {
        switch op.Phase {
        case PhaseFlushData:
            // 在数据刷新过程中注入网络分区
            if op.Progress > 0.3 && op.Progress < 0.7 {
                injections = append(injections, FailureInjection{
                    Type: FailureNetworkPartition,
                    Node: nodeID,
                    Timing: fmt.Sprintf("flush_data_%.0f", op.Progress*100),
                    Description: "在数据刷新30%-70%时网络分区",
                })
            }

        case PhaseFlushMetadata:
            // 在元数据刷新过程中注入节点崩溃
            injections = append(injections, FailureInjection{
                Type: FailureNodeCrash,
                Node: nodeID,
                Timing: "flush_metadata",
                Description: "在元数据刷新时节点崩溃",
            })

        case PhaseCloseFile:
            // 在文件关闭时注入磁盘满
            injections = append(injections, FailureInjection{
                Type: FailureDiskFull,
                Node: nodeID,
                Timing: "close_file",
                Description: "在文件关闭时磁盘满",
            })
        }
    }

    return injections
}
```

---

## 三、非侵入式Stash状态感知设计

### 3.1 设计思路

**核心思想**：通过分析测试用例中的文件操作模式来推断stash状态，结合节点上下线频率来模拟stash过程中的故障。

**优点**：
- ✅ 非侵入式，不需要修改内核代码
- ✅ 利用现有的测试用例信息，无需额外监控
- ✅ 实现简单，易于集成到现有框架
- ✅ 可以通过种子生成控制来间接控制状态

**潜在问题**：
- ⚠️ 推断的准确性依赖于测试用例的设计
- ⚠️ 难以精确控制故障注入的时机
- ⚠️ 无法感知实际的stash进度（如30%、70%等）

### 3.2 基于文件操作模式的状态推断

#### 3.2.1 文件操作分析器

```go
// 文件操作分析器
type FileOperationAnalyzer struct {
    operations map[int][]FileOp  // 节点ID -> 操作序列
    patterns   map[string]StashPattern
}

type FileOp struct {
    NodeID    int
    OpType    string  // "write", "fsync", "close", "open"
    FilePath   string
    Timestamp  int
    Size       int
}

type StashPattern struct {
    PatternName string
    Operations []string
    StashPhase StashPhase
    Probability float64
}

// 预定义的stash操作模式
var stashPatterns = []StashPattern{
    {
        PatternName: "normal_stash",
        Operations: []string{"open", "write", "write", "fsync", "close"},
        StashPhase: PhaseComplete,
        Probability: 0.4,
    },
    {
        PatternName: "interrupted_stash",
        Operations: []string{"open", "write", "fsync"},  // 未close
        StashPhase: PhaseFlushData,
        Probability: 0.3,
    },
    {
        PatternName: "concurrent_stash",
        Operations: []string{"open", "write", "open", "write", "fsync", "fsync"},
        StashPhase: PhaseFlushData,
        Probability: 0.2,
    },
    {
        PatternName: "restore_pattern",
        Operations: []string{"open", "read", "read", "close"},
        StashPhase: Restoring,
        Probability: 0.1,
    },
}

// 分析测试用例推断stash状态
func (analyzer *FileOperationAnalyzer) InferStashState(ps []*Prog) map[int]StashPhase {
    states := make(map[int]StashPhase)

    for nodeID, p := range ps {
        ops := analyzer.extractFileOps(p)
        pattern := analyzer.matchPattern(ops)
        if pattern != nil {
            states[nodeID] = pattern.StashPhase
        }
    }

    return states
}

// 提取文件操作
func (analyzer *FileOperationAnalyzer) extractFileOps(p *Prog) []FileOp {
    ops := make([]FileOp, 0)

    for _, call := range p.Calls {
        if call.Meta.Name == "write" || call.Meta.Name == "writev" ||
           call.Meta.Name == "pwrite64" || call.Meta.Name == "pwritev" {
            ops = append(ops, FileOp{
                OpType: "write",
                Size: analyzer.getWriteSize(call),
            })
        } else if call.Meta.Name == "fsync" || call.Meta.Name == "fdatasync" {
            ops = append(ops, FileOp{
                OpType: "fsync",
            })
        } else if call.Meta.Name == "close" {
            ops = append(ops, FileOp{
                OpType: "close",
            })
        } else if strings.HasPrefix(call.Meta.Name, "open") {
            ops = append(ops, FileOp{
                OpType: "open",
                FilePath: analyzer.getFilePath(call),
            })
        }
    }

    return ops
}

// 匹配操作模式
func (analyzer *FileOperationAnalyzer) matchPattern(ops []FileOp) *StashPattern {
    opTypes := make([]string, len(ops))
    for i, op := range ops {
        opTypes[i] = op.OpType
    }

    for _, pattern := range stashPatterns {
        if analyzer.matchPatternSequence(opTypes, pattern.Operations) {
            return &pattern
        }
    }

    return nil
}

// 模式序列匹配（支持模糊匹配）
func (analyzer *FileOperationAnalyzer) matchPatternSequence(ops []string, pattern []string) bool {
    if len(ops) < len(pattern) {
        return false
    }

    // 精确匹配
    if analyzer.equalSequences(ops[:len(pattern)], pattern) {
        return true
    }

    // 模糊匹配：允许插入额外操作
    return analyzer.fuzzyMatch(ops, pattern)
}

func (analyzer *FileOperationAnalyzer) equalSequences(a, b []string) bool {
    if len(a) != len(b) {
        return false
    }
    for i := range a {
        if a[i] != b[i] {
            return false
        }
    }
    return true
}

func (analyzer *FileOperationAnalyzer) fuzzyMatch(ops []string, pattern []string) bool {
    patternIdx := 0
    for _, op := range ops {
        if patternIdx < len(pattern) && op == pattern[patternIdx] {
            patternIdx++
        }
    }
    return patternIdx == len(pattern)
}
```

#### 3.2.2 基于节点角色的动态故障注入

```go
// 节点角色动态分配器
type NodeRoleAllocator struct {
    nodeRoles     map[int]NodeRole
    roleHistory   map[int][]RoleChange
    allocationStrategy string  // "static", "dynamic", "adaptive"
}

type RoleChange struct {
    From      NodeRole
    To        NodeRole
    Timestamp time.Time
    Reason    string
}

// 基于测试用例动态分配角色
func (alloc *NodeRoleAllocator) AllocateRoles(ps []*Prog) map[int]NodeRole {
    roles := make(map[int]NodeRole)

    // 分析每个节点的操作特征
    for nodeID, p := range ps {
        role := alloc.inferRole(p, nodeID)
        roles[nodeID] = role

        // 记录角色变化
        if oldRole, exists := alloc.nodeRoles[nodeID]; exists {
            alloc.roleHistory[nodeID] = append(alloc.roleHistory[nodeID], RoleChange{
                From: oldRole,
                To: role,
                Timestamp: time.Now(),
                Reason: "testcase_analysis",
            })
        }
        alloc.nodeRoles[nodeID] = role
    }

    return roles
}

// 推断节点角色
func (alloc *NodeRoleAllocator) inferRole(p *Prog, nodeID int) NodeRole {
    writeCount := 0
    readCount := 0
    syncCount := 0

    for _, call := range p.Calls {
        switch {
        case strings.Contains(call.Meta.Name, "write"):
            writeCount++
        case strings.Contains(call.Meta.Name, "read"):
            readCount++
        case call.Meta.Name == "fsync" || call.Meta.Name == "fdatasync":
            syncCount++
        }
    }

    // 基于操作比例推断角色
    totalOps := writeCount + readCount + syncCount
    if totalOps == 0 {
        return RoleUnknown
    }

    writeRatio := float64(writeCount) / float64(totalOps)
    syncRatio := float64(syncCount) / float64(totalOps)

    // hmdfs节点通常有较多的写和同步操作
    if writeRatio > 0.3 && syncRatio > 0.1 {
        return RoleHybrid  // 既做服务器也做客户端
    } else if writeRatio > 0.5 {
        return RoleServer  // 主要是写入，像服务器
    } else {
        return RoleClient  // 主要是读取，像客户端
    }
}

// 基于角色选择故障注入策略
func (alloc *NodeRoleAllocator) SelectFailureStrategy(nodeID int,
                                                  currentState StashPhase) FailureStrategy {
    role := alloc.nodeRoles[nodeID]

    switch role {
    case RoleHybrid:
        // 混合角色节点更关键，注入更复杂的故障
        if currentState == Stashing {
            return FailureStrategy{
                Type: FailureNetworkPartition,
                Nodes: alloc.getConnectedNodes(nodeID),
                Timing: "during_stash",
                Priority: 0.9,
                Description: "混合角色节点在stash时网络分区",
            }
        }
    case RoleServer:
        // 服务器节点崩溃影响更大
        return FailureStrategy{
            Type: FailureNodeCrash,
            Nodes: []int{nodeID},
            Timing: "during_operation",
            Priority: 0.85,
            Description: "服务器节点崩溃",
        }
    case RoleClient:
        // 客户端节点可以注入网络延迟
        return FailureStrategy{
            Type: FailureNetworkDelay,
            Nodes: []int{nodeID},
            Timing: "during_operation",
            Priority: 0.7,
            Description: "客户端节点网络延迟",
        }
    }

    return FailureStrategy{}
}
```

#### 3.2.3 基于种子构成的故障概率调整

```go
// 种子构成分析器
type SeedCompositionAnalyzer struct {
    compositionHistory []CompositionRecord
}

type CompositionRecord struct {
    SeedHash    string
    NodeOps     map[int]int  // 节点ID -> 操作数量
    WriteRatio  map[int]float64
    FailureRate float64
    BugFound    bool
}

// 分析种子构成并调整故障概率
func (analyzer *SeedCompositionAnalyzer) AdjustFailureProbability(ps []*Prog) map[int]float64 {
    probs := make(map[int]float64)

    for nodeID, p := range ps {
        composition := analyzer.analyzeComposition(p)
        prob := analyzer.calculateFailureProbability(composition)
        probs[nodeID] = prob
    }

    return probs
}

// 分析种子构成
func (analyzer *SeedCompositionAnalyzer) analyzeComposition(p *Prog) SeedComposition {
    totalOps := len(p.Calls)
    writeOps := 0
    syncOps := 0
    fileOps := 0

    for _, call := range p.Calls {
        if strings.Contains(call.Meta.Name, "write") {
            writeOps++
        } else if call.Meta.Name == "fsync" || call.Meta.Name == "fdatasync" {
            syncOps++
        } else if strings.Contains(call.Meta.Name, "open") ||
                strings.Contains(call.Meta.Name, "close") {
            fileOps++
        }
    }

    return SeedComposition{
        TotalOps: totalOps,
        WriteOps: writeOps,
        SyncOps: syncOps,
        FileOps: fileOps,
        WriteRatio: float64(writeOps) / float64(totalOps),
        SyncRatio: float64(syncOps) / float64(totalOps),
    }
}

type SeedComposition struct {
    TotalOps   int
    WriteOps   int
    SyncOps    int
    FileOps    int
    WriteRatio float64
    SyncRatio  float64
}

// 计算故障概率
func (analyzer *SeedCompositionAnalyzer) calculateFailureProbability(comp SeedComposition) float64 {
    baseProb := 0.1  // 基础故障概率

    // 写操作多，更容易触发stash，增加故障概率
    if comp.WriteRatio > 0.5 {
        baseProb += 0.2
    }

    // 同步操作多，说明正在进行持久化，增加故障概率
    if comp.SyncRatio > 0.2 {
        baseProb += 0.15
    }

    // 文件操作多，说明活跃度高，增加故障概率
    if comp.FileOps > 10 {
        baseProb += 0.1
    }

    // 限制最大概率
    if baseProb > 0.8 {
        baseProb = 0.8
    }

    return baseProb
}

// 在种子生成时集成故障概率
func (r *randGen) generateWithFailureAwareness(target *Target, ncalls int,
                                          ct *ChoiceTable, corpus [][]*Prog,
                                          sCalls *SpecialCalls, srvNum int,
                                          enableC2san bool, hmcfg *Hmdfs_config,
                                          idx int) *Prog {
    p := r.generate(target, ncalls, ct, corpus, sCalls, enableC2san, hmcfg, idx)

    // 分析种子构成
    analyzer := &SeedCompositionAnalyzer{}
    failureProbs := analyzer.AdjustFailureProbability([]*Prog{p})

    // 根据故障概率决定是否插入故障
    if failureProbs[idx] > r.Float64() {
        // 插入故障
        r.insertFailureBasedOnComposition(p, idx, failureProbs[idx])
    }

    return p
}
```

---

## 四、基于网络流量的拓扑感知和故障注入

### 4.1 设计思路

**核心思想**：使用实际网络流量来分析连接和节点状态，结合种子构成来预测流量分布。

**优点**：
- ✅ 动态反映实际连接状态
- ✅ 可以识别关键节点（接收请求多的节点）
- ✅ 与种子生成相结合，实现闭环优化
- ✅ 不依赖静态拓扑配置

**潜在问题**：
- ⚠️ 需要额外的流量监控机制
- ⚠️ 流量分析可能增加系统开销
- ⚠️ 预测准确性依赖于种子质量

### 4.2 轻量级流量监控

```go
// 流量监控器（非侵入式）
type TrafficMonitor struct {
    trafficData  map[int]*NodeTraffic
    windowSize  time.Duration
    updateChan chan TrafficUpdate
}

type NodeTraffic struct {
    NodeID          int
    InboundBytes    uint64
    OutboundBytes   uint64
    RequestCount    uint64
    ResponseCount   uint64
    ActiveConnections []int
    LastUpdate     time.Time
    ImportanceScore float64
}

type TrafficUpdate struct {
    SrcNode  int
    DstNode  int
    Bytes    uint64
    IsRequest bool
    Timestamp time.Time
}

// 计算节点重要性分数
func (monitor *TrafficMonitor) CalculateImportanceScores() map[int]float64 {
    scores := make(map[int]float64)

    maxInbound := uint64(0)
    maxRequests := uint64(0)

    // 找到最大值用于归一化
    for _, traffic := range monitor.trafficData {
        if traffic.InboundBytes > maxInbound {
            maxInbound = traffic.InboundBytes
        }
        if traffic.RequestCount > maxRequests {
            maxRequests = traffic.RequestCount
        }
    }

    // 计算重要性分数
    for nodeID, traffic := range monitor.trafficData {
        inboundScore := 0.0
        if maxInbound > 0 {
            inboundScore = float64(traffic.InboundBytes) / float64(maxInbound)
        }

        requestScore := 0.0
        if maxRequests > 0 {
            requestScore = float64(traffic.RequestCount) / float64(maxRequests)
        }

        connectionScore := float64(len(traffic.ActiveConnections)) / 10.0  // 假设最多10个连接

        // 加权计算
        scores[nodeID] = 0.5*inboundScore + 0.3*requestScore + 0.2*connectionScore
    }

    return scores
}

// 基于重要性选择故障目标
func (monitor *TrafficMonitor) SelectFailureTargets(count int) []int {
    scores := monitor.CalculateImportanceScores()

    // 按重要性排序
    type NodeScore struct {
        NodeID int
        Score  float64
    }

    nodeScores := make([]NodeScore, 0)
    for nodeID, score := range scores {
        nodeScores = append(nodeScores, NodeScore{nodeID, score})
    }

    sort.Slice(nodeScores, func(i, j int) bool {
        return nodeScores[i].Score > nodeScores[j].Score
    })

    // 选择top-k节点
    targets := make([]int, 0)
    for i := 0; i < count && i < len(nodeScores); i++ {
        targets = append(targets, nodeScores[i].NodeID)
    }

    return targets
}
```

### 4.3 基于种子构成的流量预测

```go
// 流量预测器
type TrafficPredictor struct {
    predictionModel map[string]TrafficPattern
    history        []PredictionRecord
}

type TrafficPattern struct {
    SeedFeatures    SeedFeatures
    ExpectedTraffic map[int]TrafficExpectation
}

type SeedFeatures struct {
    NodeCount       int
    WriteIntensity  map[int]float64  // 节点ID -> 写入强度
    SyncFrequency   map[int]float64  // 节点ID -> 同步频率
    Concurrency     int              // 并发操作数
}

type TrafficExpectation struct {
    InboundRate   float64  // 预期入站流量率
    OutboundRate  float64  // 预期出站流量率
    ConnectionCount int      // 预期连接数
}

type PredictionRecord struct {
    SeedHash       string
    Predicted      map[int]TrafficExpectation
    Actual         map[int]NodeTraffic
    Accuracy       float64
}

// 预测流量分布
func (predictor *TrafficPredictor) PredictTraffic(ps []*Prog) map[int]TrafficExpectation {
    features := predictor.extractFeatures(ps)

    // 查找相似的历史模式
    similarPatterns := predictor.findSimilarPatterns(features)

    if len(similarPatterns) == 0 {
        // 没有相似模式，使用默认预测
        return predictor.defaultPrediction(features)
    }

    // 基于相似模式聚合预测
    return predictor.aggregatePredictions(similarPatterns)
}

// 提取种子特征
func (predictor *TrafficPredictor) extractFeatures(ps []*Prog) SeedFeatures {
    features := SeedFeatures{
        NodeCount: len(ps),
        WriteIntensity: make(map[int]float64),
        SyncFrequency: make(map[int]float64),
        Concurrency: 0,
    }

    for nodeID, p := range ps {
        writeCount := 0
        syncCount := 0
        totalOps := len(p.Calls)

        for _, call := range p.Calls {
            if strings.Contains(call.Meta.Name, "write") {
                writeCount++
            } else if call.Meta.Name == "fsync" || call.Meta.Name == "fdatasync" {
                syncCount++
            }
        }

        if totalOps > 0 {
            features.WriteIntensity[nodeID] = float64(writeCount) / float64(totalOps)
            features.SyncFrequency[nodeID] = float64(syncCount) / float64(totalOps)
        }

        // 估算并发度
        if writeCount > 0 && syncCount > 0 {
            features.Concurrency++
        }
    }

    return features
}

// 查找相似模式
func (predictor *TrafficPredictor) findSimilarPatterns(features SeedFeatures) []TrafficPattern {
    similar := make([]TrafficPattern, 0)

    for _, pattern := range predictor.predictionModel {
        similarity := predictor.calculateSimilarity(features, pattern.SeedFeatures)
        if similarity > 0.7 {  // 相似度阈值
            similar = append(similar, pattern)
        }
    }

    return similar
}

// 计算特征相似度
func (predictor *TrafficPredictor) calculateSimilarity(a, b SeedFeatures) float64 {
    // 节点数量相似度
    nodeCountSim := 1.0 - math.Abs(float64(a.NodeCount-b.NodeCount))/float64(max(a.NodeCount, b.NodeCount))

    // 写入强度相似度
    writeIntensitySim := 0.0
    commonNodes := 0
    for nodeID := range a.WriteIntensity {
        if _, exists := b.WriteIntensity[nodeID]; exists {
            diff := math.Abs(a.WriteIntensity[nodeID] - b.WriteIntensity[nodeID])
            writeIntensitySim += 1.0 - diff
            commonNodes++
        }
    }
    if commonNodes > 0 {
        writeIntensitySim /= float64(commonNodes)
    }

    // 同步频率相似度
    syncFreqSim := 0.0
    commonNodes = 0
    for nodeID := range a.SyncFrequency {
        if _, exists := b.SyncFrequency[nodeID]; exists {
            diff := math.Abs(a.SyncFrequency[nodeID] - b.SyncFrequency[nodeID])
            syncFreqSim += 1.0 - diff
            commonNodes++
        }
    }
    if commonNodes > 0 {
        syncFreqSim /= float64(commonNodes)
    }

    // 加权综合相似度
    return 0.4*nodeCountSim + 0.3*writeIntensitySim + 0.3*syncFreqSim
}

// 聚合预测结果
func (predictor *TrafficPredictor) aggregatePredictions(patterns []TrafficPattern) map[int]TrafficExpectation {
    aggregated := make(map[int]TrafficExpectation)

    for _, pattern := range patterns {
        for nodeID, expect := range pattern.ExpectedTraffic {
            if _, exists := aggregated[nodeID]; !exists {
                aggregated[nodeID] = expect
            } else {
                // 平均聚合
                aggregated[nodeID].InboundRate = (aggregated[nodeID].InboundRate + expect.InboundRate) / 2.0
                aggregated[nodeID].OutboundRate = (aggregated[nodeID].OutboundRate + expect.OutboundRate) / 2.0
                aggregated[nodeID].ConnectionCount = (aggregated[nodeID].ConnectionCount + expect.ConnectionCount) / 2
            }
        }
    }

    return aggregated
}

// 默认预测（当没有相似模式时）
func (predictor *TrafficPredictor) defaultPrediction(features SeedFeatures) map[int]TrafficExpectation {
    expectations := make(map[int]TrafficExpectation)

    for nodeID := range features.WriteIntensity {
        // 基于写入强度预测流量
        writeIntensity := features.WriteIntensity[nodeID]
        syncFreq := features.SyncFrequency[nodeID]

        expectations[nodeID] = TrafficExpectation{
            InboundRate: writeIntensity * 1000.0,  // 假设基准流量
            OutboundRate: writeIntensity * 800.0,
            ConnectionCount: int(writeIntensity * 5.0),
        }
    }

    return expectations
}
```

### 4.4 流量感知的故障注入

```go
// 流量感知的故障注入器
type TrafficAwareFailureInjector struct {
    monitor       *TrafficMonitor
    predictor     *TrafficPredictor
    failurePolicy FailurePolicy
}

type FailurePolicy struct {
    PreferImportantNodes bool
    PreferHighTraffic    bool
    BalanceLoad        bool
}

// 基于流量状态选择故障策略
func (inj *TrafficAwareFailureInjector) SelectFailureStrategy(ps []*Prog) FailureStrategy {
    // 预测流量
    predictedTraffic := inj.predictor.PredictTraffic(ps)

    // 获取实际流量
    actualTraffic := inj.monitor.trafficData

    // 计算流量偏差
    deviations := inj.calculateTrafficDeviations(predictedTraffic, actualTraffic)

    // 选择故障目标
    targetNodes := inj.selectTargetsBasedOnTraffic(deviations, predictedTraffic)

    // 生成故障策略
    return inj.generateStrategy(targetNodes, deviations)
}

// 计算流量偏差
func (inj *TrafficAwareFailureInjector) calculateTrafficDeviations(predicted, actual map[int]TrafficExpectation) map[int]float64 {
    deviations := make(map[int]float64)

    for nodeID, pred := range predicted {
        if actual, exists := actual[nodeID]; exists {
            // 计算实际流量与预测流量的偏差
            actualRate := float64(actual.InboundBytes + actual.OutboundBytes)
            predictedRate := pred.InboundRate + pred.OutboundRate

            if predictedRate > 0 {
                deviation := math.Abs(actualRate-predictedRate) / predictedRate
                deviations[nodeID] = deviation
            }
        }
    }

    return deviations
}

// 基于流量偏差选择目标
func (inj *TrafficAwareFailureInjector) selectTargetsBasedOnTraffic(deviations map[int]float64,
                                                              predicted map[int]TrafficExpectation) []int {
    targets := make([]int, 0)

    // 策略1：选择偏差大的节点（异常流量）
    if inj.failurePolicy.PreferHighTraffic {
        type NodeDeviation struct {
            NodeID    int
            Deviation float64
        }

        nodeDeviations := make([]NodeDeviation, 0)
        for nodeID, deviation := range deviations {
            nodeDeviations = append(nodeDeviations, NodeDeviation{nodeID, deviation})
        }

        sort.Slice(nodeDeviations, func(i, j int) bool {
            return nodeDeviations[i].Deviation > nodeDeviations[j].Deviation
        })

        // 选择偏差最大的节点
        for i := 0; i < 2 && i < len(nodeDeviations); i++ {
            targets = append(targets, nodeDeviations[i].NodeID)
        }
    }

    // 策略2：选择重要性高的节点
    if inj.failurePolicy.PreferImportantNodes {
        importanceScores := inj.monitor.CalculateImportanceScores()

        type NodeScore struct {
            NodeID int
            Score  float64
        }

        nodeScores := make([]NodeScore, 0)
        for nodeID, score := range importanceScores {
            nodeScores = append(nodeScores, NodeScore{nodeID, score})
        }

        sort.Slice(nodeScores, func(i, j int) bool {
            return nodeScores[i].Score > nodeScores[j].Score
        })

        // 选择重要性最高的节点
        for i := 0; i < 2 && i < len(nodeScores); i++ {
            if !contains(targets, nodeScores[i].NodeID) {
                targets = append(targets, nodeScores[i].NodeID)
            }
        }
    }

    return targets
}

// 生成故障策略
func (inj *TrafficAwareFailureInjector) generateStrategy(targetNodes []int,
                                                          deviations map[int]float64) FailureStrategy {
    if len(targetNodes) == 0 {
        return FailureStrategy{}
    }

    // 根据偏差类型选择故障类型
    avgDeviation := 0.0
    for _, deviation := range deviations {
        avgDeviation += deviation
    }
    avgDeviation /= float64(len(deviations))

    var failureType FailureType
    if avgDeviation > 0.5 {
        // 流量偏差大，可能是网络问题
        failureType = FailureNetworkPartition
    } else {
        // 流量正常，可以注入节点崩溃
        failureType = FailureNodeCrash
    }

    return FailureStrategy{
        Type: failureType,
        Nodes: targetNodes,
        Timing: "based_on_traffic_analysis",
        Priority: 0.8,
        Description: fmt.Sprintf("基于流量分析的故障注入，目标节点: %v", targetNodes),
    }
}
```

---

## 五、综合设计方案

### 5.1 种子-流量-故障闭环系统

```go
// 综合故障注入系统
type IntegratedFailureSystem struct {
    seedAnalyzer      *FileOperationAnalyzer
    trafficMonitor    *TrafficMonitor
    trafficPredictor *TrafficPredictor
    failureInjector  *TrafficAwareFailureInjector
    roleAllocator    *NodeRoleAllocator
}

// 主决策函数
func (sys *IntegratedFailureSystem) DecideFailureInjection(ps []*Prog) FailureStrategy {
    // 步骤1：分析种子构成，推断stash状态
    stashStates := sys.seedAnalyzer.InferStashState(ps)

    // 步骤2：分析节点角色
    nodeRoles := sys.roleAllocator.AllocateRoles(ps)

    // 步骤3：预测流量分布
    predictedTraffic := sys.trafficPredictor.PredictTraffic(ps)

    // 步骤4：获取实际流量
    actualTraffic := sys.trafficMonitor.trafficData

    // 步骤5：综合决策
    strategy := sys.makeIntegratedDecision(stashStates, nodeRoles,
                                       predictedTraffic, actualTraffic)

    return strategy
}

// 综合决策
func (sys *IntegratedFailureSystem) makeIntegratedDecision(stashStates map[int]StashPhase,
                                                          nodeRoles map[int]NodeRole,
                                                          predictedTraffic map[int]TrafficExpectation,
                                                          actualTraffic map[int]*NodeTraffic) FailureStrategy {
    candidates := make([]FailureStrategy, 0)

    // 候选策略1：基于stash状态
    for nodeID, state := range stashStates {
        if state == Stashing || state == Restoring {
            candidates = append(candidates, FailureStrategy{
                Type: FailureNodeCrash,
                Nodes: []int{nodeID},
                Timing: fmt.Sprintf("during_%s", state),
                Priority: 0.9,
                Source: "stash_state",
            })
        }
    }

    // 候选策略2：基于节点角色
    for nodeID, role := range nodeRoles {
        if role == RoleHybrid {
            candidates = append(candidates, FailureStrategy{
                Type: FailureNetworkPartition,
                Nodes: []int{nodeID},
                Timing: "role_based",
                Priority: 0.85,
                Source: "node_role",
            })
        }
    }

    // 候选策略3：基于流量分析
    deviations := sys.calculateDeviations(predictedTraffic, actualTraffic)
    for nodeID, deviation := range deviations {
        if deviation > 0.6 {
            candidates = append(candidates, FailureStrategy{
                Type: FailureNetworkPartition,
                Nodes: []int{nodeID},
                Timing: "traffic_anomaly",
                Priority: 0.8 + deviation*0.2,
                Source: "traffic_analysis",
            })
        }
    }

    // 选择最优策略
    return sys.selectBestStrategy(candidates)
}

// 选择最优策略
func (sys *IntegratedFailureSystem) selectBestStrategy(candidates []FailureStrategy) FailureStrategy {
    if len(candidates) == 0 {
        return FailureStrategy{}
    }

    // 按优先级排序
    sort.Slice(candidates, func(i, j int) bool {
        return candidates[i].Priority > candidates[j].Priority
    })

    // 返回最高优先级的策略
    return candidates[0]
}
```

---

## 六、实施建议

### 6.1 阶段规划

#### 阶段1：基础架构改造（1-2周）
1. 修改节点角色模型，支持去中心化架构
2. 实现动态拓扑感知功能
3. 扩展故障注入接口

#### 阶段2：非侵入式状态感知（2-3周）
1. 实现文件操作分析器
2. 实现节点角色推断
3. 开发操作模式匹配算法

#### 阶段3：流量监控和预测（2-3周）
1. 实现轻量级流量监控
2. 开发流量预测模型
3. 收集训练数据

#### 阶段4：综合决策系统（2-3周）
1. 实现综合故障注入系统
2. 开发决策算法
3. 集成各个模块

#### 阶段5：测试和优化（2-3周）
1. 在hmdfs上进行测试
2. 调优参数和阈值
3. 性能优化

### 6.2 关键参数配置

#### 文件操作分析参数
```go
const (
    // 操作模式匹配参数
    PatternMatchThreshold = 0.7  // 模式匹配相似度阈值
    FuzzyMatchEnabled = true     // 是否启用模糊匹配

    // 节点角色推断参数
    ServerWriteRatioThreshold = 0.5    // 服务器写入比例阈值
    HybridWriteRatioThreshold = 0.3    // 混合角色写入比例阈值
    HybridSyncRatioThreshold = 0.1      // 混合角色同步比例阈值
)
```

#### 流量监控参数
```go
const (
    // 流量监控参数
    TrafficWindowSize = 5 * time.Minute  // 流量统计窗口大小
    UpdateInterval = 1 * time.Second     // 更新间隔

    // 重要性计算参数
    InboundWeight = 0.5    // 入站流量权重
    RequestWeight = 0.3     // 请求权重
    ConnectionWeight = 0.2   // 连接权重
)
```

#### 故障注入参数
```go
const (
    // 故障概率参数
    BaseFailureProbability = 0.1        // 基础故障概率
    WriteIntensityBonus = 0.2          // 写入强度加成
    SyncFrequencyBonus = 0.15          // 同步频率加成
    FileActivityBonus = 0.1            // 文件活跃度加成
    MaxFailureProbability = 0.8          // 最大故障概率

    // 故障选择参数
    DeviationThreshold = 0.5            // 流量偏差阈值
    ImportanceThreshold = 0.7           // 重要性阈值
    SimilarityThreshold = 0.7           // 相似度阈值
)
```

---

## 七、总结

### 7.1 核心设计原则

1. **非侵入式设计**：尽量不修改现有Monarch框架和Linux内核代码
2. **状态感知**：通过分析测试用例和网络流量来推断系统状态
3. **智能决策**：基于多维度信息（状态、角色、流量）进行故障注入决策
4. **闭环优化**：将故障注入效果反馈到种子生成和故障选择中
5. **可扩展性**：设计支持多种故障类型和注入策略

### 7.2 关键创新点

1. **基于文件操作模式的状态推断**：通过分析测试用例中的文件操作序列来推断stash状态
2. **基于种子构成的流量预测**：利用测试用例的特征来预测网络流量分布
3. **多维度综合决策**：结合状态、角色、流量等多个维度进行故障注入决策
4. **动态拓扑感知**：根据实际连接关系和流量模式来生成网络分区策略

### 7.3 预期效果

1. **提高bug发现率**：通过状态感知和智能故障选择，更容易触发stash功能中的并发错误和内存错误
2. **提升测试效率**：通过流量预测和闭环优化，减少无效的故障注入
3. **增强测试覆盖**：通过多维度分析，覆盖更多的测试场景和状态组合
4. **降低实现复杂度**：通过非侵入式设计，减少对现有框架的修改

---

## 八、附录

### 8.1 关键代码位置索引

#### Stash功能核心代码
- stash状态管理：[stash.c:626-680](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L626-L680)
- 并发控制：[stash.c:724-751](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L724-L751)
- 内存管理：[stash.c:538-609](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L538-L609)
- 恢复逻辑：[stash.c:1461-1515](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\stash.c#L1461-L1515)

#### Monarch故障注入代码
- 故障枚举：[proc.go:466-551](file:///d:\科研\博士复现\原版备份\Monarch-master\src\syz-fuzzer\proc.go#L466-L551)
- 故障注入：[mutation.go:1021-1111](file:///d:\科研\博士复现\原版备份\Monarch-master\src\prog\mutation.go#L1021-L1111)
- 执行接口：[common_linux.h:114-259](file:///d:\科研\博士复现\原版备份\Monarch-master\src\executor\common_linux.h#L114-L259)

### 8.2 术语表

| 术语 | 英文 | 说明 |
|------|------|------|
| Stash | Stash | hmdfs的离线缓存功能 |
| 并发错误 | Concurrency Errors | 多线程/多进程并发访问导致的错误 |
| 竞态条件 | Race Conditions | 多个线程同时访问共享资源导致的错误 |
| 死锁 | Deadlocks | 多个线程互相等待对方释放锁导致的错误 |
| 数据竞争 | Data Races | 未同步的并发访问导致的错误 |
| 内存错误 | Memory Errors | 内存管理相关的错误 |
| Use-after-free | Use-after-free | 使用已释放的内存 |
| 内存泄漏 | Memory Leaks | 分配的内存未释放 |
| Double-free | Double-free | 同一内存被释放多次 |
| 空指针解引用 | Null Pointer Dereference | 解引用空指针 |
| 语义错误 | Semantic Errors | 逻辑和状态相关的错误 |
| 数据不一致 | Data Inconsistency | 数据在不同位置不一致 |
| 状态机错误 | State Machine Errors | 状态转换逻辑错误 |
| 恢复失败 | Recovery Failures | 恢复操作失败 |
| 故障注入 | Fault Injection | 主动注入故障来测试系统 |
| 网络分区 | Network Partition | 网络连接被切断 |
| 节点崩溃 | Node Crash | 节点突然停止运行 |
| 流量监控 | Traffic Monitoring | 监控网络流量 |
| 状态感知 | State Awareness | 感知系统当前状态 |
| 拓扑感知 | Topology Awareness | 感知网络拓扑结构 |
| 种子生成 | Seed Generation | 生成测试用例 |
| 突变 | Mutation | 修改测试用例 |
| 适应度指标 | Fitness Metrics | 评估测试用例质量的指标 |

### 8.3 参考配置文件

当前Monarch使用的配置文件位于`fuzz-config`目录下，可以参考：
- [fuzz-config/all-config/cephfs/1-2-2-2/cephfs-normal.cfg](file:///d:\科研\博士复现\原版备份\Monarch-master\fuzz-config\all-config\cephfs\1-2-2-2\cephfs-normal.cfg)
- [fuzz-config/all-config/cephfs/1-2-2-2/cephfs-failure.cfg](file:///d:\科研\博士复现\原版备份\Monarch-master\fuzz-config\all-config\cephfs\1-2-2-2\cephfs-failure.cfg)

配置文件格式说明参见：[fuzz-config/README.md](file:///d:\科研\博士复现\原版备份\Monarch-master\fuzz-config\README.md)

---

**文档版本**：1.0
**创建日期**：2026-03-06
**最后更新**：2026-03-06
**维护者**：HMDFS模糊测试项目组
