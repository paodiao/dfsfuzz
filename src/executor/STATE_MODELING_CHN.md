# 面向Fuzzing反馈的分布式文件系统状态建模

## 1. 背景

代码覆盖率告诉我们*哪些代码被执行了*，但无法告诉我们*这些代码产生了什么行为*。
对于一个分布式文件系统而言，两种完全不同的调度可以覆盖相同的代码边，
却产生截然不同的文件系统状态——一种是存在一致性违规的，另一种则没有。

传统的AFL风格边覆盖率在分布式系统中存在根本性盲区：

- **快速饱和**：HMDFS实现为响应式事件循环——相同的`write()`路径在每次写操作中都会执行，
  无论操作的是哪个文件、哪个偏移量，或者其他哪个节点正在并发访问。
- **缺乏因果信息**：边覆盖率将每个节点的代码执行视为彼此孤立的。它无法区分
  "节点A先写入、节点B后读取"与"节点A和节点B并发写入"。
- **缺乏状态感知**：覆盖率不关心测试完成后文件系统的实际状态——
  无论文件是被创建、删除、损坏，还是残留了过期内容。

状态感知的反馈是Monarch模糊测试流水线的合理下一步。
本文档调研了状态表示技术的现状，并针对HMDFS特定的场景推荐了一条前进路径。

---

## 2. HMDFS系统特性

HMDFS是一个去中心化的可堆叠覆盖文件系统。其架构为状态建模带来了特定的挑战：

| 特性 | 状态建模启示 |
|---------|---------------------------|
| **merge_view** — 所有节点看到一个统一命名空间 | Fuzzer的外部视图是`merge_view`——状态建模应与此对齐 |
| **device_view/local & device_view/remote** — 每个节点的缓存副本 | 对不同视图的操作具有不同的一致性保证 |
| **异步回写** — `write()`在数据到达服务器前就返回 | 在任意时刻的状态快照可能会捕获到"在途"写入 |
| **Stash/restore** — 离线节点在本地缓存写入 | 节点崩溃+恢复产生的状态转换可能在快照之间是不可见的 |
| **带TTL的Dentry cache** — 目录列表可能是过期的 | `readdir()`的结果取决于缓存时效，而非仅仅是文件系统状态 |
| **Comrade列表** — 跨节点跟踪merge_view文件 | 同一个merge_view inode在不同客户端上可能对应不同的下层inode |
| **分层命名空间上的CRUD** — mkdir/creat/unlink/rename/write/read | 对同一路径的操作可能冲突；对不同路径的操作可交换 |

---

## 3. 9种方法的调研

### 3.1 Lamport时间线与Happens-Before摘要（基线：Mallory）

**原理**：每个节点维护一个单调递增的Lamport时钟。
每个事件携带时间戳；消息传递传播因果关系。
*happens-before*关系定义了对事件的偏序。反馈信号是对哪些因果路径被覆盖到的摘要。

**Mallory的实现**（Meng等人，CCS 2023）：
- 从截获的网络包、客户端请求/响应以及编译时代码插桩中动态构建Lamport时间线。
- 将时间线抽象为*happens-before摘要*——每个节点的
  （事件类型A happened-before 事件类型B）对集合。
- 使用MinHash将相似的抽象聚类为状态。
- Q-learning选择故障类型以最大化状态探索。

**核心洞察**："一个分布式系统中没有两次运行会产生相同的时间线。
新的观测总是会产生新的反馈，尽管许多运行在测试目的上是等效的。"
*抽象*步骤是关键的创新——将原始时间线压缩为可比较的摘要。

**HMDFS适配度：3/5**

| 优势 | 劣势 |
|----------|----------|
| 捕获跨节点的因果顺序 | 对文件内容、目录结构或一致性一无所知 |
| Mallory的抽象层可推广到任何事件类型 | 需要在内核层面追踪所有跨节点消息 |
| 适用于对stash/writeback时序的推理 | HMDFS套接字追踪会带来不小的插桩开销 |

**所需数据**：每个节点的Lamport时钟；每个HMDFS套接字send/recv的追踪记录；
每个系统调用的追踪记录（函数名、参数、节点、时钟）。

**参考文献**：Mallory (Meng et al., CCS 2023)；Lamport (1978) "Time, Clocks,
and the Ordering of Events in a Distributed System"。

---

### 3.2 向量时钟 + 状态快照

**原理**：将Lamport时钟推广为N维向量——比Lamport更强（能精确区分并发与因果关系）。
在同步点对文件系统进行快照，每个快照都带上当前的向量时钟时间戳。

**HMDFS适配度：4/5**

| 优势 | 劣势 |
|----------|----------|
| 精确的并发检测（并发 vs. 因果） | 每条消息O(N)的开销（N = 节点数） |
| 在时钟标记点进行状态快照能产生更丰富的反馈 | 快照频率需在精度与开销之间做权衡 |
| 适用于对stash/writeback时序分析 | 每次快照需要遍历整个文件系统 |

**参考文献**：Fidge (1988)；Mattern (1989)；Amazon Dynamo的向量时钟冲突解决方案。

---

### 3.3 操作DAG

**原理**：将操作（mkdir、write、unlink、rename等）建模为有向无环图的顶点。
边表示因果依赖（happens-before）或数据依赖（读取来自写入）。一次测试产生一个DAG。
新颖性 = 新的DAG拓扑结构。

**HMDFS适配度：5/5**

| 优势 | 劣势 |
|----------|----------|
| 操作本身就是文件系统操作——完美的语义匹配 | 图同构计算昂贵（需要近似方法） |
| DAG捕获了一次测试的完整因果结构 | 需要追踪操作间的因果依赖 |
| 非常适合merge_view的交错分析 | 实现复杂度高 |

**所需数据**：每个节点的完整操作序列；每个操作的因果上下文；数据依赖边（一次读取观察到的是哪次写入）。

**参考文献**：SAMC (Semantic-Aware Model Checking)；CoCain (ASPLOS 2023)；WiFe
(OSDI 2020)。

---

### 3.4 基于CRDT的模型

**原理**：将文件系统建模为无冲突复制数据类型（Conflict-free Replicated Data Types）。
文件系统树 = 只增集合（路径，元数据）对 + 最后写入者胜出的内容寄存器。
操作产生本地状态更新；合并传播到副本。

**HMDFS适配度：2/5**

| 优势 | 劣势 |
|----------|----------|
| CRDT专为具有收敛保证的并发更新而设计 | POSIX语义（硬链接、rename原子性、权限）很难用CRDT表达 |
| 适用于检测一致性错误 | Writeback/stash机制比简单的CRDT合并更复杂 |
| 具有清晰的代数性质 | 需要为每个文件系统操作建立形式化的CRDT模型 |

**参考文献**：Shapiro et al. (2011)；Kleppmann et al. (2019)；AntidoteDB；
CISE (Najafzadeh et al., 2020)。

---

### 3.5 文件系统树签名

**原理**：直接对可观测的文件系统状态进行哈希。状态 = 完整的文件树及其属性：
`path → (inode, type, size, mtime, uid, gid, mode, content_hash, xattrs)`。
树按规范顺序（按路径排序）序列化后进行哈希。新颖性 = 新的哈希值。

**HMDFS适配度：4/5**

| 优势 | 劣势 |
|----------|----------|
| 直接捕获Fuzzer关心的内容 | 只能看到最终状态，无法看到中间状态或操作顺序 |
| 实现简单——Monarch的`write_metadata`已经在收集这些数据 | 内容哈希 + 树遍历对于大型文件系统开销很大 |
| 自然地适用于跨节点一致性检查 | 无语义 |
| Monarch的`MdCmp`和`ConcFSCheck`已经在消费这些数据 | |

**所需数据**：已由`write_metadata()` → `fsMd[path].StatMd` + `Checksum`收集。

**参考文献**：Monarch自身的SymSC检查器；Tripwire/AIDE（文件系统完整性检查）；
git基于Merkle树的内容寻址。

---

### 3.6 从追踪中挖掘谓词

**原理**：从事件追踪中提取表征测试的逻辑谓词。示例：
- `WRITE_BEFORE_READ(file, node_A, node_B)` — 节点A的写入happened-before节点B的读取
- `MKDIR_DURING_NODE_OFFLINE(dir, node)` — 在某个节点不可达期间创建目录
- `WRITEPAGE_ACROSS_NODES(file)` — 在一个节点上写，在另一个节点上回写
- `CONCURRENT_UNLINK_AND_OPEN(file)` — unlink和open在时间上重叠
- `RENAME_DURING_READDIR(dir)` — 在目录列表期间进行rename

谓词构成一个布尔*特征向量*。新颖性 = 一个新的谓词组合被满足。

**HMDFS适配度：5/5**

| 优势 | 劣势 |
|----------|----------|
| 恰好捕获感兴趣的语义场景 | 谓词设计是领域特定的（需要HMDFS专业知识） |
| 无需新的数据收集——使用已有的eBPF追踪事件 | 谓词空间必须手动策划或自动挖掘 |
| 非常适合HMDFS特定的模式（stash/writeback、dentry TTL、comrade冲突） | 完备性取决于谓词覆盖范围 |

**所需数据**：已收集：eBPF kretprobe事件（15个merge函数 + `WRITEPAGE_CB`）、每节点`fsMd`、
`infos`（每次调用的errno/ret值）。

**参考文献**：Daikon（动态不变量检测）；TxF (EuroSys 2013)；Liblit
et al. (2005)。

---

### 3.7 基于图的状态模型

**原理**：将整个DFS（分布式文件系统）执行过程建模为一个异构图。节点：
物理节点、文件、目录、inode、stash条目、缓存条目。
边：`created_by`、`read_by`、`cached_in`、`stashed_to`、`synced_to`、
`belongs_to_node`。新颖性 = 新的图结构（通过Weisfeiler-Lehman图核或graph2vec嵌入）。

**HMDFS适配度：4/5**

| 优势 | 劣势 |
|----------|----------|
| 最丰富的表示——捕获所有实体和关系 | 静态图丢失时间顺序，除非边带有时间戳 |
| 非常适合comrade列表、stash图、缓存依赖的可视化 | 图同构是NP难的；需要近似方法 |
| | 实现复杂度高 |

