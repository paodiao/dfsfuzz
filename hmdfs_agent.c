/*
 * HMDFS Agent - 合并hmdfs_dfs_service和hmdfs_proxy_local的功能
 * 功能：
 * 1. 管理远程节点列表
 * 2. 主动连接到远程节点
 * 3. 监听本地端口，接收远程节点连接
 * 4. 处理HMDFS模块的通知
 * 5. 发送CMD_UPDATE_SOCKET命令
 * 6. 实现心跳检测和设备状态管理
 * 7. 支持日志文件记录
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/types.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <sys/statfs.h>
#include <sys/sysmacros.h>
#include <fcntl.h>
#include <errno.h>
#include <time.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <pthread.h>
#include <sys/inotify.h>
#include <signal.h>
#include <sys/select.h>
#include <limits.h>
#include <linux/limits.h>
#include <stdint.h>
#include <ctype.h>
#include <stdarg.h>     // 提供va_list等可变参数支持
#include <sys/time.h>  // 提供gettimeofday函数支持
#include <linux/tcp.h>
#include <poll.h>
#include <sys/un.h>

/* 定义小端序类型 - 兼容内核模块中的类型定义 */
typedef uint16_t __le16;
typedef uint32_t __le32;
typedef uint8_t __u8;
typedef uint32_t __u32;

/* 网络字节序转换宏 */
#define cpu_to_le16(x) htons(x)
#define le16_to_cpu(x) ntohs(x)
#define cpu_to_le32(x) htonl(x)
#define le32_to_cpu(x) ntohl(x)

/* HMDFS常量定义 */
#define HMDFS_CID_SIZE 64
#define HMDFS_KEY_SIZE 32
#define HMDFS_SYSFS_PATH "/sys/fs/hmdfs/"
#define HMDFS_CMD_FILE "/cmd"
#define HMDFS_MAX_MESSAGE_LEN 8 * 1024 * 1024  // 8MB

/* 通知类型 */
#define NOTIFY_GET_SESSION 0
#define NOTIFY_OFFLINE 1
#define NOTIFY_NONE 2

/* poll消息类型 */
#define NONE_EVENT -1
#define READ_EVENT 1
#define TIME_OUT_EVENT 0

/* 命令类型 */
#define CMD_UPDATE_SOCKET 0
#define CMD_UPDATE_DEVSL 1
#define CMD_OFF_LINE 2

/* 连接状态 */
enum SocketStat {
    SOCKET_STAT_ACCEPT = 0,
    SOCKET_STAT_OPEN,
};

/* 无效套接字 */
#define INVALID_SOCKET_FD -1

/* 日志级别 */
#define LOG_LEVEL_ERROR 0
#define LOG_LEVEL_WARNING 1
#define LOG_LEVEL_INFO 2
#define LOG_LEVEL_DEBUG 3

/* 远程节点最大数量 */
#define MAX_REMOTE_NODES 10

/* 客户端最大数量 */
#define MAX_CLIENTS 100

/* 心跳检测配置 */
#define HEARTBEAT_INTERVAL 30      // 心跳间隔（秒）
#define HEARTBEAT_TIMEOUT 90        // 心跳超时时间（秒）

/* 恢复检查间隔 */
#define RECOVERY_CHECK_INTERVAL 10  // 每10秒检查一次

#define MAX_LOG_LENGTH 1024

int g_log_level = LOG_LEVEL_INFO;
pthread_mutex_t log_mutex = PTHREAD_MUTEX_INITIALIZER;
FILE *g_log_file = NULL;

/* 设备状态枚举定义 */
typedef enum {
    DEVICE_STATUS_OFFLINE = 0,       // 设备离线
    DEVICE_STATUS_ONLINE = 1,        // 设备在线
    DEVICE_STATUS_AUTHENTICATED = 2, // 设备已认证
    DEVICE_STATUS_CONNECTED = 3,     // 设备已连接
    DEVICE_STATUS_ERROR = 4,         // 设备出现错误
    DEVICE_STATUS_TIMEOUT = 5        // 设备心跳超时
} device_status;

/* 远程节点配置 */
typedef struct {
    char ip[INET_ADDRSTRLEN];    // 远程节点IP
    int port;                    // 远程节点端口
    char cid[HMDFS_CID_SIZE + 1];    // 远程节点CID
    uint32_t devsl;             // 远程节点设备安全级别
    device_status status;        // 节点状态
    time_t last_heartbeat;       // 上次心跳时间
    time_t last_update;          // 上次状态更新时间
    int fd;                      // 连接文件描述符
    volatile int connecting;     // 重连进行中（防止多线程重复 connect）
} remote_node;

/* 客户端连接结构体 */
typedef struct {
    int client_fd;                // 客户端文件描述符
    remote_node *node;            // 关联的远程节点
    struct agent_config *config;  // 代理配置
    pthread_t thread_id;          // 线程ID
} peer_connection;

/* 本地代理配置 */
typedef struct {
    int local_port;               // 本地监听端口，0表示不启用
    char local_ip[INET_ADDRSTRLEN];  // 本机IP地址
    char local_cid[HMDFS_CID_SIZE + 1]; // 本地CID
    char log_file[256];           // 日志文件路径
    char mount_point[256];        // 挂载点路径
    remote_node nodes[MAX_REMOTE_NODES]; // 远程节点列表
    int node_count;               // 远程节点数量
} agent_config;

/* 通知参数结构体 */
typedef struct {
    int32_t notify;             // 通知类型
    int32_t fd;                 // 文件描述符
    uint8_t remote_cid[HMDFS_CID_SIZE]; // 远程设备标识
} __attribute__((packed)) notify_param;

/* 更新套接字参数结构体 */
typedef struct {
    int32_t cmd;                // 命令类型
    int32_t newfd;              // 新文件描述符
    uint32_t devsl;             // 设备安全级别
    uint8_t status;             // 设备状态
    uint8_t masterkey[HMDFS_KEY_SIZE]; // 设备主密钥
    uint8_t cid[HMDFS_CID_SIZE]; // 设备标识
} __attribute__((packed)) update_socket_param;

typedef struct {
    int32_t cmd;
    uint8_t remote_cid[HMDFS_CID_SIZE];
} __attribute__((packed)) offline_param;

/* HMDFS命令结构 */
typedef struct {
    __u8 reserved;     // 保留字段
    __u8 cmd_flag;     // 命令标志: 0=C_REQUEST(请求), 1=C_RESPONSE(响应)
    __u8 command;      // 命令类型
    __u8 reserved2;    // 保留字段
} __attribute__((packed)) hmdfs_cmd;

/* HMDFS消息头结构 */
typedef struct {
    __u8 magic;       // 消息魔术字: HMDFS_MSG_MAGIC (0xF7)
    __u8 version;     // HMDFS版本号: 0x40
    __le16 reserved;  // 保留字段
    __le32 data_len;  // 数据长度 (小端序)
    hmdfs_cmd operations; // 命令操作
    __le32 ret_code;  // 返回码 (小端序)
    __le32 msg_id;    // 消息ID (小端序)
    __le32 reserved1; // 保留字段
} __attribute__((packed)) hmdfs_head_cmd;

/* 全局变量 */
agent_config g_config;
int g_server_fd = -1;
int g_inotify_fd = -1;
int g_watch_fd = -1;
char g_cmd_file_path[PATH_MAX];
//peer_connection *g_connections = NULL;
int g_connection_count = 0;
volatile int g_running = 1;

/* 心跳检测相关变量 */
pthread_t g_heartbeat_thread = 0; // 心跳检测线程ID
int g_heartbeat_enabled = 0;      // 是否启用心跳检测

pthread_t g_listener_thread = 0;
pthread_t g_connector_thread = 0;
pthread_t g_notify_handler_thread = 0;
pthread_t g_connect_checker_thread = 0;
pthread_t g_sysfs_checker_thread = 0;
pthread_t g_netup_listener_thread = 0;
volatile int g_netup_srv_fd = -1;   /* netup unix socket fd, closed by signal handler to wake accept */

/* executor 的 syz_failure_net_up 通过此 unix socket 通知 agent 网络已恢复 */
#define NETUP_SOCK_PATH "/tmp/hmdfs-netup.sock"

/* 互斥锁 */
pthread_mutex_t g_connection_mutex = PTHREAD_MUTEX_INITIALIZER;
pthread_mutex_t g_device_mutex = PTHREAD_MUTEX_INITIALIZER;
pthread_mutex_t g_inotify_mutex = PTHREAD_MUTEX_INITIALIZER;

/* 日志函数 */
void log_message(int level, const char *format, ...) {
    va_list args;
    char time_str[20];
    char message[MAX_LOG_LENGTH];
    const char *level_str[] = {"ERROR", "WARNING", "INFO", "DEBUG"};
    time_t now;
    struct tm *tm_info;
    
    // 参数检查
    if (level < 0 || level > 3 || format == NULL) {
        return;
    }
    
    if (level > g_log_level) {
        return;
    }
    
    // 线程安全
    pthread_mutex_lock(&log_mutex);
    
    // 获取时间
    now = time(NULL);
    tm_info = localtime(&now);
    strftime(time_str, sizeof(time_str), "%Y-%m-%d %H:%M:%S", tm_info);
    
    // 格式化消息
    va_start(args, format);
    vsnprintf(message, sizeof(message), format, args);
    va_end(args);
    
    // 输出到控制台
    printf("[%s] [%s] %s\n", time_str, level_str[level], message);
    
    // 输出到日志文件
    if (g_log_file != NULL) {
        if (fprintf(g_log_file, "[%s] [%s] %s\n", 
                    time_str, level_str[level], message) < 0) {
            // 文件写入错误处理
            printf("Failed to write to log file!\n");
        }
        fflush(g_log_file);
    }
    
    pthread_mutex_unlock(&log_mutex);
}

/* 初始化日志 */
int init_log(const char *log_file) {
    if (log_file != NULL && strlen(log_file) > 0) {
        g_log_file = fopen(log_file, "a");
        if (g_log_file == NULL) {
            log_message(LOG_LEVEL_ERROR, "Failed to open log file %s: %s", log_file, strerror(errno));
            return -1;
        }
        setvbuf(g_log_file, NULL, _IOLBF, 0);
        log_message(LOG_LEVEL_INFO, "Log file initialized: %s", log_file);
    }
    return 0;
}

/**
 * 获取挂载点对应的设备号
 * @param mountpoint 挂载点路径
 * @return 设备号（dev_t，转换为unsigned int）
 * @retval 0 表示失败
 */
