#args
start_ip=$1
srv_cnt=$2
client_cnt=$3
mode=$4
cur_idx=$5

if [ $cur_idx -lt $srv_cnt ]; then
	#mount -t ext4 /dev/vda /root/glusterfs-server -o sync
    systemctl restart glusterd || exit 1
else
	umount -f /root/glusterfs-client 2>/dev/null || true
	mount -t glusterfs "$start_ip:/test-volume" /root/glusterfs-client || exit 1
fi
sleep 1s
