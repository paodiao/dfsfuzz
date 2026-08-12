# HMDFS Dentry Cache功能模糊测试设计文档

## 文档概述

本文档记录了针对hmdfs分布式文件系统dentry cache功能的模糊测试设计方案，包括bug类型分析、故障注入方法设计、状态感知机制等核心内容。本文档旨在为后续的模糊测试实现提供完整的设计参考。

**背景说明**：dentry cache是hmdfs中用于缓存远程节点目录项信息的关键功能，每个节点会将其它节点的dentry信息缓存到本地，以提高目录查找的性能。该功能主要涉及[hmdfs_dentryfile.c](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c)中的缓存管理逻辑，并在[file_remote.c](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\file_remote.c)和[inode_remote.c](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\inode_remote.c)中被广泛使用。

**与stash功能的对比**：与stash功能（节点离线时缓存文件数据）不同，dentry cache功能关注的是目录元数据的缓存，其状态机相对简单，但缓存查找和过期机制更为复杂。详细对比参见本文档第七章。

---

## 一、Dentry Cache功能Bug类型分析

### 1.1 错误类型优先级排序

根据对hmdfs dentry cache功能代码的分析（主要参考[hmdfs_dentryfile.c](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c)），错误类型按出现概率从高到低排序：

#### 第一优先级：并发错误（最容易出现）

**核心问题**：dentry cache功能涉及大量的并发操作，包括多节点同时访问、缓存更新、锁管理等。

**具体类型**：

##### 1.1.1 竞态条件（Race Conditions）

**关键代码位置**：

**1. 缓存查找和添加的竞态**

- 参见代码：[hmdfs_dentryfile.c:1224-1248](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1224-L1248)
```c
struct cache_file_node *__find_cfn(struct hmdfs_sb_info *sbi, const char *cid,
                               const char *path, bool server)
{
    struct cache_file_node *cfn = NULL;
    struct list_head *head = NULL;

    head = get_list_head(sbi, server);

    list_for_each_entry(cfn, head, list) {
        if (dentry_file_match(cfn, cid, path)) {
            refcount_inc(&cfn->ref);  // 竞态点1：引用计数增加
            return cfn;
        }
    }
    return NULL;
}
```

- 参见代码：[hmdfs_dentryfile.c:1273-1313](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1273-L1313)
```c
static struct file *insert_cfn(struct hmdfs_sb_info *sbi, const char *filename,
       const char *path, const char *cid, bool server)
{
    const struct cred *old_cred = NULL;
    struct cache_file_node *cfn = NULL;
    struct cache_file_node *exist = NULL;
    struct list_head *head = NULL;
    struct file *filp = NULL;

    cfn = create_cfn(sbi, path, cid, server);
    if (!cfn)
        return ERR_PTR(-ENOMEM);

    old_cred = hmdfs_override_creds(sbi->system_cred);
    filp = filp_open(filename, O_RDWR | O_LARGEFILE, 0);
    hmdfs_revert_creds(old_cred);
    if (IS_ERR(filp)) {
        hmdfs_err("open file failed, err=%ld", PTR_ERR(filp));
        goto out;
    }

    head = get_list_head(sbi, server);

    mutex_lock(&sbi->cache_list_lock);
    exist = __find_cfn(sbi, cid, path, server);  // 可能持有cache_list_lock时调用__find_cfn
    if (!exist) {
        cfn->filp = filp;
        list_add_tail(&cfn->list, head);  // 竞态点2：添加到链表
    } else {
        mutex_unlock(&sbi->cache_list_lock);
        release_cfn(exist);
        filp_close(filp, NULL);
        filp = ERR_PTR(-EEXIST);
        goto out;
    }
    mutex_unlock(&sbi->cache_list_lock);
    return filp;
out:
    free_cfn(cfn);
    return filp;
}
```

**2. 缓存重新验证时的竞态**

- 参见代码：[hmdfs_dentryfile.c:2230-2252](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2230-L2252)
```c
bool hmdfs_cache_revalidate(unsigned long conn_time, uint64_t dev_id,
                struct dentry *dentry)
{
    bool ret = false;
    struct clearcache_item *item = NULL;
    struct hmdfs_dentry_info *d_info = hmdfs_d(dentry);
    unsigned int timeout;

    if (!d_info)
        return ret;

    timeout = hmdfs_sb(dentry->d_sb)->dcache_timeout;
    spin_lock(&d_info->cache_list_lock);  // 竞态点3：锁获取
    list_for_each_entry(item, &(d_info->cache_list_head), list) {
        if (dev_id == item->dev_id) {
            ret = cache_item_revalidate(conn_time, item->time,
                            timeout);
            break;
        }
    }
    spin_unlock(&d_info->cache_list_lock);
    return ret;
}
```

**3. 缓存项查找和释放的竞态**

- 参见代码：[hmdfs_dentryfile.c:2209-2228](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2209-L2228)
```c
struct clearcache_item *hmdfs_find_cache_item(uint64_t dev_id,
                          struct dentry *dentry)
{
    struct clearcache_item *item = NULL;
    struct hmdfs_dentry_info *d_info = hmdfs_d(dentry);

    if (!d_info)
        return NULL;

    spin_lock(&d_info->cache_list_lock);
    list_for_each_entry(item, &(d_info->cache_list_head), list) {
        if (dev_id == item->dev_id) {
            kref_get(&item->ref);  // 竞态点4：引用计数增加
            spin_unlock(&d_info->cache_list_lock);
            return item;
        }
    }
    spin_unlock(&d_info->cache_list_lock);
    return NULL;
}
```

**诱发场景**：
- 多个线程同时查找和添加同一dentry缓存
- 节点离线/上线事件与缓存操作的并发
- 缓存重新验证与缓存更新的并发
- 多个客户端同时访问同一目录

##### 1.1.2 死锁（Deadlocks）

**关键代码位置**：

**1. 多个锁的获取顺序问题**

- 涉及的锁：`cache_list_lock`、`cache_pull_lock`、`remote_cache_list_lock`、`fid_lock`
- 参见代码：[hmdfs_dentryfile.c:1296-1308](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1296-L1308)
```c
mutex_lock(&sbi->cache_list_lock);
exist = __find_cfn(sbi, cid, path, server);  // 可能持有cache_list_lock时调用__find_cfn
if (!exist) {
    cfn->filp = filp;
    list_add_tail(&cfn->list, head);
} else {
    mutex_unlock(&sbi->cache_list_lock);
    release_cfn(exist);  // 可能触发其他锁，造成死锁
    filp_close(filp, NULL);
    filp = ERR_PTR(-EEXIST);
    goto out;
}
mutex_unlock(&sbi->cache_list_lock);
```

**2. 缓存项释放时的锁竞争**

- 参见代码：[hmdfs_dentryfile.c:1535-1539](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1535-L1539)
```c
void release_cfn(struct cache_file_node *cfn)
{
    if (refcount_dec_and_test(&cfn->ref))
        free_cfn(cfn);  // 可能需要获取cache_list_lock，造成死锁
}
```

- 参见代码：[hmdfs_dentryfile.c:1205-1212](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1205-L1212)
```c
static void free_cfn(struct cache_file_node *cfn)
{
    if (!IS_ERR_OR_NULL(cfn->filp))
        filp_close(cfn->filp, NULL);

    kfree(cfn->relative_path);
    kfree(cfn);
}
```

**3. 缓存查找时的锁竞争**