uint64_t get_mount_dev_id(const char *mountpoint) {
    // 方法3：回退到stat挂载点本身
    struct stat st;
    if (stat(mountpoint, &st) == 0) {
        return st.st_dev;
    }
    
    // 方法1：尝试通过statfs获取（最快）
    struct statfs fs;
    if (statfs(mountpoint, &fs) == 0) {
        // f_fsid通常包含设备号
        if (fs.f_fsid.__val[0] != 0 || fs.f_fsid.__val[1] != 0) {
            // 对于大多数文件系统，f_fsid[0]是设备号
            return fs.f_fsid.__val[0];
        }
    }
    
    // 方法2：通过/proc/mountinfo获取（最可靠）
    FILE *fp = fopen("/proc/self/mountinfo", "r");
    if (fp) {
        char line[1024];
        unsigned int major = 0, minor = 0;
        
        while (fgets(line, sizeof(line), fp)) {
            // 移除换行符
            line[strcspn(line, "\n")] = 0;
            
            char *target_start = strstr(line, " - ");
            if (!target_start) continue;
            
            // 找到挂载点路径开始的位置
            char *target = target_start + 3;
            target = strchr(target, ' ');
            if (!target) continue;
            target++;
            
            // 检查是否是我们需要的挂载点
            char *space = strchr(target, ' ');
            if (space) *space = 0;
            
            if (strcmp(target, mountpoint) == 0) {
                // 提取设备号（格式：major:minor）
                sscanf(line, "%*d %*d %u:%u", &major, &minor);
                break;
            }
        }
        fclose(fp);
        
        if (major != 0 || minor != 0) {
            return makedev(major, minor);
        }
    }
    
    return 0;
}

int socketConnected(int sock) {
    if(sock < 0) {
        return 0;
    }
    struct tcp_info info;
    memset(&info, 0, sizeof(info));
    socklen_t len = sizeof(info);
    if (getsockopt(sock, IPPROTO_TCP, TCP_INFO, &info, &len) < 0) {
        return 0;
    }
    return info.tcpi_state;
}

/* 生成随机CID - 连续16进制格式 */
void generate_cid(char *cid) {
    if (!cid) return;
    
    // 生成随机数种子
    static int seed_initialized = 0;
    if (!seed_initialized) {
        struct timeval tv;
        gettimeofday(&tv, NULL);
        srand(tv.tv_sec * 1000000 + tv.tv_usec);
        seed_initialized = 1;
    }
    
    // 生成足够的随机数来填满整个64字节缓冲区
    // 每个%08x生成8个十六进制字符
    uint32_t r1 = rand();
    uint32_t r2 = rand();
    uint32_t r3 = rand();
    uint32_t r4 = rand();
    uint32_t r5 = rand();
    uint32_t r6 = rand();
    uint32_t r7 = rand();
    uint32_t r8 = rand();
    
    // 生成64字节的连续16进制字符串 (64个字符 + 1个结束符)
    snprintf(cid, HMDFS_CID_SIZE + 1, "%08x%08x%08x%08x%08x%08x%08x%08x",
             r1, r2, r3, r4, r5, r6, r7, r8);
    cid[HMDFS_CID_SIZE] = '\0';
}

/* 检查sysfs是否可用 */
int check_sysfs_available(void) {
    struct stat st;
    char cmd_path[PATH_MAX];
    int fd;
    
    // 检查sysfs目录是否存在
    if (stat(HMDFS_SYSFS_PATH, &st) != 0 || !S_ISDIR(st.st_mode)) {
        return 0; // sysfs不可用
    }
    uint64_t hmdfs_s_dev = get_mount_dev_id(g_config.mount_point);
    if(hmdfs_s_dev == 0) {
        return 0;
    }
    // 构建cmd文件路径
    snprintf(cmd_path, PATH_MAX, "%s%lu%s", HMDFS_SYSFS_PATH, hmdfs_s_dev, HMDFS_CMD_FILE);
    
    // 检查cmd文件是否存在
    if (stat(cmd_path, &st) != 0) {
        return 0; // cmd文件不存在
    }
    
    // 测试打开文件
    fd = open(cmd_path, O_RDWR);
    if (fd < 0) {
        return 0; // 无法打开cmd文件
    }
    close(fd);
    
    return 1; // sysfs可用
}

int init_cmd_path(void) {
    uint64_t hmdfs_s_dev = get_mount_dev_id(g_config.mount_point);
    if(hmdfs_s_dev == 0) {
        log_message(LOG_LEVEL_ERROR, "can not get mount dev id");
        return -1;
    }
    log_message(LOG_LEVEL_INFO, "get mount dev id %lu", hmdfs_s_dev);
    snprintf(g_cmd_file_path, PATH_MAX, "%s%lu%s", HMDFS_SYSFS_PATH, hmdfs_s_dev, HMDFS_CMD_FILE);
    return 0;
}

/* 初始化inotify，监控HMDFS sysfs */
int init_inotify(void) {
    uint64_t hmdfs_s_dev = get_mount_dev_id(g_config.mount_point);
    if(hmdfs_s_dev == 0) {
        log_message(LOG_LEVEL_ERROR, "can not get mount dev id");
        return -1;
    }
    log_message(LOG_LEVEL_INFO, "get mount dev id %lu", hmdfs_s_dev);
    // 构建命令文件路径
    snprintf(g_cmd_file_path, PATH_MAX, "%s%lu%s", HMDFS_SYSFS_PATH, hmdfs_s_dev, HMDFS_CMD_FILE);
    
    pthread_mutex_lock(&g_inotify_mutex);
    
    // 初始化inotify
    g_inotify_fd = inotify_init();
    if (g_inotify_fd < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to initialize inotify: %s", strerror(errno));
        pthread_mutex_unlock(&g_inotify_mutex);
        return -1;
    }
    
    // 添加监控
    g_watch_fd = inotify_add_watch(g_inotify_fd, HMDFS_SYSFS_PATH, IN_CREATE | IN_MODIFY);
    if (g_watch_fd < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to add inotify watch: %s", strerror(errno));
        close(g_inotify_fd);
        g_inotify_fd = -1;
        pthread_mutex_unlock(&g_inotify_mutex);
        return -1;
    }
    
    log_message(LOG_LEVEL_INFO, "Inotify initialized, watching %s", HMDFS_SYSFS_PATH);
    
    pthread_mutex_unlock(&g_inotify_mutex);
    return 0;
}

/* 清理inotify资源 */
void cleanup_inotify(void) {
    pthread_mutex_lock(&g_inotify_mutex);
    
    if (g_watch_fd != -1) {
        inotify_rm_watch(g_inotify_fd, g_watch_fd);
        g_watch_fd = -1;
    }
    if (g_inotify_fd != -1) {
        close(g_inotify_fd);
        g_inotify_fd = -1;
    }
    
    pthread_mutex_unlock(&g_inotify_mutex);
}

/* 恢复HMDFS连接和inotify监控 */
int recover_hmdfs_connection(void) {
    log_message(LOG_LEVEL_INFO, "Attempting to recover HMDFS connection...");
    
    // 先清理现有资源
    cleanup_inotify();
    
    // 检查sysfs是否可用
    if (!check_sysfs_available()) {
        log_message(LOG_LEVEL_WARNING, "HMDFS sysfs not available, recovery failed");
        return -1;
    }
    
    // 重新初始化inotify
    if (init_inotify() < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to reinitialize inotify during recovery");
        return -1;
    }
    
    log_message(LOG_LEVEL_INFO, "HMDFS connection recovered successfully");
    return 0;
}

/* 更新设备状态 */
void update_device_status(remote_node *node, device_status status) {
    if (node == NULL) {
        return;
    }
    
    //pthread_mutex_lock(&g_device_mutex);
    
    device_status old_status = node->status;
    node->status = status;
    node->last_update = time(NULL);
    
    // 如果设备上线，更新心跳时间
    if (status == DEVICE_STATUS_ONLINE || status == DEVICE_STATUS_CONNECTED) {
        node->last_heartbeat = node->last_update;
    }
    
    //pthread_mutex_unlock(&g_device_mutex);
    
    // 记录状态变化
    if (old_status != status) {
        log_message(LOG_LEVEL_INFO, "Device status changed: CID=%s, IP=%s, Port=%d, OldStatus=%d, NewStatus=%d", 
                   node->cid, node->ip, node->port, old_status, status);
    }
}

/* 发送心跳包 */
void send_heartbeat(remote_node *node) {
    if (node == NULL || node->fd == -1) {
        return;
    }
    
    // 构造心跳包 - 使用HMDFS消息格式，命令字段设为0
    hmdfs_head_cmd heartbeat_cmd;
    memset(&heartbeat_cmd, 0, sizeof(heartbeat_cmd));
    
    heartbeat_cmd.magic = 0xF7; // HMDFS_MSG_MAGIC
    heartbeat_cmd.version = 0x40; // HMDFS版本号
    heartbeat_cmd.data_len = 0; // 心跳包无数据
    heartbeat_cmd.operations.reserved = 0;
    heartbeat_cmd.operations.cmd_flag = 0; // 请求
    heartbeat_cmd.operations.command = 0; // 心跳命令 (使用保留命令)
    heartbeat_cmd.ret_code = 0;
    heartbeat_cmd.msg_id = 0;
    heartbeat_cmd.reserved1 = 0;
    
    // 发送心跳包
    ssize_t bytes_sent = send(node->fd, &heartbeat_cmd, sizeof(heartbeat_cmd), 0);
    if (bytes_sent != sizeof(heartbeat_cmd)) {
        log_message(LOG_LEVEL_ERROR, "Failed to send heartbeat to %s:%d: %s", 
                   node->ip, node->port, strerror(errno));
        update_device_status(node, DEVICE_STATUS_ERROR);
    } else {
        log_message(LOG_LEVEL_DEBUG, "Heartbeat sent to %s:%d", node->ip, node->port);
    }
}

/* 处理设备超时 */
void handle_device_timeout(remote_node *node) {
    if (node == NULL) {
        return;
    }
    
    log_message(LOG_LEVEL_WARNING, "Device timeout: CID=%s, IP=%s, Port=%d", 
               node->cid, node->ip, node->port);
    
    // 更新设备状态为离线
    update_device_status(node, DEVICE_STATUS_OFFLINE);
    
    // 关闭连接
    if (node->fd != -1) {
        close(node->fd);
        node->fd = -1;
    }
}

