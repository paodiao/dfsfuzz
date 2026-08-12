# HMDFS 分布式堆叠文件系统架构与特性详解

## 一、系统概述

### 1.1 什么是HMDFS

HMDFS（Huawei Multi-Device File System）是一种**分布式堆叠文件系统**，基于Linux VFS的stackable filesystem架构实现。它允许多个设备节点在保持各自本地存储独立性的同时，通过虚拟文件系统层实现跨设备的文件共享与同步。

### 1.2 分布式文件系统理论基础

HMDFS的设计遵循分布式文件系统的核心理论：

#### 最终一致性模型（Eventual Consistency）

HMDFS采用**最终一致性**模型，这是分布式系统在CAP定理中权衡AP（可用性+分区容忍性）的典型选择：

- **Availability（可用性）**：即使在部分节点离线时，本地节点仍可进行读写操作
- **Partition Tolerance（分区容忍性）**：网络分区时系统继续运行
- **牺牲强一致性**：不保证所有节点在任何时刻看到相同的数据，但保证经过足够时间后所有节点数据趋于一致

```
客户端写入 → 本地持久化 → 异步传播 → 远程节点接收 → 最终一致
   (t₀)         (t₁)        (t₂)        (t₃)          (tₙ)
```

#### 异步复制与写回

HMDFS使用**异步复制**机制实现数据传播：
- 写操作首先在本地的底层文件系统上完成（保证本地持久性）
- 然后通过后台写回线程异步传播到远程节点
- 这种设计提高了本地操作的性能，但引入了数据不一致窗口

#### 分布式元数据管理

每个节点维护自己的元数据副本，通过以下机制保持近似一致：
- **Dentry Cache超时机制**：缓存条目在超时后失效，强制重新获取
- **版本号机制**：通过version字段检测元数据变化
- **Syncfs同步点**：提供显式的一致性同步点

---

## 二、架构设计

### 2.1 整体架构

```
┌──────────────────────────────────────────────────────────────┐
│                      用户空间应用                              │
├──────────────────────────────────────────────────────────────┤
│                      Linux VFS                                │
├──────────────────────────────────────────────────────────────┤
│                    HMDFS 堆叠层                                │
│  ┌─────────────────┐  ┌─────────────────┐                   │
│  │  Device View    │  │   Merge View    │                   │
│  │  (设备视图)      │  │   (合并视图)     │                   │
│  │                 │  │                 │                   │
│  │ ┌─────┐┌──────┐ │  │ ┌─────────────┐ │                   │
│  │ │local││remote│ │  │ │统一命名空间  │ │                   │
│  │ │(本地││(远程 │ │  │ │(所有节点文件 │ │                   │
│  │ │文件)││文件) │ │  │ │  合并)      │ │                   │
│  │ └─────┘└──────┘ │  │ └─────────────┘ │                   │
│  └─────────────────┘  └─────────────────┘                   │
├──────────────────────────────────────────────────────────────┤
│                  本地底层文件系统                                │
│                  (ext4/f2fs/btrfs)                             │
├──────────────────────────────────────────────────────────────┤
│                     存储设备                                    │
└──────────────────────────────────────────────────────────────┘

                    ↕ 网络通信层 ↕
                    
              其他节点的HMDFS实例
```

### 2.2 堆叠文件系统概念

HMDFS属于**堆叠文件系统（Stackable Filesystem）**，这是Linux VFS提供的一种架构模式：

- **堆叠在现有文件系统之上**：不直接管理存储设备，而是包装另一个文件系统
- **透明拦截**：拦截VFS层的所有操作，可以添加额外逻辑后再传递给下层
- **双重路径**：每个HMDFS的dentry/inode都对应一个底层文件系统的dentry/inode

### 2.3 核心数据结构

#### 超级块信息（hmdfs_sb_info）

