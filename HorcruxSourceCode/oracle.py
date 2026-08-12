# define the oracle of dfs metedata corruption
import filecmp
import os
import time

def check_ceph_oracle():
    # check the metadatastructure
    ceph_hosts = ["Ceph1","Ceph2","Ceph3"]
    mout_positions = ["/mnt/mycephfs1/test_dfs_fuzzer/smf/","/mnt/mycephfs2/test_dfs_fuzzer/smf/","/mnt/mycephfs3/test_dfs_fuzzer/smf/"]
    res1 = check_ceph_metadata_structure(ceph_hosts)
    res2 = check_ceph_pg_stat(ceph_hosts)
    res3 = check_data_pool(ceph_hosts)
    res4 = check_time_and_authority(mout_positions)
    res = res1 and res2 and res3 and res4
    return res

def check_ceph_metadata_structure(ceph_nodes_hosts):
#     the parameter is the ip list of the ceph nodes
    if len(ceph_nodes_hosts) == 0:
        print("DfsFuzzer ERROR: checking failure => Please pass in the hosts of the ceph nodes")
    list_metadata_files = []
    for host in ceph_nodes_hosts:
        # execute a command like: "ssh Ceph1 "rados -p cephfs.test-fs.meta ls" > ceph1.out"
        command = "ssh "
        command += host
        command += " \"rados -p cephfs.test-fs.meta ls\" > "
        file_name = host + ".metadata"
        command += file_name
        print(command)
        os.system(command)
        list_metadata_files.append(file_name)
    time.sleep(2)
    for i in range(len(list_metadata_files)-1):
        cmp1 = list_metadata_files[i]
        cmp2 = list_metadata_files[i+1]
        res = filecmp.cmp(cmp1,cmp2,shallow=False)
        if not res:
            # two metadata are different
            print("DfsFuzzer ERROR: checking failure => " + list_metadata_files[i+1] + " is differnet from other metadata")
            return False
    return True

def check_ceph_pg_stat(ceph_nodes_hosts):
    if len(ceph_nodes_hosts) == 0:
        print("DfsFuzzer ERROR: checking failure => Please pass in the hosts of the ceph nodes")
    list_pg_files = []
    for host in ceph_nodes_hosts:
        # execute a command like: "ssh Ceph1 "rados -p cephfs.test-fs.meta ls" > ceph1.out"
        command = "ssh "
        command += host
        command += " \"ceph pg dump pgs\" > "
        file_name = host + ".pg"
        command += file_name
        print(command)
        os.system(command)
        list_pg_files.append(file_name)
    time.sleep(2)
    for i in range(len(list_pg_files)-1):
        cmp1 = list_pg_files[i]
        cmp2 = list_pg_files[i+1]
        res = filecmp.cmp(cmp1,cmp2,shallow=False)
        if not res:
            # two metadata are different
            print("DfsFuzzer ERROR: checking failure => " + list_pg_files[i+1] + " is differnet from other pg")
            return False
    return True
# ceph pg dump

def check_data_pool(ceph_nodes_hosts):
    if len(ceph_nodes_hosts) == 0:
        print("DfsFuzzer ERROR: checking failure => Please pass in the hosts of the ceph nodes")
    list_datapool_files = []
    for host in ceph_nodes_hosts:
        # execute a command like: "ssh Ceph1 "rados -p cephfs.test-fs.meta ls" > ceph1.out"
        command = "ssh "
        command += host
        command += " \"rados ls -p cephfs.test-fs.data\" > "
        file_name = host + ".datapool"
        command += file_name
        print(command)
        os.system(command)
        list_datapool_files.append(file_name)
    time.sleep(2)
    for i in range(len(list_datapool_files)-1):
        cmp1 = list_datapool_files[i]
        cmp2 = list_datapool_files[i+1]
        res = filecmp.cmp(cmp1,cmp2,shallow=False)
        if not res:
            # two metadata are different
            print("DfsFuzzer ERROR: checking failure => " + list_datapool_files[i+1] + " is differnet from other data pool")
            return False
    return True
# rados ls -p cephfs.test-fs.data

def check_time_and_authority(mout_positions):
    if len(mout_positions) == 0:
        print("DfsFuzzer ERROR: checking failure => Please pass in the hosts of the ceph nodes")
    tree_files = []
    count = 0
    for pos in mout_positions:
        # execute a command like: "ssh Ceph1 "rados -p cephfs.test-fs.meta ls" > ceph1.out"
        command = "tree -p -u -g -s -D " + pos + " -J > "
        file_name = str(count) + ".tree"
        command += file_name
        command1 = "sed -i \'2d\' " + file_name
        print(command)
        print(command1)
        os.system(command)
        os.system(command1)
        count = count + 1
        tree_files.append(file_name)
    time.sleep(2)
    for i in range(len(tree_files)-1):
        cmp1 = tree_files[i]
        cmp2 = tree_files[i+1]
        res = filecmp.cmp(cmp1,cmp2,shallow=False)
        if not res:
            # two metadata are different
            print("DfsFuzzer ERROR: checking failure => " + tree_files[i+1] + " is differnet from other tree file")
            return False
    return True
    # tree -p -u -g -s -D /mnt/mycephfs2/test_dfs_fuzzer/smf/ -

# mout_positions = ["/mnt/mycephfs1/test_dfs_fuzzer/smf/","/mnt/mycephfs2/test_dfs_fuzzer/smf/","/mnt/mycephfs3/test_dfs_fuzzer/smf/"]
# print(check_time_and_authority(mout_positions))
