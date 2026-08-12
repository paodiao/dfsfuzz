File/stash: 
| 模板名称 | 客户端数 | 共享资源 | 操作序列骨架 | 关键测试点 |
| --- | --- | --- | --- | --- |
| 写-写冲突（不同偏移）| 2 | 同一文件 | C1: open(F)→write(off1, len1)→close C2: open(F)→write(off2, len2)→close | 异步写回队列合并，最终文件内容完整 |
| 写-写冲突（相同偏移）| 2 | 同一文件 | C1: open(F)→write(off, len, dataA)→close C2: open(F)→write(off, len, dataB)→close | 最后写入获胜？冲突解决？数据覆盖正确性 |
| 读写并发（写后立即读）| 2 | 同一文件 | C1: write(F, data) C2: read(F)	| 读可见性（本地缓存 vs 强制远端） |
| 读写并发（读过程中写）| 2 | 同一文件 | C1: read(F)长读 C2: write(F)中途 | 读取过程中写入，是否读到部分旧数据+新数据 |
| 暂存风暴（归属节点离线）| 4 | 同一归属节点多个文件 | C1..4: write(Fi)→close FAULT: offline(owner)→online(owner) | 暂存生成，上线后写回顺序，数据不丢失 |
| 暂存写回过程中的并发读 | 3 | 同一文件 | C1: write(F)→close (归属节点离线，生成暂存) C2: online(owner)→写回中 C3: read(F) | 写回过程中读取，是否读到部分数据或旧数据 |
| 归属节点故障与客户端重试 | 3 | 同一文件 | C1..3: write → close FAULT: crash(owner) （写回未完成时）→recover(owner) | 客户端重试机制，数据最终一致性 |
| 网络分区下的读写 | 3 | 同一文件 | PARTITION({C1,owner}, {C2}) C1: write(F) C2: read(F) HEAL | 分区期间写入，愈合后读可见性 |
| 缓存过期与并发更新 | 2 | 同一文件 | C1: write(F) → sleep(TTL+1) C2: read(F) | 缓存过期后重新验证，读取到最新值 |



Inode/metadata/dentrycache: 
| 模板名称 | 客户端数 | 共享资源 | 操作序列骨架 | 关键测试点 |
| --- | --- | --- | --- | --- |
| 并发创建同一目录 | 3 | 同一目录 | C1..3: mkdir(D) | 原子性：一个成功，其余EEXIST，dentry一致 |
| 并发创建/删除同一文件 | 2 | 同一文件 | C1: creat(F) C12: unlink(F) |	文件存在性最终一致，无悬挂dentry |
| 并发创建和访问 | 2 | 同一文件 | C1: mkdir(D) C2: stat(D) | 最终一致性 |
| 并发重命名与打开 | 2 | 同一文件 | C1: rename(F, G) C2: open(F)/open(G) | rename原子性：open看到旧或新路径，不丢文件 |
| 并发重命名与写入/读取 | 2 | 同一文件 | C1: rename(F, G) C2: write(F)/read(F) | rename原子性 |
| 并发重命名与创建 | 2 | 两个文件 | C1: rename(F, G) C2: creat(G) | 一致性，不同顺序结果不同 |
| 并发重命名与重命名 | 2 | 两个文件 | C1: rename(A, B) C2: rename(B, A) | 重命名交换，死锁或最终状态一致 |
| 并发目录遍历与创建 | 2 | 同一目录 | C1: readdir(D) C2: creat(D/F) | 遍历时创建，是否遗漏或重复 |
| 并发目录遍历与删除 | 2 | 同一目录 | C1: readdir(D) C2: unlink(D/F) | 遍历时删除，是否出现无效条目 |
| 并发截断与读取 | 2 | 同一文件 | C1: truncate(F, new_size) C2: read(F) | 截断过程中读取，读到旧数据、新数据或EOF |
| 并发截断与写入 | 2 | 同一文件 | C1: truncate(F, small) C2: write(F, large_offset) | 截断后写入越界，是否扩展文件或失败 |
| 多个节点同时rename同一文件 | 3 | 同一文件 | C1: rename(F, G) C2: rename(F, H) C3: rename(F, I) | rename并发冲突，只有一个成功，其他失败 |
| 删除父目录时，子目录内正在创建 | 2 | 父目录和子目录 | C1: rmdir(D) C2: mkdir(D/C/file)或creat(D/C/file) | 目录树的原子性、删除与创建的交织 |
| 重命名父目录时，子目录内读取文件 | 2 | 父目录和子目录 | C1: rename(D, E) C2: open(D/F)或read(D/F) | 路径解析的原子性、dentry 缓存的更新时机、跨节点缓存一致性 |
| 删除非空父目录（强制删除）与子目录内写入 | 2 | 父目录和子目录 | C1: 递归删除 D（unlink(D/F) → rmdir(D) C2: write(D/F, data) | 递归删除与并发写入的交互、暂存机制在目录删除时的行为 |
| 属性更新与获取 | 2 | 同一文件 | C1: chmod(F) C2/owner: getattr(F) | 一致性，缓存更新 |