- 参见代码：[hmdfs_dentryfile.c:2139-2196](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2139-L2196)
```c
bool get_remote_dentry_file(struct dentry *dentry, struct hmdfs_peer *con)
{
    struct hmdfs_dentry_info *d_info = hmdfs_d(dentry);
    struct cache_file_node *cfn = NULL;
    struct hmdfs_sb_info *sbi = con->sbi;
    char *relative_path = NULL;
    int err = 0;
    struct file *filp = NULL;
    struct clearcache_item *item;

    if (hmdfs_cache_revalidate(READ_ONCE(con->conn_time), con->device_id,
                   dentry))
        return false;

    relative_path = hmdfs_get_dentry_relative_path(dentry);
    if (unlikely(!relative_path)) {
        hmdfs_err("get relative path failed %d", -ENOMEM);
        return false;
    }
    mutex_lock(&d_info->cache_pull_lock);  // 获取cache_pull_lock
    if (hmdfs_cache_revalidate(READ_ONCE(con->conn_time), con->device_id,
                   dentry))
        goto out_unlock;

    item = hmdfs_find_cache_item(con->device_id, dentry);  // 可能获取cache_list_lock
    if (item) {
        remote_file_revalidate_item(dentry, con, item, relative_path);
        kref_put(&item->ref, release_cache_item);
        goto out_unlock;
    }

    cfn = find_cfn(sbi, con->cid, relative_path, false);  // 可能获取cache_list_lock
    if (cfn) {
        remote_file_revalidate_cfn(dentry, con, cfn, relative_path);
        release_cfn(cfn);
        goto out_unlock;
    }

    filp = hmdfs_get_new_dentry_file(con, relative_path, NULL);
    if (IS_ERR(filp)) {
        err = PTR_ERR(filp);
        goto out_unlock;
    }

    err = hmdfs_add_file_to_cache(dentry, con, filp, relative_path);
    if (unlikely(err))
        hmdfs_err("add cache list failed devid:%lu err:%d",
              (unsigned long)con->device_id, err);
    fput(filp);

out_unlock:
    mutex_unlock(&d_info->cache_pull_lock);
    if (err && err != -ENOENT)
        hmdfs_err("readdir failed dev:%lu err:%d",
              (unsigned long)con->device_id, err);
    kfree(relative_path);
    return true;
}
```

**诱发场景**：
- 不同代码路径以不同顺序获取多个锁
- 异常路径下锁释放顺序不一致
- 中断处理与正常操作的锁竞争
- 缓存查找和缓存更新操作的锁竞争

##### 1.1.3 数据竞争（Data Races）

**关键代码位置**：

**1. cache_file_node的并发访问和释放**

- 参见代码：[hmdfs_dentryfile.c:1205-1212](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1205-L1212)
```c
static void free_cfn(struct cache_file_node *cfn)
{
    if (!IS_ERR_OR_NULL(cfn->filp))
        filp_close(cfn->filp, NULL);

    kfree(cfn->relative_path);
    kfree(cfn);
}
```

**2. clearcache_item的引用计数管理**

- 参见代码：[hmdfs_dentryfile.c:2267-2275](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2267-L2275)
```c
void release_cache_item(struct kref *ref)
{
    struct clearcache_item *item =
        container_of(ref, struct clearcache_item, ref);

    if (item->filp)
        fput(item->filp);
    kfree(item);
}
```

**3. cache_list_lock保护的数据的并发访问**

- 参见代码：[hmdfs_dentryfile.c:1224-1248](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1224-L1248)
```c
struct cache_file_node *__find_cfn(struct hmdfs_sb_info *sbi, const char *cid,
                               const char *path, bool server)
{
    struct cache_file_node *cfn = NULL;
    struct list_head *head = NULL;

    head = get_list_head(sbi, server);

    list_for_each_entry(cfn, head, list) {
        if (dentry_file_match(cfn, cid, path)) {
            refcount_inc(&cfn->ref);
            return cfn;
        }
    }
    return NULL;
}
```

**诱发场景**：
- 多个线程同时更新cache指针
- 引用计数检查与引用计数更新之间的时间窗口
- 原子操作与普通操作的并发访问
- 缓存列表的并发遍历和修改

#### 第二优先级：内存错误（次容易出现）

**核心问题**：涉及复杂的内存管理，包括缓存结构、引用计数等。

**具体类型**：

##### 1.2.1 Use-after-free

**关键代码位置**：

**1. cache_file_node结构体的生命周期管理**

- 参见代码：[hmdfs_dentryfile.c:1541-1555](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1541-L1555)
```c
void remove_cfn(struct cache_file_node *cfn)
{
    struct hmdfs_sb_info *sbi = cfn->sbi;
    bool deleted;

    mutex_lock(&sbi->cache_list_lock);
    deleted = list_empty(&cfn->list);
    if (!deleted)
        list_del_init(&cfn->list);
    mutex_unlock(&sbi->cache_list_lock);
    if (!deleted) {
        delete_dentry_file(cfn->filp);  // 可能使用已释放的cfn
        release_cfn(cfn);
    }
}
```

**2. clearcache_item的引用计数管理问题（kref_get/kref_put）**

- 参见代码：[hmdfs_dentryfile.c:2209-2228](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2209-L2228)
```c
struct clearcache_item *hmdfs_find_cache_item(uint64_t dev_id,
                          struct dentry *dentry)
{
    struct clearcache_item *item = NULL;
    struct hmdfs_dentry_info *d_info = hmdfs_d(dentry);

    if (!d_info)
        return NULL;

    spin_lock(&d_info->cache_list_lock);
    list_for_each_entry(item, &(d_info->cache_list_head), list) {
        if (dev_id == item->dev_id) {
            kref_get(&item->ref);  // 增加引用计数
            spin_unlock(&d_info->cache_list_lock);
            return item;
        }
    }
    spin_unlock(&d_info->cache_list_lock);
    return NULL;
}
```

- 参见代码：[hmdfs_dentryfile.c:2254-2265](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2254-L2265)
```c
void remove_cache_item(struct clearcache_item *item)
{
    bool deleted;

    spin_lock(&item->d_info->cache_list_lock);
    deleted = list_empty(&item->list);
    if (!deleted)
        list_del_init(&item->list);
    spin_unlock(&item->d_info->cache_list_lock);
    if (!deleted)
        kref_put(&item->ref, release_cache_item);  // 减少引用计数
}
```

**诱发场景**：
- 异常路径下cache被提前释放
- 引用计数管理错误
- 多个代码路径释放同一资源
- 并发访问导致引用计数错误

##### 1.2.2 内存泄漏（Memory Leaks）

**关键代码位置**：

**1. 异常路径下资源未释放**

- 参见代码：[hmdfs_dentryfile.c:1250-1271](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1250-L1271)
```c
struct cache_file_node *create_cfn(struct hmdfs_sb_info *sbi, const char *path,
                   const char *cid, bool server)
{
    struct cache_file_node *cfn = kzalloc(sizeof(*cfn), GFP_KERNEL);

    if (!cfn)
        return NULL;

    cfn->relative_path = kstrdup(path, GFP_KERNEL);
    if (!cfn->relative_path)
        goto out;  // cfn未释放，造成内存泄漏

    refcount_set(&cfn->ref, 1);
    strncpy(cfn->cid, cid, HMDFS_CFN_CID_SIZE - 1);
    cfn->cid[HMDFS_CFN_CID_SIZE - 1] = '\0';
    cfn->sbi = sbi;
    cfn->server = server;
    return cfn;
out:
    free_cfn(cfn);
    return NULL;
}
```

**2. relative_path、dentry_group等临时缓冲区的泄漏**

- 参见代码：[hmdfs_dentryfile.c:542-620](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L542-L620)
```c
int read_dentry(struct hmdfs_sb_info *sbi, char *file_name,
        struct dir_context *ctx)
{
    unsigned long pos = (unsigned long)(ctx->pos);
    unsigned long group_id = (pos << (1 + DEV_ID_BIT_NUM)) >>
                     (POS_BIT_NUM - GROUP_ID_BIT_NUM);
    unsigned long offset = pos & OFFSET_BIT_MASK;
    struct hmdfs_dentry_group *dentry_group = NULL;
    struct file *handler = NULL;
    int group_num = 0;
    int iterate_result = 0;
    int i, j;
    const struct cred *saved_cred;

    saved_cred = hmdfs_override_fsids(false);
    if (!saved_cred) {
        hmdfs_err("prepare cred failed!");
        return -ENOMEM;
    }


    if (!file_name)
        return -EINVAL;

    dentry_group = kzalloc(sizeof(*dentry_group), GFP_KERNEL);
    if (!dentry_group)
        return -ENOMEM;

    handler = hmdfs_get_or_create_dents(sbi, file_name);
    if (IS_ERR_OR_NULL(handler)) {
        kfree(dentry_group);  // 异常路径下释放dentry_group
        return -ENOENT;
    }

    group_num = get_dentry_group_cnt(file_inode(handler));

    for (i = group_id; i < group_num; i++) {
        hmdfs_metainfo_read(sbi, handler, dentry_group,
                    sizeof(struct hmdfs_dentry_group), i);
        for (j = offset; j < DENTRY_PER_GROUP; j++) {
            int len;
            int file_type = 0;
            bool is_continue;

            len = le16_to_cpu(dentry_group->nsl[j].namelen);
            if (!test_bit_le(j, dentry_group->bitmap) || len == 0)
                continue;

            if (S_ISDIR(le16_to_cpu(dentry_group->nsl[j].i_mode)))
                file_type = DT_DIR;
            else if (S_ISREG(le16_to_cpu(
                     dentry_group->nsl[j].i_mode)))
                file_type = DT_REG;
            else if (S_ISLNK(le16_to_cpu(
                     dentry_group->nsl[j].i_mode)))
                file_type = DT_LNK;
            else
                continue;

            pos = hmdfs_set_pos(0, i, j);
            is_continue = dir_emit(
                ctx, dentry_group->filename[j], len,
                le64_to_cpu(dentry_group->nsl[j].i_ino),
                file_type);
            if (!is_continue) {
                ctx->pos = pos;
                iterate_result = 1;
                goto done;
            }
        }
        offset = 0;
    }

done:
    hmdfs_revert_fsids(saved_cred);
    kfree(dentry_group);  // 正常路径下释放dentry_group
    fput(handler);
    return iterate_result;
}
```