**参考文献**：Weisfeiler-Lehman图核；graph2vec, GraphSAGE；Sherlock
(OSDI 2020)。

---

### 3.8 可交换性与等价类

**原理**：在操作序列上定义等价关系。两个序列如果产生相同的可观测文件系统状态，
则视为等价。基于可交换性对操作进行分组：
- 不同文件上的操作可交换
- 同一文件上不重叠偏移量的操作可交换
- 读取与读取可交换；mkdir(A)和mkdir(B)可交换
- mkdir(A)和mkdir(A)不可交换（第二次会失败）

反馈 = 等价类的哈希值（归一化序列）。这过滤掉了调度噪声——
等价的交错被映射到相同的信号。

**HMDFS适配度：4/5**

| 优势 | 劣势 |
|----------|----------|
| 极好的降噪能力——等价的交错不会膨胀语料库 | 为POSIX操作定义完整的可交换性模型并非易事 |
| 清晰简洁的语义模型 | 需要领域特定的形式化规范 |
| 适用于merge_view并发操作 | |

**参考文献**：Molly (OSDI 2015)；CSPEC (SOSP 2017)；Sieve (ASPLOS 2021)。

---

### 3.9 混合组合

**原理**：结合互补的方法。针对HMDFS的最佳组合：

**（推荐）谓词挖掘 + 文件系统树哈希**

- **谓词挖掘**：从eBPF事件追踪中提取语义模式。捕获状态是*如何*产生的
  （因果模式、并发、故障交互）。
- **文件系统树哈希**：捕获最终状态是*什么*。提供一个独立的"基本事实"，
  与如何达成该状态无关。
- 两者都使用Monarch已收集的数据——**零新增插桩**。

新颖性反馈 =（新谓词被满足）OR（新的文件树哈希）。

---

### 3.10 在每种方法中区分操作的不同结果

谓词挖掘和操作DAG都可以区分结果（返回值、参数、目标文件）不同的同种操作。
关键区别在于*如何*区分——手动与自动——以及随之而来的权衡。

#### 3.10.1 为什么ret/args很重要

相同的操作，不同的结果意味着不同的行为：

| 操作 | 结果 | 为何不同 |
|-----------|----------|-----------------|
| `write(fd, 128)` vs `write(fd, 0)` | 写入128字节 vs. 磁盘已满 | 不同的系统状态 |
| `mkdir(/A)` vs 再次 `mkdir(/A)` | ret=0 vs. ret=-EEXIST | 第二次调用看到不同的命名空间 |
| `open(f, O_RDWR)` vs `open(f, O_CREAT\|O_RDWR)` | args[1]标志不同 | O_CREAT可能创建，纯open则失败 |
| `rename(A, B)` 同目录 vs 跨目录 | args[0]和args[2]不同 | 跨目录rename具有不同的原子性保证 |
| `lookup(dir)` 成功 vs 失败 (ENOENT) | dentry cache命中 vs. 未命中 | 不同的内部代码路径 |

我们的`HmdfsTraceEvent`已经从eBPF kretprobe中捕获了`{FuncID, Ret, Ino, Args[6]}`——
按结果区分需要**零新增数据收集**。

#### 3.10.2 谓词挖掘：手动粒度

谓词模板被扩展，加入结果条件：

```
之前（粗粒度）：
    MKDIR(path)
    WRITE(file)
    OPEN(path)

之后（结果感知）：
    MKDIR_SUCCESS(path)      ≡ ret == 0
    MKDIR_EEXIST(path)       ≡ ret == -EEXIST
    MKDIR_ENOSPC(path)       ≡ ret == -ENOSPC
    WRITE_TO(file, bytes=N)  ≡ args[2] == N
    WRITE_APPEND(file)       ≡ args[1] == 0
    OPEN_O_CREAT(path)       ≡ (args[1] & O_CREAT) != 0
    OPEN_RDONLY(path)        ≡ (args[1] & O_ACCMODE) == O_RDONLY
    RENAME_CROSS_DIR         ≡ args[0] != args[2]
    LOOKUP_HIT(path)         ≡ ret == 0
    LOOKUP_MISS(path)        ≡ ret == -ENOENT
```

**`HmdfsTraceEvent`的每个字段都可以成为谓词参数**：

| 字段 | 可作为参数的方式 |
|-------|-------------------|
| `FuncID` | 操作类型（谓词模板选择器） |
| `Ret` | 成功/失败/errno分类 |
| `Ino` | "同一文件"标识（例如 `WRITE_THEN_READ_SAME_FILE`） |
| `Args[0..5]` | 标志、偏移量、长度、父inode指针 |

**优势**：你来选择哪些区别是重要的。反馈粒度恰好是你定义的——不多不少。

**风险**：你没有定义的就是盲区。一个没有编写的谓词模板就是一个没有追踪的行为维度。

#### 3.10.3 操作DAG：自动编码

每个操作成为一个DAG顶点。顶点标识由所有可用字段计算：

```
vertex_hash = hash(op_type, path, ret, args, ino)
```

不同的结果**自动**产生不同的顶点。DAG签名在任何事件发生变化时自然改变——
不需要手动模板设计。

**优势**：零盲区。每个变化都会被捕获。

**风险**：信号爆炸。如果`write(file, offset=0, len=1)`和
`write(file, offset=1, len=1)`产生不同的顶点，即使它们在语义上可能是等价的，
Fuzzer也会将其视为"新颖"的。抑制这种噪声需要额外的*抽象层*
（顶点等价类、图同构近似或降维）。

Mallory通过在happens-before摘要上应用MinHash聚类来解决这一噪声问题——
这是一个明确将相似但非相同的时间线归入同一状态的抽象层。

#### 3.10.4 总结

| | 谓词挖掘 | 操作DAG |
|---|:--:|:--:|
| 结果区分 | 手动（模板 + 条件） | 自动（哈希包含一切） |
| 粒度控制 | 强（由你定义） | 弱（需要抽象层） |
| 盲区风险 | 存在（未编写的谓词） | 无（自动） |
| 噪声风险 | 低（仅定义的谓词触发） | 高（每个参数变化都算） |
| 从当前数据实现 | 就绪——谓词直接消费`HmdfsTraceEvent` | 就绪——DAG顶点消费同一结构体 |

哪种方法更适合取决于Fuzzing阶段：早期探索倾向于自动DAG编码（捕获意外行为），
而有针对性的缺陷搜寻倾向于手动谓词（聚焦于已知危险模式）。

---

### 3.11 确定性模拟 / 基于模型的测试

**原理**：被测系统运行在模拟器内部，所有非确定性（消息排序、时钟偏差、故障时机）
都被显式控制。每次执行都是完全可复现的。单机模拟器通过系统性地重排事件来
探索不同的交错。FoundationDB的模拟框架是典型的范例——他们的整个测试流水线
在单机的确定性模拟器中运行。

**状态表示**：系统的*内部*逻辑状态，在模拟器中完全可见（不仅仅是外部可观测状态）。

**新颖状态检测**：对比每次交错后的逻辑状态。可复现性确保任何缺陷都能被确定性地重放。

**HMDFS适配度：1/5**

| 优势 | 劣势 |
|----------|----------|
| 完美的可复现性——发现的任何缺陷都可以轻松重放 | HMDFS是一个内核模块——无法在用户空间模拟器中运行 |
| 完整的内部状态可见性 | 需要将文件系统移植到模拟框架中 |
| FoundationDB级别的覆盖能力 | 与基于KVM/QEMU的Fuzzing架构根本性不兼容 |

**所需数据**：不适用——需要HMDFS内核代码在一个用户空间确定性模拟器中运行。

**参考文献**：FoundationDB模拟框架；Turmoil (Tokio)；P语言
(Microsoft Research)。

---

### 3.12 从追踪中挖掘不变量（Daikon风格）

**原理**：从执行追踪中自动推断程序不变量。
不变量是一个逻辑谓词，在所有观测到的到达某一程序点的运行中都成立——
例如，成功`open()`之后 `fd > 0`，或一次`write()`之后
`file.size >= write.offset + write.len`。类似Daikon的工具
在函数入口/出口点插桩程序变量并挖掘统计不变量。
对先前推断的不变量的违反表示出现了新行为。

**状态表示**：一组关于程序变量及其关系的逻辑谓词。
与谓词挖掘（方法6）不同——此处的谓词是从数据中*推断*出来的，而非由用户*设计*。

**新颖状态检测**：新行为 = 违反先前持有的不变量，或发现一个新的、从未见过的不变量。

**HMDFS适配度：3/5**

| 优势 | 劣势 |
|----------|----------|
| 零手动谓词设计——全自动化 | HMDFS是内核代码——插桩必须是编译时的（类似于我们的追踪点）|
| 与丰富的变量级追踪配合良好 | 当前的eBPF追踪捕获的是返回值，而非内部程序变量 |
| 补充谓词挖掘（方法6） | 需要对HMDFS内部代码路径进行逐变量插桩 |

**所需数据**：对HMDFS内部函数进行插桩，以在函数入口/出口处捕获局部变量值
（超出当前eBPF kretprobe的捕获范围）。当前的`HmdfsTraceEvent.Args[6]`可以作为
指针级不变量的起点。

**参考文献**：Daikon (Ernst et al., 2007)；DIDUCE (Hangal & Lam, 2002)；
TerrAscope (Joshi et al., 2019)。

---

### 3.13 基于Span的追踪（分布式追踪，OpenTelemetry风格）

**原理**：每个操作生成一个*span*（由trace_id、span_id、parent_span_id标识）。
Span跨节点传播——客户端A上的一个`write()`创建父span，客户端B上相应的
`writepage_cb`创建一个子span，通过parent_span_id链接。结果是
一个分层的span DAG。Jaeger、Zipkin和Dapper开创了这一模型。

**状态表示**：一个span DAG，其中边表示父子关系（分层的），
与Lamport时间线中的扁平happens-before边不同。

**与Lamport时间线的关键区别**：Lamport边是扁平的时间箭头。Span边是*嵌套*的——
一个包含`lookup` + `getattr`子span的`readdir` span，在语义上比Lamport时间线上
三个并排的事件更丰富。这种层次结构在协议层面捕获了"什么引起了什么"。

