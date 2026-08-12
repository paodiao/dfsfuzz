MOUNT_POINT="/mnt/hmdfs/100/non_account"

echo "Simulating HMDFS node crash..."

# 1. Kill agent
echo "Killing hmdfs_agent processes..."
ps -aux|grep hmdfs_|awk '{print $2}'|while read line ; do kill $line; done

# 2. umount hmdfs
echo "Unmounting hmdfs..."
umount -l "$MOUNT_POINT" 2>/dev/null || echo "Already unmounted"

echo "HMDFS node crash simulation complete."