```c
struct hmdfs_sb_info {
    // ========== 路径配置 ==========
    char *local_dst;    // 本地文件存储路径（对应device_view/local）
    char *real_dst;     // 实际本地路径
    char *local_src;    // 本地源路径
    char *cache_dir;    // 缓存目录
    char *cloud_dir;    // 云端目录

    // ========== 节点连接管理 ==========
    struct {
        struct mutex node_lock;        // 节点列表保护锁
        struct list_head node_list;    // 所有连接节点的列表
        atomic_t conn_seq;             // 连接序列号
        unsigned long recent_ol;       // 最近在线时间
    } connections;

    // ========== 缓存控制 ==========
    unsigned int write_cache_timeout;  // 写缓存超时（秒），默认30
    unsigned int dcache_timeout;       // 目录项缓存超时（秒），默认30
    unsigned int dcache_precision;     // 缓存精度
    unsigned long dcache_threshold;    // 缓存阈值
    struct list_head client_cache;     // 客户端缓存条目列表
    struct list_head server_cache;     // 服务端缓存条目列表

    // ========== 写回机制 ==========
    struct hmdfs_writeback *h_wb;           // 客户端写回控制器
    struct hmdfs_server_writeback *h_swb;   // 服务端写回控制器
    unsigned int wb_timeout_ms;             // 写回超时（毫秒），默认60000

    // ========== 功能开关 ==========
    unsigned int s_merge_switch;      // 是否启用合并视图
    unsigned int s_cloud_disk_switch; // 是否启用云盘
    bool s_case_sensitive;            // 路径是否大小写敏感
    bool s_offline_stash;             // 是否启用离线暂存
    bool s_dentry_cache;              // 是否启用dentry缓存

    // ========== 同步控制 ==========
    struct hmdfs_syncfs_info hsi;     // 跨节点同步信息

    // ========== 启动检测 ==========
    uint64_t boot_cookie;             // 启动cookie，用于检测节点重启
};
```

#### Inode信息（hmdfs_inode_info）

```c
struct hmdfs_inode_info {
    // ========== 合并视图特有 ==========
    struct hmdfs_dentry_info_merge *merge_info;  // 合并视图目录项信息
    struct rb_root file_root;                    // 文件红黑树（用于快速查找comrade）
    
    // ========== 远程连接 ==========
    struct hmdfs_peer *conn;                     // 关联的远程节点连接
    int inode_type;                              // inode类型（本地/远程/合并）
    
    // ========== 版本控制 ==========
    u64 version;                                 // 元数据版本号
    int write_cnt;                               // 写入计数器
};
```

#### 目录项信息（合并视图）

```c
struct hmdfs_dentry_info_merge {
    unsigned long ctime;                // 创建时间
    int type;                           // 类型
    int work_count;                     // 异步查找工作计数
    struct mutex work_lock;             // 工作锁
    wait_queue_head_t wait_queue;       // 等待队列（等待异步查找完成）
    __u8 dentry_type;                   // 目录项类型
    struct mutex comrade_list_lock;     // 伙伴列表锁
    struct list_head comrade_list;      // 伙伴列表（核心！）
};

// 伙伴结构 - 表示同一个文件在不同节点上的对应项
struct hmdfs_dentry_comrade {
    uint64_t dev_id;            // 设备ID（HMDFS_DEVID_LOCAL表示本地）
    struct dentry *lo_d;        // 底层dentry（指向实际文件系统中的dentry）
    struct list_head list;      // 链表节点
};
```

**关键概念 - Comrade（伙伴）**：

在合并视图中，一个虚拟的dentry可以对应多个节点上的实际文件。每个对应项称为一个"comrade"。这是HMDFS实现多节点文件统一视图的核心机制。

```
merge_view/document.txt
├── comrade[dev_id=LOCAL] → /data/local/node1/document.txt
├── comrade[dev_id=1001] → /data/local/node1001/document.txt
└── comrade[dev_id=1002] → /data/local/node1002/document.txt
```

---

## 三、视图机制详解

### 3.1 设备视图（Device View）

设备视图提供按节点组织的文件访问方式，包含两个主要目录：

#### 目录结构

```
device_view/
├── local/          # 本地节点的文件
│   ├── file1.txt
│   ├── file2.txt
│   └── dir1/
│       └── file3.txt
│
└── remote/         # 远程节点的文件
    ├── node_1001/  # 节点1001的文件
    │   ├── fileA.txt
    │   └── dirA/
    │       └── fileB.txt
    │
    └── node_1002/  # 节点1002的文件
        ├── fileX.txt
        └── dirX/
            └── fileY.txt
```

#### 设备视图的设计意义

1. **明确文件归属**：可以清楚看到哪些文件是本地的，哪些是远程的
2. **隔离访问**：可以单独访问某个节点的文件
3. **调试友好**：便于诊断跨节点问题

#### 设备视图的操作特点

- **local目录**：直接操作本地底层文件系统
- **remote目录**：通过网络访问远程节点的文件
- **无合并逻辑**：不涉及文件合并，只是简单的分类展示