/* 心跳检测线程函数 */
void *check_device_heartbeat(void *arg) {
    log_message(LOG_LEVEL_DEBUG, "Heartbeat detection thread started");
    
    while (g_running) {
        pthread_mutex_lock(&g_device_mutex);
        
        time_t current_time = time(NULL);
        
        for (int i = 0; i < g_config.node_count; i++) {
            remote_node *node = &g_config.nodes[i];
            
            // 跳过离线设备
            if (node->status == DEVICE_STATUS_OFFLINE) {
                continue;
            }
            
            // 检查心跳超时
            if (current_time - node->last_heartbeat > HEARTBEAT_TIMEOUT) {
                log_message(LOG_LEVEL_WARNING, "Device heartbeat timeout: CID=%s, IP=%s, Port=%d, LastHeartbeat=%ld, CurrentTime=%ld", 
                           node->cid, node->ip, node->port, node->last_heartbeat, current_time);
                handle_device_timeout(node);
            } 
            // 发送心跳包
            else if (current_time - node->last_heartbeat > HEARTBEAT_INTERVAL) {
                send_heartbeat(node);
            }
        }
        
        pthread_mutex_unlock(&g_device_mutex);
        
        // 睡眠一段时间后继续检测
        sleep(HEARTBEAT_INTERVAL / 2);
    }
    
    log_message(LOG_LEVEL_DEBUG, "Heartbeat detection thread stopped");
    return NULL;
}

/* 清理心跳检测线程 */
void cleanup_heartbeat(void) {
    if (g_heartbeat_enabled && g_heartbeat_thread != 0) {
        pthread_cancel(g_heartbeat_thread);
        pthread_join(g_heartbeat_thread, NULL);
        g_heartbeat_thread = 0;
        log_message(LOG_LEVEL_INFO, "Heartbeat detection thread stopped");
    }
}

/* 为 socket 设置 TCP keepalive：对端无 FIN 的半开断线（umount/VM 断电）也能被内核 recv 检测到 */
void setup_keepalive(int fd) {
    if (fd < 0) {
        return;
    }
    int keepalive = 1;
    int keepidle = 10, keepintvl = 5, keepcnt = 3;
    setsockopt(fd, SOL_SOCKET, SO_KEEPALIVE, &keepalive, sizeof(keepalive));
    setsockopt(fd, IPPROTO_TCP, TCP_KEEPIDLE, &keepidle, sizeof(keepidle));
    setsockopt(fd, IPPROTO_TCP, TCP_KEEPINTVL, &keepintvl, sizeof(keepintvl));
    setsockopt(fd, IPPROTO_TCP, TCP_KEEPCNT, &keepcnt, sizeof(keepcnt));
}

/* 建立到远程节点的连接（非阻塞 + 超时，避免对端不可达时卡住线程） */
int connect_to_remote_node(remote_node *node) {
    if (!node || strlen(node->ip) == 0 || node->port <= 0) {
        log_message(LOG_LEVEL_ERROR, "Invalid remote node configuration");
        return -1;
    }
    
    int sockfd = socket(AF_INET, SOCK_STREAM, 0);
    if (sockfd < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create socket: %s", strerror(errno));
        return -1;
    }
    
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_port = htons(node->port);
    if (inet_pton(AF_INET, node->ip, &addr.sin_addr) <= 0) {
        log_message(LOG_LEVEL_ERROR, "Invalid IP address: %s", node->ip);
        close(sockfd);
        return -1;
    }
    
    // 非阻塞连接 + poll 超时：对端不可达时避免阻塞 connect 卡住 connector 线程
    int flags = fcntl(sockfd, F_GETFL, 0);
    if (flags < 0 || fcntl(sockfd, F_SETFL, flags | O_NONBLOCK) < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to set non-blocking: %s", strerror(errno));
        close(sockfd);
        return -1;
    }
    
    int ret = connect(sockfd, (struct sockaddr *)&addr, sizeof(addr));
    if (ret < 0 && errno != EINPROGRESS) {
        log_message(LOG_LEVEL_ERROR, "Failed to connect to %s:%d: %s", 
                   node->ip, node->port, strerror(errno));
        close(sockfd);
        return -1;
    }
    
    if (ret < 0) {
        // EINPROGRESS：等待连接完成，2 秒超时
        struct pollfd pfd;
        pfd.fd = sockfd;
        pfd.events = POLLOUT;
        int pret = poll(&pfd, 1, 2000);
        if (pret <= 0) {
            log_message(LOG_LEVEL_ERROR, "Connect timeout to %s:%d", node->ip, node->port);
            close(sockfd);
            return -1;
        }
        // 检查连接是否真正成功
        int err = 0;
        socklen_t errlen = sizeof(err);
        if (getsockopt(sockfd, SOL_SOCKET, SO_ERROR, &err, &errlen) < 0 || err != 0) {
            log_message(LOG_LEVEL_ERROR, "Connect failed to %s:%d: %s", 
                       node->ip, node->port, strerror(err));
            close(sockfd);
            return -1;
        }
    }
    
    // 恢复原阻塞模式（socket 后续移交给 HMDFS 内核模块）
    fcntl(sockfd, F_SETFL, flags);
    
    // TCP keepalive：对端无 FIN 的半开断线（umount/VM 断电）也能被内核 recv 检测到
    setup_keepalive(sockfd);
    
    log_message(LOG_LEVEL_INFO, "Connected to remote node %s:%d, fd=%d", 
               node->ip, node->port, sockfd);
    return sockfd;
}

/* Try to reconnect a single node. Mutual exclusion is handled by the caller
 * via node->connecting. Returns 0 on success, -1 on failure.
 */
static int try_reconnect_node(remote_node *node) {
    pthread_mutex_lock(&g_device_mutex);
    // Self-managed mutual exclusion: another reconnect (netup worker or
    // GET_SESSION branch) is in progress for this node.
    if (node->connecting) {
        pthread_mutex_unlock(&g_device_mutex);
        return -1;
    }
    node->connecting = 1;
    pthread_mutex_unlock(&g_device_mutex);

    int remote_fd = connect_to_remote_node(node);

    pthread_mutex_lock(&g_device_mutex);
    node->connecting = 0;
    if (remote_fd >= 0) {
        update_socket_param cmd;
        memset(&cmd, 0, sizeof(cmd));
        cmd.cmd = CMD_UPDATE_SOCKET;
        cmd.newfd = remote_fd;
        cmd.devsl = 3;
        cmd.status = SOCKET_STAT_OPEN; // active connection
        memset(cmd.masterkey, 0, HMDFS_KEY_SIZE);
        memcpy(cmd.cid, node->cid, HMDFS_CID_SIZE);
        if (send_update_socket_cmd(&cmd) < 0) {
            log_message(LOG_LEVEL_ERROR, "Failed to send UPDATE_SOCKET cmd for node %s", node->cid);
            close(remote_fd);
            update_device_status(node, DEVICE_STATUS_OFFLINE);
            pthread_mutex_unlock(&g_device_mutex);
            return -1;
        }
        log_message(LOG_LEVEL_INFO, "Sent UPDATE_SOCKET cmd to HMDFS for node %s, fd=%d", node->cid, remote_fd);
        node->fd = remote_fd;
        update_device_status(node, DEVICE_STATUS_CONNECTED);
        pthread_mutex_unlock(&g_device_mutex);
        return 0;
    }
    // Reconnect failed: normalize to OFFLINE so a later netup signal can retry.
    log_message(LOG_LEVEL_INFO, "Reconnect failed for node %s, mark OFFLINE", node->cid);
    update_device_status(node, DEVICE_STATUS_OFFLINE);
    pthread_mutex_unlock(&g_device_mutex);
    return -1;
}


static void *netup_reconnect_worker(void *arg) {
    remote_node *node = (remote_node *)arg;
    try_reconnect_node(node);
    return NULL;
}

/* Listen for the net-up signal from the executor (syz_failure_net_up) and
 * reconnect all OFFLINE nodes in parallel. Event-driven: blocking accept,
 * close(srv) wakes it on shutdown. The socket file is created here (bind).
 */