**新颖状态检测**：新的span DAG拓扑——新的父子关系或新的嵌套模式。

**HMDFS适配度：4/5**

| 优势 | 劣势 |
|----------|----------|
| 层次结构捕获操作因果关系 | 需要span之间的父子链接传播 |
| 自然地映射到VFS操作（open → read → close）| HMDFS追踪点需要交叉引用才能链接span |
| 我们的eBPF事件可以按ino+时间窗口聚合为span | |

**所需数据**：已有的eBPF追踪事件（已收集）。聚合逻辑：
将短时间窗口内（例如1ms）的事件按`ino`分组到单个span中。
跨节点span链接通过匹配`remote_ino`和`WritePageCB`事件实现。

**参考文献**：Google Dapper (2010)；OpenTelemetry；Jaeger；Zipkin。

---

### 3.14 面向状态系统的基于属性的测试（QuickCheck风格）

**原理**：定义系统的形式化状态机模型：一个状态类型
（例如 `Map Path FileState`），以及操作的前/后条件（`mkdir`：
"父目录存在，子目录不存在" → "子目录存在，具有空目录状态"）。
测试框架随机生成操作序列，在实际系统上执行它们，并检查观测到的状态是否与模型的
预测一致。Erlang QuickCheck和Rust `proptest`实现了这种模式。

**状态表示**：用户定义的抽象状态类型和转换函数——显式且形式化。

**新颖状态检测**：任何模型与实际系统出现分歧的状态 = 发现了一个缺陷。

**HMDFS适配度：3/5**

| 优势 | 劣势 |
|----------|----------|
| 对建模属性提供形式化正确性保证 | 需要建模HMDFS的完整类POSIX状态机 |
| 同时发现状态错误和崩溃错误 | 建模工作量与DFS复杂性成正比 |
| 适用于文件系统（POSIX语义被广泛理解） | 异步回写和stash/restore使模型复杂化 |

**所需数据**：一个HMDFS操作的形式化状态机模型。这是一个设计产物，而非运行时数据。
运行时验证使用`fsMd`进行状态观测（已可用）。

**参考文献**：Erlang QuickCheck (Hughes, 2007)；Rust proptest状态机；
Haskell Hedgehog顺序/并行测试。

---

### 3.15 基于增量的状态抽象

**原理**：不记录*绝对*文件系统状态（完整树哈希），而是记录*增量*——
在调度期间实际发生了什么变化。一个增量是一组`(path, before_state, after_state)`元组。
状态签名 = 增量的哈希，而非树的哈希。未变化的文件被隐式忽略。

**状态表示**：`{path → (before: Stat, after: Stat) | after != before}`
加上`{path → CREATED | DELETED}`条目。

**新颖状态检测**：对任何路径的新的（before, after）对，或新的变化组合。
一个修改了路径A和B的调度与一个修改了路径A和C的调度是不同的，
即使最终树的哈希值相同（不太可能，但可能因碰撞而出现）。

**HMDFS适配度：4/5**

| 优势 | 劣势 |
|----------|----------|
| 比完整树哈希更细的粒度 | 需要旧FileTree进行比较 |
| 捕获*哪些*文件发生了变化，而不仅仅是*有东西*变了 | |
| Monarch的`SyncFileTreeFromFsMd`内部已经计算了差异 | |

**所需数据**：旧FileTree（来自前一次的`SyncFileTreeFromFsMd`）、当前fsMd（来自`write_metadata`）。
两者在`executeRaw`中都已可用。

---

### 3.16 基于TSC的全局Happens-Before（硬件时钟全序）

**原理**：使用硬件TSC（时间戳计数器）作为跨所有VM的全局时钟。
由于所有QEMU VM共享同一个物理主机TSC，每个VM有各自的偏移量（`+invtsc`），
任何VM的`rdtsc() - tsc_offset`都会得到相同的主机TSC。
这给出了跨所有节点的所有事件的*全序*。从此全序推导出*偏序*（happens-before）：
执行区间重叠的事件是"并发的"；区间不重叠的事件可以排序。

**状态表示**：二维——一个按TSC排序的全序事件序列，
叠加上从区间重叠分析推导出的happens-before偏序。

**与Lamport时间线的关键区别**：Lamport需要发送和接收消息来传播逻辑时钟——
它从通信中推导因果关系。基于TSC的排序从*物理时间*（区间不重叠）推导因果关系。
两者是互补的：Lamport捕获应用级因果关系，TSC捕获挂钟排序。

**新颖状态检测**：新的happens-before边模式、新的并发事件集合或新的事件交错。

**HMDFS适配度：5/5**

| 优势 | 劣势 |
|----------|----------|
| 无需消息追踪——使用硬件TSC | 异步回写破坏了"etime = 完成"的假设 |
| TSC偏移基础设施在Monarch中已就位 | 需要单独追踪`WritePageCB`来获取写入完成时间 |
| 适用于共享同一主机的QEMU VM | 不适用于真实（非虚拟化）的多机环境 |

**所需数据**：每次调用的TSC开始/结束时间戳（已在`callReply`中，
已解析到`CheckInfo.Stime/Etime`）；每个VM的`tsc_offset`（已在
`argv[15]`中）；`WritePageCB`追踪点时间戳（perf收集在进行中）。
所有数据通道已构建或正在构建中。

---

### 3.17 内容寻址状态（Merkle DAG）

**原理**：每个文件通过其内容哈希进行内容寻址（已在fsMd中的`Checksum`中）。
每个目录的标识是其子项哈希串联的哈希值。这形成了一个Merkle DAG——
Git用于表示仓库状态的同一结构。根哈希唯一标识整个文件系统树。

**状态表示**：一棵Merkle树：`dir_hash = hash(child1_hash || child2_hash || ...)`
其中叶子哈希是文件内容校验和。两棵Merkle树之间的差异精确标识了哪些路径发生了变化，
以及处于层次结构的哪个级别。

**新颖状态检测**：新的根哈希 = 新颖状态。或者更精细地：
任何级别的新子树哈希 = 新颖的子行为。这自然地捕获了"只有文件/A/B发生了变化，其他一切不变"。

**HMDFS适配度：5/5**

| 优势 | 劣势 |
|----------|----------|
| 精确的变更定位——知道*哪个*子树发生了变化 | 构建Merkle树需要完整的目录遍历 |
| 内容校验和已在fsMd中收集（`Checksum`字段） | 父哈希必须从子哈希计算（不能直接从fsMd获取）|
| Git风格的语义直观且被广泛理解 | |

**所需数据**：每节点fsMd（已收集）。从fsMd条目通过重建目录层次结构来计算Merkle树。
根哈希 = 反馈信号。

**与文件树哈希（方法5）的关键区别**：方法5对完整的树序列化进行哈希。
方法17构建Merkle DAG——能够进行每个子树的差异检测。
如果只有`/subtree/A/file.txt`发生变化，只有从根到`file.txt`路径上的哈希改变——
DAG的其余部分不变。这实现了*局部化*的新颖性检测，而非*全局*。

---

### 3.18 模型引导的Fuzzing（基于TLA+形式化规范的覆盖率）

**原理**：使用分布式系统的抽象形式化模型（用TLA+编写）来定义*覆盖率*。
不是将代码边或文件哈希作为覆盖率度量，而是将形式化模型的状态空间作为覆盖目标。
Fuzzer生成测试输入并检查哪些抽象模型状态被到达。新行为 = 命中了一个此前未观察到的模型状态。

**核心洞察**：抽象模型通常是在协议设计与验证的早期阶段开发的（Two-Phase Commit、Raft、Paxos），
但在测试时很少被使用。此方法将形式化验证的工作（模型编写）与实际的测试（Fuzz活动）连接起来。

**状态表示**：一个TLA+模型的状态空间——一个权威的、形式化定义的所有可能系统状态的集合。
覆盖率据此参考来衡量。

**新颖状态检测**：任何尚未被Fuzzer覆盖的模型状态都是"新颖的"。
首次命中的模型状态 → 反馈信号。

**HMDFS适配度：2/5**

| 优势 | 劣势 |
|----------|----------|
| 形式化精确——模型状态是明确无歧义的 | HMDFS没有TLA+模型（编写一个是重大工程投入）|
| 覆盖率定义由形式化模型验证，而非猜测 | 模型必须与实现同步维护 |
| 在Etcd-raft和RedisRaft中发现了13个此前未知的缺陷 | 覆盖文件系统状态比共识协议状态更复杂 |

**所需数据**：一个HMDFS的TLA+（或PlusCal）模型，以及从实现级别观测值（我们的eBPF追踪事件）
到模型状态的映射。两者都是设计产物，而非运行时数据。

**参考文献**：Gulcan et al., "Model-Guided Fuzzing of Distributed Systems,"
ACM TOSEM 2024/2025 (based on arXiv:2410.02307)。

---

### 3.19 基于内存的状态推断（LSH内存快照）

**原理**：在内存分配和网络I/O处插入编译时探针。在运行时，
对长生命周期内存区域进行快照，应用*局部敏感哈希*（Locality-Sensitive Hashing，LSH）
将内存内容映射为唯一的状态标识符。Fuzzer从观测到的（memory_snapshot → next_memory_snapshot）
转换中逐步构建协议状态机——完全是自动的，无需任何手动状态标注或协议规范。

**核心洞察**：长生命周期的堆/全局内存区域隐式编码了协议状态。
LSH提供了模糊匹配——略微不同的内存内容（不同的缓冲区指针、计数器）仍然可以映射到相同的状态。

**状态表示**：一系列经过LSH哈希的内存快照。每个快照是一个状态ID。
快照之间的转换构成状态机。

**新颖状态检测**：新的LSH状态ID（或已知ID之间的新转换）= 新颖行为。

**HMDFS适配度：3/5**

| 优势 | 劣势 |
|----------|----------|
| 完全自动——无需手动状态标注 | HMDFS是内核代码；编译时内存探针更难插入 |
| LSH处理微小的内存变化（不同的指针值、计数器差异）| 内核模块中的长生命周期内存比用户空间更难做快照 |
| 适用于网络服务器（已在FTP、SMTP、HTTP、SSH、SIP、RTSP上验证） | 在系统调用粒度上做内核内存快照增加不小的开销 |