### 3.2 合并视图（Merge View）

合并视图将所有节点的文件统一展示在一个命名空间下。

#### 目录结构

```
merge_view/
├── file1.txt       # 可能存在于多个节点
├── file2.txt       # 只在本地存在
├── fileA.txt       # 只在节点1001存在
└── dir1/           # 可能在多个节点存在同名目录
    ├── file3.txt
    └── file4.txt
```

#### 合并视图的核心机制

##### 1. 查找操作（Lookup）

```c
// inode_merge.c: hmdfs_lookup_merge
struct dentry *hmdfs_lookup_merge(struct inode *parent_inode,
                                  struct dentry *child_dentry,
                                  unsigned int flags) {
    struct hmdfs_sb_info *sbi = hmdfs_sb(parent_inode->i_sb);
    struct hmdfs_dentry_info_merge *mdi;
    struct hmdfs_peer *conn = NULL;
    struct list_head onstack_comrades_head;
    struct dentry *lo_d;
    struct hmdfs_dentry_comrade *comrade;

    mdi = hmdfs_dm(child_dentry);
    INIT_LIST_HEAD(&onstack_comrades_head);

    // ===== 第一步：查找本地 =====
    lo_d = lookup_local_dentry(parent_inode, child_dentry, 
                               HMDFS_DEVID_LOCAL, flags);
    if (lo_d && !IS_ERR(lo_d)) {
        comrade = alloc_comrade(lo_d, HMDFS_DEVID_LOCAL);
        link_comrade(&onstack_comrades_head, comrade);
    }

    // ===== 第二步：异步查找所有远程节点 =====
    list_for_each_entry(conn, &sbi->connections.node_list, list) {
        // 为每个节点创建异步查找工作
        merge_lookup_async(mdi, sbi, conn->devid, name, flags);
    }

    // ===== 第三步：等待所有异步查找完成 =====
    wait_for_lookup_completion(mdi);

    // ===== 第四步：分配comrades到dentry =====
    assign_comrades_unlocked(child_dentry, &onstack_comrades_head);
    return child_dentry;
}
```

**关键点**：
- 使用**异步并行查找**提升性能
- 通过 `work_count` 跟踪未完成的查找工作数
- 所有查找完成后才返回结果

##### 2. 创建操作（Create/Mkdir）

```c
// inode_merge.c: hmdfs_create_merge
int hmdfs_create_merge(struct inode *dir, struct dentry *dentry, 
                       umode_t mode, bool excl) {
    struct hmdfs_recursive_para rec_op_para;

    hmdfs_init_recursive_para(&rec_op_para, F_CREATE_MERGE, mode, 
                              excl, dentry->d_name.name);

    // 关键：只在本地创建文件！
    ret = create_lo_d_child(dir, dentry, false, &rec_op_para);
    return ret;
}

// create_lo_d_child
int create_lo_d_child(struct inode *i_parent, struct dentry *d_child,
                      bool is_dir, struct hmdfs_recursive_para *rec_op_para) {
    struct hmdfs_sb_info *sbi = hmdfs_sb(i_parent->i_sb);
    struct dentry *lo_d_parent;

    // 关键：始终获取本地节点的dentry
    lo_d_parent = hmdfs_get_lo_d(d_parent, HMDFS_DEVID_LOCAL);
    
    if (!lo_d_parent) {
        // 如果父目录在本地不存在，递归创建
        ret = create_lo_d_parent_recur(d_pparent, d_parent, rec_op_para);
    }

    // 在本地创建文件/目录
    ret = hmdfs_create_lower_dentry(i_parent, d_child, lo_d_parent, 
                                    is_dir, rec_op_para);
    return ret;
}
```

**重要行为**：
- 在合并视图创建文件时，**文件只在当前执行操作的节点上创建**
- 创建后会分配一个新的comrade，`dev_id = HMDFS_DEVID_LOCAL`
- 这个文件不会立即出现在其他节点上，需要通过写回机制异步传播

##### 3. 重命名操作（Rename）