void *netup_listener_thread_func(void *arg) {
    unlink(NETUP_SOCK_PATH);
    int srv = socket(AF_UNIX, SOCK_STREAM, 0);
    if (srv < 0) {
        log_message(LOG_LEVEL_ERROR, "netup listener: socket failed: %s", strerror(errno));
        return NULL;
    }
    struct sockaddr_un addr;
    memset(&addr, 0, sizeof(addr));
    addr.sun_family = AF_UNIX;
    strncpy(addr.sun_path, NETUP_SOCK_PATH, sizeof(addr.sun_path) - 1);
    if (bind(srv, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        log_message(LOG_LEVEL_ERROR, "netup listener: bind failed: %s", strerror(errno));
        close(srv);
        return NULL;
    }
    chmod(NETUP_SOCK_PATH, 0666);
    listen(srv, 8);
    g_netup_srv_fd = srv;
    log_message(LOG_LEVEL_INFO, "netup listener started on %s", NETUP_SOCK_PATH);

    while (g_running) {
        int cfd = accept(srv, NULL, NULL);
        if (cfd < 0) {
            if (errno == EINTR)
                continue;
            if (!g_running)
                break;
            usleep(10000);
            continue;
        }
        char sig;
        int n = read(cfd, &sig, 1);
        close(cfd);
        if (n <= 0)
            continue;
        log_message(LOG_LEVEL_INFO, "netup signal received, reconnecting offline nodes");
        for (int i = 0; i < g_config.node_count; i++) {
            remote_node *node = &g_config.nodes[i];
            int need = 0;
            pthread_mutex_lock(&g_device_mutex);
            if (node->fd == -1 && node->status == DEVICE_STATUS_OFFLINE && !node->connecting)
                need = 1;
            pthread_mutex_unlock(&g_device_mutex);
            if (need) {
                pthread_t th;
                if (pthread_create(&th, NULL, netup_reconnect_worker, node) == 0)
                    pthread_detach(th);
            }
        }
    }

    if (g_netup_srv_fd == srv) {   // signal handler may have closed it already
        g_netup_srv_fd = -1;
        close(srv);
    }
    unlink(NETUP_SOCK_PATH);
    log_message(LOG_LEVEL_INFO, "netup listener stopped");
    return NULL;
}

/* 向HMDFS发送更新套接字命令 */
int send_update_socket_cmd(const update_socket_param *cmd) {
    int fd = open(g_cmd_file_path, O_WRONLY);
    if (fd < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to open cmd file for writing: %s", strerror(errno));
        return -1;
    }
    
    ssize_t bytes_written = write(fd, cmd, sizeof(update_socket_param));
    close(fd);
    
    if (bytes_written != sizeof(update_socket_param)) {
        log_message(LOG_LEVEL_ERROR, "Failed to write update socket cmd, expected %zu, got %zd", 
                   sizeof(update_socket_param), bytes_written);
        return -1;
    }
    
    return 0;
}

/* 向HMDFS发送更新套接字命令 */
int send_offline_cmd(const offline_param *cmd) {
    int fd = open(g_cmd_file_path, O_WRONLY);
    if (fd < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to open cmd file for writing: %s", strerror(errno));
        return -1;
    }
    
    ssize_t bytes_written = write(fd, cmd, sizeof(offline_param));
    close(fd);
    
    if (bytes_written != sizeof(offline_param)) {
        log_message(LOG_LEVEL_ERROR, "Failed to write offline cmd, expected %zu, got %zd", 
                   sizeof(offline_param), bytes_written);
        return -1;
    }
    
    return 0;
}

/* 读取HMDFS的通知参数 */
int read_notify_param(notify_param *param) {
    int fd = open(g_cmd_file_path, O_RDONLY);
    if (fd < 0) {
        // HMDFS模块可能尚未加载，这是正常情况
        return -1;
    }
    
    // 与官方实现保持一致，先将文件指针定位到开头
    lseek(fd, 0, SEEK_SET);
    
    // 初始化通知类型为NONE
    param->notify = NOTIFY_NONE;
    
    ssize_t bytes_read = read(fd, param, sizeof(notify_param));
    close(fd);
    
    if (bytes_read != sizeof(notify_param) || param->notify == NOTIFY_NONE) {
        // 没有有效通知
        return -1;
    }
    
    return 0;
}

int set_and_send_offline(remote_node *node) {
    offline_param cmd;
    cmd.cmd = CMD_OFF_LINE;
    if (node && strlen(node->cid) > 0) {
        memcpy(cmd.remote_cid, node->cid, HMDFS_CID_SIZE);
    } else {
        log_message(LOG_LEVEL_ERROR, "node without cid!");
        return -1;
    }
    
    if(send_offline_cmd(&cmd) < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to send OFFLINE cmd for node %s", node->cid);
        return -1;
    }
    else {
        log_message(LOG_LEVEL_INFO, "Sent OFFLINE cmd to HMDFS for node %s", node->cid);
    }
    
    return 0;
}

/* 将updatesocket命令发送到HMDFS模块 */
int set_and_send_socket(int conn_fd, remote_node *node) {
    //peer_connection *conn = (peer_connection *)arg;
    
    log_message(LOG_LEVEL_DEBUG, "Starting send socket %d to hmdfs", conn_fd);
    
    // 当远程节点连接时，直接将socket fd发送给HMDFS内核模块
    update_socket_param cmd;
    memset(&cmd, 0, sizeof(cmd));
    cmd.cmd = CMD_UPDATE_SOCKET;
    cmd.newfd = conn_fd;
    cmd.devsl = 3;
    cmd.status = SOCKET_STAT_ACCEPT; // 使用官方定义的状态值，服务器接受连接
    memset(cmd.masterkey, 0, HMDFS_KEY_SIZE); // 初始化为全0，后续由内核处理
    
    // 使用远程节点的CID
    if (node && strlen(node->cid) > 0) {
        memcpy(cmd.cid, node->cid, HMDFS_CID_SIZE);
    } else {
        close(conn_fd);
        log_message(LOG_LEVEL_ERROR, "node without cid!");
        return -1;
        //strcpy((char *)cmd.cid, g_config.local_cid);
    }
    
    // 发送命令给HMDFS模块
    if (send_update_socket_cmd(&cmd) < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to send UPDATE_SOCKET cmd for client %d", conn_fd);
        close(conn_fd);
        return -1;
    } else {
        log_message(LOG_LEVEL_INFO, "Sent UPDATE_SOCKET cmd to HMDFS for client %d, fd=%d", 
                   conn_fd, conn_fd);
        
        // HMDFS模块已经接管socket，不需要再从socket读取数据
        // 只需要等待连接关闭的信号
        // 这里我们使用select来检测连接是否关闭
        /*fd_set read_fds;
        struct timeval timeout;
        int ret;
        
        while (g_running) {
            FD_ZERO(&read_fds);
            FD_SET(conn->client_fd, &read_fds);
            
            // 设置超时为5秒
            timeout.tv_sec = 5;
            timeout.tv_usec = 0;
            
            ret = select(conn->client_fd + 1, &read_fds, NULL, NULL, &timeout);
            if (ret < 0) {
                // 发生错误
                log_message(LOG_LEVEL_ERROR, "Select error on socket %d: %s", 
                           conn->client_fd, strerror(errno));
                break;
            } else if (ret == 0) {
                // 超时，继续等待
                continue;
            } else {
                // 有数据可读，说明连接可能关闭
                char buffer[1];
                ret = recv(conn->client_fd, buffer, 1, MSG_PEEK);
                if (ret <= 0) {
                    // 连接关闭
                    log_message(LOG_LEVEL_INFO, "Remote node disconnected from socket %d", conn->client_fd);
                    break;
                }
            }
        }*/
    }
    
    // 清理资源
    /*close(conn->client_fd);
    
    pthread_mutex_lock(&g_connection_mutex);
    // 从客户端列表中移除
    for (int i = 0; i < g_connection_count; i++) {
        if (g_connections[i].client_fd == conn->client_fd) {
            // 将最后一个客户端移到当前位置
            g_connections[i] = g_connections[g_connection_count - 1];
            g_connection_count--;
            break;
        }
    }
    pthread_mutex_unlock(&g_connection_mutex);
    
    log_message(LOG_LEVEL_DEBUG, "Data forwarding ended for client %d", conn->client_fd);*/
    return 0;
}

/* 处理新的客户端连接 */
void handle_new_connection(int client_fd, struct sockaddr_in *client_addr) {
    char client_ip[INET_ADDRSTRLEN];
    inet_ntop(AF_INET, &client_addr->sin_addr, client_ip, INET_ADDRSTRLEN);
    
    log_message(LOG_LEVEL_INFO, "New connection from %s:%d, fd=%d", 
               client_ip, ntohs(client_addr->sin_port), client_fd);
    
    // 入向连接显式设置 TCP keepalive（不依赖 accept 对监听 socket 参数的继承）
    setup_keepalive(client_fd);
    
    // pthread_mutex_lock(&g_connection_mutex);
    
    // 检查是否超过最大客户端数量
    /* if (g_connection_count >= MAX_CLIENTS) {
        log_message(LOG_LEVEL_ERROR, "Max clients reached, rejecting connection from %s:%d", 
                   client_ip, ntohs(client_addr->sin_port));
        close(client_fd);
        // pthread_mutex_unlock(&g_connection_mutex);
        return;
    } */
    
    // 基于IP地址的精确匹配
    remote_node *node = NULL;
    for (int i = 0; i < g_config.node_count; i++) {
        if (strcmp(g_config.nodes[i].ip, client_ip) == 0) {
            node = &g_config.nodes[i];
            log_message(LOG_LEVEL_INFO, "Exact IP match found for %s, using configured node %s", 
                       client_ip, node->cid);
            break;
        }
    }
    
    
    // 如果没有找到任何可用节点，仍然处理连接，不拒绝
    if (!node) {
        log_message(LOG_LEVEL_WARNING, "No available remote nodes configured, still accepting connection from %s", 
                   client_ip);
        // 我们仍然处理连接，但没有关联的远程节点
        close(client_fd);
        log_message(LOG_LEVEL_WARNING, "Connection without matched node closed: %s", client_ip);
    } else {
        // 去重：已有连接且仍然存活时拒绝新连接；已有 fd 但已断开（对端已关）
        // 则先清理旧 fd，再接受新连接——双断场景即时收敛，无需等 checker/
        // connector 的轮询周期
        if (node->fd != -1) {
            if (socketConnected(node->fd) == 1) {
                log_message(LOG_LEVEL_WARNING, "Node %s already has live connection fd=%d, rejecting new fd=%d",
                           node->cid, node->fd, client_fd);
                close(client_fd);
                return;
            }
            log_message(LOG_LEVEL_INFO, "Node %s stale fd=%d, replacing with new fd=%d",
                       node->cid, node->fd, client_fd);
            close(node->fd);
            node->fd = -1;
        }
        // 创建设备连接
        //pthread_mutex_lock(&g_device_mutex);
        /* int client_index = g_connection_count;
        g_connections[client_index].client_fd = client_fd;
        g_connections[client_index].node = node;
        g_connections[client_index].config = &g_config;
        g_connection_count++; */
        if(set_and_send_socket(client_fd, node) == 0) {
            node->fd = client_fd;
            update_device_status(node, DEVICE_STATUS_CONNECTED);
        }
        //pthread_mutex_unlock(&g_device_mutex);
    }
    
    // 创建线程处理数据转发
    /*if (pthread_create(&g_connections[client_index].thread_id, NULL, forward_data, &g_connections[client_index]) != 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create thread: %s", strerror(errno));
        close(client_fd);
        pthread_mutex_unlock(&g_connection_mutex);
        return;
    }*/
    
    // 分离线程
    //pthread_detach(g_connections[client_index].thread_id);
    
    
    // pthread_mutex_unlock(&g_connection_mutex);

    
}

/* 初始化服务器socket */
int init_server_socket(void) {
    if (g_config.local_port <= 0) {
        // 本地端口为0，表示不启用本地监听
        return 0;
    }
    
    // 创建socket
    g_server_fd = socket(AF_INET, SOCK_STREAM, 0);
    if (g_server_fd < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create server socket: %s", strerror(errno));
        return -1;
    }
    
    // 设置socket选项
    int opt = 1;
    if (setsockopt(g_server_fd, SOL_SOCKET, SO_REUSEADDR, &opt, sizeof(opt)) < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to set socket options: %s", strerror(errno));
        close(g_server_fd);
        g_server_fd = -1;
        return -1;
    }
    if (setsockopt(g_server_fd, SOL_SOCKET, SO_REUSEPORT, &opt, sizeof(opt)) < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to set socket options: %s", strerror(errno));
        close(g_server_fd);
        g_server_fd = -1;
        return -1;
    }
    // 绑定地址
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_ANY);
    addr.sin_port = htons(g_config.local_port);
    
    if (bind(g_server_fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to bind to port %d: %s", 
                   g_config.local_port, strerror(errno));
        close(g_server_fd);
        g_server_fd = -1;
        return -1;
    }
    
    // 开始监听
    if (listen(g_server_fd, 10) < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to listen on port %d: %s", 
                   g_config.local_port, strerror(errno));
        close(g_server_fd);
        g_server_fd = -1;
        return -1;
    }
    
    // TCP keepalive：accept 的入向连接继承监听 socket 的 keepalive 设置，
    // 配合出向端的 keepalive，使任意方向的半开断线都能被内核 recv 检测到
    setup_keepalive(g_server_fd);
    
    log_message(LOG_LEVEL_INFO, "Listening on port %d", g_config.local_port);
    return 0;
}

