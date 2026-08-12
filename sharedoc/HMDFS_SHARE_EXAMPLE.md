# HMDFS .share 目录使用示例

本文档提供一个完整的示例，展示如何使用 HMDFS 的 `.share` 目录进行文件共享操作。

## 1. 概念说明

### .share 目录
- `.share` 是 HMDFS 内部用于实现文件级精细共享的保留目录
- 它不是一个物理目录，而是在内存中动态管理的共享表的抽象表示
- 通过 `.share` 目录，用户可以实现单个文件的精确共享，而不是整个目录

### 共享机制
- 使用 `HMDFS_IOC_SET_SHARE_PATH` ioctl 命令将文件添加到共享表
- 其他设备可以通过 `/.share/<filename>` 路径访问共享文件
- 共享项有超时机制（默认120秒），超时后自动从共享表中移除

## 2. 示例程序

### 编译示例程序

```bash
gcc hmdfs_share_example.c -o hmdfs_share_example
```

### 运行示例程序

**前提条件**：
1. HMDFS 已正确安装并加载
2. HMDFS 已挂载到 `/mnt/hmdfs` 目录

**步骤1：创建一个测试文件**

```bash
echo "Hello HMDFS Share" > /mnt/hmdfs/test_share.txt
```

**步骤2：使用示例程序将文件添加到 .share 目录**

```bash
./hmdfs_share_example /mnt/hmdfs /mnt/hmdfs/test_share.txt
```

**预期输出**：
```
1. 打开HMDFS挂载点: /mnt/hmdfs
2. 打开要共享的文件: /mnt/hmdfs/test_share.txt
3. 使用ioctl命令将文件添加到.share目录...
4. 文件共享成功！
   共享文件: /mnt/hmdfs/test_share.txt
   可通过其他设备的 /.share/test_share.txt 访问该文件
```

## 3. 验证共享

### 在其他设备上访问共享文件

**步骤1：确保其他设备已挂载 HMDFS**

```bash
mount -t hmdfs -o local_dst=/home/hmdfs/local,node_id=test2 192.168.1.101:/ /mnt/hmdfs
```

**步骤2：通过 .share 目录访问共享文件**

```bash
cat /mnt/hmdfs/.share/test_share.txt
```

**预期输出**：
```
Hello HMDFS Share
```

## 4. 完整的使用流程

### 设备1（共享方）

```bash
# 1. 创建测试文件
echo "This is a shared file" > /mnt/hmdfs/shared_document.txt

# 2. 运行共享程序
./hmdfs_share_example /mnt/hmdfs /mnt/hmdfs/shared_document.txt
```

### 设备2（访问方）

```bash
# 1. 查看共享文件列表
ls -la /mnt/hmdfs/.share/

# 2. 读取共享文件内容
cat /mnt/hmdfs/.share/shared_document.txt

# 3. 复制共享文件到本地
cp /mnt/hmdfs/.share/shared_document.txt /home/user/local_copy.txt
```

## 5. 注意事项

1. **仅支持普通文件**：目前 HMDFS 只允许共享普通文件，不支持目录、链接等
2. **超时机制**：共享项默认120秒超时，超时后需要重新共享
3. **访问权限**：共享文件的访问权限由原文件的权限决定
4. **文件名限制**：共享文件名长度不能超过 NAME_MAX（通常为255字节）
5. **共享表大小**：默认最多支持128个共享项

## 6. 卸载和清理

```bash
# 卸载 HMDFS
umount /mnt/hmdfs

# 清理测试文件
rm -f /mnt/hmdfs/test_share.txt /home/user/local_copy.txt
```

## 7. 故障排除

### 1. 共享失败

**症状**：`设置共享路径失败: Operation not permitted`

**可能原因**：
- 尝试共享非普通文件
- 没有足够的权限
- 文件已在共享表中

### 2. 无法访问共享文件

**症状**：`No such file or directory`

**可能原因**：
- 共享项已超时
- 源文件已被删除
- 共享时指定的文件名与实际文件名不一致

### 3. 共享表已满

**症状**：`设置共享路径失败: Too many open files`

**解决方法**：
- 等待旧的共享项超时自动清理
- 或重启 HMDFS 模块清空共享表

## 8. 与直接目录共享的区别

| 特性 | 直接目录共享 | .share目录共享 |
|------|--------------|---------------|
| 共享粒度 | 整个目录 | 单个文件 |
| 设置方式 | mount命令时指定 | ioctl命令动态添加 |
| 访问方式 | 直接访问挂载目录 | 通过/.share/<filename>访问 |
| 灵活性 | 较低 | 较高 |
| 适用场景 | 批量文件共享 | 单个文件精确共享 |

## 9. 代码说明

示例程序中使用的核心结构：

```c
struct hmdfs_share_control {
    __u32 src_fd;      // 要共享的文件描述符
    char cid[64];      // 设备ID，"0"表示所有设备可访问
};
```

核心ioctl命令：
```c
ioctl(fd, HMDFS_IOC_SET_SHARE_PATH, &sc)
```

其中：
- `fd` 是HMDFS挂载点目录的文件描述符
- `sc` 是共享控制结构体
- `HMDFS_IOC_SET_SHARE_PATH` 是设置共享路径的ioctl命令