```c
// inode_merge.c: do_rename_merge
int do_rename_merge(struct inode *old_dir, struct dentry *old_dentry,
                    struct inode *new_dir, struct dentry *new_dentry,
                    unsigned int flags) {
    struct hmdfs_dentry_info_merge *dim_old = hmdfs_dm(old_dentry);
    struct dentry *lo_d_old, *lo_d_new_dir;
    struct hmdfs_dentry_comrade *comrade;

    // ===== 遍历所有comrade执行重命名 =====
    list_for_each_entry(comrade, &dim_old->comrade_list, list) {
        lo_d_old = comrade->lo_d;
        
        // 获取目标目录在对应节点上的dentry
        lo_d_new_dir = hmdfs_get_lo_d(d_new_dir, comrade->dev_id);
        
        // 在该节点上执行底层重命名
        ret = vfs_rename(&rename_data);
        if (ret)
            goto out;

        // 为新路径创建comrade
        new_comrade = alloc_comrade(lo_p_new.dentry, comrade->dev_id);
        link_comrade_unlocked(new_dentry, new_comrade);
    }

out:
    return ret;
}
```

**关键约束**：
- 重命名**不能跨目录**（hmdfs_rename_merge中会检查）
- 必须在同一父目录下进行
- 遍历所有comrade确保一致性

##### 4. 删除操作（Unlink/Rmdir）

```c
// inode_merge.c: hmdfs_unlink_merge
int hmdfs_unlink_merge(struct inode *dir, struct dentry *dentry) {
    struct hmdfs_dentry_info_merge *dim = hmdfs_dm(dentry);
    struct hmdfs_dentry_comrade *comrade;
    int ret = 0;

    // ===== 遍历所有comrade执行删除 =====
    list_for_each_entry(comrade, &dim->comrade_list, list) {
        lo_d = comrade->lo_d;
        ret = vfs_unlink(d_inode(lo_d->d_parent), lo_d, NULL);
        if (ret)
            break;
    }

    return ret;
}
```

---

## 四、分布式一致性机制

### 4.1 写回机制（Writeback）—— 实现最终一致性的核心

#### 架构设计

```
┌────────────────────────────────────────────────────────────┐
│                    用户写入                                   │
│                       ↓                                      │
├────────────────────────────────────────────────────────────┤
│              Page Cache（页面缓存）                           │
│                  ↓ 标记为Dirty                               │
├────────────────────────────────────────────────────────────┤
│         写回工作线程（wb_thread）                             │
│         - 定期扫描dirty页面（interval ≈ 5秒）                 │
│         - 批量发送以提升效率                                  │
│              ↓                                              │
├────────────────────────────────────────────────────────────┤
│         网络发送层                                            │
│         - 通过socket发送到远程节点                             │
│         - 带超时控制（wb_timeout_ms，默认60秒）                │
│              ↓                                              │
├────────────────────────────────────────────────────────────┤
│         远程节点接收层                                         │
│         - 接收数据                                            │
│         - 写入本地底层文件系统                                 │
│         - 发送确认                                            │
└────────────────────────────────────────────────────────────┘
```

#### 写回流程

```c
// client_writeback.c
int hmdfs_writepage(struct page *page, struct writeback_control *wbc) {
    struct inode *inode = page->mapping->host;
    struct hmdfs_peer *conn;
    int ret;

    // ===== 1. 获取远程连接 =====
    conn = hmdfs_get_remote_conn(inode);
    if (!conn)
        return 0;  // 没有远程节点，无需写回

    // ===== 2. 检查节点状态 =====
    if (conn->status != NODE_STAT_ONLINE) {
        // 节点离线，触发stash机制
        hmdfs_stash_writepage(conn, page);
        return 0;
    }

    // ===== 3. 发送页面到远程节点 =====
    ret = hmdfs_send_writepage(conn, page, wbc);
    if (ret < 0) {
        // 发送失败，标记为stash
        hmdfs_stash_add_page(page);
        return ret;
    }

    // ===== 4. 等待确认（带超时） =====
    ret = wait_for_completion_timeout(&page->writeback_done,
                                      msecs_to_jiffies(wb_timeout_ms));
    if (ret == 0) {
        // 超时处理
        hmdfs_handle_wb_timeout(page);
        return -ETIMEDOUT;
    }

    // 写回成功
    return 0;
}
```

#### 超时控制参数

```c
#define HMDFS_DEF_WB_TIMEOUT_MS 60000    // 默认写回超时：60秒
#define HMDFS_MAX_WB_TIMEOUT_MS 900000   // 最大写回超时：15分钟
```

**分布式系统意义**：
- 超时是分布式系统处理部分失败的必要机制
- 超时后将操作加入stash，保证了**不丢数据**的语义
- 但也意味着在超时期间，远程节点的数据可能落后于本地节点（不一致窗口）