**诱发场景**：
- 错误处理路径中资源未释放
- 中断处理时清理不完整
- 循环中的内存分配未配对释放
- 异常路径下goto跳过释放代码

##### 1.2.3 Double-free

**关键代码位置**：

**1. 多个代码路径可能释放同一资源**

- 参见代码：[hmdfs_dentryfile.c:1505-1514](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1505-L1514)
```c
void __destroy_cfn(struct list_head *head)
{
    struct cache_file_node *cfn = NULL;
    struct cache_file_node *n = NULL;

    list_for_each_entry_safe(cfn, n, head, list) {
        list_del_init(&cfn->list);
        release_cfn(cfn);  // 可能重复释放
    }
}
```

- 参见代码：[hmdfs_dentryfile.c:1516-1522](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1516-L1522)
```c
void hmdfs_cfn_destroy(struct hmdfs_sb_info *sbi)
{
    mutex_lock(&sbi->cache_list_lock);
    __destroy_cfn(&sbi->client_cache);
    __destroy_cfn(&sbi->server_cache);
    mutex_unlock(&sbi->cache_list_lock);
}
```

**诱发场景**：
- 正常路径和异常路径都释放同一资源
- 错误恢复时重复释放
- 引用计数管理错误导致多次释放
- 并发访问导致重复释放

##### 1.2.4 空指针解引用（Null Pointer Dereference）

**关键代码位置**：

**1. cfn可能为NULL但未检查**

- 参见代码：[hmdfs_dentryfile.c:1364-1368](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1364-L1368)
```c
cfn = find_cfn(con->sbi, cid, relative_path, server);
if (cfn) {
    release_cfn(cfn);
    return filp;
}
```

**2. d_info、item等指针的空指针检查**

- 参见代码：[hmdfs_dentryfile.c:2209-2228](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2209-L2228)
```c
struct clearcache_item *hmdfs_find_cache_item(uint64_t dev_id,
                          struct dentry *dentry)
{
    struct clearcache_item *item = NULL;
    struct hmdfs_dentry_info *d_info = hmdfs_d(dentry);

    if (!d_info)  // 检查d_info是否为NULL
        return NULL;

    spin_lock(&d_info->cache_list_lock);
    list_for_each_entry(item, &(d_info->cache_list_head), list) {
        if (dev_id == item->dev_id) {
            kref_get(&item->ref);
            spin_unlock(&d_info->cache_list_lock);
            return item;
        }
    }
    spin_unlock(&d_info->cache_list_lock);
    return NULL;
}
```

**诱发场景**：
- 初始化失败导致指针为NULL
- 并发访问导致指针被置NULL
- 错误传播路径中指针检查遗漏
- 多层指针访问时中间层为NULL

#### 第三优先级：语义错误（相对较少）

**核心问题**：涉及数据一致性、状态机正确性等逻辑错误。

**具体类型**：

##### 1.3.1 数据不一致

**关键代码位置**：

**1. 缓存文件元数据与实际数据不匹配**

- 参见代码：[hmdfs_dentryfile.c:1631-1644](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1631-L1644)
```c
int read_header(struct hmdfs_sb_info *sbi, struct file *filp,
        struct hmdfs_dcache_header *header)
{
    ssize_t bytes;
    loff_t pos = 0;

    bytes = cache_file_read(sbi, filp, header, sizeof(*header), &pos);
    if (bytes != sizeof(*header)) {
        hmdfs_err("read file failed, err:%zd", bytes);
        return -EIO;
    }

    return 0;
}
```

**2. 缓存验证失败但未正确处理**

- 参见代码：[hmdfs_dentryfile.c:1662-1676](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1662-L1676)
```c
static int cache_check_case_sensitive(struct hmdfs_sb_info *sbi,
                struct file *filp)
{
    struct hmdfs_dcache_header header;

    if (read_header(sbi, filp, &header))
        return 0;

    if (sbi->s_case_sensitive != (bool)header.case_sensitive) {
        hmdfs_info("Case sensitive inconsistent, current fs is: %d, cache is %d, will drop cache",
               sbi->s_case_sensitive, header.case_sensitive);
        return 0;
    }
    return 1;
}
```

**3. 缓存重新验证逻辑错误**

- 参见代码：[hmdfs_dentryfile.c:2230-2252](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2230-L2252)
```c
bool hmdfs_cache_revalidate(unsigned long conn_time, uint64_t dev_id,
                struct dentry *dentry)
{
    bool ret = false;
    struct clearcache_item *item = NULL;
    struct hmdfs_dentry_info *d_info = hmdfs_d(dentry);
    unsigned int timeout;

    if (!d_info)
        return ret;

    timeout = hmdfs_sb(dentry->d_sb)->dcache_timeout;
    spin_lock(&d_info->cache_list_lock);
    list_for_each_entry(item, &(d_info->cache_list_head), list) {
        if (dev_id == item->dev_id) {
            ret = cache_item_revalidate(conn_time, item->time,
                            timeout);
            break;
        }
    }
    spin_unlock(&d_info->cache_list_lock);
    return ret;
}
```

**诱发场景**：
- 节点崩溃导致缓存文件写入不完整
- 网络分区导致元数据和数据不同步
- 并发更新导致缓存损坏
- 缓存验证逻辑错误导致数据不一致

##### 1.3.2 缓存过期错误

**关键代码位置**：

**1. 缓存过期判断逻辑错误**

- 参见代码：[hmdfs_dentryfile.c:2230-2252](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2230-L2252)

**2. 节点状态与缓存状态不一致**

- 参见代码：[hmdfs_dentryfile.c:2139-2196](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2139-L2196)

**诱发场景**：
- 节点离线/上线事件导致缓存状态错误
- 并发操作导致缓存状态不一致
- 缓存超时设置不合理
- 节点连接时间更新错误

##### 1.3.3 缓存查找失败

**关键代码位置**：

**1. 缓存查找失败但未正确处理**

- 参见代码：[hmdfs_dentryfile.c:2139-2196](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2139-L2196)
```c
bool get_remote_dentry_file(struct dentry *dentry, struct hmdfs_peer *con)
{
    struct hmdfs_dentry_info *d_info = hmdfs_d(dentry);
    struct cache_file_node *cfn = NULL;
    struct hmdfs_sb_info *sbi = con->sbi;
    char *relative_path = NULL;
    int err = 0;
    struct file *filp = NULL;
    struct clearcache_item *item;

    if (hmdfs_cache_revalidate(READ_ONCE(con->conn_time), con->device_id,
                   dentry))
        return false;

    relative_path = hmdfs_get_dentry_relative_path(dentry);
    if (unlikely(!relative_path)) {
        hmdfs_err("get relative path failed %d", -ENOMEM);
        return false;
    }
    mutex_lock(&d_info->cache_pull_lock);
    if (hmdfs_cache_revalidate(READ_ONCE(con->conn_time), con->device_id,
                   dentry))
        goto out_unlock;

    item = hmdfs_find_cache_item(con->device_id, dentry);
    if (item) {
        remote_file_revalidate_item(dentry, con, item, relative_path);
        kref_put(&item->ref, release_cache_item);
        goto out_unlock;
    }

    cfn = find_cfn(sbi, con->cid, relative_path, false);
    if (cfn) {
        remote_file_revalidate_cfn(dentry, con, cfn, relative_path);
        release_cfn(cfn);
        goto out_unlock;
    }

    filp = hmdfs_get_new_dentry_file(con, relative_path, NULL);
    if (IS_ERR(filp)) {
        err = PTR_ERR(filp);
        goto out_unlock;
    }

    err = hmdfs_add_file_to_cache(dentry, con, filp, relative_path);
    if (unlikely(err))
        hmdfs_err("add cache list failed devid:%lu err:%d",
              (unsigned long)con->device_id, err);
    fput(filp);

out_unlock:
    mutex_unlock(&d_info->cache_pull_lock);
    if (err && err != -ENOENT)
        hmdfs_err("readdir failed dev:%lu err:%d",
              (unsigned long)con->device_id, err);
    kfree(relative_path);
    return true;
}
```

