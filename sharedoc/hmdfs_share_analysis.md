# HMDFS 数据共享功能详细分析

## 1. 共享内容

HMDFS 的数据共享功能主要用于**共享文件**（通过 `struct file` 指针管理）。具体来说，它允许将本地文件共享给其他设备或所有设备。

## 2. 共享实现机制

### 2.1 核心数据结构

#### 共享表（Share Table）
- 每个超级块（`struct hmdfs_sb_info`）维护一个共享表
- 用于管理所有共享项，包括插入、查找、更新和删除操作
- 最大容量：128 个共享项（`HMDFS_SHARE_ITEMS_MAX`）

#### 共享项（Share Item）
```c
struct hmdfs_share_item {
    struct file *file;           // 共享的文件指针
    struct qstr relative_path;   // 相对路径
    char cid[HMDFS_CID_SIZE];    // 目标设备CID
    bool opened;                 // 是否被打开
    bool timeout;                // 是否超时
    struct list_head list;       // 链表节点
    struct delayed_work d_work;  // 延迟工作队列（超时处理）
    struct hmdfs_share_table *hst; // 所属共享表
};
```

### 2.2 共享目录机制

- HMDFS 使用特殊目录 `.share`（`SHARE_RESERVED_DIR`）实现共享功能
- 当文件位于 `.share` 目录下时，会被识别为共享文件
- 通过 `in_share_dir()` 和 `is_share_dir()` 函数判断文件是否在共享目录中

### 2.3 共享范围

- **特定设备共享**：通过指定目标设备的 CID 进行共享
- **全局共享**：使用常量 `SHARE_ALL_DEVICE`（值为 "0"）共享给所有设备

## 3. 共享生命周期管理

### 3.1 超时机制

- 共享项默认超时时间：120 秒（`HMDFS_SHARE_ITEM_TIMEOUT_S`）
- 使用延迟工作队列（`struct delayed_work`）实现超时处理
- 超时后自动移除共享项并释放资源
- 当共享文件被打开时，会取消超时定时器
- 当共享文件被关闭时，会重新启动超时定时器

### 3.2 共享项状态转换

```
插入共享项 → 未打开状态 → 打开状态 → 关闭状态 → 超时 → 移除
     ↑                       ↓                 ↑
     └───────────────────────┘                 └───────────
```

## 4. 核心函数分析

### 4.1 初始化与清理

- `hmdfs_init_share_table()`：初始化共享表，创建工作队列
- `hmdfs_clear_share_table()`：清理共享表，释放所有共享项
- `hmdfs_clear_first_item()`：当共享项数量超过限制时，清理第一个非超时项

### 4.2 共享项管理

- `hmdfs_lookup_share_item()`：根据相对路径查找共享项
- `insert_share_item()`：插入新的共享项
- `update_share_item()`：更新共享项（文件指针或目标设备）
- `remove_and_release_share_item()`：移除并释放共享项

### 4.3 访问控制

- `hmdfs_check_share_access_permission()`：检查设备是否有权访问共享文件
  - 检查 CID 是否匹配或是否为全局共享
  - 设置共享项为打开状态并取消超时

### 4.4 状态管理

- `hmdfs_close_share_item()`：关闭共享项，重置状态并重启超时
- `reset_item_opened_status()`：重置共享项的打开状态
- `hmdfs_clear_share_item_offline()`：当设备离线时，清理相关共享项

### 4.5 共享文件识别

- `hmdfs_is_share_file()`：识别文件是否为共享文件
  - 递归检查文件及其下层文件
  - 基于文件类型和路径判断

### 4.6 路径管理

- `get_path_from_share_table()`：根据相对路径从共享表中获取实际文件路径

## 5. 共享流程

1. **创建共享**：将文件放入 `.share` 目录，系统自动创建共享项
2. **查找共享**：通过相对路径在共享表中查找共享项
3. **访问共享**：检查访问权限，若允许则打开共享文件
4. **使用共享**：远程设备可以访问共享文件
5. **关闭共享**：关闭共享文件，重置状态并重启超时
6. **超时清理**：超时后自动清理共享项

## 6. 共享限制

- 最大共享项数量：128 个
- 共享项默认超时：120 秒
- 共享文件必须位于 `.share` 目录下

## 7. 关键代码位置

| 功能 | 文件位置 | 关键函数 |
|------|----------|----------|
| 共享表管理 | hmdfs/hmdfs_share.c | `hmdfs_init_share_table()` |
| 共享项管理 | hmdfs/hmdfs_share.c | `hmdfs_lookup_share_item()`, `insert_share_item()` |
| 访问控制 | hmdfs/hmdfs_share.c | `hmdfs_check_share_access_permission()` |
| 超时处理 | hmdfs/hmdfs_share.c | `share_item_timeout_work()` |
| 共享文件识别 | hmdfs/hmdfs_share.c | `hmdfs_is_share_file()` |
| 共享定义 | hmdfs/hmdfs_share.h | 数据结构和常量定义 |

## 8. 总结

HMDFS 的数据共享功能是通过共享表和共享项机制实现的，允许将文件通过 `.share` 目录共享给特定设备或所有设备。它具有以下特点：

- **基于文件的共享**：直接共享文件，而非目录或其他资源
- **灵活的共享范围**：支持特定设备共享和全局共享
- **自动生命周期管理**：超时自动清理，减少资源占用
- **严格的访问控制**：基于 CID 的访问权限检查
- **高效的共享项查找**：通过相对路径快速查找共享项

这种共享机制使得 HMDFS 能够在分布式环境中高效地实现设备间的文件共享，同时保持良好的资源管理和访问控制。