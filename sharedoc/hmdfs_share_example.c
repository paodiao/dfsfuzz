#include <stdio.h>
#include <stdlib.h>
#include <fcntl.h>
#include <sys/ioctl.h>
#include <unistd.h>
#include <string.h>

// HMDFS共享控制结构体定义
struct hmdfs_share_control {
    __u32 src_fd;
    char cid[64];
};

// 假设HMDFS的ioctl命令定义
#define HMDFS_IOC_MAGIC 'H'
#define HMDFS_IOC_SET_SHARE_PATH _IOW(HMDFS_IOC_MAGIC, 11, struct hmdfs_share_control)

int main(int argc, char *argv[]) {
    int fd, src_fd;
    struct hmdfs_share_control sc;
    char cid[64] = "0"; // 0表示所有设备可访问
    
    if (argc != 3) {
        fprintf(stderr, "Usage: %s <hmdfs_mount_point> <file_to_share>\n", argv[0]);
        fprintf(stderr, "Example: %s /mnt/hmdfs /mnt/hmdfs/myfile.txt\n", argv[0]);
        return 1;
    }
    
    const char *hmdfs_mount = argv[1];
    const char *file_to_share = argv[2];
    
    // 1. 打开HMDFS挂载点目录
    printf("1. 打开HMDFS挂载点: %s\n", hmdfs_mount);
    fd = open(hmdfs_mount, O_RDONLY | O_DIRECTORY);
    if (fd < 0) {
        perror("打开HMDFS挂载点失败");
        return 1;
    }
    
    // 2. 打开要共享的文件
    printf("2. 打开要共享的文件: %s\n", file_to_share);
    src_fd = open(file_to_share, O_RDONLY);
    if (src_fd < 0) {
        perror("打开要共享的文件失败");
        close(fd);
        return 1;
    }
    
    // 3. 设置共享控制结构体
    memset(&sc, 0, sizeof(sc));
    sc.src_fd = src_fd;
    strncpy(sc.cid, cid, sizeof(sc.cid) - 1);
    
    // 4. 使用ioctl命令将文件添加到.share目录
    printf("3. 使用ioctl命令将文件添加到.share目录...\n");
    if (ioctl(fd, HMDFS_IOC_SET_SHARE_PATH, &sc) < 0) {
        perror("设置共享路径失败");
        close(src_fd);
        close(fd);
        return 1;
    }
    
    // 5. 关闭文件描述符
    close(src_fd);
    close(fd);
    
    // 6. 显示共享结果
    printf("4. 文件共享成功！\n");
    printf("   共享文件: %s\n", file_to_share);
    printf("   可通过其他设备的 /.share/<filename> 访问该文件\n");
    
    return 0;
}