**所需数据**：对HMDFS进行编译时插桩，在VFS入口/出口点插入内存快照探针。
在运行时，与我们已收集的eBPF kretprobe事件配合使用。

**参考文献**：Natella, "StateAFL: Greybox fuzzing for stateful network
servers," EMSE 2021 (arXiv:2110.06253)。

---

### 3.20 基于枚举的状态识别（自动状态变量发现）

**原理**：协议实现通常使用带有命名常量（`INIT`、`READY`、`WAITING`）的`enum`类型变量
来表示当前状态。通过在编译时分析源代码，可以自动识别这些状态变量。
在Fuzzing期间，跟踪这些变量被赋值的值序列，生成已探索状态空间的"地图"——
同样是零手动标注。

**核心洞察**：对使用最广泛的50个开源协议实现进行实证分析发现，
**每个实现**都使用赋值为命名常量的状态变量来表示状态。这是一个通用的编码模式，
可以被自动利用。

**状态表示**：对于每个识别出的枚举变量，记录其赋值的序列。
状态 =（变量名 → 当前值）元组。

**新颖状态检测**：新的（变量名 → 值）对，或已知状态之间的新转换。

**HMDFS适配度：3/5**

| 优势 | 劣势 |
|----------|----------|
| 普遍适用于任何具有枚举状态变量的代码库 | HMDFS状态部分存在于位标志中（inode状态、stash状态），不总是枚举 |
| 完全自动——无需手动标注 | 需要对HMDFS代码进行源码级静态分析 |
| 在知名协议实现中发现了多个CVE | 可能遗漏编码在非枚举数据结构（链表、计数器）中的状态 |

**所需数据**：对HMDFS源码进行静态分析以识别枚举状态变量。
运行时插桩以追踪值赋值。与我们的eBPF追踪互补。

**参考文献**：Ba, Böhme, Mirzamomen, Roychoudhury, "Stateful Greybox
Fuzzing," USENIX Security 2022 (arXiv:2204.02545)。

---

### 3.21 负载方差引导的Fuzzing（Themis，面向DFS）

**原理**：专门为分布式文件系统设计。将客户端请求和系统配置输入同时建模为操作序列。
使用*负载方差*作为反馈信号——Fuzzer主动尝试让不同节点的负载尽可能不同，
因为不均衡是DFS部署中缺陷的主要来源。负载检测器监控每个节点的资源使用情况
（CPU、内存、I/O）并识别不均衡。

**核心洞察**：DFS缺陷通常表现为*不均衡*——一个节点过载，另一个节点空闲。
最大化负载方差可以系统地暴露负载敏感的缺陷（由于协调节点过载导致的挂起、崩溃、数据不一致）。

**状态表示**：系统的跨节点负载分布向量，加上产生该分布的
操作/配置序列。

**新颖状态检测**：新的负载分布模式（不同的节点间偏差）或达到极端方差的新操作序列。

**HMDFS适配度：4/5**

| 优势 | 劣势 |
|----------|----------|
| 面向DFS——专为文件系统测试设计 | 负载方差是"有趣行为"的一个维度，但并非穷举 |
| 将客户端请求和系统配置结合作为输入空间 | 需要每个节点的资源监控（CPU、内存、I/O）——新的数据收集 |
| 在包括CephFS和GlusterFS在内的4个真实DFS中发现了10个新缺陷 | 负载探测可能干扰Fuzzer的时序 |

**所需数据**：执行期间每个节点的资源指标（CPU、内存、
I/O），以及操作序列（我们的eBPF追踪）。配置空间建模
（节点数、副本数、条带大小）——设计产物。

**参考文献**：Chen et al., "Themis: Finding Imbalance Failures in Distributed
File Systems via a Load Variance Model," SOSP 2025。

---

## 4. 比较分析（全部21种方法）

| 方法 | 状态丰富度 | 并发性 | FS语义 | HMDFS适配度 | 复杂度 |
|----------|:---:|:---:|:---:|:---:|:---:|
| 1. Lamport时间线 | ★★ | ★★★★ | ★ | ★★★ | 中 |
| 2. 向量时钟 + 快照 | ★★★ | ★★★★★ | ★★★ | ★★★★ | 高 |
| 3. 操作DAG | ★★★★ | ★★★★★ | ★★★★ | **★★★★★** | 高 |
| 4. CRDT模型 | ★★★ | ★★★★ | ★★ | ★★ | 非常高 |
| 5. 文件树哈希 | ★★★ | ★ | **★★★★★** | ★★★★ | 低 |
| 6. 谓词挖掘 | ★★★★ | ★★★★ | **★★★★★** | **★★★★★** | 中 |
| 7. 基于图的状态 | **★★★★★** | ★★★ | ★★★★ | ★★★★ | 非常高 |
| 8. 可交换性等价 | ★★★ | **★★★★★** | ★★★ | ★★★★ | 非常高 |
| 9. 混合（6+5） | **★★★★★** | ★★★★ | **★★★★★** | **★★★★★** | 中-低 |
| 10. 确定性模拟 | **★★★★★** | **★★★★★** | **★★★★★** | ★ | — (不适用) |
| 11. 不变量挖掘（Daikon）| ★★★ | ★★★ | ★★★ | ★★★ | 高 |
| 12. 基于Span的追踪 | ★★★★ | ★★★★ | ★★★ | ★★★★ | 中 |
| 13. 基于属性的测试 | ★★★ | ★★★ | ★★★★ | ★★★ | 非常高 |
| 14. 基于增量的状态 | ★★★★ | ★ | **★★★★★** | ★★★★ | 低 |
| 15. 基于TSC的Happens-Before | ★★★★ | **★★★★★** | ★★ | **★★★★★** | 低-中 |
| 16. Merkle DAG状态 | ★★★★ | ★ | **★★★★★** | **★★★★★** | 中 |
| 17. 混合（6+14+15+16）| **★★★★★** | **★★★★★** | **★★★★★** | **★★★★★** | 中 |
| 18. 模型引导的Fuzzing（TLA+）| ★★★★ | ★★★ | ★★★ | ★★ | 非常高 |
| 19. 基于内存的状态（LSH）| ★★★★ | ★★★ | ★★★ | ★★★ | 高 |
| 20. 基于枚举的状态ID | ★★★ | ★★★★ | ★★ | ★★★ | 中 |
| 21. 负载方差引导（Themis）| ★★★ | ★★ | ★★★★ | ★★★★ | 中 |

---

## 5. 推荐方案与演进路径

### 5.1 主要推荐：代码覆盖率 + 操作DAG

两个维度覆盖分布式文件系统fuzzing的核心问题：

| 反馈维度 | 方法 | 捕获什么 | 数据状态 |
|---|---|---|---|
| 代码级探索 | 边覆盖率（现有） | *执行了什么代码* — 新代码路径 | 已完成 (KCOV) |
| 因果结构 | 操作DAG（方法3） | *操作之间如何因果关联* — 新的交叠模式、新的冲突模式 | 数据就绪 (eBPF追踪) |

**为什么两个维度足够**：

- **边覆盖率**驱动*探索*——fuzzer发现哪些系统调用和参数组合到达新的代码区域。
  这是AFL经过验证的反馈循环。
- **操作DAG**驱动*行为发现*——fuzzer发现哪些因果模式（写后读、并发mkdir、
  writepage屏障）已经被触发。这是"分布式系统的新代码边等价物"——
  Mallory的核心洞察适配到文件系统。

两个维度实现为**两条独立的反馈通道**：代码覆盖率喂养现有的`maxSignal`集合，
操作DAG喂养专用的`maxDagSignal`集合，并有独立的dashboard统计
（`dag pair signal`、`dag schedule signal`、`dag corpus`）。通道分离是为了
让DAG反馈可以独立于覆盖率进行分析。两者回答同一个问题——*这个bit是否见过？*——
但从不混合：DAG bit 不会作为覆盖率上报，覆盖率 bit 也不会作为 DAG 模式上报。

额外的维度（树结构抽象、操作分布向量、谓词挖掘、span追踪等）并非独立的反馈信号；
它们是*同一底层数据的不同编码方式*。正确构建的操作DAG已经捕获了这些替代方案
试图近似的因果结构。

### 5.2 阶段计划

**阶段1（数据已收集，需要算法）**：

```
  代码覆盖率（现有，KCOV → AFL风格边哈希 → maxSignal）
+ 操作DAG（eBPF追踪 → 基于ino的因果规则 → DAG → DAG哈希 → maxDagSignal）
  ────────────────────────────────────────────────────────
  两条独立的反馈通道（覆盖率 vs DAG）
```

操作DAG从eBPF追踪数据中构造，使用以下因果规则（方法3.3中已枚举）：

| 规则 | 从eBPF数据推导 |
|------|-------------|
| 读写依赖（同ino，区间重叠） | `FuncID ∈ {WRITE, READ}`，同 `Ino`，`args[offset]`/`args[len]` 重叠 |
| 写写依赖（同ino，区间重叠） | `FuncID ∈ {WRITE, WRITE}`，同 `Ino`，区间重叠 |
| Fsync屏障（同ino，write在fsync前） | `FuncID ∈ {WRITE} → FuncID ∈ {FSYNC}`，同 `Ino` |
| WRITEPAGE_CB锚点（ino，writeback完成在read之前） | `FuncID ∈ {WRITEPAGE_CB}` → `FuncID ∈ {READ}`，同 `Ino` |
| 命名空间：创建在打开之前 | `FuncID ∈ {CREATE, MKDIR} → OPEN`，通过 `Ino` 链确定路径关系 |
| 命名空间：删除/创建冲突 | `FuncID ∈ {UNLINK, RMDIR}` 与 `FuncID ∈ {CREATE, MKDIR}` 重叠，相同路径/ino |
| 无关系 | 不同 `Ino`，无父子命名关系 |

这需要**零新数据采集**——所有字段（`FuncID`、`Ino`、`Args`、`Timestamp`）
已存在于 `HmdfsTraceEvent` 中。