### 4.2 离线检测与暂存机制（Stash）

#### 节点状态机

```
    ┌──────────┐
    │  ONLINE  │ ←──────────────┐
    │  (在线)   │                │
    └────┬─────┘                │
         │ Socket发送/接收失败   │ 恢复完成
         ↓                      │
    ┌──────────┐                │
    │ OFFLINE  │                │
    │  (离线)   │                │
    └────┬─────┘                │
         │ 重连成功              │
         ↓                      │
    ┌──────────┐                │
    │RECOVERING│ ───────────────┘
    │ (恢复中)  │
    └──────────┘
```

#### 离线检测

HMDFS通过socket通信的成功/失败来检测节点离线：

```c
// comm/connection.c
static int hmdfs_send_message(struct hmdfs_peer *conn, 
                              struct hmdfs_message *msg) {
    int ret;
    
    // 尝试发送消息
    ret = kernel_send(conn->sock, msg->data, msg->len, 0);
    
    if (ret < 0) {
        // 发送失败，标记节点离线
        conn->status = NODE_STAT_OFFLINE;
        conn->last_seen = jiffies;
        
        // 触发离线事件回调
        hmdfs_stash_add_node_evt_cb();
        
        // 将待发送消息加入stash
        hmdfs_stash_add_message(conn, msg);
        
        return ret;
    }
    
    // 更新最后活跃时间
    conn->last_seen = jiffies;
    return 0;
}
```

#### 暂存机制

当节点离线时，HMDFS使用stash机制暂存无法同步的操作：

```c
// stash.c
struct hmdfs_stash_entry {
    struct list_head list;
    struct page *page;           // 暂存的页面数据
    struct hmdfs_message *msg;   // 暂存的消息
    int type;                    // 类型（页面/消息/元数据操作）
    unsigned long timestamp;     // 暂存时间
};

int hmdfs_stash_writepage(struct hmdfs_peer *conn, 
                          struct hmdfs_writepage_context *ctx) {
    struct hmdfs_stash_entry *entry;
    
    // 创建暂存条目
    entry = kmalloc(sizeof(*entry), GFP_KERNEL);
    entry->page = ctx->page;
    entry->type = STASH_TYPE_PAGE;
    entry->timestamp = jiffies;
    
    // 添加到暂存列表
    list_add_tail(&entry->list, &conn->stash_list);
    
    // 持久化到本地stash工作目录（防止重启丢失）
    hmdfs_stash_save_to_work_dir(conn, entry);
    
    return 0;
}
```

#### 恢复机制

节点重新上线后，执行stash恢复：

```c
int hmdfs_stash_recover(struct hmdfs_peer *conn) {
    struct hmdfs_stash_entry *entry, *tmp;
    
    conn->status = NODE_STAT_RECOVERING;
    
    // 遍历暂存列表，重新发送
    list_for_each_entry_safe(entry, tmp, &conn->stash_list, list) {
        ret = hmdfs_send_stash_entry(conn, entry);
        if (ret < 0) {
            // 发送失败，保留在stash中
            continue;
        }
        
        // 发送成功，移除暂存条目
        list_del(&entry->list);
        kfree(entry);
    }
    
    conn->status = NODE_STAT_ONLINE;
    return 0;
}
```

**分布式系统意义**：
- Stash机制实现了**分区容忍性**：即使网络分区，本地操作仍然可以继续
- 恢复机制保证了**最终一致性**：分区恢复后，数据最终会传播到所有节点
- 持久化stash防止了**数据丢失**

### 4.3 目录项缓存（Dentry Cache）

#### 缓存设计

在分布式文件系统中，缓存是提升性能的关键，但也带来了一致性挑战。HMDFS的dentry cache设计：

```c
struct hmdfs_cache_entry {
    struct list_head list;
    char *name;
    int namelen;
    int d_type;
    unsigned long timestamp;       // 缓存时间戳
    struct hmdfs_peer *peer;       // 所属节点
};
```

#### 缓存验证