**诱发场景**：
- 节点上线时路径配置错误
- 缓存查找过程中节点再次离线
- 磁盘空间不足导致缓存创建失败
- 网络错误导致缓存查找失败

### 1.2 最容易诱发bug的节点状态和集群状态

#### 1.2.1 节点状态组合

**高危险场景**：

##### 节点频繁上下线
- 节点在缓存查找过程中又上线
- 节点在缓存重新验证过程中又离线
- 参见代码：[hmdfs_dentryfile.c:2139-2196](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2139-L2196)
```c
bool get_remote_dentry_file(struct dentry *dentry, struct hmdfs_peer *con)
{
    // ...
    if (hmdfs_cache_revalidate(READ_ONCE(con->conn_time), con->device_id,
                   dentry))
        return false;

    relative_path = hmdfs_get_dentry_relative_path(dentry);
    if (unlikely(!relative_path)) {
        hmdfs_err("get relative path failed %d", -ENOMEM);
        return false;
    }
    mutex_lock(&d_info->cache_pull_lock);
    if (hmdfs_cache_revalidate(READ_ONCE(con->conn_time), con->device_id,
                   dentry))
        goto out_unlock;
    // ...
}
```

**诱发原因**：
- 缓存验证过程中节点状态变化
- 缓存查找操作被中断
- 缓存文件与实际状态不一致

##### 部分节点离线
- 多节点场景下部分节点离线
- 导致缓存不一致和查找失败
- 参见代码：[inode_remote.c:170-215](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\inode_remote.c#L170-L215)

##### 节点崩溃
- 在缓存查找过程中崩溃
- 在缓存重新验证过程中崩溃
- 导致缓存文件损坏

#### 1.2.2 集群状态

**高危险场景**：

##### 网络分区
- 客户端与服务器网络中断
- 服务器之间网络中断
- 导致缓存查找和重新验证失败
- 参见代码：[file_remote.c:472-502](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\file_remote.c#L472-L502)

##### 高并发访问
- 多个客户端同时访问同一目录
- 多个客户端同时触发缓存查找
- 参见代码：[hmdfs_dentryfile.c:1224-1248](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1224-L1248)

**诱发原因**：
- 锁竞争加剧
- 缓存查找冲突
- 资源争用

##### 资源受限
- 磁盘空间不足
- 内存不足
- 导致缓存创建失败
- 参见代码：[hmdfs_dentryfile.c:1250-1271](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1250-L1271)

##### 长时间运行
- 大量dentry被缓存
- 长时间离线后缓存过期
- 导致缓存文件累积和查找延迟

#### 1.2.3 文件状态

**高危险场景**：

##### 大目录
- 大目录的dentry缓存查找耗时较长
- 更容易在过程中遇到节点状态变化
- 参见代码：[hmdfs_dentryfile.c:542-620](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L542-L620)

##### 深层目录结构
- 深层目录的路径解析复杂
- 缓存查找路径较长
- 参见代码：[hmdfs_dentryfile.c:183-238](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L183-L238)

##### 符号链接
- 符号链接的路径解析复杂
- 缓存管理复杂
- 参见代码：[hmdfs_dentryfile.c:1408-1503](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1408-L1503)

### 1.3 提升测试效率的策略建议

根据以上分析，提升dentry cache测试效率的策略按优先级排序：

#### 第一优先级：设计并实现针对缓存操作的故障注入方法

**当前Monarch的故障注入能力**：
- `syz_failure_crash_client`：客户端崩溃
- `syz_failure_crash_server`：服务器崩溃
- `syz_failure_sync`：同步点
- `syz_failure_send/recv`：消息同步

**建议增强**：

**a) 基于并发操作的故障注入**
```c
// 通过生成并发目录访问测试用例来触发竞态条件
// 多个客户端同时访问同一目录，在关键时刻注入节点/网络故障
syz_failure_node_offline(node_id, mode)  // mode: graceful/abrupt
syz_failure_node_online(node_id, delay)
syz_failure_network_partition(node_group1, node_group2, duration)
```

**b) 时机感知的缓存故障注入**
```c
// 通过监控目录操作模式，在缓存操作的关键时机注入故障
syz_failure_inject_cache_at(node_id, timing, fault_type)
// 例如：在检测到大量目录遍历时，在缓存查找过程中注入网络分区
// timing: "during_cache_lookup", "during_cache_revalidate", "large_directory_traversal"
```

**c) 资源限制故障**
```c
// 通过限制系统资源来触发资源管理错误
syz_failure_disk_full(node_id, threshold)  // 模拟磁盘满，触发缓存文件创建失败
syz_failure_memory_pressure(node_id, level)  // 模拟内存压力，触发缓存分配失败
syz_failure_file_handle_exhaust(node_id, limit)  // 模拟文件描述符耗尽
```

**d) 并发操作测试用例生成**
```c
// 生成并发目录访问测试用例，而不是直接注入并发故障
// 通过测试用例生成来触发并发场景
generate_concurrent_dir_access(dir_path, num_clients, pattern)
generate_concurrent_cache_test(dir_path, num_clients, pattern)
```

#### 第二优先级：优化种子生成和突变

**a) 针对dentry cache的种子生成**
- 生成大量目录遍历和文件查找的序列
- 在关键位置插入节点离线/上线操作
- 生成并发访问同一目录的场景
- 生成深层目录结构的遍历操作

**b) 语义感知的突变**
- 保留目录操作序列的语义完整性
- 重点突变缓存操作的时机和类型
- 突变目录结构的参数（深度、大小、文件数量等）

#### 第三优先级：设计新的适应度指标

虽然应该从整体考虑，但针对dentry cache可以设计：

**a) 缓存覆盖指标**
- 覆盖所有缓存操作类型（查找、添加、删除、重新验证）
- 覆盖所有锁的获取/释放组合
- 覆盖所有缓存状态转换

**b) 并发场景覆盖**
- 记录并发访问的目录数量
- 记录并发执行的缓存操作数量
- 记录缓存冲突的次数

**c) 缓存失效场景覆盖**
- 覆盖不同的缓存失效时机
- 覆盖不同的缓存失效类型
- 覆盖不同的缓存恢复策略

#### 第四优先级：优化种子调度和优先级

- 优先调度触发缓存查找的种子
- 优先调度在缓存失效场景下成功的种子
- 优先调度覆盖新缓存操作路径的种子
- 优先调度包含大目录操作的种子

### 1.4 关键测试场景

基于以上分析，重点测试以下场景：

1. **节点在缓存查找过程中崩溃**
2. **节点在缓存重新验证过程中崩溃**
3. **多个客户端同时访问同一目录时节点离线**
4. **节点频繁上下线**
5. **网络分区下的缓存查找和重新验证**
6. **大目录的缓存查找**
7. **深层目录结构的缓存操作**
8. **资源受限情况下的缓存创建**
9. **长时间运行后的缓存过期**
10. **符号链接的缓存操作**

---

## 二、针对dentry cache的故障注入方法设计

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
    CacheState  CacheState  // dentry cache相关状态
}

type CacheState int

const (
    CacheNone CacheState = iota
    CacheLookup
    CacheAdd
    CacheRevalidate
    CacheDelete
    CacheExpired
)

