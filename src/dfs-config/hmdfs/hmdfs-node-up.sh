#args
start_ip=$1
params=$2
executor_index=$3

node_cnt=${params%% *}
cids=${params#* }
cur_idx=$executor_index

# 定义源目录和目标挂载点
SOURCE_DIR="/data/service/el2/100/non_account"
MOUNT_POINT="/mnt/hmdfs/100/non_account"
CACHE_DIR="/data/service/el2/100/cache"

# 检查是否已挂载
if ! mount | grep -q "$MOUNT_POINT "; then
    echo "need mount"
    mount -t hmdfs -o merge,local_dst="$MOUNT_POINT",cache_dir="$CACHE_DIR" "$SOURCE_DIR" "$MOUNT_POINT"
    
    # 再次检查挂载是否成功
    if mount | grep -q "$MOUNT_POINT "; then
        echo "mount hmdfs success!"
    else
        echo "mount hmdfs fail!"
        exit 1
    fi
else
    echo "already mounted"
fi

/home/hmdfs_agent/hmdfs_agent node_num="$node_cnt" init_ip="$start_ip" local_idx="$cur_idx" cids="$cids" port=12345 mount_point="$MOUNT_POINT" log_file=/home/hmdfs_agent/hmdfs_agent.log