```c
int hmdfs_dentry_is_valid(struct dentry *dentry) {
    struct hmdfs_sb_info *sbi = hmdfs_sb(dentry->d_sb);
    struct hmdfs_inode_info *info = hmdfs_i(dentry->d_inode);
    
    // 1. 检查缓存是否启用
    if (!sbi->s_dentry_cache || sbi->dcache_timeout == 0)
        return 0;
    
    // 2. 检查是否超时
    if (time_elapsed(info->cache_time) > sbi->dcache_timeout)
        return 0;  // 缓存过期
    
    // 3. 检查版本号是否匹配
    if (info->version != info->cached_version)
        return 0;  // 版本不匹配
    
    // 4. 检查节点状态
    if (info->peer && info->peer->status != NODE_STAT_ONLINE)
        return 0;  // 节点离线
    
    return 1;  // 缓存有效
}
```

#### 缓存失效场景

- **超时失效**：超过 `dcache_timeout`（默认30秒）
- **版本变化**：inode version发生变化
- **节点离线**：远程节点状态变为OFFLINE
- **操作失效**：执行删除、重命名等操作后显式失效

**分布式系统意义**：
- 超时机制是分布式缓存保持一致性的经典策略（类似DNS TTL）
- 版本号提供了更强的变化检测能力
- 但30秒的超时窗口意味着在此期间不同节点可能看到不同的目录结构

### 4.4 跨节点同步（Syncfs）

#### Syncfs架构

```c
struct hmdfs_syncfs_info {
    wait_queue_head_t wq;
    atomic_t wait_count;
    int remote_ret;
    unsigned long long version;
    
    spinlock_t v_lock;
    bool is_executing;          // 是否有syncfs正在执行
    
    // syncfs队列管理
    struct list_head wait_list;      // 等待列表
    struct list_head pending_list;   // 挂起列表
    spinlock_t list_lock;
};
```

#### Syncfs队列模型

```
时间线：t1 → t2 → t3 → t4 → t5

syncfs_1 (t1) ─┐
syncfs_2 (t2) ─┼→ pending_list（会被丢弃）
               │
syncfs_3 (t3) ─┼→ 正在执行
               │
syncfs_4 (t4) ─┼→ wait_list（等待执行）
syncfs_5 (t5) ─┘

syncfs_3完成后:
- 丢弃pending_list中的syncfs_1, syncfs_2
- 执行wait_list中最新的syncfs_5
- 丢弃syncfs_4
```

**设计意义**：
- 避免过多的syncfs导致性能下降
- 保证最新的同步请求被执行
- 丢弃过时的同步请求，提高效率

---

## 五、分布式系统中容易出错的特征

### 5.1 并发一致性 Bug

在分布式文件系统中，并发操作是最容易引入bug的地方。以下是在HMDFS源码中发现的易出错场景：

#### 5.1.1 竞态条件（Race Conditions）

**场景1：并发创建同名文件**

```
节点A: create("file.txt") ───┐
                              ├→ 都成功？都失败？只有一个成功？
节点B: create("file.txt") ───┘

HMDFS行为：各自在本地创建，通过写回传播
潜在Bug：两个节点都认为自己是文件的"创建者"
```

**源码位置**：`inode_merge.c:hmdfs_create_merge`
**风险点**：创建操作只在本地执行，没有分布式锁保护

**场景2：并发写入同一文件**

```
节点A: write("file.txt", offset=0, data="AAAA") ──┐
                                                    ├→ 最终内容是什么？
节点B: write("file.txt", offset=0, data="BBBB") ──┘

HMDFS行为：异步写回，最后到达的覆盖先到达的
潜在Bug：写入顺序不确定，结果不可重现
```

**源码位置**：`client_writeback.c:hmdfs_writepage`
**风险点**：异步写回不保证全局顺序

**场景3：并发删除**

```
节点A: unlink("file.txt") ───┐
                              ├→ 都成功？只有一个成功？
节点B: unlink("file.txt") ───┘

HMDFS行为：遍历所有comrade执行删除
潜在Bug：如果一个comrade删除失败，其他comrade已经删除
```

**源码位置**：`inode_merge.c:hmdfs_unlink_merge`
**风险点**：没有回滚机制

#### 5.1.2 原子性违反（Atomicity Violations）

**场景：重命名与其他操作并发**

```
节点A: rename("old.txt", "new.txt") ──┐
                                       ├→ open("old.txt") 应该成功还是失败？
节点B: open("old.txt") ────────────────┘

HMDFS行为：rename遍历comrade，open查找comrade
潜在Bug：open可能在rename执行到一半时执行，看到部分更新的状态
```

**源码位置**：`inode_merge.c:do_rename_merge`
**风险点**：rename不是原子地更新所有comrade