**阶段2（可选增强，如果后续需要）**：

```
+ 谓词挖掘（叠加在DAG之上的语义谓词）
+ 基于Span的追踪（层次化结构，另一种DAG视角）
+ 基于属性的测试（需要形式化HMDFS状态模型）
```

### 5.3 设计原则：双信号，同一问题

Monarch 将代码覆盖率与操作 DAG 放在**两个独立的信号集合**中（`maxSignal` 与
`maxDagSignal`），但它们回答同一个问题："这个信号 bit 是新的吗？"fuzzer 从不
混合它们——DAG 新意驱动自己的 corpus 入队与统计，覆盖率统计保持纯净。
分离是刻意的分析选择（见 `DAG_KNOWN_ISSUES.md` 附录）；如果将来想要统一的
corpus 预算，两条通道随时可以合并回单一的 `maxSignal`。

Mallory的关键教训是*抽象*使反馈变得实用。我们选择操作DAG作为唯一的
行为级维度的依据是：文件系统的因果关系是有限且可枚举的——不像通用
分布式系统那样需要从消息传递中推断因果关系。文件系统的状态空间受其
命名空间和定义的操作集合所限——使基于DAG的反馈信号既可计算又全面。

---

## 6. 操作DAG反馈设计

本节从*调研*过渡到*设计*：在推荐反馈策略（代码覆盖率 + 操作DAG，参见§5.1）
的基础上，详细说明操作DAG如何从Monarch现有数据管道中构建，以及如何
将其约简为可操作的反馈比特位。

### 6.1 数据管道

```
eBPF事件 → HmdfsTraceEvent → ino → path 映射 (fsMd)
     │                                    │
     │                                    ▼
     │                             独立顶点:
     │                         (func_id, path, ret_bucket)
     │                                    │
     ▼                                    ▼
  TSC全局时间线                       路径解析
  (全序: stime/etime)                 (§6.2.1)
           │                            │
           └────────────┬───────────────┘
                        │
              ┌─────────┼─────────┐
              │                   │
         HB DAG边             并发对
    (路径关系 +             (时间重叠)
     修改者 + 不重叠)
              │                   │
              └─────────┬─────────┘
                        │
                   Pair集合 (HB + 并发)
                        │
                  ┌─────┴─────┐
                  │           │
             per-pair哈希   schedule哈希
             (类型级         (组合级
              新颖性)        新颖性)
                  │           │
                  └─────┬─────┘
                        ▼
              maxDagSignal（schedule位 →
              maxDagSchedSignal）：独立通道，
              从不并入覆盖率 maxSignal
```

**关键区分**：*TSC全局时间线*是原始输入——所有eBPF事件按其硬件时间戳排序。
*HB DAG*是从中提取的偏序——只包含同时满足路径关系规则和修改者/观察者条件
（§6.3）的因果边。DAG的边全部是happens-before边。时间上重叠的并发操作
不出现在DAG中；它们作为并发对单独捕获在签名中（§6.3.1）。

### 6.2 顶点定义

一个顶点代表一次eBPF捕获的merge-view VFS函数返回事件。

| 字段 | 来源 | 说明 |
|------|------|------|
| `func_id` | `HmdfsTraceEvent.FuncID` | 16种操作类型（MKDIR、WRITE、WRITEPAGE_CB等） |
| `path` | `HmdfsTraceEvent.Ino` → fsMd反查 | `merge_view/...` 路径；跨节点、跨schedule稳定 |
| `ret_bucket` | `HmdfsTraceEvent.Ret` → 分桶映射 | `SUCCESS / EEXIST / ENOENT / FAILURE / WRITEPAGE_DONE / WRITEPAGE_ERR` |
| `off` | write/read: `kiocb->ki_pos`；truncate: `iattr->ia_size`（仅当 `ATTR_SIZE`）；其余 0 | `offset_bucket` 的输入（结束位置——见 #22） |
| `size` | fsMd `StatMd.Size`（执行后） | `offset_bucket` 的参照（TAIL/BEYOND 边界）；**本身不是特征** |

**为什么用`path`而非`ino`**：HMDFS的merge_view inode号在不同客户端节点上
不同（参见§2）。`path`通过fsMd的`StatMd.Ino`反查得到，提供跨节点稳定的文件身份。

**`ino → path`反查**：

- kretprobe事件：`BPF_CORE_READ(inode, i_ino)` 给出完整的merge_view inode
  → 直接匹配 `fsMd[path].StatMd.Ino`。
- writepage事件：tracepoint的`ino_raw`字段（低32位）匹配
  `fsMd[path].StatMd.Ino & 0xFFFFFFFF`。
- 瞬态文件（在一个schedule中创建后又删除）：从eBPF事件中的
  `creat`/`mkdir`参数（`args[0]`）提取`path`。

### 6.2.1 路径匹配策略

顶点路径并非仅通过fsMd的`ino → path`反查来解析。只要可能，都从测试程序的调用序列
中获取路径——系统调用参数已包含fuzzer生成的路径。通过时间戳窗口将eBPF事件匹配到
调用，比基于inode的查找更简单、也更精确。

**三层路径解析**：

| 层级 | 操作类型 | 路径来源 | 方法 |
|:--:|------|------|------|
| 1 — 直接参数 | `mkdir`、`creat`、`unlink`、`rmdir` | `call.Args[0]`（路径字符串本身） | `extractPathFromCall(call)` |
| 2 — FD追踪 | `write`、`read`、`fsync`、`open`、`release`、`getattr`、`setattr`、`iterate` | 同prog内向前查找返回该fd的`open`/`creat`调用 | `resolveFdToPath(prog, call)` |
| 3 — ino兜底 | `writepage_cb`、未匹配事件 | fsMd：`{path → StatMd.Ino}` | `ino → path`反查 |

**匹配算法**：

```
for each call in ps[progIdx]:
    path = 通过层级1或2解析路径
    if path == "": continue

    candidates = [call.stime, call.etime] 窗口内
                 func_id匹配的eBPF事件

    if 1个候选 → 直接赋值

    elif >1个候选：
        // 名字冲突回退 — 测试程序使用唯一名字
        for 候选:
            d_name = 候选路径的最后一层组件名
            if d_name == path的最后一层组件名:
                赋值; break
        if 仍未匹配: 兜底取窗口内第一个候选

rename: old_path和new_path直接从`call.Args[0]`和`call.Args[1]`获取。无需反查。
```

**理由**：由于Monarch通过`randomSuffix`为每个测试生成唯一文件名，d_name匹配
非常可靠。回退链（时间戳窗口 → d_name → 第一个候选）确保即使在突变破坏了
名字唯一性的情况下也不会出错。

顶点**不合并**——每个eBPF事件成为自己的顶点。抽象层在签名时处理去重（§6.4）。

### 6.3 HB边

有向边 A→B 在同时满足**三个**条件时存在：

1. **因果依赖**：A和B共享一个路径关系（见下文）。
2. **A是成功的修改者**（§6.3.1）。失败的修改者（如`mkdir /A`返回`EEXIST`、
   `write`返回`ENOSPC`）不改变文件系统状态，无法对后续操作施加因果影响。
   其输出边被抑制。
3. **时序排序**：A.etime < B.stime（间隔不重叠）。

HB DAG是从TSC全局时间线（§6.1，全序）中推导出的*偏序*。它只包含
**直接**边——每条边都通过两个顶点的直接比较建立，绝不通过传递闭包。
传递信息（如`A→B→C`隐含A因果前驱于C）由schedule hash（§6.4）隐式
编码——它捕获所有pairs在一起出现的完整集合。并非时间线中每对相邻事件
都成为边——只有满足全部三个条件的才成为HB边。时间上重叠的并发操作不
产生DAG边；它们作为并发对单独捕获在签名中（§6.3.1）。

| 路径关系 | 判定条件 | 示例 |
|---------|---------|------|
| **SAME_PATH** | `A.path == B.path` | `mkdir /A` → `rmdir /A` |
| **SAME_INODE** | `A.ino == B.ino` 且 A成功；若双方都有offset/length则需重叠，否则同ino即成立 | `write /f(0→128)` → `read /f(64→192)`、`setattr /f` → `read /f` |
| **BARRIER** | A或B是 `fsync`/`fdatasync`。注：`writepage_cb`延长write的有效etime，之后正常的SAME_INODE处理交互——BARRIER本身不产生独立关系对 | `write /f` → `fsync /f` |
| **PARENT_CHILD** | 一个路径是另一个的前缀 且 前缀路径是目录 | `rmdir /A` → `mkdir /A/B`（直接），`mkdir /A` → `creat /A/B/C/file.txt`（间接） |
| **SAME_PARENT** | 路径共享同一个父目录 | `mkdir /A/X`, `mkdir /A/Y` |

**SAME_INODE对无offset/length操作的细化**：`WRITE`和`READ`在`Args[1]`/`Args[2]`
中携带`offset`/`length`，可以精确进行重叠检测。像`SETATTR`（truncate、chmod）
和`WRITEPAGE_CB`这样的操作则没有。当任一操作缺少offset/length时，任何
同ino关系都被视为因果——修改者改变了文件，所有后续同ino操作都受影响。

**PARENT_CHILD覆盖任意祖先/后代关系**，不限于直接父子。如果创建了 `/A` 然后
创建了 `/A/B/C/file.txt`，即使 `/A/B` 没有被显式操作，也存在因果边。DAG的
传递闭包无法通过从未发生过的中间操作来捕获这种关系。前缀检查覆盖所有层级。
只声明直接父子会留下盲区。

因为文件系统中只有目录才能有后代，前缀路径必须是目录节点类型。两个常规文件
共享一个路径前缀（如 `/A/f1` 和 `/A/f2`）不产生PARENT_CHILD——它们归入
SAME_PARENT或无关系。

**Rename双路径边规则**：`RENAME`顶点携带两个路径——`old_path`和`new_path`，
直接从调用参数获取（§6.2.1）。在happens-before边判定中，rename顶点的角色是方向性的：

| 比较方向 | 使用的路径 | 理由 |
|---|---|---|
| 其他操作 → RENAME | `old_path` | 之前操作影响的是位于旧位置的文件 |
| RENAME → 其他操作 | `new_path` | rename已将文件移动到了新位置 |