/* 查找远程节点 */
remote_node *find_remote_node(const char *cid) {
    if (!cid) {
        return NULL;
    }
    
    pthread_mutex_lock(&g_device_mutex);
    for (int i = 0; i < g_config.node_count; i++) {
        if (strncmp(g_config.nodes[i].cid, cid, HMDFS_CID_SIZE) == 0) {
            remote_node *node = &g_config.nodes[i];
            pthread_mutex_unlock(&g_device_mutex);
            return node;
        }
    }
    pthread_mutex_unlock(&g_device_mutex);
    return NULL;
}

void HandleAllNotify(int fd) {
    notify_param param;
    update_socket_param cmd;

    while (g_running) {
        lseek(fd, 0, SEEK_SET);
        param.notify = NOTIFY_NONE;
        int readSize = read(fd, &param, sizeof(notify_param));
        if ((readSize < (int)sizeof(notify_param)) || (param.notify == NOTIFY_NONE)) {
            return;
        }
        // remote_cid is 64 bytes without NUL (kernel memcpy); make a
        // NUL-terminated copy for string use (log/strlen/find_remote_node).
        char cid_str[HMDFS_CID_SIZE + 1];
        memcpy(cid_str, param.remote_cid, HMDFS_CID_SIZE);
        cid_str[HMDFS_CID_SIZE] = '\0';
        log_message(LOG_LEVEL_INFO, "Received notify: type=%d, fd=%d, remote_cid=%s", 
               param.notify, param.fd, cid_str);
    
        switch (param.notify) {
            case NOTIFY_GET_SESSION: {
                log_message(LOG_LEVEL_INFO, "Handling NOTIFY_GET_SESSION for CID %s", cid_str);
                update_socket_param cmd;
                // Empty CID: reconnect all configured remote nodes
                if (strlen(cid_str) == 0) {
                    log_message(LOG_LEVEL_INFO, "No specific CID provided, connecting to all configured remote nodes");
                    for (int i = 0; i < g_config.node_count; i++) {
                        remote_node *node = &g_config.nodes[i];
                        pthread_mutex_lock(&g_device_mutex);
                        // Mutual exclusion with the netup reconnect workers
                        if (node->connecting) {
                            pthread_mutex_unlock(&g_device_mutex);
                            continue;
                        }
                        if (node->fd != -1) { // stale fd, close and reconnect
                            close(node->fd);
                            node->fd = -1;
                        }
                        node->connecting = 1;
                        // Kernel decided the connection must be rebuilt (GET_SESSION),
                        // so reconnect unconditionally (do not trust user-space TCP state).
                        int remote_fd = connect_to_remote_node(node);
                        if (remote_fd >= 0) {
                            memset(&cmd, 0, sizeof(cmd));
                            cmd.cmd = CMD_UPDATE_SOCKET;
                            cmd.newfd = remote_fd;
                            cmd.devsl = 3;
                            cmd.status = SOCKET_STAT_OPEN; // active connection
                            memset(cmd.masterkey, 0, HMDFS_KEY_SIZE);
                            memcpy(cmd.cid, node->cid, HMDFS_CID_SIZE);
                            if (send_update_socket_cmd(&cmd) < 0) {
                                log_message(LOG_LEVEL_ERROR, "Failed to send UPDATE_SOCKET cmd for node %s", node->cid);
                                close(remote_fd);
                                update_device_status(node, DEVICE_STATUS_OFFLINE);
                            } else {
                                log_message(LOG_LEVEL_INFO, "Sent UPDATE_SOCKET cmd to HMDFS for node %s, fd=%d", node->cid, remote_fd);
                                node->fd = remote_fd;
                                update_device_status(node, DEVICE_STATUS_CONNECTED);
                            }
                        } else {
                            // Reconnect failed: normalize to OFFLINE so a later
                            // netup signal can pick the node up again.
                            log_message(LOG_LEVEL_INFO, "Reconnect failed for node %s, mark OFFLINE", node->cid);
                            update_device_status(node, DEVICE_STATUS_OFFLINE);
                        }
                        node->connecting = 0;
                        pthread_mutex_unlock(&g_device_mutex);
                    }
                } else {
                    // Specific CID request
                    remote_node *node = find_remote_node(cid_str);
                    if (node) {
                        pthread_mutex_lock(&g_device_mutex);
                        if (node->connecting) {
                            pthread_mutex_unlock(&g_device_mutex);
                            log_message(LOG_LEVEL_INFO, "Node %s reconnecting, skip", node->cid);
                        } else {
                            if (node->fd != -1) { // stale fd, close and reconnect
                                close(node->fd);
                                node->fd = -1;
                            }
                            node->connecting = 1;
                            int remote_fd = connect_to_remote_node(node);
                            if (remote_fd >= 0) {
                                memset(&cmd, 0, sizeof(cmd));
                                cmd.cmd = CMD_UPDATE_SOCKET;
                                cmd.newfd = remote_fd;
                                cmd.devsl = 3;
                                cmd.status = SOCKET_STAT_OPEN;
                                memset(cmd.masterkey, 0, HMDFS_KEY_SIZE);
                                memcpy(cmd.cid, node->cid, HMDFS_CID_SIZE);
                                if (send_update_socket_cmd(&cmd) < 0) {
                                    log_message(LOG_LEVEL_ERROR, "Failed to send UPDATE_SOCKET cmd for node %s", node->cid);
                                    close(remote_fd);
                                    update_device_status(node, DEVICE_STATUS_OFFLINE);
                                } else {
                                    log_message(LOG_LEVEL_INFO, "Sent UPDATE_SOCKET cmd to HMDFS for node %s, fd=%d", node->cid, remote_fd);
                                    node->fd = remote_fd;
                                    update_device_status(node, DEVICE_STATUS_CONNECTED);
                                }
                            } else {
                                log_message(LOG_LEVEL_INFO, "Reconnect failed for node %s, mark OFFLINE", node->cid);
                                update_device_status(node, DEVICE_STATUS_OFFLINE);
                            }
                            node->connecting = 0;
                            pthread_mutex_unlock(&g_device_mutex);
                        }
                    } else {
                        log_message(LOG_LEVEL_ERROR, "No matching remote node found for CID %s", cid_str);
                    }
                }
                break;
            }

            case NOTIFY_OFFLINE: {
                log_message(LOG_LEVEL_INFO, "Handling NOTIFY_OFFLINE for CID %s", cid_str);
                // 处理设备离线通知
                remote_node *node = find_remote_node(cid_str);
                pthread_mutex_lock(&g_device_mutex);
                if (node) {
                    update_device_status(node, DEVICE_STATUS_OFFLINE);
                    if (node->fd != -1) {
                        close(node->fd);
                        node->fd = -1;
                    }
                }
                pthread_mutex_unlock(&g_device_mutex);
                break;
            }
        
            default:
                log_message(LOG_LEVEL_WARNING, "Unknown notify type: %d", param.notify);
                break;
        }
    }
}