#### 5.1.3 因果一致性违反（Causal Consistency）

**场景：写后读**

```
节点A: write("file.txt", "data1")
       sync()
       → 通知节点B

节点B: 收到通知后 read("file.txt")
       → 可能读到旧数据（写回还没完成）
```

**风险点**：syncfs只保证触发写回，不保证写回完成

### 5.2 分区与恢复 Bug

#### 5.2.1 脑裂（Split-Brain）

当网络分区导致多个子网络时：

```
分区前：A ─── B ─── C（都在线）

分区后：A    B ─── C
       ↓     ↓
     写入X   写入Y
     (都叫"file.txt")

恢复后：A ─── B ─── C
       → X和Y冲突！
```

**HMDFS处理**：通过stash暂存，恢复后传播
**潜在Bug**：冲突解决策略不明确

#### 5.2.2 Stash溢出

长时间离线可能导致stash空间耗尽：

```
节点A离线10分钟
├── 写入了1000个文件
├── 修改了500个文件
├── 删除了200个文件
└── Stash空间满了！

→ 新操作怎么办？丢弃？阻塞？返回错误？
```

**源码位置**：`stash.c`
**潜在Bug**：没有看到明确的溢出处理逻辑

#### 5.2.3 部分恢复失败

恢复时部分文件同步失败：

```
节点A恢复
├── 1000个stash条目
├── 成功发送998个
├── 2个发送失败（网络波动？）
└→ 失败的条目保留在stash
   → 何时重试？无限重试？
```

### 5.3 缓存一致性 Bug

#### 5.3.1 缓存不一致窗口

```
t0: 节点A创建 file.txt
t1: 节点B查找 file.txt → 缓存未命中，查到（刚写回）
t2: 节点C查找 file.txt → 缓存命中（但缓存的是旧状态：不存在）
    → 节点C认为file.txt不存在！

不一致窗口：t0 到 t0+dcache_timeout（默认30秒）
```

#### 5.3.2 版本回退

```
节点A: 修改文件，version=5
       写回发送到节点B

网络延迟：写回包晚于另一个写回包到达
节点B: 先收到version=6的写回
       后收到version=5的写回
       → 用旧数据覆盖了新数据！
```

### 5.4 超时相关 Bug

#### 5.4.1 写回超时

```
节点A: 写入文件
       开始写回 → 等待确认

60秒后：超时！
        → 标记为stash？
        → 但节点B实际上已经收到了（只是确认包丢失）
        
结果：节点B可能收到两份相同的写回
     → 数据重复？
```

#### 5.4.2 Syncfs超时

```
syncfs_1开始执行
├── 需要等待所有远程节点响应
├── 某个节点响应慢
└→ syncfs_1长时间阻塞
   
后续syncfs_2, syncfs_3, ...
├── 进入pending_list被丢弃
└→ 用户以为同步了，实际没有
```

### 5.5 启动检测相关 Bug

HMDFS使用boot_cookie检测节点是否重启：

```c
uint64_t boot_cookie;  // 每次启动随机生成
```

**场景**：

```
节点A启动，boot_cookie=0x1234
节点B连接，记录cookie=0x1234

节点A重启，boot_cookie=0x5678
节点B仍然记录cookie=0x1234

→ 节点B如何检测到节点A重启了？
→ 重启前的stash数据还有效吗？
```

### 5.6 边界条件 Bug

#### 5.6.1 文件名与路径

```c
#define HMDFS_XATTR_SIZE_MAX 4096      // xattr最大大小
#define HMDFS_LISTXATTR_SIZE_MAX 4096  // listxattr最大大小
```

- 文件名长度接近NAME_MAX（255）时
- 路径深度接近PATH_MAX（4096）时
- xattr大小接近限制时

#### 5.6.2 大文件与并发

- 文件大小超过2GB/4GB时的处理
- 大文件并发写入时的性能
- 页面缓存压力

#### 5.6.3 多节点场景

- 节点数量很多时的comrade列表性能
- 并发连接数限制
- 网络带宽争用

---

## 六、模糊测试反馈指标设计

### 6.1 覆盖率指标

| 指标 | 说明 | 实现建议 |
|-----|------|---------|
| **系统调用覆盖率** | 触发的文件系统系统调用 | 标准coverage |
| **HMDFS内部路径覆盖** | 触发的HMDFS内部函数 | 插桩关键函数 |
| **状态转换覆盖** | 触发的节点状态转换 | 记录状态变化 |
| **错误路径覆盖** | 触发的错误处理路径 | 插桩错误处理 |