对于并发对，两个路径分别与对方操作的路径进行比较。可以为`old_path`、`new_path`、
两者皆可、或两者皆不可记录并发对。

**SAME_PARENT不产生HB边**——sibling文件之间没有直接因果依赖。
它会在签名中作为并发对出现（参见§6.4）。

**并发操作**（时间间隔重叠）→ 无论路径关系如何，都不产生HB边。
它们在签名中被捕获为并发对。

### 6.3.1 修改者 vs 观察者分类

并非所有VFS操作在因果影响力上都是对等的。操作可以分为两类：

**修改者**——成功后改变文件系统状态，成功时可以对后续操作施加因果影响。
返回码表示失败（如`-EEXIST`、`-ENOSPC`、`-EIO`）的修改者不产生输出HB边：
`WRITE`、`MKDIR`、`CREAT`、`RMDIR`、`UNLINK`、`RENAME`、
`SETATTR`、`FSYNC`。（注：`WRITEPAGE_CB`不是独立的修改者——它延长write操
作的有效完成时间；延长的write区间随后通过正常的SAME_INODE与其他操作交互。）

**观察者**——读取文件系统状态但不改变它。不能对后续操作施加因果影响：
`READ`、`LOOKUP`、`ITERATE`、`GETATTR`、`OPEN`、`RELEASE`。

**HB边规则（精炼版）**：

A→B存在当且仅当：
1. A和B之间存在路径关系。
2. **A是成功的修改者。**（A.ret_bucket ∈ {SUCCESS, WRITEPAGE_DONE}）
3. A.etime < B.stime（间隔不重叠）。

| A | B | HB边？ | 理由 |
|---|---|---|---|:--:|------|
| 修改者（成功） | 修改者 | ✅ | A的成功修改影响B的执行 |
| 修改者（成功） | 观察者 | ✅ | A的成功修改影响B观测到的内容 |
| 修改者（失败） | 任意 | ❌ | 失败的修改者未改变任何东西；无因果影响 |
| 观察者 | 任意 | ❌ | 观察者未改变任何东西；无因果影响 |

示例：`WRITE /f` → `READ /f`产生HB边，因为写入改变了读取将观测到的文件内容。
但`LOOKUP /f` → `READ /f`不产生HB边——lookup的结果可能被缓存，但它不会改变
read观测到的数据。

**并发对规则（精炼版）**：

并发对在签名中记录：顶点对共享路径关系 **且** 执行区间重叠 **且** 至少其中
一个是修改者。两个观察者在相关路径上并发（例如两个并发的`LOOKUP /f`事件）
不产生签名条目——它们的并发没有行为意义。

### 6.4 抽象签名层

反馈签名从两个独立来源推导，都根植于TSC全局时间线（§6.1）：

- **HB对**：从HB DAG中提取——通过*直接*因果边连接的顶点对。
  DAG只包含直接边（§6.3）；传递关系由schedule hash隐式编码。
- **并发对**：直接从TSC时间线计算——执行区间重叠、满足修改者/观察者规则
  （§6.3.1）、且共享路径关系的顶点对。它们不要求或使用DAG。

两者通过相同的管道进行抽象：将每个顶点抽象为其5特征元组，确定关系类型，
哈希，插入`maxDagSignal`（专用DAG通道；见§5.3）。

对于每对共享路径关系且满足修改者/观察者规则的顶点 (A, B)（HB边需A成功）：

1. **确定 `temporal_rel`**：
   - HB DAG中A→B可到达 → `HB`
   - HB DAG中B→A可到达 → `HB`
   - 两者皆不可到达 且 区间重叠 → `CONCURRENT`
   - 两者皆不可到达 且 不重叠 → 该对不产生签名条目（A发生于B之前但无因果依赖）

2. **确定 `path_rel`**：`{SAME_PATH, SAME_INODE, PARENT_CHILD, SAME_PARENT}`之一。

3. **将每个顶点抽象为6特征元组**：

   | 特征 | 取值 | 理由 |
   |------|------|------|
   | `func_id` | 16种类型 | 操作类型 |
   | `ret_bucket` | SUCCESS/EEXIST/ENOENT/FAILURE/WRITEPAGE_DONE/WRITEPAGE_ERR | 不同结果 = 不同代码路径 |
   | `depth_bucket` | 0 / 1 / 2-4 / 5+ | 影响lookup迭代次数和comrade查找深度 |
   | `node_type` | FILE / DIR | 导向 `file_ops` vs `dir_ops` VFS函数表 |
   | `is_persist` | true / false | 导向stash/restore vs 普通writeback |
   | `offset_bucket` | NA / 0 / MID / TAIL / BEYOND | 区分**同位置并发写**（真实数据竞争）与不同位置并发写；TAIL = 最后不满页（size % 4096），BEYOND = pos ≥ size（稀疏写/扩展）——对应HMDFS写回行为（file_remote.c `hmdfs_get_writecount`） |

   **去掉的特征及原因**：
   - `is_initial`（是否为初始文件）——已存在的远程文件和新创建的文件都走
     HMDFS远程RPC路径；这个区分已被`func_id`捕获（OPEN vs CREAT）。
    - `TMP_DIR`——Monarch测试框架的人为分类，不是HMDFS的行为差异。
    - `length`——写长度影响写回合并，但其主要行为影响（跨文件大小/页边界）
      已被`offset_bucket`捕获（kretprobe读到的是结束位置 ki_pos ≈ pos+len，
      跨边界的写落入BEYOND桶）。单独编码length会产生噪声而无额外信号。
      （注：原始设计也拒绝了`offset`——认为是参数级噪声无额外信号。现被重新审视：
      `offset_bucket`不是参数级区分，而是**并发语义**区分——分离真实数据竞争
      （同位置）与独立区写——这正是数据竞争反馈缺失的维度。见
      `DAG_KNOWN_ISSUES.md` #22。）
    - `初始文件 vs 新创建文件`——已存在的远程文件也走HMDFS RPC，与新创建文件的路径一致。

   **检索值映射**：
   - `ret_bucket`：ret ≥ 0 → SUCCESS；ret == -EEXIST → EEXIST；ret == -ENOENT → ENOENT；FUNC_WRITEPAGE_CB且ret == 0 → WRITEPAGE_DONE；FUNC_WRITEPAGE_CB且ret != 0 → WRITEPAGE_ERR；其他负值 → FAILURE
   - `depth_bucket` = `path`中'/'的数量直接分桶（`merge_view/`前缀计入）。0（无'/'）实际不可达——所有路径都带`merge_view/`前缀；1表示`merge_view/file`根级文件；2-4为浅层嵌套（如`merge_view/cid/file`）；5+为深层嵌套（'/' 数 ≥5，如`merge_view/a/b/c/d/e`）
   - `node_type`：`StatMd.Mode & S_IFDIR != 0` → DIR，否则 → FILE
   - `is_persist`：`hmcfg.Persistence_dir`前缀匹配 → true
   - `offset_bucket`：非读写/truncate函数（无偏移语义）→ NA；pos == 0 → 0；pos ≥ size → BEYOND；size % 4096 > 0 且 pos ≥ size−(size % 4096) → TAIL；否则 → MID。`off` 来源：write/read 的 `kiocb->ki_pos`、truncate 的 `iattr->ia_size`（仅当 `ia_valid & ATTR_SIZE`）；`size` 取 post-exec fsMd（分桶参照，**本身不入特征**）

4. **计算签名哈希**：

   ```
   sig_hash = hash(
       func_id_A, ret_bucket_A, depth_bucket_A, node_type_A, is_persist_A, offset_bucket_A,
       func_id_B, ret_bucket_B, depth_bucket_B, node_type_B, is_persist_B, offset_bucket_B,
       temporal_rel, path_rel
   )
   ```

5. **插入`maxDagSignal`**：哈希从uint64截断为uint32，合并到专用的DAG信号集合
   （`maxDagSignal`）中，该集合**独立于**代码覆盖率的`maxSignal`（§5.3）。
   fuzzer从不混合两条通道：DAG新意驱动自己的corpus入队（`dag corpus`统计），
   并通过`dag pair signal`/`dag schedule signal`统计上报，覆盖率统计保持纯净。

6. **Schedule级哈希（双粒度）**：

    ```
    all_pair_hashes = sorted({hash(pair) for all pairs})
    schedule_hash = hash(all_pair_hashes)  → uint64 → uint32 → maxDagSchedSignal
    ```

    per-pair哈希（第5步）奖励新*pair类型*的发现——`(MKDIR, RMDIR, CONCURRENT)`
    pair首次出现时，贡献一个新的信号bit。然而，一旦所有pairwise类型都被
    发现，一个在同一次执行中组合了三种已见过的pair类型的schedule将产生
    零新bits。schedule级哈希弥补了这一缺口：它编码了所有pair在*一起出现*
    的组合，奖励那些全局结构新颖的schedule，即使其每个构成pair类型都已被
    分别见过。

    schedule哈希在同一schedule的重复执行中保持稳定：per-pair哈希已经过
    抽象（顶点特征已经折叠了具体路径和偏移），因此有序集合对给定的因果
    模式是确定性的。不会引入额外噪声。

    **对corpus的驱动作用**：只有per-pair bits驱动corpus入队（每个新pair为
    该执行赢得一次triage名额）；schedule bit仅作统计。schedule新颖性意味着
    新的pair组合但不一定意味着新pair（见§6.5/`DAG_KNOWN_ISSUES.md`附录），
    且组合空间是指数级的——用它驱动corpus会让几乎所有执行都判为新颖。

**为什么两个都需要（而非只用schedule hash）**：

如果只使用schedule hash，每个产生新pair组合的schedule都恰好获得一个
新信号bit——无论它发现了三个全新的pair类型还是只是重新组合了五十个已知
的pair。Per-pair bits量化了"发现了多少新东西"，用于corpus入队与统计；
schedule hash以单个统计bit奖励"这个组合是新的"。