/* 处理HMDFS通知 */
void handle_hmdfs_notify(void) {
    notify_param param;
    update_socket_param cmd;
    
    // 读取通知参数
    if (read_notify_param(&param) < 0) {
        return;
    }
    
    // remote_cid is 64 bytes without NUL (kernel memcpy); make a
    // NUL-terminated copy for string use (log/strlen/find_remote_node).
    char cid_str[HMDFS_CID_SIZE + 1];
    memcpy(cid_str, param.remote_cid, HMDFS_CID_SIZE);
    cid_str[HMDFS_CID_SIZE] = '\0';
    log_message(LOG_LEVEL_INFO, "Received notify: type=%d, fd=%d, remote_cid=%s", 
               param.notify, param.fd, cid_str);
    
    switch (param.notify) {
        case NOTIFY_GET_SESSION: {
            log_message(LOG_LEVEL_INFO, "Handling NOTIFY_GET_SESSION for CID %s", cid_str);
            update_socket_param cmd;
            // Empty CID: reconnect all configured remote nodes
            if (strlen(cid_str) == 0) {
                log_message(LOG_LEVEL_INFO, "No specific CID provided, connecting to all configured remote nodes");
                for (int i = 0; i < g_config.node_count; i++) {
                    remote_node *node = &g_config.nodes[i];
                    pthread_mutex_lock(&g_device_mutex);
                    // Mutual exclusion with the netup reconnect workers
                    if (node->connecting) {
                        pthread_mutex_unlock(&g_device_mutex);
                        continue;
                    }
                    if (node->fd != -1) { // stale fd, close and reconnect
                        close(node->fd);
                        node->fd = -1;
                    }
                    node->connecting = 1;
                    int remote_fd = connect_to_remote_node(node);
                    if (remote_fd >= 0) {
                        memset(&cmd, 0, sizeof(cmd));
                        cmd.cmd = CMD_UPDATE_SOCKET;
                        cmd.newfd = remote_fd;
                        cmd.devsl = 3;
                        cmd.status = SOCKET_STAT_OPEN;
                        memset(cmd.masterkey, 0, HMDFS_KEY_SIZE);
                        memcpy(cmd.cid, node->cid, HMDFS_CID_SIZE);
                        if (send_update_socket_cmd(&cmd) < 0) {
                            log_message(LOG_LEVEL_ERROR, "Failed to send UPDATE_SOCKET cmd for node %s", node->cid);
                            close(remote_fd);
                            update_device_status(node, DEVICE_STATUS_OFFLINE);
                        } else {
                            log_message(LOG_LEVEL_INFO, "Sent UPDATE_SOCKET cmd to HMDFS for node %s, fd=%d", node->cid, remote_fd);
                            node->fd = remote_fd;
                            update_device_status(node, DEVICE_STATUS_CONNECTED);
                        }
                    } else {
                        log_message(LOG_LEVEL_INFO, "Reconnect failed for node %s, mark OFFLINE", node->cid);
                        update_device_status(node, DEVICE_STATUS_OFFLINE);
                    }
                    node->connecting = 0;
                    pthread_mutex_unlock(&g_device_mutex);
                }
            } else {
                // Specific CID request
                remote_node *node = find_remote_node(cid_str);
                if (node) {
                    pthread_mutex_lock(&g_device_mutex);
                    if (node->connecting) {
                        pthread_mutex_unlock(&g_device_mutex);
                        log_message(LOG_LEVEL_INFO, "Node %s reconnecting, skip", node->cid);
                    } else {
                        if (node->fd != -1) { // stale fd, close and reconnect
                            close(node->fd);
                            node->fd = -1;
                        }
                        node->connecting = 1;
                        int remote_fd = connect_to_remote_node(node);
                        if (remote_fd >= 0) {
                            memset(&cmd, 0, sizeof(cmd));
                            cmd.cmd = CMD_UPDATE_SOCKET;
                            cmd.newfd = remote_fd;
                            cmd.devsl = 3;
                            cmd.status = SOCKET_STAT_OPEN;
                            memset(cmd.masterkey, 0, HMDFS_KEY_SIZE);
                            memcpy(cmd.cid, node->cid, HMDFS_CID_SIZE);
                            if (send_update_socket_cmd(&cmd) < 0) {
                                log_message(LOG_LEVEL_ERROR, "Failed to send UPDATE_SOCKET cmd for node %s", node->cid);
                                close(remote_fd);
                                update_device_status(node, DEVICE_STATUS_OFFLINE);
                            } else {
                                log_message(LOG_LEVEL_INFO, "Sent UPDATE_SOCKET cmd to HMDFS for node %s, fd=%d", node->cid, remote_fd);
                                node->fd = remote_fd;
                                update_device_status(node, DEVICE_STATUS_CONNECTED);
                            }
                        } else {
                            log_message(LOG_LEVEL_INFO, "Reconnect failed for node %s, mark OFFLINE", node->cid);
                            update_device_status(node, DEVICE_STATUS_OFFLINE);
                        }
                        node->connecting = 0;
                        pthread_mutex_unlock(&g_device_mutex);
                    }
                } else {
                    log_message(LOG_LEVEL_ERROR, "No matching remote node found for CID %s", cid_str);
                }
            }
            break;
        }

        case NOTIFY_OFFLINE: {
            log_message(LOG_LEVEL_INFO, "Handling NOTIFY_OFFLINE for CID %s", cid_str);
            // 处理设备离线通知
            remote_node *node = find_remote_node(cid_str);
            pthread_mutex_lock(&g_device_mutex);
            if (node) {
                update_device_status(node, DEVICE_STATUS_OFFLINE);
                if (node->fd != -1) {
                    close(node->fd);
                    node->fd = -1;
                }
            }
            pthread_mutex_unlock(&g_device_mutex);
            break;
        }
        
        default:
            log_message(LOG_LEVEL_WARNING, "Unknown notify type: %d", param.notify);
            break;
    }
}

/* 解析配置文件 */
int parse_config_file(const char *filename) {
    FILE *fp = fopen(filename, "r");
    if (!fp) {
        log_message(LOG_LEVEL_ERROR, "Error opening config file %s: %s", filename, strerror(errno));
        return -1;
    }
    
    char line[256];
    int line_num = 0;
    int node_index = 0;
    
    // 默认配置
    memset(&g_config, 0, sizeof(g_config));
    g_config.local_port = 12345;
    strcpy(g_config.local_ip, "127.0.0.1"); // 默认本地IP为127.0.0.1
    generate_cid(g_config.local_cid);
    
    while (fgets(line, sizeof(line), fp)) {
        line_num++;
        
        // 跳过空行和注释
        if (line[0] == '#' || line[0] == '\n' || line[0] == '\r') {
            continue;
        }
        
        // 移除换行符
        char *newline = strchr(line, '\n');
        if (newline) *newline = '\0';
        newline = strchr(line, '\r');
        if (newline) *newline = '\0';
        
        // 跳过空白行
        if (strlen(line) == 0) {
            continue;
        }
        
        // 查找等号
        char *eq = strchr(line, '=');
        if (!eq) {
            log_message(LOG_LEVEL_ERROR, "Invalid config line %d: %s", line_num, line);
            continue;
        }
        
        *eq = '\0';
        char *key = line;
        char *value = eq + 1;
        
        // 去除前后空格
        while (*key && isspace(*key)) key++;
        while (key[strlen(key)-1] && isspace(key[strlen(key)-1])) key[strlen(key)-1] = '\0';
        while (*value && isspace(*value)) value++;
        while (value[strlen(value)-1] && isspace(value[strlen(value)-1])) value[strlen(value)-1] = '\0';
        
        // 处理配置项
        if (strcmp(key, "port") == 0) {
            g_config.local_port = atoi(value);
        } else if (strcmp(key, "local_ip") == 0) {
            strncpy(g_config.local_ip, value, INET_ADDRSTRLEN);
            g_config.local_ip[INET_ADDRSTRLEN - 1] = '\0';
        } else if (strcmp(key, "local_cid") == 0) {
            strncpy(g_config.local_cid, value, HMDFS_CID_SIZE);
            g_config.local_cid[HMDFS_CID_SIZE] = '\0';
        } else if (strcmp(key, "log_file") == 0) {
            strncpy(g_config.log_file, value, sizeof(g_config.log_file));
        } else if (strncmp(key, "remote_ip_", 10) == 0) {
            node_index = atoi(key + 10);
            if (node_index < MAX_REMOTE_NODES) {
                strncpy(g_config.nodes[node_index].ip, value, sizeof(g_config.nodes[node_index].ip) - 1);
                g_config.nodes[node_index].ip[sizeof(g_config.nodes[node_index].ip) - 1] = '\0';
            }
        } else if (strncmp(key, "remote_port_", 12) == 0) {
            /* node_index = atoi(key + 12);
            if (node_index < MAX_REMOTE_NODES) {
                g_config.nodes[node_index].port = atoi(value);
            } */
        } else if (strncmp(key, "remote_cid_", 11) == 0) {
            node_index = atoi(key + 11);
            if (node_index < MAX_REMOTE_NODES) {
                strncpy(g_config.nodes[node_index].cid, value, HMDFS_CID_SIZE);
                g_config.nodes[node_index].cid[HMDFS_CID_SIZE] = '\0';
            }
        } else if (strcmp(key, "mount_point") == 0) {
            strncpy(g_config.mount_point, value, sizeof(g_config.mount_point));
        }
    }
    
    fclose(fp);

    // 计算实际配置的节点数量
    g_config.node_count = 0;
    time_t current_time = time(NULL);
    for (int i = 0; i < MAX_REMOTE_NODES; i++) {
        if (strlen(g_config.nodes[i].ip) > 0) {
            g_config.nodes[i].status = DEVICE_STATUS_ONLINE; // 初始状态为在线
            g_config.nodes[i].last_heartbeat = current_time;
            g_config.nodes[i].last_update = current_time;
            g_config.nodes[i].fd = -1;
            g_config.nodes[i].port = g_config.local_port;
            if (g_config.nodes[i].devsl == 0) {
                g_config.nodes[i].devsl = 3; // 默认设备序列号为3
            }
            g_config.node_count++;
        } else {
            break;
        }
    }

    for(int i = 0; i < g_config.node_count; i++) {
        if(strlen(g_config.nodes[i].cid) == 0) {
            log_message(LOG_LEVEL_ERROR, "Node without cid");
            return -1;
        }
    }
    
    // 日志输出配置信息
    log_message(LOG_LEVEL_INFO, "HMDFS Agent configuration:");
    log_message(LOG_LEVEL_INFO, "  Local IP: %s", g_config.local_ip);
    if (g_config.local_port > 0) {
        log_message(LOG_LEVEL_INFO, "  Local Port: %d", g_config.local_port);
    } else {
        log_message(LOG_LEVEL_INFO, "  Local Port: Disabled");
    }
    log_message(LOG_LEVEL_INFO, "  Local CID: %s", g_config.local_cid);
    log_message(LOG_LEVEL_INFO, "  Log File: %s", g_config.log_file);
    log_message(LOG_LEVEL_INFO, "  Mount Point: %s", g_config.mount_point);
    log_message(LOG_LEVEL_INFO, "  Remote Nodes: %d", g_config.node_count);
    for (int i = 0; i < g_config.node_count; i++) {
        log_message(LOG_LEVEL_INFO, "    Node %d: IP=%s, Port=%d, CID=%s, DevSL=%u, Status=%d", 
                   i, g_config.nodes[i].ip, g_config.nodes[i].port, g_config.nodes[i].cid, 
                   g_config.nodes[i].devsl, g_config.nodes[i].status);
    }
    
    return 0;
}