### 6.2 一致性指标

| 指标 | 说明 | 检查方式 |
|-----|------|---------|
| **文件存在性一致** | 各节点文件存在性 | stat比较 |
| **文件内容一致** | 各节点文件内容 | checksum |
| **目录结构一致** | 各节点目录结构 | readdir比较 |
| **元数据一致** | 权限、时间戳等 | stat比较 |
| **xattr一致** | 扩展属性 | getxattr比较 |

### 6.3 并发指标

| 指标 | 说明 | 检测方式 |
|-----|------|---------|
| **并发操作对数** | 触发的并发操作 | 时间重叠检测 |
| **竞争条件触发** | 非确定性结果 | 多次执行比较 |
| **原子性违反** | 操作中间状态可见 | 检查中间状态 |
| **因果一致性违反** | 因果关系被破坏 | 检查操作顺序 |

### 6.4 离线恢复指标

| 指标 | 说明 | 检测方式 |
|-----|------|---------|
| **stash触发次数** | 触发stash的次数 | 计数 |
| **stash恢复成功率** | 恢复成功/总恢复 | 统计 |
| **数据丢失** | 恢复后数据丢失 | 比较前后状态 |
| **数据重复** | 恢复后数据重复 | 检查唯一性 |

---

## 七、关键代码位置索引

| 功能模块 | 源文件 | 关键函数/结构 |
|---------|--------|--------------|
| 挂载与初始化 | super.c | hmdfs_parse_options |
|  | main.c | hmdfs_init_fs |
| 设备视图操作 | file_local.c | hmdfs_read_local, hmdfs_write_local |
|  | inode.c | hmdfs_lookup, hmdfs_create |
| 合并视图操作 | inode_merge.c | hmdfs_lookup_merge, hmdfs_create_merge |
|  |  | do_rename_merge, hmdfs_unlink_merge |
|  | file_merge.c | hmdfs_read_merge, hmdfs_write_merge |
| 客户端写回 | client_writeback.c | hmdfs_writepage |
| 服务端写回 | server_writeback.c | hmdfs_server_writeback |
| 离线暂存 | stash.c | hmdfs_stash_writepage |
| 连接管理 | comm/connection.c | hmdfs_connect |
| 协议处理 | comm/protocol.c | hmdfs_send_message |
| 缓存管理 | inode.c | hmdfs_cache_* |
| 跨节点同步 | main.c | hmdfs_sync_fs |

---

## 八、配置参数

### 8.1 挂载参数

```bash
mount -t hmdfs none /mnt/hmdfs \
    -o local_dst=/data/local \
    -o merge \
    -o ra_pages=128 \
    -o cache_dir=/data/cache
```

| 参数 | 类型 | 默认值 | 说明 |
|-----|------|-------|------|
| local_dst | string | - | 本地存储路径 |
| merge | bool | false | 启用合并视图 |
| ra_pages | int | 128 | 预读页数 |
| cache_dir | string | - | 缓存目录 |
| sensitive | bool | false | 大小写敏感 |
| no_offline_stash | bool | false | 禁用离线暂存 |
| no_dentry_cache | bool | false | 禁用目录项缓存 |

### 8.2 内部超时参数

| 参数 | 默认值 | 最大值 | 说明 |
|-----|-------|-------|------|
| write_cache_timeout | 30秒 | - | 写缓存超时 |
| dcache_timeout | 30秒 | - | 目录项缓存超时 |
| wb_timeout_ms | 60秒 | 15分钟 | 写回超时 |

---

## 九、总结

HMDFS是一个典型的**最终一致性分布式文件系统**，其核心设计权衡：

1. **可用性优先**：本地操作不依赖网络，即使部分节点离线也能继续
2. **异步同步**：通过写回机制实现数据传播，接受不一致窗口
3. **容错恢复**：通过stash机制处理离线场景，保证不丢数据

**最容易出错的领域**：
- 并发操作的一致性和原子性
- 离线恢复的完整性和幂等性
- 缓存一致性的时效性
- 超时处理的正确性
- 边界条件的鲁棒性

这些正是模糊测试应该重点关注的方向。

---

*文档版本：v2.0*
*基于HMDFS源码和分布式文件系统理论分析*
*适用于模糊测试反馈指标设计*