**并发对为什么重要**：`(MKDIR, RMDIR, HB, SAME_PATH)`和
`(MKDIR, RMDIR, CONCURRENT, SAME_PATH)`在语义上不同——前者是顺序的"先创建后删除"，
后者是并发的冲突。两者必须产生可区分的签名位。

**集合语义的自动去重**：如果3个WRITE顶点都与1个READ顶点形成相同的关系对，
每个产生相同的哈希→有序集合将其合并为一个条目。签名捕获的是*是否*出现了
某个模式，而不是*出现了多少次*。

### 6.5 与突变器的关系

突变设计（DCT权重、经文件树选择路径、分组突变、时间对齐的并发插入——§6.5.2）
能够产生多样化的因果模式。Op DAG反馈现已**接入突变循环**：新DAG对反馈回
DCT表（每个新对映射为其`(rootCall, variant)`组合并标记为有产出）。
triage成功仅入 corpus、不做任何静态归因——DCT 学习完全由动态 pair 反馈驱动
（见 `DAG_KNOWN_ISSUES.md` #20）。反馈回路如下：

```
chooseVariant 选中 (root, variant) → NoYield++；未产出者优先；≥阈值降权
ChooseTemporal 选择组合的插入形态（并发/因果）→ 执行
→ DAG 新对 → MarkExplored + MarkYield + 权重 +1
→ pair 时态（CONCURRENT/HB）→ UpdateTemporalWeight（第二层，按形态）
```

原始设计识别的三个增强方向中，两个已实现、一个推迟：

| 方向 | 状态 | 实现 |
|------|:--:|------|
| 路径关系探索度追踪 | **已实现** | DCT 为每个 `(rootCall, variant)` 维护 `Explored` 表；`chooseVariant` 只从从未产出信号且仍在探索预算内（`NoYield < 20`）的组合中选取 |
| 自适应权重下调 | **已实现** | DCT 为每个组合维护 `NoYield` 计数：选中时 +1，产出时（`MarkYield`，来自DAG新对）清零，连续20次无产出则权重 −5（下限1） |
| 时态形态层 | **已实现** | 组合下的第二层：`TemporalWeights`（并发 vs 因果/HB 形态，默认 50/50）。`insertCallFromDCT` 按 `ChooseTemporal` 选形态；因果形态插入到 `firstBoundaryAfter`（root 完成后的最早边界，倾向 HB 对）。`feedbackDagPairs` 按实际产出的 pair 时态更新形态权重；**所有新颖 pair（并发或 HB）统一驱动方向 1/2**（`MarkYield`：组合按综合产出奖励，无论形态——见 `DAG_KNOWN_ISSUES.md` #16） |
| 指令性突变 | 推迟 | "我需要在深度3+的DIR上产生WRITE→READ的SAME_INODE HB对，但尚未见过" → 专门构造。需要合成器；评估前两个方向后再决定。更广泛的建模调研（并发/因果关系结构）见 `DAG_KNOWN_ISSUES.md` #17 |

两个已实现方向共用同一个信号源：`feedbackDagPairs`（fuzzer侧将新DAG对映射为
DCT组合）。

**方向1 — 路径关系探索度追踪（已实现）**

- 机制：DCT 表为每个 `(rootCall, variant)` 维护 `Explored` 标志；
  `chooseVariant` 收集"从未产出信号且仍在探索预算内（`NoYield < 20`）"的
  组合，当该候选池非空时**只从池内选择**（独占式偏向——见
  `DAG_KNOWN_ISSUES.md` #7）。
- 信号源：`MarkYield`（新DAG对经`feedbackDagPairs`）置 `Explored = true`。
- 参数：探索预算 20（与方向2共用 `NoYield` 计数）。
- 实现：`distributed_choice.go`（`Explored`、`chooseVariant`）、`proc.go`
  （`feedbackDagPairs`）、`dag.go`（`DagPairToVariant` 映射）。
- 粒度：DCT 组合（`callName` + `pathRel`），粗于 DAG 特征桶——偏向作用在
  生成/突变的组合选择层，而非 pair 空间本身。

**方向2 — 自适应权重下调（已实现）**

- 机制：每个组合维护 `NoYield` 计数（每次选中 +1）；达到 20 次连续无产出
  时权重 −5（下限 1）、重算 `TotalWeights`、并重置计数（防止连续降权）。
- 信号源：`MarkYield` 重置 `NoYield = 0`。
- 参数：`noYieldThreshold = 20`、`noYieldDelta = 5`。
- per-proc 语义：DCT 表每 proc 一份，计数与降权各自独立演化。
- 实现：`distributed_choice.go`（`noYieldTick`、`MarkYield`）。

**方向3 — 指令性突变（推迟）**

- 概念：从"先行动后观察"（探索式：随机修改 → 执行 → 看产生什么信号）
  转为"先定目标再构造"（指令式：查询缺失模式 → 反向构造能产生它的程序）。
- 例子：`(WRITE→READ, HB, SAME_INODE, depth≥3)` 从未出现 → 选深度 3+ 路径
  + `open/write/read` fd 链 + 顺序时序（保证 HB 而非 CONCURRENT）→ 执行验证。
- 三个前置依赖：① 缺失模式追踪（从已知 `maxDagSignal` 空间反推未见组合——
  方向1的延伸）；② 从抽象特征到具体程序的合成器（程序合成问题；现成积木：
  `generateCallByName`、fd 链构造、barrier/nanosleep 时序控制）；③ 验证闭环
  （执行后检查目标 pair 是否触发，未命中则调整）。
- 弱化版已就位：§6.5.3 调用移动重排已有调用逼近目标；完整版是凭空构造。
- 优先级：推迟——依赖三项前置条件，且应先用方向1/2的实验数据确认
  "缺失模式是否真的是探索瓶颈"。

### 6.5.1 PathCross：扩展DCT以支持跨节点路径引用

当前DCT使用星型拓扑——所有并发节点统一参照节点0的根调用路径解析各自路径。
在DAG中，每个跨节点pair都是`(node0, nodeN)`。节点间（如`(node1, node2)`）
的横向交互从不被直接生成，限制了PARENT_CHILD和SAME_PARENT模式的多样性。

**PathCross**（`PathRelation=7`）允许一个非节点0的调用路径参照**另一个对等节点**
已解析的路径来表示。

**生成保证**：节点按顺序生成（0 → 1 → 2 → …）。如果节点N选中PathCross，
其对等节点从{0..N-1}中随机选取——这些节点已有已解析路径。这确保了任意
节点数的有效引用。

**设计**：选项A——将`IsCross`标志直接嵌入`CallVariant`，在DCT表中为每个
`(rootCall, variantCall, PathRelation, IsCross)`组合分配独立权重。这使得
跨节点变体与同节点变体可以独立调优（例如PathCross+PathChild权重为8，
同节点PathChild权重为15）。

| 变体字段 | 取值 | 含义 |
|:---|:--:|------|
| `PathRelation` | 任意 | 用于解析路径的关系类型（Child、Same、Sibling等） |
| `IsCross` | false | 路径相对于节点0的根调用（现有行为） |
| `IsCross` | true | 路径相对于对等节点已解析的路径（新行为） |

**连锁示例（5个节点）**：

```
node0: mkdir /A
node1: IsCross=false, PathChild → mkdir /A/B
node2: IsCross=false, PathSibling → creat /A/X
node3: IsCross=true, 对等=node1, PathChild → mkdir /A/B/C
node4: IsCross=true, 对等=node2, PathSame → write /A/X

DAG pairs:
  (node1, node0, PARENT_CHILD, HB)
  (node2, node0, SAME_PARENT, CONCURRENT)
  (node3, node1, PARENT_CHILD, HB)   ← 跨节点pair
  (node4, node2, SAME_INODE, CONCURRENT)  ← 跨节点pair
```

前两对在星型拓扑中已存在。后两对——直接连接对等节点——需要PathCross。

**实施清单**：

| 文件 | 改动 |
|------|------|
| `distributed_choice.go`（枚举） | 在`PathRelation`中加入`PathCross = 7` |
| `distributed_choice.go`（变体） | 在`CallVariant`中加入`IsCross bool` |
| `distributed_choice.go`（表） | 在`initDefaultConfig`中加入`IsCross`变体循环；设置初始权重 |
| `distributed_choice.go`（信息） | 在`DistributedChoiceInfo`中加入`ResolvedPaths map[int]string` |
| `rand.go`（生成） | 当`IsCross`为true时，从{0..N-1}中选取对等节点并使用其已解析路径 |
| `mutation.go`（MutateGroupPath） | 重新解析组路径时处理`IsCross` |
| `mutation.go`（insertCallFromDCT） | 同上 |

**状态：未实现（推迟）。** 先评估§6.5已集成的DAG反馈回路及其饱和特性；
如果观测到的pair空间显示星型拓扑是瓶颈，PathCross是下一个候选。

### 6.5.2 基于执行时间对齐的并发插入

pattern/DCT突变路径插入的并发调用原本按**调用数组索引**对齐
（`min(insertPos, len(p.Calls))` 或参考插入位置）：每个prog在相同索引处插入。
由于各prog的调用数量与单调用耗时不同，相同索引并不意味着相同执行时刻——
"并发"调用实际上在不同时刻执行，很少真正重叠。

由于每个已执行调用都带有其最近一次执行窗口
（`Call.CheckInfo.Stime/Etime`，raw guest TSC），插入位置现在改为按**执行时间**对齐：

```
refTime = 0                                     若 insertPos == 0      （prog 开头对齐）
        = Etime(Calls[insertPos-1]) − tscoff[0]  若前驱调用有时间信息
        = −1                                     否则（无参考 → 回退索引对齐）

对每个其他 prog p：
    j* = argmin_j | boundaryTime(p, j) − refTime |   boundaryTime(p, j) = Etime(Calls[j-1]) − tscoff[p]
    在 j* 处插入并发调用                          （无时间信息时回退索引对齐）
```

细节：

- **时间来源**：**最近一次执行**的 `CheckInfo`（`Prog.Clone` 复制共享指针，
  每次执行刷新）。同一种子重复使用（如 smash 100 轮）保持单一稳定的参考布局；
  corpus 重执行（triage）会刷新它。