/* 解析命令行参数配置 */
int parse_config_cmd(const char *cmd_line) {
    if (!cmd_line) {
        log_message(LOG_LEVEL_ERROR, "Command line is NULL");
        return -1;
    }
    
    char *cmd_copy = strdup(cmd_line);
    if (!cmd_copy) {
        log_message(LOG_LEVEL_ERROR, "Failed to allocate memory for command line");
        return -1;
    }
    
    int node_num = 0;
    int local_idx = -1;
    char init_ip[INET_ADDRSTRLEN] = {0};
    int port = 12345;
    char cids[2048] = {0};
    char log_file[256] = {0};
    char mount_point[256] = {0};
    
    char *token = strtok(cmd_copy, " ");
    while (token != NULL) {
        if (strncmp(token, "node_num=", 9) == 0) {
            node_num = atoi(token + 9);
        } else if (strncmp(token, "local_idx=", 10) == 0) {
            local_idx = atoi(token + 10);
        } else if (strncmp(token, "init_ip=", 8) == 0) {
            strncpy(init_ip, token + 8, INET_ADDRSTRLEN - 1);
            init_ip[INET_ADDRSTRLEN - 1] = '\0';
        } else if (strncmp(token, "port=", 5) == 0) {
            port = atoi(token + 5);
        } else if (strncmp(token, "cids=", 5) == 0) {
            strncpy(cids, token + 5, sizeof(cids) - 1);
            cids[sizeof(cids) - 1] = '\0';
        } else if (strncmp(token, "mount_point=", 12) == 0) {
            strncpy(mount_point, token + 12, sizeof(mount_point) - 1);
            mount_point[sizeof(mount_point) - 1] = '\0';
        } else if (strncmp(token, "log_file=", 9) == 0) {
            strncpy(log_file, token + 9, sizeof(log_file) - 1);
            log_file[sizeof(log_file) - 1] = '\0';
        }
        token = strtok(NULL, " ");
    }
    
    free(cmd_copy);
    
    log_message(LOG_LEVEL_INFO, "Parsing command line config: node_num=%d, local_idx=%d, init_ip=%s, port=%d",
               node_num, local_idx, init_ip, port);
    
    if (strlen(cids) > 0) {
        log_message(LOG_LEVEL_INFO, "CIDs: %s", cids);
    } else {
        log_message(LOG_LEVEL_ERROR, "Need cids");
        return -1;
    }
    
    if (strlen(log_file) > 0) {
        log_message(LOG_LEVEL_INFO, "Log File: %s", log_file);
    }
    
    if (node_num <= 0 || node_num > MAX_REMOTE_NODES + 1) {
        log_message(LOG_LEVEL_ERROR, "Invalid node_num: %d (must be 1-%d)", node_num, MAX_REMOTE_NODES + 1);
        return -1;
    }
    
    if (local_idx < 0 || local_idx >= node_num) {
        log_message(LOG_LEVEL_ERROR, "Invalid local_idx: %d (must be 0-%d)", local_idx, node_num - 1);
        return -1;
    }
    
    if (strlen(init_ip) == 0) {
        log_message(LOG_LEVEL_ERROR, "init_ip is required");
        return -1;
    }
    
    struct in_addr init_addr;
    if (inet_pton(AF_INET, init_ip, &init_addr) != 1) {
        log_message(LOG_LEVEL_ERROR, "Invalid init_ip: %s", init_ip);
        return -1;
    }
    
    memset(&g_config, 0, sizeof(g_config));
    g_config.local_port = port;
    
    if (strlen(log_file) > 0) {
        snprintf(g_config.log_file, sizeof(g_config.log_file), "%s", log_file);
    }
    
    if (strlen(mount_point) > 0) {
        snprintf(g_config.mount_point, sizeof(g_config.mount_point), "%s", mount_point);
    }
    
    unsigned char init_ip_bytes[4];
    memcpy(init_ip_bytes, &init_addr.s_addr, 4);
    
    unsigned char local_ip_bytes[4];
    memcpy(local_ip_bytes, init_ip_bytes, 4);
    local_ip_bytes[3] += local_idx;
    
    struct in_addr local_addr;
    memcpy(&local_addr.s_addr, local_ip_bytes, 4);
    inet_ntop(AF_INET, &local_addr, g_config.local_ip, INET_ADDRSTRLEN);
    
    char cid_copy[2048];
    strncpy(cid_copy, cids, sizeof(cid_copy) - 1);
    cid_copy[sizeof(cid_copy) - 1] = '\0';
    
    int cid_count = 0;
    char *cid_token = strtok(cid_copy, ";");
    while (cid_token != NULL && cid_count < node_num) {
        if (cid_count == local_idx) {
            strncpy(g_config.local_cid, cid_token, HMDFS_CID_SIZE);
            g_config.local_cid[HMDFS_CID_SIZE] = '\0';
        }
        cid_count++;
        cid_token = strtok(NULL, ";");
    }
    
    if (strlen(g_config.local_cid) == 0) {
        log_message(LOG_LEVEL_ERROR, "Local CID not found in cids at index %d", local_idx);
        return -1;
    }
    
    g_config.node_count = 0;
    time_t current_time = time(NULL);
    int remote_idx = 0;
    
    for (int i = 0; i < node_num; i++) {
        if (i == local_idx) {
            continue;
        }
        
        if (remote_idx >= MAX_REMOTE_NODES) {
            log_message(LOG_LEVEL_ERROR, "Too many remote nodes (max %d)", MAX_REMOTE_NODES);
            break;
        }
        
        unsigned char remote_ip_bytes[4];
        memcpy(remote_ip_bytes, init_ip_bytes, 4);
        remote_ip_bytes[3] += i;
        
        struct in_addr remote_addr;
        memcpy(&remote_addr.s_addr, remote_ip_bytes, 4);
        inet_ntop(AF_INET, &remote_addr, g_config.nodes[remote_idx].ip, INET_ADDRSTRLEN);
        
        g_config.nodes[remote_idx].port = port;
        g_config.nodes[remote_idx].devsl = 3;
        g_config.nodes[remote_idx].status = DEVICE_STATUS_ONLINE;
        g_config.nodes[remote_idx].fd = -1;
        g_config.nodes[remote_idx].last_heartbeat = current_time;
        g_config.nodes[remote_idx].last_update = current_time;
        
        strncpy(cid_copy, cids, sizeof(cid_copy) - 1);
        cid_copy[sizeof(cid_copy) - 1] = '\0';
        cid_count = 0;
        cid_token = strtok(cid_copy, ";");
        while (cid_token != NULL && cid_count <= i) {
            if (cid_count == i) {
                strncpy(g_config.nodes[remote_idx].cid, cid_token, HMDFS_CID_SIZE);
                g_config.nodes[remote_idx].cid[HMDFS_CID_SIZE] = '\0';
            }
            cid_count++;
            cid_token = strtok(NULL, ";");
        }
        
        if (strlen(g_config.nodes[remote_idx].cid) == 0) {
            log_message(LOG_LEVEL_ERROR, "CID not found for remote node at index %d", i);
            return -1;
        }
        
        g_config.node_count++;
        remote_idx++;
    }
    
    log_message(LOG_LEVEL_INFO, "HMDFS Agent configuration from command line:");
    log_message(LOG_LEVEL_INFO, "  Local IP: %s", g_config.local_ip);
    log_message(LOG_LEVEL_INFO, "  Local Port: %d", g_config.local_port);
    log_message(LOG_LEVEL_INFO, "  Local CID: %s", g_config.local_cid);
    if (strlen(g_config.log_file) > 0) {
        log_message(LOG_LEVEL_INFO, "  Log File: %s", g_config.log_file);
    }
    log_message(LOG_LEVEL_INFO, "  Remote Nodes: %d", g_config.node_count);
    for (int i = 0; i < g_config.node_count; i++) {
        log_message(LOG_LEVEL_INFO, "    Node %d: IP=%s, Port=%d, CID=%s, DevSL=%u, Status=%d", 
                   i, g_config.nodes[i].ip, g_config.nodes[i].port, g_config.nodes[i].cid, 
                   g_config.nodes[i].devsl, g_config.nodes[i].status);
    }
    
    return 0;
}

/* 信号处理函数 */
void handle_signal(int sig) {
    // async-signal-safe only: no printf/stdio (lock reentrancy), no
    // pthread_mutex (self-deadlock if delivered to a lock-holding thread),
    // no exit()/atexit (cleanup_agent takes the mutex). Just flag shutdown
    // and wake the blocking threads; cleanup runs in cleanup_agent via atexit
    // after main's joins return.
    static const char msg[] = "signal received, shutting down...\n";
    write(STDERR_FILENO, msg, sizeof(msg) - 1);
    g_running = 0;
    if (g_netup_srv_fd >= 0) {
        close(g_netup_srv_fd);   // wake netup_listener accept
        g_netup_srv_fd = -1;     // avoid double close / stale fd reuse
    }
    if (g_server_fd >= 0)
        close(g_server_fd);      // wake listener
}


void cleanup_agent() {
    g_running = 0;
    unlink(NETUP_SOCK_PATH);
    
    // 清理心跳检测线程
    //cleanup_heartbeat();
    
    // 关闭服务器socket
    if (g_server_fd >= 0) {
        close(g_server_fd);
        g_server_fd = -1;
    }
    
    // 关闭所有客户端连接
    pthread_mutex_lock(&g_device_mutex);
    for (int i = 0; i < g_config.node_count; i++) {
        if(g_config.nodes[i].fd != -1) {
            close(g_config.nodes[i].fd);
            g_config.nodes[i].fd = -1;
            if(g_config.nodes[i].status == DEVICE_STATUS_CONNECTED) {
                if(set_and_send_offline(&g_config.nodes[i]) == 0) {
                    update_device_status(&g_config.nodes[i], DEVICE_STATUS_OFFLINE);
                }
            }
        }
    }
    pthread_mutex_unlock(&g_device_mutex);
    
    // 清理inotify资源
    // cleanup_inotify();
    
    // 关闭日志文件
    if (g_log_file) {
        fclose(g_log_file);
        g_log_file = NULL;
    }
    
    // 释放客户端数组
    /* if (g_connections) {
        free(g_connections);
        g_connections = NULL;
    } */
    
}

// 监听线程函数
void *listener_thread_func(void *arg) {
    // init_server_socket();
    /* if (g_server_fd < 0) {
        fprintf(stderr, "Failed to create listen socket\n");
        return NULL;
    } */
    
    log_message(LOG_LEVEL_INFO, "Listener thread started, waiting for connections...");

    fd_set readfds;

    while (g_running) {
        struct sockaddr_in client_addr;
        socklen_t addr_len = sizeof(client_addr);
        
        // 使用select设置超时，以便定期检查running状态
        
        struct timeval timeout = {0, 200000};  // 200毫秒超时
        
        FD_ZERO(&readfds);
        FD_SET(g_server_fd, &readfds);
        
        int ret = select(g_server_fd + 1, &readfds, NULL, NULL, &timeout);
        if (ret < 0) {
            if (errno == EINTR) continue;
            log_message(LOG_LEVEL_ERROR, "select LISTEN");
            break;
        }
        
        if (ret == 0) {
            // 超时，检查是否继续运行
            continue;
        }
        
        if (FD_ISSET(g_server_fd, &readfds)) {
            pthread_mutex_lock(&g_device_mutex);
            int client_fd = accept(g_server_fd, (struct sockaddr *)&client_addr, &addr_len);
            if (client_fd < 0) {
                log_message(LOG_LEVEL_ERROR, "accept SOCK");
                pthread_mutex_unlock(&g_device_mutex);
                continue;
            }
            handle_new_connection(client_fd, &client_addr);
            pthread_mutex_unlock(&g_device_mutex);
        }
    }
    
    close(g_server_fd);
    log_message(LOG_LEVEL_INFO, "Listener thread stopped");
    
    return NULL;
}