// 新的拓扑结构
type ClusterTopology struct {
    Nodes       []NodeInfo
    Connections [][]Conn  // 全连接图
    IsDynamic   bool  // 是否动态拓扑
}
```

### 2.3 基于dentry cache状态的定制化故障注入

**核心思想**：根据dentry cache功能的状态机设计故障注入策略，但故障注入仍然在节点/网络级别

```go
// dentry cache状态感知的故障注入器
type CacheAwareFailureInjector struct {
    topology        *ClusterTopology
    cacheStates     map[int]CacheState
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
    CacheStates map[int]CacheState
    Result      FailureResult
}

type FailureResult int

const (
    ResultBugFound FailureResult = iota
    ResultCoverageIncrease
    ResultNoEffect
    ResultSystemUnstable
)

// 基于dentry cache状态生成故障策略
func (inj *CacheAwareFailureInjector) GenerateFailureStrategies() []FailureStrategy {
    strategies := make([]FailureStrategy, 0)

    // 策略1：在缓存查找过程中注入节点崩溃
    for nodeID, state := range inj.cacheStates {
        if state == CacheLookup {
            strategies = append(strategies, FailureStrategy{
                Type: FailureNodeCrash,
                Nodes: []int{nodeID},
                Timing: "during_cache_lookup",
                Priority: 0.9,  // 高优先级
                Description: "节点在缓存查找过程中崩溃，触发缓存查找失败",
            })
        }
    }

    // 策略2：在缓存重新验证过程中注入网络分区
    for nodeID, state := range inj.cacheStates {
        if state == CacheRevalidate {
            connectedNodes := inj.topology.Nodes[nodeID].Connections
            if len(connectedNodes) > 1 {
                strategies = append(strategies, FailureStrategy{
                    Type: FailureNetworkPartition,
                    Nodes: []int{nodeID, connectedNodes[0]},
                    Timing: "during_cache_revalidate",
                    Priority: 0.85,
                    Description: "节点在缓存重新验证过程中与部分节点网络分区，触发缓存验证失败",
                })
            }
        }
    }

    // 策略3：多节点并发缓存查找时注入故障
    lookupNodes := make([]int, 0)
    for nodeID, state := range inj.cacheStates {
        if state == CacheLookup {
            lookupNodes = append(lookupNodes, nodeID)
        }
    }
    if len(lookupNodes) >= 2 {
        strategies = append(strategies, FailureStrategy{
            Type: FailureNetworkPartition,
            Nodes: lookupNodes[:2],  // 选择前两个正在查找的节点
            Timing: "concurrent_cache_lookup",
            Priority: 0.8,
            Description: "多个节点并发缓存查找时网络分区，触发并发竞态条件",
        })
    }

    // 策略4：缓存添加过程中注入节点崩溃
    for nodeID, state := range inj.cacheStates {
        if state == CacheAdd {
            strategies = append(strategies, FailureStrategy{
                Type: FailureNodeCrash,
                Nodes: []int{nodeID},
                Timing: "during_cache_add",
                Priority: 0.85,
                Description: "节点在缓存添加过程中崩溃，触发缓存添加失败",
            })
        }
    }

    // 策略5：缓存过期时注入网络延迟
    for nodeID, state := range inj.cacheStates {
        if state == CacheExpired {
            strategies = append(strategies, FailureStrategy{
                Type: FailureNetworkDelay,
                Nodes: []int{nodeID},
                Timing: "cache_expired",
                Priority: 0.75,
                Description: "缓存过期时网络延迟，触发缓存重新验证延迟",
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

**核心思想**：在dentry cache操作的关键时机注入故障

```go
// 时机感知的故障注入器
type TimingAwareFailureInjector struct {
    cacheMonitor *CacheOperationMonitor
}

type CacheOperationMonitor struct {
    operations map[int]*CacheOperation  // 节点ID -> 操作
}

type CacheOperation struct {
    NodeID      int
    Phase       CachePhase
    StartTime   time.Time
    DirCount    int
    Progress    float64  // 0.0 - 1.0
}

type CachePhase int

const (
    PhasePrepare CachePhase = iota
    PhaseLookup
    PhaseAdd
    PhaseRevalidate
    PhaseDelete
    PhaseComplete
)

// 在关键时机注入故障
func (inj *TimingAwareFailureInjector) InjectAtCriticalTiming() []FailureInjection {
    injections := make([]FailureInjection, 0)

    for nodeID, op := range inj.cacheMonitor.operations {
        switch op.Phase {
        case PhaseLookup:
            // 在缓存查找过程中注入网络分区
            if op.Progress > 0.3 && op.Progress < 0.7 {
                injections = append(injections, FailureInjection{
                    Type: FailureNetworkPartition,
                    Node: nodeID,
                    Timing: fmt.Sprintf("lookup_%.0f", op.Progress*100),
                    Description: "在缓存查找30%-70%时网络分区",
                })
            }

        case PhaseAdd:
            // 在缓存添加过程中注入节点崩溃
            injections = append(injections, FailureInjection{
                Type: FailureNodeCrash,
                Node: nodeID,
                Timing: "cache_add",
                Description: "在缓存添加时节点崩溃",
            })

        case PhaseRevalidate:
            // 在缓存重新验证时注入网络延迟
            injections = append(injections, FailureInjection{
                Type: FailureNetworkDelay,
                Node: nodeID,
                Timing: "cache_revalidate",
                Description: "在缓存重新验证时网络延迟",
            })

        case PhaseDelete:
            // 在缓存删除时注入磁盘满
            injections = append(injections, FailureInjection{
                Type: FailureCacheOverflow,
                Node: nodeID,
                Timing: "cache_delete",
                Description: "在缓存删除时磁盘满",
            })
        }
    }

    return injections
}
```

---

## 三、非侵入式Dentry Cache状态感知设计

### 3.1 设计思路

**核心思想**：通过分析测试用例中的目录操作模式来推断dentry cache状态，结合节点上下线频率来模拟缓存操作过程中的故障。

**优点**：
- ✅ 非侵入式，不需要修改内核代码
- ✅ 利用现有的测试用例信息，无需额外监控
- ✅ 实现简单，易于集成到现有框架
- ✅ 可以通过种子生成控制来间接控制状态

**潜在问题**：
- ⚠️ 推断的准确性依赖于测试用例的设计
- ⚠️ 难以精确控制故障注入的时机
- ⚠️ 无法感知实际的缓存进度（如30%、70%等）

### 3.2 基于目录操作模式的状态推断

#### 3.2.1 目录操作分析器

```go
// 目录操作分析器
type DirOperationAnalyzer struct {
    operations map[int][]DirOp  // 节点ID -> 操作序列
    patterns   map[string]CachePattern
}

type DirOp struct {
    NodeID    int
    OpType    string  // "readdir", "lookup", "stat", "mkdir", "rmdir", "unlink"
    DirPath    string
    Timestamp  int
    FileCount  int
}

type CachePattern struct {
    PatternName string
    Operations []string
    CachePhase CachePhase
    Probability float64
}

// 预定义的dentry cache操作模式
var cachePatterns = []CachePattern{
    {
        PatternName: "normal_cache_lookup",
        Operations: []string{"readdir", "lookup", "lookup"},
        CachePhase: PhaseComplete,
        Probability: 0.4,
    },
    {
        PatternName: "interrupted_cache_lookup",
        Operations: []string{"readdir", "lookup"},  // 未完成
        CachePhase: PhaseLookup,
        Probability: 0.3,
    },
    {
        PatternName: "concurrent_cache_lookup",
        Operations: []string{"readdir", "readdir", "lookup", "lookup"},
        CachePhase: PhaseLookup,
        Probability: 0.2,
    },
    {
        PatternName: "cache_revalidate_pattern",
        Operations: []string{"readdir", "readdir", "lookup"},
        CachePhase: PhaseRevalidate,
        Probability: 0.1,
    },
}

// 分析测试用例推断dentry cache状态
func (analyzer *DirOperationAnalyzer) InferCacheState(ps []*Prog) map[int]CachePhase {
    states := make(map[int]CachePhase)

    for nodeID, p := range ps {
        ops := analyzer.extractDirOps(p)
        pattern := analyzer.matchPattern(ops)
        if pattern != nil {
            states[nodeID] = pattern.CachePhase
        }
    }

    return states
}

// 提取目录操作
func (analyzer *DirOperationAnalyzer) extractDirOps(p *Prog) []DirOp {
    ops := make([]DirOp, 0)

    for _, call := range p.Calls {
        if call.Meta.Name == "getdents" || call.Meta.Name == "getdents64" ||
           call.Meta.Name == "getdents64_r" || call.Meta.Name == "getdents_unistat" {
            ops = append(ops, DirOp{
                OpType: "readdir",
                FileCount: analyzer.getFileCount(call),
            })
        } else if call.Meta.Name == "openat" || call.Meta.Name == "openat2" {
            ops = append(ops, DirOp{
                OpType: "lookup",
                DirPath: analyzer.getDirPath(call),
            })
        } else if call.Meta.Name == "fstatat" || call.Meta.Name == "newfstatat" {
            ops = append(ops, DirOp{
                OpType: "stat",
            })
        } else if call.Meta.Name == "mkdirat" {
            ops = append(ops, DirOp{
                OpType: "mkdir",
            })
        } else if call.Meta.Name == "unlinkat" {
            ops = append(ops, DirOp{
                OpType: "unlink",
            })
        }
    }

    return ops
}

// 匹配操作模式
func (analyzer *DirOperationAnalyzer) matchPattern(ops []DirOp) *CachePattern {
    opTypes := make([]string, len(ops))
    for i, op := range ops {
        opTypes[i] = op.OpType
    }

    for _, pattern := range cachePatterns {
        if analyzer.matchPatternSequence(opTypes, pattern.Operations) {
            return &pattern
        }
    }

    return nil
}

// 模式序列匹配（支持模糊匹配）
func (analyzer *DirOperationAnalyzer) matchPatternSequence(ops []string, pattern []string) bool {
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

func (analyzer *DirOperationAnalyzer) equalSequences(a, b []string) bool {
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

func (analyzer *DirOperationAnalyzer) fuzzyMatch(ops []string, pattern []string) bool {
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
    readdirCount := 0
    lookupCount := 0
    mkdirCount := 0

    for _, call := range p.Calls {
        switch {
        case call.Meta.Name == "getdents" || call.Meta.Name == "getdents64":
            readdirCount++
        case call.Meta.Name == "openat" || call.Meta.Name == "openat2":
            lookupCount++
        case call.Meta.Name == "mkdirat":
            mkdirCount++
        }
    }

    // 基于操作比例推断角色
    totalOps := readdirCount + lookupCount + mkdirCount
    if totalOps == 0 {
        return RoleUnknown
    }

    readdirRatio := float64(readdirCount) / float64(totalOps)
    lookupRatio := float64(lookupCount) / float64(totalOps)

    // hmdfs节点通常有较多的readdir和lookup操作
    if readdirRatio > 0.3 && lookupRatio > 0.3 {
        return RoleHybrid  // 既做服务器也做客户端
    } else if readdirRatio > 0.5 {
        return RoleServer  // 主要是目录遍历，像服务器
    } else {
        return RoleClient  // 主要是文件查找，像客户端
    }
}

// 基于角色选择故障注入策略
func (alloc *NodeRoleAllocator) SelectFailureStrategy(nodeID int,
                                                  currentCacheState CachePhase) FailureStrategy {
    role := alloc.nodeRoles[nodeID]

    switch role {
    case RoleHybrid:
        // 混合角色节点更关键，注入更复杂的故障
        if currentCacheState == CacheLookup {
            return FailureStrategy{
                Type: FailureNetworkPartition,
                Nodes: alloc.getConnectedNodes(nodeID),
                Timing: "during_cache_lookup",
                Priority: 0.9,
                Description: "混合角色节点在缓存查找时网络分区",
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
    ReaddirRatio  map[int]float64
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
    readdirOps := 0
    lookupOps := 0
    dirOps := 0

    for _, call := range p.Calls {
        if call.Meta.Name == "getdents" || call.Meta.Name == "getdents64" {
            readdirOps++
        } else if call.Meta.Name == "openat" || call.Meta.Name == "openat2" {
            lookupOps++
        } else if strings.Contains(call.Meta.Name, "dir") {
            dirOps++
        }
    }

    return SeedComposition{
        TotalOps: totalOps,
        ReaddirOps: readdirOps,
        LookupOps: lookupOps,
        DirOps: dirOps,
        ReaddirRatio: float64(readdirOps) / float64(totalOps),
        LookupRatio: float64(lookupOps) / float64(totalOps),
    }
}

type SeedComposition struct {
    TotalOps   int
    ReaddirOps int
    LookupOps  int
    DirOps     int
    ReaddirRatio float64
    LookupRatio  float64
}

// 计算故障概率
func (analyzer *SeedCompositionAnalyzer) calculateFailureProbability(comp SeedComposition) float64 {
    baseProb := 0.1  // 基础故障概率

    // readdir操作多，更容易触发缓存查找，增加故障概率
    if comp.ReaddirRatio > 0.5 {
        baseProb += 0.2
    }

    // lookup操作多，说明正在进行缓存查找，增加故障概率
    if comp.LookupRatio > 0.2 {
        baseProb += 0.15
    }

    // 目录操作多，说明活跃度高，增加故障概率
    if comp.DirOps > 10 {
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
    ReaddirIntensity  map[int]float64  // 节点ID -> readdir强度
    LookupFrequency   map[int]float64  // 节点ID -> lookup频率
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
        ReaddirIntensity: make(map[int]float64),
        LookupFrequency: make(map[int]float64),
        Concurrency: 0,
    }

    for nodeID, p := range ps {
        readdirCount := 0
        lookupCount := 0
        totalOps := len(p.Calls)

        for _, call := range p.Calls {
            if call.Meta.Name == "getdents" || call.Meta.Name == "getdents64" {
                readdirCount++
            } else if call.Meta.Name == "openat" || call.Meta.Name == "openat2" {
                lookupCount++
            }
        }

        if totalOps > 0 {
            features.ReaddirIntensity[nodeID] = float64(readdirCount) / float64(totalOps)
            features.LookupFrequency[nodeID] = float64(lookupCount) / float64(totalOps)
        }

        // 估算并发度
        if readdirCount > 0 && lookupCount > 0 {
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

    // readdir强度相似度
    readdirIntensitySim := 0.0
    commonNodes := 0
    for nodeID := range a.ReaddirIntensity {
        if _, exists := b.ReaddirIntensity[nodeID]; exists {
            diff := math.Abs(a.ReaddirIntensity[nodeID] - b.ReaddirIntensity[nodeID])
            readdirIntensitySim += 1.0 - diff
            commonNodes++
        }
    }
    if commonNodes > 0 {
        readdirIntensitySim /= float64(commonNodes)
    }

    // lookup频率相似度
    lookupFreqSim := 0.0
    commonNodes = 0
    for nodeID := range a.LookupFrequency {
        if _, exists := b.LookupFrequency[nodeID]; exists {
            diff := math.Abs(a.LookupFrequency[nodeID] - b.LookupFrequency[nodeID])
            lookupFreqSim += 1.0 - diff
            commonNodes++
        }
    }
    if commonNodes > 0 {
        lookupFreqSim /= float64(commonNodes)
    }

    // 加权综合相似度
    return 0.4*nodeCountSim + 0.3*readdirIntensitySim + 0.3*lookupFreqSim
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

    for nodeID := range features.ReaddirIntensity {
        // 基于readdir强度预测流量
        readdirIntensity := features.ReaddirIntensity[nodeID]
        lookupFreq := features.LookupFrequency[nodeID]

        expectations[nodeID] = TrafficExpectation{
            InboundRate: readdirIntensity * 1000.0,  // 假设基准流量
            OutboundRate: readdirIntensity * 800.0,
            ConnectionCount: int(readdirIntensity * 5.0),
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
    dirAnalyzer      *DirOperationAnalyzer
    trafficMonitor    *TrafficMonitor
    trafficPredictor *TrafficPredictor
    failureInjector  *TrafficAwareFailureInjector
    roleAllocator    *NodeRoleAllocator
}

// 主决策函数
func (sys *IntegratedFailureSystem) DecideFailureInjection(ps []*Prog) FailureStrategy {
    // 步骤1：分析种子构成，推断dentry cache状态
    cacheStates := sys.dirAnalyzer.InferCacheState(ps)

    // 步骤2：分析节点角色
    nodeRoles := sys.roleAllocator.AllocateRoles(ps)

    // 步骤3：预测流量分布
    predictedTraffic := sys.trafficPredictor.PredictTraffic(ps)

    // 步骤4：获取实际流量
    actualTraffic := sys.trafficMonitor.trafficData

    // 步骤5：综合决策
    strategy := sys.makeIntegratedDecision(cacheStates, nodeRoles,
                                       predictedTraffic, actualTraffic)

    return strategy
}

// 综合决策
func (sys *IntegratedFailureSystem) makeIntegratedDecision(cacheStates map[int]CachePhase,
                                                          nodeRoles map[int]NodeRole,
                                                          predictedTraffic map[int]TrafficExpectation,
                                                          actualTraffic map[int]*NodeTraffic) FailureStrategy {
    candidates := make([]FailureStrategy, 0)

    // 候选策略1：基于dentry cache状态
    for nodeID, state := range cacheStates {
        if state == CacheLookup || state == CacheRevalidate {
            candidates = append(candidates, FailureStrategy{
                Type: FailureNodeCrash,
                Nodes: []int{nodeID},
                Timing: fmt.Sprintf("during_%s", state),
                Priority: 0.9,
                Source: "cache_state",
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
1. 实现目录操作分析器
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

#### 目录操作分析参数
```go
const (
    // 操作模式匹配参数
    PatternMatchThreshold = 0.7  // 模式匹配相似度阈值
    FuzzyMatchEnabled = true     // 是否启用模糊匹配

    // 节点角色推断参数
    ServerReaddirRatioThreshold = 0.5    // 服务器readdir比例阈值
    HybridReaddirRatioThreshold = 0.3    // 混合角色readdir比例阈值
    HybridLookupRatioThreshold = 0.3      // 混合角色lookup比例阈值
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
    ReaddirIntensityBonus = 0.2          // readdir强度加成
    LookupFrequencyBonus = 0.15          // lookup频率加成
    DirActivityBonus = 0.1            // 目录活跃度加成
    MaxFailureProbability = 0.8          // 最大故障概率

    // 故障选择参数
    DeviationThreshold = 0.5            // 流量偏差阈值
    ImportanceThreshold = 0.7           // 重要性阈值
    SimilarityThreshold = 0.7           // 相似度阈值
)
```

---

## 七、Dentry Cache与Stash功能的测试差异对比

### 7.1 功能特性对比

| 特性 | Stash功能 | Dentry Cache功能 |
|------|-----------|-----------------|
| **核心目的** | 节点离线时缓存文件数据 | 缓存远程节点的目录项信息 |
| **数据类型** | 文件内容数据 | 目录元数据（文件名、inode号等） |
| **生命周期** | 节点离线期间 | 持续存在，定期过期 |
| **状态机** | 复杂（NONE→STASHING→RESTORING→NONE） | 相对简单（查找→添加→删除→过期） |
| **并发度** | 高（多节点同时stash/restore） | 中等（多节点同时查找目录） |
| **内存管理** | 复杂（cache结构、page缓存） | 中等（cache_file_node、clearcache_item） |
| **锁复杂度** | 高（多个锁、锁顺序问题） | 中等（cache_list_lock、cache_pull_lock） |

### 7.2 Bug类型优先级对比

#### Stash功能的Bug优先级：
1. **并发错误**（最高）
   - 竞态条件：状态转换、多客户端访问
   - 死锁：多锁获取顺序问题
   - 数据竞争：cache指针、状态变量

2. **内存错误**（次高）
   - Use-after-free：cache结构体、inode引用
   - 内存泄漏：异常路径资源未释放
   - Double-free：多路径释放同一资源

3. **语义错误**（相对较少）
   - 数据不一致：元数据与实际数据不匹配
   - 状态机错误：状态转换逻辑错误
   - 恢复失败：文件恢复操作失败

#### Dentry Cache功能的Bug优先级：
1. **并发错误**（最高）
   - 竞态条件：缓存查找和添加、缓存重新验证
   - 死锁：多锁获取顺序问题
   - 数据竞争：cache_file_node、clearcache_item引用计数

2. **内存错误**（次高）
   - Use-after-free：cache_file_node结构体
   - 内存泄漏：异常路径资源未释放
   - Double-free：多路径释放同一资源

3. **语义错误**（相对较少）
   - 数据不一致：缓存文件元数据不匹配
   - 缓存过期错误：缓存验证逻辑错误
   - 缓存查找失败：查找操作失败处理

**关键差异**：
- **Stash功能**更关注状态机转换错误和恢复失败
- **Dentry Cache功能**更关注缓存过期和查找失败
- **两者**都高度关注并发错误和内存错误

### 7.3 测试重点对比

#### Stash功能测试重点：
1. **状态转换测试**
   - 节点离线时的stash状态转换
   - 节点上线时的restore状态转换
   - 状态转换过程中的并发操作

2. **数据完整性测试**
   - stash文件的数据完整性
   - 恢复后的数据一致性
   - 元数据和数据的同步

3. **资源管理测试**
   - stash文件的创建和删除
   - cache结构体的生命周期
   - 内存和磁盘资源的使用

#### Dentry Cache功能测试重点：
1. **缓存查找测试**
   - 缓存查找的正确性
   - 缓存未命中时的处理
   - 缓存查找的并发安全性

2. **缓存过期测试**
   - 缓存超时机制
   - 缓存重新验证逻辑
   - 节点状态变化时的缓存失效

3. **缓存一致性测试**
   - 多节点缓存的一致性
   - 缓存与实际目录状态的一致性
   - 缓存更新和删除的正确性

**关键差异**：
- **Stash功能**更关注数据完整性和状态转换
- **Dentry Cache功能**更关注缓存查找和过期机制
- **两者**都关注并发安全性和资源管理

### 7.4 故障注入策略对比

#### Stash功能的故障注入策略：
```c
// 针对stash功能的故障注入
syz_failure_node_offline(node_id, mode)           // 节点离线
syz_failure_node_online(node_id, delay)            // 节点上线
syz_failure_network_partition(node_group1, node_group2, duration)  // 网络分区
syz_failure_disk_full(node_id, threshold)         // 磁盘满
syz_failure_stash_in_progress(node_id, progress)   // stash进行中
syz_failure_restore_in_progress(node_id, progress) // restore进行中
```

**关键时机**：
- stash操作进行到30%、50%、70%时注入故障
- restore操作进行到30%、50%、70%时注入故障
- 文件写入过程中节点离线
- 大文件stash/restore过程中节点崩溃

#### Dentry Cache功能的故障注入策略：
```c
// 针对dentry cache的故障注入（基于节点/网络级别）
syz_failure_node_offline(node_id, mode)           // 节点离线，触发缓存失效
syz_failure_node_online(node_id, delay)            // 节点上线，触发缓存重新验证
syz_failure_network_partition(node_group1, node_group2, duration)  // 网络分区，触发缓存查找失败
syz_failure_disk_full(node_id, threshold)         // 磁盘满，触发缓存文件创建失败
syz_failure_memory_pressure(node_id, level)        // 内存压力，触发缓存分配失败
```

**关键时机**：
- 大量目录遍历过程中注入节点离线（触发缓存查找失败）
- 节点频繁上下线时注入网络分区（触发缓存重新验证失败）
- 缓存文件创建过程中注入磁盘满（触发缓存分配失败）
- 多个客户端同时访问同一目录时注入网络延迟（触发缓存并发问题）

**关键差异**：
- **Stash功能**更关注节点状态变化和文件操作时机
- **Dentry Cache功能**更关注目录操作模式和节点状态变化对缓存的影响
- **两者**都需要时机感知的故障注入，但故障注入都在节点/网络级别

### 7.5 种子生成和突变策略对比

#### Stash功能的种子生成：
1. **文件操作序列**
   - 生成大量文件打开/写入/关闭的序列
   - 在关键位置插入节点离线/上线操作
   - 生成并发访问同一文件的场景

2. **文件特征**
   - 大文件（触发长时间stash/restore）
   - 小文件（触发频繁stash/restore）
   - 硬链接文件（复杂的路径解析）

3. **节点状态变化**
   - 节点在文件操作过程中离线
   - 节点在stash过程中上线
   - 节点频繁上下线

#### Dentry Cache功能的种子生成：
1. **目录操作序列**
   - 生成大量目录遍历和文件查找的序列
   - 在关键位置插入节点离线/上线操作
   - 生成并发访问同一目录的场景

2. **目录结构特征**
   - 大目录（大量文件和子目录）
   - 深层目录结构（多层嵌套）
   - 符号链接（复杂的路径解析）

3. **缓存操作特征**
   - 缓存查找失败场景
   - 缓存重新验证场景
   - 缓存过期场景

**关键差异**：
- **Stash功能**更关注文件操作和文件特征
- **Dentry Cache功能**更关注目录操作和目录结构
- **两者**都需要考虑节点状态变化和并发场景

### 7.6 适应度指标对比

#### Stash功能的适应度指标：
1. **状态覆盖指标**
   - 覆盖所有stash状态转换路径
   - 覆盖所有锁的获取/释放组合
   - 覆盖所有文件操作类型

2. **并发场景覆盖**
   - 记录并发访问的文件数量
   - 记录并发执行的stash/restore操作数量
   - 记录锁竞争的次数

3. **故障场景覆盖**
   - 覆盖不同的故障注入时机
   - 覆盖不同的故障类型组合
   - 覆盖不同的文件大小和类型

#### Dentry Cache功能的适应度指标：
1. **缓存覆盖指标**
   - 覆盖所有缓存操作类型（查找、添加、删除、重新验证）
   - 覆盖所有锁的获取/释放组合
   - 覆盖所有缓存状态转换

2. **并发场景覆盖**
   - 记录并发访问的目录数量
   - 记录并发执行的缓存操作数量
   - 记录缓存冲突的次数

3. **缓存失效场景覆盖**
   - 覆盖不同的缓存失效时机
   - 覆盖不同的缓存失效类型
   - 覆盖不同的缓存恢复策略

**关键差异**：
- **Stash功能**更关注状态转换和文件操作
- **Dentry Cache功能**更关注缓存操作和缓存失效
- **两者**都关注并发场景和故障场景

### 7.7 种子调度和优先级对比

#### Stash功能的种子调度：
- 优先调度触发stash状态的种子
- 优先调度在故障场景下成功的种子
- 优先调度覆盖新状态转换路径的种子
- 优先调度包含大文件操作的种子

#### Dentry Cache功能的种子调度：
- 优先调度触发缓存查找的种子
- 优先调度在缓存失效场景下成功的种子
- 优先调度覆盖新缓存操作路径的种子
- 优先调度包含大目录操作的种子

**关键差异**：
- **Stash功能**优先调度与stash状态相关的种子
- **Dentry Cache功能**优先调度与缓存操作相关的种子
- **两者**都优先调度覆盖新路径的种子

### 7.8 总结

#### 主要差异：
1. **功能特性**：Stash关注文件数据缓存，Dentry Cache关注目录元数据缓存
2. **Bug类型**：Stash更关注状态机错误，Dentry Cache更关注缓存过期错误
3. **测试重点**：Stash关注数据完整性，Dentry Cache关注缓存查找和过期
4. **故障注入**：Stash关注节点状态变化，Dentry Cache关注缓存操作故障
5. **种子生成**：Stash关注文件操作，Dentry Cache关注目录操作
6. **适应度指标**：Stash关注状态覆盖，Dentry Cache关注缓存覆盖

#### 共同点：
1. **并发错误**都是最高优先级的bug类型
2. **内存错误**都是次高优先级的bug类型
3. **都需要时机感知的故障注入**
4. **都需要考虑并发场景**
5. **都需要优化种子调度和优先级**

#### 测试策略建议：
针对dentry cache功能的模糊测试，应该：
1. **侧重缓存操作**：设计针对缓存查找、添加、删除、重新验证的故障注入
2. **关注目录结构**：生成包含大目录、深层目录、符号链接的测试用例
3. **优化缓存失效场景**：设计缓存过期、缓存损坏、缓存溢出的测试场景
4. **提升缓存覆盖**：设计覆盖所有缓存操作类型和状态转换的适应度指标
5. **优化种子调度**：优先调度触发缓存操作和缓存失效的种子

---

## 八、总结

### 8.1 核心设计原则

1. **非侵入式设计**：尽量不修改现有Monarch框架和Linux内核代码
2. **状态感知**：通过分析测试用例和网络流量来推断系统状态
3. **智能决策**：基于多维度信息（状态、角色、流量）进行故障注入决策
4. **闭环优化**：将故障注入效果反馈到种子生成和故障选择中
5. **可扩展性**：设计支持多种故障类型和注入策略

### 8.2 关键创新点

1. **基于目录操作模式的状态推断**：通过分析测试用例中的目录操作序列来推断dentry cache状态
2. **基于种子构成的流量预测**：利用测试用例的特征来预测网络流量分布
3. **多维度综合决策**：结合状态、角色、流量等多个维度进行故障注入决策
4. **动态拓扑感知**：根据实际连接关系和流量模式来生成网络分区策略

### 8.3 预期效果

1. **提高bug发现率**：通过状态感知和智能故障选择，更容易触发dentry cache功能中的并发错误和内存错误
2. **提升测试效率**：通过流量预测和闭环优化，减少无效的故障注入
3. **增强测试覆盖**：通过多维度分析，覆盖更多的测试场景和状态组合
4. **降低实现复杂度**：通过非侵入式设计，减少对现有框架的修改

---

## 九、附录

### 9.1 关键代码位置索引

#### Dentry Cache功能核心代码
- 缓存查找：[hmdfs_dentryfile.c:1224-1248](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1224-L1248)
- 缓存添加：[hmdfs_dentryfile.c:1273-1313](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1273-L1313)
- 缓存重新验证：[hmdfs_dentryfile.c:2230-2252](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2230-L2252)
- 内存管理：[hmdfs_dentryfile.c:1205-1212](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L1205-L1212)
- 缓存查找逻辑：[hmdfs_dentryfile.c:2139-2196](file:///d:\科研\博士复现\原版备份\Monarch-master\hmdfs\hmdfs_dentryfile.c#L2139-L2196)

#### Monarch故障注入代码
- 故障枚举：[proc.go:466-551](file:///d:\科研\博士复现\原版备份\Monarch-master\src\syz-fuzzer\proc.go#L466-L551)
- 故障注入：[mutation.go:1021-1111](file:///d:\科研\博士复现\原版备份\Monarch-master\src\prog\mutation.go#L1021-L1111)
- 执行接口：[common_linux.h:114-259](file:///d:\科研\博士复现\原版备份\Monarch-master\src\executor\common_linux.h#L114-L259)

### 9.2 术语表

| 术语 | 英文 | 说明 |
|------|------|------|
| Dentry Cache | Dentry Cache | hmdfs的目录项缓存功能 |
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
| 缓存过期错误 | Cache Expiration Errors | 缓存验证和过期相关的错误 |
| 缓存查找失败 | Cache Lookup Failures | 缓存查找操作失败 |
| 故障注入 | Fault Injection | 主动注入故障来测试系统 |
| 网络分区 | Network Partition | 网络连接被切断 |
| 节点崩溃 | Node Crash | 节点突然停止运行 |
| 流量监控 | Traffic Monitoring | 监控网络流量 |
| 状态感知 | State Awareness | 感知系统当前状态 |
| 拓扑感知 | Topology Awareness | 感知网络拓扑结构 |
| 种子生成 | Seed Generation | 生成测试用例 |
| 突变 | Mutation | 修改测试用例 |
| 适应度指标 | Fitness Metrics | 评估测试用例质量的指标 |

### 9.3 参考配置文件

当前Monarch使用的配置文件位于`fuzz-config`目录下，可以参考：
- [fuzz-config/all-config/cephfs/1-2-2-2/cephfs-normal.cfg](file:///d:\科研\博士复现\原版备份\Monarch-master\fuzz-config\all-config\cephfs\1-2-2-2\cephfs-normal.cfg)
- [fuzz-config/all-config/cephfs/1-2-2-2/cephfs-failure.cfg](file:///d:\科研\博士复现\原版备份\Monarch-master\fuzz-config\all-config\cephfs\1-2-2-2\cephfs-failure.cfg)

配置文件格式说明参见：[fuzz-config/README.md](file:///d:\科研\博士复现\原版备份\Monarch-master\fuzz-config\README.md)

---

**文档版本**：1.0
**创建日期**：2026-03-08
**最后更新**：2026-03-08
**维护者**：HMDFS模糊测试项目组