- **跨VM归一化**：窗口是各VM的 raw TSC；比较时减去各VM的 `tsc_offset` 进入
  全局域——与DAG全局时间线使用相同的归一化。
- **fd约束**：`findInsertPosition` 在选中的 open/close fd 范围内对齐；
  `insertCallFromDCT` 在时间对齐会把需要 fd 的调用放到其 open 之前时，
  回退到索引对齐位置。
- **覆盖范围**：`insertCallFromPattern`（ops 与 verification 调用）和
  `insertCallFromDCT`（并发调用）都对齐；生成路径
  （`generateFromDistributedChoiceTable`）不动——新生成的程序没有执行历史可对齐。

### 6.5.3 基于时间对齐的调用移动

**状态：未实现（规划中）。** 以下设计是 `MoveCallTimeAligned` 的实现规格；
另见 `DAG_KNOWN_ISSUES.md` #11。

§6.5.2 通过*插入*新生成的调用来构造并发；§6.5.3 改为*移动已有调用*。移动
有两个优势：调用数量不变（无需验证新调用），且被移动的调用已经成功执行过，
只有在移动破坏了其依赖时才会失败。

**移动能产生什么**：pair 哈希对 A/B 顺序和 temporal 关系都敏感，因此重排
已有调用能产生新 bit：

- **并发对**：把 A 移入 B 的执行窗口——temporal 从 HB 翻转为 CONCURRENT。
- **反向 HB 对**：把 A 移到 B 之后——方向翻转（hash(A→B) ≠ hash(B→A)）。

两种模式都通过与 §6.5.2 相同的时间对齐机制定位（全局 TSC 域参考时间、
基于前驱边界时间的 `TimeAlignedInsertPos`）：

```
模式 = 并发 (60%)   refTime = Stime(B) − tscoff[目标]      → 插入 B 的窗口内
模式 = 反向 (40%)   refTime = Etime(B) − tscoff[目标] + δ  → 插入 B 之后
```

模式比例（60/40）是 `MoveCallTimeAligned` 中的可调常量。

**候选过滤（`callIsMovable`）**：调用可移动当且仅当——非 fd 调用
（`!IsFdRequiredCall`）、非 `open`/`close`、有路径参数、有时间信息
（`CheckInfo != nil`）、且无调用引用其结果（`Ret.uses` 为空）。`syz_failure`
系列伪调用无路径参数，自动被排除。

**依赖检查**：被移动调用的路径必须存在于 `lcs.FileTree`（merge_view 状态的
近似；`lcs == nil` 时跳过）。若路径在目标时刻已消失，调用失败并产生
FAILURE 桶 pair——价值低但无害。

**接入**：`mutateHmdfs` 先执行类型专用突变器（WithDCT/Stash/Dcache）；
失败后尝试 `MoveCallTimeAligned`，再失败回退标准 `Mutate`。

**与 §6.5.2 对照**：

| 维度 | 插入（§6.5.2） | 移动（§6.5.3） |
|---|---|---|
| 调用来源 | 新生成 | 已有（已验证） |
| 调用数量 | +1..n | 不变 |
| 目标 | 并发 | 并发 + 反向 HB |
| fd 处理 | fd 范围内对齐/回退 | 只移动非 fd 调用 |
| 失败代价 | 新调用无效 | 已有调用失效（低概率） |



### 6.5.4 动态组突变（惰性分组，无 GroupID）

**状态：已实现。** 组级突变不再使用生成时静态决定的 GroupID（后续突变会改变并发与因果结构，静态分组逐步过时；且全部反馈链本就动态计算）。分组在突变需要时**基于执行时间线惰性计算**，不持久化任何分组状态：

```
pickAnchor(ps, r, wantReadWrite)      从 ps[0] 随机选有路径+时间线的调用
  -> findGroupCalls(ps, anchor)        其它 prog 中执行窗口与锚重叠的调用
                                      （s1<e2 && s2<e1）∪ 直接因果后继
                                      （每 prog 中锚完成后开始的最早调用），
                                      经 tscoff 归一化到全局 TSC 域；
                                      同 prog 内串行不重叠
  -> pathRelBetween(anchorPath, cPath) 路径几何关系（Same/Child/Parent/
                                      Sibling/NoRel），现场计算
```

统一组 = 锚 + 并发者 + 直接因果后继（见 `DAG_KNOWN_ISSUES.md` #18）——四个突变对因果对与并发对一视同仁：数据共享产生"顺序 write→read 同 offset"（一致性验证）、路径迁移把因果链成员一起搬走、删除时因果成员随组处理。

**四个动态突变**（`MutateInodeOpsWithDCT`/`MutateFileopsWithDCT` 分发：removeGroup 20% / removeOneInGroup 10% / 路径迁移 10% / 数据突变 10% / 插入 50%）：

- `MutateGroupPathDynamic`：锚迁移到新 base 路径（`pickNewBasePath`）；并发者按现场关系相对解析；fd 调用经 `resolveFdTarget` 回溯；非并发主干调用留在原地（无需第二遍跟随逻辑）。
- `RemoveGroupDynamic`：删除锚及其全部并发者。
- `RemoveOneInGroupDynamic`：删除集合中 fd 安全的一个调用（`AnalyzeProgFds`，保留故障注入伪调用）。
- `MutateGroupDataDynamic`：锚必须是读写调用；集合内全部读写调用共享同一随机 offset（write 还共享 length，经 `updateWriteDataBuf`）——确定性对应被移除的插入侧概率 OffsetSame。范围取锚路径文件大小（无则 1MB 兜底）。

**路径迁移保持相对关系（P1 决策）**：路径突变器把已验证模式推广到其他路径形状，不重塑关系——生成新 rel 组合是 `insertCallFromDCT`/`ChooseVariant` 的职责。重随机 rel 会 (a) 与生成机制重叠，(b) 拆散并发同路径对（hmdfs 冲突核心场景），(c) 污染 DCT 权重学习（路径维度反馈被归因到 rel 维度）。

**移除内容**：`CallProps.GroupID/PathRel/IsFromDCT/OffsetRel/LengthRel`（5 个序列化 key）、`Prog.Groups/LastGroupID`、`GroupMeta`/`GroupSourceType`、`AllocGroupID/renumberGroups/GetGroupPositions/RemoveGroup/collectAll*/...`、~55 处 `SetGroupID` 调用、插入侧概率 offset/length 选择（`ChooseOffsetRel`/`ChooseLengthRel`/权重表）——共享偏移改由 `MutateGroupDataDynamic` 定向产生。序列化向后兼容（反序列化忽略未知 key）。见 `DAG_KNOWN_ISSUES.md` #13/#14/#15。

**回归守卫**：`prog/path_mutation_test.go`（`TestPathRelBetween`、`TestFindConcurrentCalls`、`TestPickAnchor`）覆盖关系分类、窗口重叠判定与锚选择。


## 7. 数据依赖关系

```
                     Monarch数据管道
                            │
         ┌──────────────────┼──────────────────┐
         │                  │                   │
    eBPF kretprobe      代码覆盖率          callReply
    (15个merge函数       (KCOV)            (Stime/Etime
     + writepage_cb)         │              + tsc_offset)
         │                  │                   │
         │                  ▼                   │
         │            边覆盖率                  │
         │            (现有)                    │
         │                  │                   │
         └──────────┬───────┘                   │
                    │                           │
                    ▼                           ▼
              操作DAG                     TSC时间线
          (基于ino的因果               (全局排序参考)
           规则 → DAG哈希)
                    │                           │
                    └──────────┬────────────────┘
                               │
                 maxDagSignal / maxDagSchedSignal
      (新DAG对 = corpus入队 + DCT反馈；
       schedule位 = 仅统计)
      — 独立通道，从不并入覆盖率 maxSignal
```

---

## 8. 参考文献

1. Lamport, L. "Time, Clocks, and the Ordering of Events in a Distributed
   System." *Communications of the ACM*, 1978.

2. Meng, R., Pîrlea, G., Roychoudhury, A., and Sergey, I. "Greybox Fuzzing of
   Distributed Systems." *ACM CCS*, 2023.

3. Fidge, C. "Timestamps in Message-Passing Systems That Preserve the Partial
   Ordering." *Australian Computer Science Conference*, 1988.

4. Mattern, F. "Virtual Time and Global States of Distributed Systems."
   *Parallel and Distributed Algorithms*, 1989.

5. Shapiro, M. et al. "A Comprehensive Study of Convergent and Commutative
   Replicated Data Types." *INRIA*, 2011.

6. Kleppmann, M. et al. "Moving Elements in List CRDTs." *PODC*, 2019.

7. Ernst, M. et al. "The Daikon System for Dynamic Detection of Likely
   Invariants." *Science of Computer Programming*, 2007.

8. Leesatapornwongsa, T. et al. "SAMC: Semantic-Aware Model Checking for Fast
   Discovery of Deep Bugs in Cloud Systems." *OSDI*, 2014.

9. Alvaro, P. et al. "Lineage-driven Fault Injection." *SIGMOD*, 2015 (Molly).

10. Kim, T. et al. "CSPEC: A Commutativity Specification Language for
    Distributed Consistency Checking." *SOSP*, 2017.

11. Sun, X. et al. "Sieve: A Middleware Approach to Scalable Distributed
    System Checking." *ASPLOS*, 2021.

12. Sherman, W. et al. "Sherlock: System-level Fault Localization for
    Distributed Systems." *OSDI*, 2020.

13. Sherman, W. et al. "CoCain: Concurrency-Aware Checking for Distributed
    Systems." *ASPLOS*, 2023.

14. Gulcan, E. B. et al. "Model-Guided Fuzzing of Distributed Systems."
    *ACM TOSEM*, 2024/2025 (arXiv:2410.02307).

15. Natella, R. "StateAFL: Greybox Fuzzing for Stateful Network Servers."
    *EMSE*, 2021 (arXiv:2110.06253).

16. Ba, J., Böhme, M., Mirzamomen, Z., Roychoudhury, A. "Stateful Greybox
    Fuzzing." *USENIX Security*, 2022.

17. Chen, Y. et al. "Themis: Finding Imbalance Failures in Distributed File
    Systems via a Load Variance Model." *SOSP*, 2025.