void *notify_handler_thread_func(void *arg) {
    // inotify never fires for kernel notifies on sysfs (the cmd file is static
    // and sysfs_notify does not produce directory inotify events), so poll the
    // notify kfifo directly. It races with sysfs_checker_thread (poll cmdFd
    // POLLPRI, immediate) on the same kfifo -- a consuming queue, so no notify
    // is processed twice. This thread is currently disabled (creation in main
    // is commented out).
    while (g_running) {
        handle_hmdfs_notify();
        sleep(1);
    }
    return NULL;
}


// 连接线程函数
void *connector_thread_func(void *arg) {
    log_message(LOG_LEVEL_INFO, "Connector thread started");
    update_socket_param cmd;
    while (g_running) {

        // 遍历所有配置的远程节点
        for (int i = 0; i < g_config.node_count; i++) {
            remote_node *node = &g_config.nodes[i];
            pthread_mutex_lock(&g_device_mutex);
            if (node->status == DEVICE_STATUS_ONLINE && node->fd == -1) {
                // 建立到远程节点的连接
                int remote_fd = connect_to_remote_node(node);
                if (remote_fd >= 0) {
                    // 准备更新套接字命令
                    memset(&cmd, 0, sizeof(cmd));
                    cmd.cmd = CMD_UPDATE_SOCKET;
                    cmd.newfd = remote_fd;
                    cmd.devsl = 3;
                    cmd.status = SOCKET_STAT_OPEN; // 使用官方定义的状态值，主动打开连接
                    memset(cmd.masterkey, 0, HMDFS_KEY_SIZE);
                    memcpy(cmd.cid, node->cid, HMDFS_CID_SIZE);
                            
                    // 发送命令给HMDFS模块
                    if (send_update_socket_cmd(&cmd) < 0) {
                        log_message(LOG_LEVEL_ERROR, "Failed to send UPDATE_SOCKET cmd for node %s", node->cid);
                        close(remote_fd);
                    } else {
                        log_message(LOG_LEVEL_INFO, "Sent UPDATE_SOCKET cmd to HMDFS for node %s, fd=%d", node->cid, remote_fd);
                        node->fd = remote_fd;
                        update_device_status(node, DEVICE_STATUS_CONNECTED);
                    }
                }
                pthread_mutex_unlock(&g_device_mutex);
            }
            else {
                pthread_mutex_unlock(&g_device_mutex);
            }
        }

        // 每8秒重试一次
        for (int i = 0; i < 4 && g_running; i++) {
            sleep(2);
        }       
        
    }
    
    log_message(LOG_LEVEL_INFO, "Connector thread stopped");
    return NULL;
}

// 定期检查连接状态并下线设备
// DISABLED: 该线程在 main() 中未创建。原因：socket fd 已通过 UPDATE_SOCKET 移交给
// HMDFS 内核模块，agent 与内核共享同一 socket；此处基于 TCP_INFO 的轮询会在连接
// 建立/竞态下误判离线，并主动发送 CMD_OFF_LINE 给内核，引发断开-重连风暴并反复
// 触发内核 connection_put 的 held lock freed（UAF）。断开检测由内核 recv 线程
// 退出后的 NOTIFY_GET_SESSION 通知驱动（见 HandleAllNotify）。
void *connect_checker_thread_func(void *arg) {
    while(g_running) {
        for (int i = 0; i < g_config.node_count; i++) {
            pthread_mutex_lock(&g_device_mutex);
            if(g_config.nodes[i].fd != -1 && g_config.nodes[i].status == DEVICE_STATUS_CONNECTED) {
                if(socketConnected(g_config.nodes[i].fd) != 1) {
                    log_message(LOG_LEVEL_INFO, "node %s offline", g_config.nodes[i].cid);
                    if(set_and_send_offline(&g_config.nodes[i]) == 0) {
                        close(g_config.nodes[i].fd);
                        g_config.nodes[i].fd = -1;
                        update_device_status(&g_config.nodes[i], DEVICE_STATUS_OFFLINE);
                    }
                }
            }
            pthread_mutex_unlock(&g_device_mutex);
        }
        
        for (int i = 0; i < 5 && g_running; i++) {
            sleep(2);
        }
    }
    
    log_message(LOG_LEVEL_INFO, "offline check thread stopped");
    return NULL;
}

// 定期检查sysfs并执行请求
void *sysfs_checker_thread_func(void *arg) {
    struct pollfd fileFd;
    int cmdFd = -1;

    cmdFd = open(g_cmd_file_path, O_RDWR);
    if (cmdFd < 0) {
        log_message(LOG_LEVEL_ERROR, "open cmd file failed");
        return NULL;
    }

    log_message(LOG_LEVEL_INFO, "open cmd file success");

    while (g_running) {
        fileFd.fd = cmdFd;
        fileFd.events = POLLPRI;
        fileFd.revents = 0;
        int ret = poll(&fileFd, 1, 200);
        switch (ret) {
            case NONE_EVENT:
                log_message(LOG_LEVEL_INFO, "none event, poll cmd exit");
                break;
            case TIME_OUT_EVENT:
                break;
            case READ_EVENT:
                HandleAllNotify(cmdFd);
                break;
            default:
                log_message(LOG_LEVEL_INFO, "poll cmd exit");
        }
    }

    close(cmdFd);
    log_message(LOG_LEVEL_INFO, "exit sysfs checker");
    return NULL;
}

/* 主函数 */
int main(int argc, char *argv[]) {
    if (argc < 2) {
        printf("Usage: %s <config_file> OR %s \"node_num=N local_idx=M init_ip=X.X.X.X port=P cids=C1;C2;... log_file=/path/to/log\"\n", argv[0], argv[0]);
        printf("  config_file: Path to configuration file\n");
        printf("  Command line options:\n");
        printf("    node_num=N     Total number of nodes\n");
        printf("    local_idx=M    Local node index (0-based)\n");
        printf("    init_ip=X.X.X.X Starting IP address\n");
        printf("    port=P         Port number (default: 12345)\n");
        printf("    cids=C1;C2;... Semicolon-separated list of all CIDs\n");
        printf("    log_file=/path  Path to log file (optional)\n");
        return -1;
    }
    
    // 初始化随机数种子
    srand(time(NULL));
    
    // 根据参数数量选择配置方式
    if (argc == 2) {
        // 使用配置文件
        printf("Using configuration file: %s\n", argv[1]);
        if (parse_config_file(argv[1]) < 0) {
            return -1;
        }
    } else {
        // 使用命令行参数
        char cmd_line[8192] = {0};
        for (int i = 1; i < argc; i++) {
            if (i > 1) {
                strcat(cmd_line, " ");
            }
            strcat(cmd_line, argv[i]);
        }
        printf("Using command line configuration: %s\n", cmd_line);
        if (parse_config_cmd(cmd_line) < 0) {
            return -1;
        }
    }
    
    // 初始化日志
    if (init_log(g_config.log_file) < 0) {
        return -1;
    }

    if (init_cmd_path() < 0) {
        return -1;
    }
    
    // 分配客户端数组
    /*g_connections = (peer_connection *)malloc(sizeof(peer_connection) * MAX_CLIENTS);
    if (g_connections == NULL) {
        log_message(LOG_LEVEL_ERROR, "Failed to allocate client array: %s", strerror(errno));
        return -1;
    } */
    // memset(g_connections, 0, sizeof(peer_connection) * MAX_CLIENTS);
    
    // 初始化服务器socket
    if (init_server_socket() < 0) {
        // free(g_connections);
        log_message(LOG_LEVEL_ERROR, "Failed to initialize listener socket");
        return -1;
    }
    
    // 初始化inotify
    if (init_inotify() < 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to initialize inotify");
        // free(g_connections);
        return -1;
    }
    
    // 设置信号处理
    signal(SIGINT, handle_signal);
    signal(SIGTERM, handle_signal);
    signal(SIGPIPE, SIG_IGN);

    atexit(cleanup_agent);
    
    // 启动监听线程
    if (pthread_create(&g_listener_thread, NULL, listener_thread_func, NULL) != 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create listener thread");
        return 1;
    }
    
    // 启动NOTIFY监听线程
    /* DISABLED: inotify watch on /sys/fs/hmdfs/ (IN_CREATE|IN_MODIFY) never fires
     * for kernel notifies (cmd file is static, sysfs_notify does not produce
     * directory inotify events), so this thread cannot receive them. Notify
     * consumption is fully covered by sysfs_checker_thread (poll cmdFd POLLPRI).
     */
    /* if (pthread_create(&g_notify_handler_thread, NULL, notify_handler_thread_func, NULL) != 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create notify handler thread");
        return 1;
    } */

    // 启动连接线程
    if (pthread_create(&g_connector_thread, NULL, connector_thread_func, NULL) != 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create connector thread");
        return 1;
    }
    
    // 启动连接检查线程
    // DISABLED: connect_checker_thread_func 会对已移交内核的 fd 做 TCP_INFO 轮询，
    // 在连接建立/竞态下误判离线并主动发 CMD_OFF_LINE 给内核，引发断开-重连风暴
    // 并反复触发内核 connection_put 的 held lock freed。断开检测由内核 recv 线程
    // 退出后的 NOTIFY_GET_SESSION 通知驱动（见 HandleAllNotify），无需用户态轮询。
    /* if (pthread_create(&g_connect_checker_thread, NULL, connect_checker_thread_func, NULL) != 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create connect checker thread");
        return 1;
    } */

    // 启动sysfs监听线程
    if (pthread_create(&g_sysfs_checker_thread, NULL, sysfs_checker_thread_func, NULL) != 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create sysfs checker thread");
        return 1;
    }

    // netup listener thread: executor's syz_failure_net_up signal -> reconnect offline nodes
    if (pthread_create(&g_netup_listener_thread, NULL, netup_listener_thread_func, NULL) != 0) {
        log_message(LOG_LEVEL_ERROR, "Failed to create netup listener thread");
        return 1;
    }
    
    // 等待所有线程完成
    pthread_join(g_listener_thread, NULL);
    //pthread_join(g_notify_handler_thread, NULL);
    pthread_join(g_connector_thread, NULL);
    //pthread_join(g_connect_checker_thread, NULL); // DISABLED: 见上方线程创建处注释
    pthread_join(g_sysfs_checker_thread, NULL);
    pthread_join(g_netup_listener_thread, NULL);
    
    log_message(LOG_LEVEL_INFO, "HMDFS Agent stopped");
    return 0;
}
