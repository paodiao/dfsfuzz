# Distributed Filesystem State Modeling for Fuzzing Feedback

## 1. Background

Code coverage tells us *what code ran*, but not *what behaviour that code produced*.
For a distributed filesystem, two completely different schedules can exercise the
same code edges yet produce radically different filesystem states — one with a
consistency violation, one without.

Traditional AFL-style edge coverage has fundamental blind spots for distributed
systems:

- **Saturates quickly**: HMDFS is implemented as a reactive event loop — the same
  `write()` path runs for every write, regardless of which file, which offset, or
  which other node is concurrently accessing it.
- **No causal information**: Edge coverage treats every node's code execution in
  isolation. It cannot distinguish "node A wrote then node B read" from "node A
  and node B wrote concurrently".
- **No state awareness**: Coverage is indifferent to what the filesystem actually
  looks like after the test — whether a file was created, deleted, corrupted, or
  left with stale content.

State-aware feedback is the logical next step for Monarch's fuzzing pipeline.
This document surveys the landscape of state representation techniques and
recommends a path forward for HMDFS specifically.

---

## 2. HMDFS System Characteristics

HMDFS is a decentralized stackable overlay filesystem. Its architecture creates
specific challenges for state modeling:

| Feature | State Modeling Implication |
|---------|---------------------------|
| **merge_view** — all nodes see a unified namespace | The fuzzer's external view is `merge_view` — state modeling should align with this |
| **device_view/local & device_view/remote** — per-node cache copies | Operations on different views have different consistency guarantees |
| **Async writeback** — `write()` returns before data reaches the server | State snapshots at arbitrary times may capture "in-flight" writes |
| **Stash/restore** — offline nodes cache writes locally | Node crash + recovery creates state transitions invisible between snapshots |
| **Dentry cache with TTL** — directory listings may be stale | `readdir()` results depend on cache age, not just filesystem state |
| **Comrade lists** — merge_view files tracked across nodes | The same merge_view inode can have different lower inodes on different clients |
| **CRUD on hierarchical namespace** — mkdir/creat/unlink/rename/write/read | Operations on the same path can conflict; operations on different paths commute |

---

## 3. Survey of 9 Approaches

### 3.1 Lamport Timelines / Happens-Before Summaries (Baseline: Mallory)

**Principle**: Each node maintains a monotonically increasing Lamport clock.
Events carry timestamps; message passing propagates causality. The
*happens-before* relation defines a partial order over events. The feedback
signal is a summary of which causal paths were exercised.

**Mallory's implementation** (Meng et al., CCS 2023):
- Dynamically builds Lamport timelines from intercepted network packets,
  client requests/responses, and compile-time code instrumentation.
- Abstracts timelines into *happens-before summaries* — per-node sets of
  (event type A happened-before event type B) pairs.
- Uses MinHash to cluster similar abstractions into states.
- Q-learning selects fault types to maximize state exploration.

**Key insight**: "Not two different runs of a distributed system will produce
the same timeline. New observations will always produce new feedback, even
though many runs are equivalent for testing purposes." The *abstraction* step is
the critical innovation — compressing raw timelines into comparable summaries.

**HMDFS fit: 3/5**

| Strength | Weakness |
|----------|----------|
| Captures causal ordering across nodes | Knows nothing about file contents, directory structure, or consistency |
| Mallory's abstraction layer generalizes to any event type | Requires tracing ALL cross-node messages at kernel level |
| Good for reasoning about stash/writeback timing | HMDFS socket tracing adds non-trivial instrumentation |

**Data needed**: Per-node Lamport clock; trace of every HMDFS socket send/recv;
trace of every syscall (name, args, node, clock).

**References**: Mallory (Meng et al., CCS 2023); Lamport (1978) "Time, Clocks,
and the Ordering of Events in a Distributed System".

---

### 3.2 Vector Clocks + State Snapshots

**Principle**: Generalize Lamport clocks to N-element vectors — stronger than
Lamport (exactly distinguishes concurrent from causally-related). Take
filesystem snapshots at synchronization points, each timestamped with the
current vector clock.

**HMDFS fit: 4/5**

| Strength | Weakness |
|----------|----------|
| Precise concurrency detection (concurrent vs. causal) | O(N) overhead per message (N = node count) |
| State snapshots at clock-tagged points create richer feedback | Snapshot frequency trades precision vs. overhead |
| Good for stash/writeback timing analysis | Large filesystem walk per snapshot |

**References**: Fidge (1988); Mattern (1989); Amazon Dynamo's vector clock
conflict resolution.

---

### 3.3 Operation DAG

**Principle**: Model operations (mkdir, write, unlink, rename, etc.) as vertices
in a directed acyclic graph. Edges represent causal dependencies
(happens-before) or data dependencies (read reads from write). A test produces
a DAG. Novelty = new DAG topology.

**HMDFS fit: 5/5**

| Strength | Weakness |
|----------|----------|
| Operations ARE filesystem operations — perfect semantic match | Graph isomorphism is expensive (approximations needed) |
| DAG captures full causal structure of a test | Requires tracing causal dependencies between operations |
| Perfect for merge_view interleaving analysis | High implementation complexity |

**Data needed**: Full operation sequence per node; causal context for each
operation; data dependency edges (which write a read observes).

**References**: SAMC (Semantic-Aware Model Checking); CoCain (ASPLOS 2023); WiFe
(OSDI 2020).

---

### 3.4 CRDT-Based Models

**Principle**: Model the filesystem as Conflict-free Replicated Data Types.
Filesystem tree = grow-only set of (path, metadata) pairs + last-writer-wins
register for content. Operations produce local state updates; merges propagate
to replicas.

**HMDFS fit: 2/5**

| Strength | Weakness |
|----------|----------|
| CRDTs are designed for concurrent updates with guaranteed convergence | POSIX semantics (hard links, rename atomicity, permissions) are hard to capture as CRDTs |
| Good for detecting consistency bugs | Writeback/stash mechanisms are more complex than simple CRDT merge |
| Clean algebraic properties | Requires formal CRDT model for every filesystem operation |

**References**: Shapiro et al. (2011); Kleppmann et al. (2019); AntidoteDB;
CISE (Najafzadeh et al., 2020).

---

### 3.5 Filesystem Tree Signature

**Principle**: Hash the observable filesystem state directly. State = complete
file tree with attributes: `path → (inode, type, size, mtime, uid, gid, mode,
content_hash, xattrs)`. Tree serialized in canonical order (sorted by path) and
hashed. Novelty = new hash.

**HMDFS fit: 4/5**

| Strength | Weakness |
|----------|----------|
| Directly captures what the fuzzer cares about | Only sees final state, not intermediate states or ordering |
| Simple to implement — Monarch's `write_metadata` already collects this data | Content hashing + tree walk is expensive for large filesystems |
| Natural for cross-node consistency checking | No concurrency semantics |
| Monarch's `MdCmp` and `ConcFSCheck` already consume this data | |

**Data needed**: Already collected by `write_metadata()` → `fsMd[path].StatMd` +
`Checksum`.

**References**: Monarch's own SymSC checker; Tripwire/AIDE (filesystem integrity
checking); git's Merkle-tree content addressing.

---

### 3.6 Predicate Mining from Traces

**Principle**: From event traces, extract logical predicates characterizing the
test. Examples:
- `WRITE_BEFORE_READ(file, node_A, node_B)` — node A's write happened-before node B's read
- `MKDIR_DURING_NODE_OFFLINE(dir, node)` — directory creation while a node was unreachable
- `WRITEPAGE_ACROSS_NODES(file)` — write on one node, writeback on another
- `CONCURRENT_UNLINK_AND_OPEN(file)` — unlink and open overlapped in time
- `RENAME_DURING_READDIR(dir)` — rename during a directory listing

Predicates form a boolean *feature vector*. Novelty = satisfaction of a new
predicate combination.

**HMDFS fit: 5/5**

| Strength | Weakness |
|----------|----------|
| Captures exactly the semantic scenarios of interest | Predicate design is domain-specific (requires HMDFS expertise) |
| Zero new data collection — uses existing eBPF trace events | Predicate space must be manually curated or auto-mined |
| Perfect for HMDFS-specific patterns (stash/writeback, dentry TTL, comrade conflicts) | Completeness depends on predicate coverage |

**Data needed**: Already collected: eBPF kretprobe events (15 merge functions +
`WRITEPAGE_CB`), `fsMd` per-node, `infos` (per-call errno/ret values).

**References**: Daikon (dynamic invariant detection); TxF (EuroSys 2013); Liblit
et al. (2005).

---

### 3.7 Graph-Based State Models

**Principle**: Model the entire DFS execution as a heterogeneous graph. Nodes:
physical nodes, files, directories, inodes, stash entries, cache entries.
Edges: `created_by`, `read_by`, `cached_in`, `stashed_to`, `synced_to`,
`belongs_to_node`. Novelty = new graph structure (via Weisfeiler-Lehman graph
kernels or graph2vec embeddings).

**HMDFS fit: 4/5**

| Strength | Weakness |
|----------|----------|
| Richest representation — captures all entities and relationships | Static graph loses temporal ordering unless edges are timestamped |
| Perfect for comrade list, stash graph, cache dependency visualization | Graph isomorphism is NP-hard; approximations needed |
| | High implementation complexity |

**References**: Weisfeiler-Lehman graph kernels; graph2vec, GraphSAGE; Sherlock
(OSDI 2020).

---

### 3.8 Commutativity & Equivalence Classes

**Principle**: Define an equivalence relation on operation sequences. Two
sequences are equivalent if they produce the same observable filesystem state.
Group operations based on commutativity:
- Operations on different files commute
- Operations on the same file with non-overlapping offsets commute
- Reads commute with reads; mkdir(A) and mkdir(B) commute
- mkdir(A) and mkdir(A) do NOT commute (one fails)

Feedback = hash of the equivalence class (normalized sequence). This filters
out scheduling noise — equivalent interleavings map to the same signal.

**HMDFS fit: 4/5**

| Strength | Weakness |
|----------|----------|
| Excellent noise reduction — equivalent interleavings don't bloat the corpus | Defining a complete commutativity model for POSIX operations is non-trivial |
| Clean semantic model | Requires domain-specific formal specification |
| Good for merge_view concurrent operations | |

**References**: Molly (OSDI 2015); CSPEC (SOSP 2017); Sieve (ASPLOS 2021).

---

### 3.9 Hybrid Combinations

**Principle**: Combine complementary approaches. The best combination specific to
HMDFS:

**(Recommended) Predicate Mining + Filesystem Tree Hash**

- **Predicate Mining**: Extracts semantic patterns from eBPF event traces. Captures
  *how* the state was produced (causal patterns, concurrency, fault interactions).
- **Filesystem Tree Hash**: Captures *what* the final state is. Provides a
  "ground truth" independent of how it was reached.
- Both use data already collected by Monarch — **zero new instrumentation required**.

Novelty feedback = (new predicate satisfied) OR (new file tree hash).

---

### 3.10 Differentiating Operation Outcomes Within Each Approach

Both predicate mining and operation DAG can distinguish the same operation with
different outcomes (return values, arguments, target files). The key difference
is *how* — manual vs. automatic — and the trade-offs that follow.

#### 3.10.1 Why ret/args Matter

Same operation, different outcome means different behaviour:

| Operation | Outcomes | Why They Differ |
|-----------|----------|-----------------|
| `write(fd, 128)` vs `write(fd, 0)` | 128 bytes written vs. disk full | Different system states |
| `mkdir(/A)` vs `mkdir(/A)` again | ret=0 vs. ret=-EEXIST | Second call sees different namespace |
| `open(f, O_RDWR)` vs `open(f, O_CREAT\|O_RDWR)` | args[1] flags differ | O_CREAT may create, plain open fails |
| `rename(A, B)` same dir vs cross dir | args[0] and args[2] differ | Cross-dir rename has different atomicity guarantees |
| `lookup(dir)` succeeds vs fails (ENOENT) | dentry cache hit vs. miss | Different internal code paths |

Our `HmdfsTraceEvent` already captures `{FuncID, Ret, Ino, Args[6]}` from
eBPF kretprobes — differentiating by outcome requires **zero new data collection**.

#### 3.10.2 Predicate Mining: Manual Granularity

Predicate templates are extended with outcome conditions:

```
Before (coarse):
    MKDIR(path)
    WRITE(file)
    OPEN(path)

After (outcome-aware):
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

**Every field of `HmdfsTraceEvent` can become a predicate parameter**:

| Field | Parameterisable As |
|-------|-------------------|
| `FuncID` | operation type (predicate template selector) |
| `Ret` | success/failure/errno bucket |
| `Ino` | "same file" identity (e.g., `WRITE_THEN_READ_SAME_FILE`) |
| `Args[0..5]` | flags, offsets, lengths, parent inode pointers |

**Advantage**: You choose which distinctions matter. Feedback granularity is
exactly what you define — no more, no less.

**Risk**: What you don't define is a blind spot. A predicate template not
written is a behaviour dimension not tracked.

#### 3.10.3 Operation DAG: Automatic Encoding

Every operation becomes a DAG vertex. The vertex identity is computed from
all available fields:

```
vertex_hash = hash(op_type, path, ret, args, ino)
```

Different outcomes produce different vertices **automatically**. The DAG
signature naturally changes whenever any event differs — no manual template
design required.

**Advantage**: Zero blind spots. Every variation is captured.

**Risk**: Signal explosion. If `write(file, offset=0, len=1)` and
`write(file, offset=1, len=1)` produce different vertices, the fuzzer treats
them as "novel" even though they may be semantically equivalent. Suppressing
this noise requires an additional *abstraction layer* (vertex equivalence
classes, graph isomorphism approximations, or dimensionality reduction).

Mallory solves this noise problem by applying MinHash clustering on
happens-before summaries — an abstraction layer that explicitly groups
similar but non-identical timelines into the same state.

#### 3.10.4 Summary

| | Predicate Mining | Operation DAG |
|---|:--:|:--:|
| Outcome differentiation | Manual (template + condition) | Automatic (hash includes everything) |
| Granularity control | Strong (you define) | Weak (needs abstraction layer) |
| Blind spot risk | Exists (unwritten predicates) | None (automatic) |
| Noise risk | Low (only defined predicates fire) | High (every arg variation matters) |
| Implementation from current data | Ready — predicates consume `HmdfsTraceEvent` directly | Ready — DAG vertices consume same struct |

Which approach fits better depends on the fuzzing stage: early exploration
favors automatic DAG encoding (catch unexpected behaviour), while targeted
bug hunting favors manual predicates (focus on known-dangerous patterns).

---

### 3.11 Deterministic Simulation / Model-Based Testing

**Principle**: The system under test runs inside a simulator where all
non-determinism (message ordering, clock skew, fault timing) is explicitly
controlled. Every execution is fully reproducible. A single-machine simulator
exercises different interleavings by reordering events systematically.
FoundationDB's simulation framework is the canonical example — their entire
testing pipeline runs in a deterministic simulator on a single machine.

**State representation**: The system's *internal* logical state, fully visible
within the simulator (not just externally observable state).

**Novel state detection**: Compare the logical state after each interleaving.
Reproducibility ensures that any bug can be replayed deterministically.

**HMDFS fit: 1/5**

| Strength | Weakness |
|----------|----------|
| Perfect reproducibility — any bug found is trivially replays | HMDFS is a kernel module — cannot run in a user-space simulator |
| Full internal state visibility | Requires porting the filesystem to a simulation framework |
| FoundationDB-level thoroughness | Fundamentally incompatible with KVM/QEMU-based fuzzing architecture |

**Data needed**: Not applicable — requires HMDFS kernel code to run inside
a user-space deterministic simulator.

**References**: FoundationDB simulation framework; Turmoil (Tokio); P language
(Microsoft Research).

---

### 3.12 Invariant Mining from Traces (Daikon-Style)

**Principle**: Automatically infer program invariants from execution traces.
An invariant is a logical predicate that holds over all observed runs of a
program point — e.g., `fd > 0` after a successful `open()`, or
`file.size >= write.offset + write.len` after a `write()`. Daikon-like tools
instrument program variables at function entry/exit points and mine statistical
invariants. Violation of a previously-inferred invariant signals novel
behaviour.

**State representation**: A set of logical predicates over program variables
and their relationships. Different from predicate mining (Approach 6) — here
the predicates are *inferred* from data, not *designed* by the user.

**Novel state detection**: New behaviour = violation of a previously-held
invariant, or discovery of a new invariant not seen before.

**HMDFS fit: 3/5**

| Strength | Weakness |
|----------|----------|
| Zero manual predicate design — automated | HMDFS is kernel code — instrumentation must be compile-time (similar to our tracepoints) |
| Works well with rich variable-level traces | Current eBPF trace captures return values, not internal program variables |
| Complements predicate mining (Approach 6) | Requires per-variable instrumentation of HMDFS internal code paths |

**Data needed**: Instrumentation of HMDFS internal functions to capture local
variable values at function entry/exit (beyond what current eBPF kretprobes
capture). Current `HmdfsTraceEvent.Args[6]` could serve as a starting point
for pointer-level invariants.

**References**: Daikon (Ernst et al., 2007); DIDUCE (Hangal & Lam, 2002);
TerrAscope (Joshi et al., 2019).

---

### 3.13 Span-Based Tracing (Distributed Tracing, OpenTelemetry-Style)

**Principle**: Each operation generates a *span* (identified by trace_id,
span_id, parent_span_id). Spans propagate across nodes — a `write()` on
client A creates a parent span, and the corresponding `writepage_cb` on
client B creates a child span linked by parent_span_id. The result is a
hierarchical DAG of spans. Jaeger, Zipkin, and Dapper pioneered this model.

**State representation**: A span DAG where edges represent parent-child
relationships (hierarchical), distinct from the flat happens-before edges in
Lamport timelines.

**Key difference from Lamport timelines**: Lamport edges are flat temporal
arrows. Span edges are *nested* — a `readdir` span containing `lookup` +
`getattr` sub-spans is semantically richer than three side-by-side events on a
Lamport timeline. The hierarchy captures "what caused what" at the protocol
level.

**Novel state detection**: New span DAG topology — new parent-child
relationships or new nesting patterns.

**HMDFS fit: 4/5**

| Strength | Weakness |
|----------|----------|
| Hierarchical structure captures operational causality | Requires parent-child link propagation between spans |
| Natural mapping to VFS operations (open → read → close) | HMDFS tracepoints need cross-referencing to link spans |
| Our eBPF events can be aggregated into spans by ino + time window | |

**Data needed**: Existing eBPF trace events (already collected). Aggregation
logic: group events by `ino` within a short time window (e.g., 1ms) into a
single span. Cross-node span linking via matching `remote_ino` and
`WritePageCB` events.

**References**: Google Dapper (2010); OpenTelemetry; Jaeger; Zipkin.

---

### 3.14 Property-Based Testing for Stateful Systems (QuickCheck-Style)

**Principle**: Define a formal state machine model of the system: a state type
(e.g., `Map Path FileState`), and operation pre/post-conditions (`mkdir`:
"parent exists, child doesn't exist" → "child exists with empty dir state").
The testing framework randomly generates operation sequences, executes them on
the real system, and checks that the observed state matches the model's
prediction. Erlang QuickCheck and Rust `proptest` implement this pattern.

**State representation**: User-defined abstract state type and transition
function — explicit and formal.

**Novel state detection**: Any state where the model and the real system
diverge = a bug found.

**HMDFS fit: 3/5**

| Strength | Weakness |
|----------|----------|
| Formal correctness guarantees for modeled properties | Requires modeling HMDFS's complete POSIX-like state machine |
| Finds both state bugs and crash bugs | Modeling effort is proportional to DFS complexity |
| Works well for filesystems (POSIX semantics are well-understood) | Async writeback and stash/restore complicate the model |

**Data needed**: A formal state machine model for HMDFS operations. This is
a design artifact, not runtime data. Runtime validation uses `fsMd` for state
observation (already available).

**References**: Erlang QuickCheck (Hughes, 2007); Rust proptest state machine;
Haskell Hedgehog sequential/parallel testing.

---

### 3.15 Delta-Based State Abstraction

**Principle**: Instead of recording the *absolute* filesystem state (full
tree hash), record the *delta* — what actually changed during the schedule.
A delta is a set of `(path, before_state, after_state)` tuples. The state
signature = hash of the delta, not hash of the tree. Unchanged files are
implicitly ignored.

**State representation**: `{path → (before: Stat, after: Stat) | after != before}`
plus `{path → CREATED | DELETED}` entries.

**Novel state detection**: New (before, after) pair for any path, or new
combination of changes. A schedule that modifies path A and B is distinct
from one that modifies A and C, even if the final trees hash to the same
value (unlikely but possible with collisions).

**HMDFS fit: 4/5**

| Strength | Weakness |
|----------|----------|
| Finer granularity than full tree hash | Requires old FileTree for comparison |
| Captures *which* files changed, not just *that something* changed | |
| Monarch's `SyncFileTreeFromFsMd` already computes a diff internally | |

**Data needed**: Old FileTree (from previous `SyncFileTreeFromFsMd`),
current fsMd (from `write_metadata`). Both already available in
`executeRaw`.

---

### 3.16 TSC-Based Global Happens-Before (Hardware Clock Total Order)

**Principle**: Use the hardware TSC (Time Stamp Counter) as a global clock
across all VMs. Since all QEMU VMs share the same physical host TSC with
per-VM offsets (`+invtsc`), the value `rdtsc() - tsc_offset` for any VM
yields the same host TSC. This gives a *total order* over all events across
all nodes. From this total order, derive a *partial order* (happens-before):
events whose execution intervals overlap are "concurrent"; events with
non-overlapping intervals can be ordered.

**State representation**: Two-dimensional — a TSC-ordered total event
sequence, overlaid with a happens-before partial order derived from interval
overlap analysis.

**Key difference from Lamport Timelines**: Lamport requires sending and
receiving messages to propagate logical clocks — it derives causality from
communication. TSC-based ordering derives causality from *physical time*
(interval non-overlap). The two are complementary: Lamport captures
application-level causality, TSC captures wall-clock ordering.

**Novel state detection**: New happens-before edge patterns, new sets of
concurrent events, or new event interleavings.

**HMDFS fit: 5/5**

| Strength | Weakness |
|----------|----------|
| Zero message tracing needed — uses hardware TSC | Async writeback breaks the "etime = completion" assumption |
| TSC offset infrastructure already in place in Monarch | Need to separately track `WritePageCB` for write completion |
| Works across QEMU VMs sharing the same host | Not applicable to real (non-virtualised) multi-machine setups |

**Data needed**: Per-call TSC start/end timestamps (already in `callReply`,
already parsed into `CheckInfo.Stime/Etime`); per-VM `tsc_offset` (already
in `argv[15]`); `WritePageCB` tracepoint timestamps (perf collection in
progress). All data channels already built or being built.

---

### 3.17 Content-Addressable State (Merkle DAG)

**Principle**: Each file is content-addressable by its content hash (already
in `Checksum` in fsMd). Each directory's identity is the hash of its
children's hashes concatenated. This forms a Merkle DAG — the same structure
Git uses to represent repository state. The root hash uniquely identifies
the entire filesystem tree.

**State representation**: A Merkle tree: `dir_hash = hash(child1_hash ||
child2_hash || ...)` where leaf hashes are file content checksums.
A delta between two Merkle trees precisely identifies which paths changed,
at what level of the hierarchy.

**Novel state detection**: New root hash = novel state. Or, more finely: new
subtree hash at any level = novel sub-behaviour. This naturally captures
"only file /A/B changed, everything else is the same."

**HMDFS fit: 5/5**

| Strength | Weakness |
|----------|----------|
| Precise change localisation — knows *which* subtree changed | Building the Merkle tree requires full directory walk |
| Content checksums already collected in fsMd (`Checksum` field) | Parent hashes must be computed from child hashes (not directly in fsMd) |
| Git-like semantics are intuitive and well-understood | |

**Data needed**: fsMd per-node (already collected). Compute Merkle tree from
fsMd entries by reconstructing the directory hierarchy. Root hash = feedback
signal.

**Key difference from File Tree Hash (Approach 5)**: Approach 5 hashes the
full tree serialization. Approach 17 builds a Merkle DAG — enabling per-subtree
difference detection. If only `/subtree/A/file.txt` changes, only the hashes
along the path from root to `file.txt` change — the rest of the DAG is
unchanged. This gives *localised* novelty detection rather than *global*.

---

### 3.18 Model-Guided Fuzzing (TLA+ Formal-Spec-Based Coverage)

**Principle**: Uses an abstract formal model of the distributed system (written in
TLA+) to define *coverage*. Rather than using code edges or file hashes as the
coverage metric, the formal model's state space becomes the coverage target.
The fuzzer generates test inputs and checks which abstract model states are
reached. Novel behaviour = hitting a model state not previously observed.

**Key insight**: Abstract models are frequently developed in early phases of
protocol design and verification (Two-Phase Commit, Raft, Paxos) but are
infrequently used at testing time. This approach bridges formal verification
effort (model writing) with practical testing (fuzz campaign).

**State representation**: The state space of a TLA+ model — an authoritative,
formally-defined set of all possible system states. Coverage is measured
against this reference.

**Novel state detection**: Any model state not yet covered by the fuzzer is
"novel." A model state hit for the first time → feedback signal.

**HMDFS fit: 2/5**

| Strength | Weakness |
|----------|----------|
| Formally precise — model states are unambiguous | HMDFS has no TLA+ model (writing one is a major engineering effort) |
| Coverage definition is validated by the formal model, not guessed | Model must be maintained alongside the implementation |
| Found 13 previously unknown bugs in Etcd-raft and RedisRaft | Overlay filesystem state is more complex than consensus protocol state |

**Data needed**: A TLA+ (or PlusCal) model of HMDFS, plus a mapping from
implementation-level observations (our eBPF trace events) to model states.
Both are design artifacts, not runtime data.

**References**: Gulcan et al., "Model-Guided Fuzzing of Distributed Systems,"
ACM TOSEM 2024/2025 (based on arXiv:2410.02307).

---

### 3.19 Memory-Based State Inference (LSH Memory Snapshots)

**Principle**: Insert compile-time probes on memory allocations and network
I/O. At runtime, take snapshots of long-lived memory regions and apply
*Locality-Sensitive Hashing* (LSH) to map memory contents to a unique state
identifier. The fuzzer incrementally builds a protocol state machine from the
observed (memory_snapshot → next_memory_snapshot) transitions — entirely
automatic, requiring zero manual state annotation or protocol specification.

**Key insight**: Long-lived heap/global memory regions encode the protocol
state implicitly. LSH provides a fuzzy match — slightly different memory
content (different buffer pointers, counters) can still map to the same state.

**State representation**: A series of LSH-hashed memory snapshots. Each
snapshot is a state ID. Transitions between snapshots form the state machine.

**Novel state detection**: New LSH state ID (or new transition between known
IDs) = novel behaviour.

**HMDFS fit: 3/5**

| Strength | Weakness |
|----------|----------|
| Fully automatic — no manual state annotation needed | HMDFS is kernel code; compile-time memory probes are harder to insert |
| LSH handles minor memory variation (different pointer values, counter diffs) | Long-lived memory in kernel modules is harder to snapshot than user-space |
| Works well for network servers (proven on FTP, SMTP, HTTP, SSH, SIP, RTSP) | Kernel memory snapshot at syscall granularity adds non-trivial overhead |

**Data needed**: Compile-time instrumentation of HMDFS to insert memory
snapshot probes at VFS entry/exit points. At runtime, paired with eBPF
kretprobe events we already collect.

**References**: Natella, "StateAFL: Greybox fuzzing for stateful network
servers," EMSE 2021 (arXiv:2110.06253).

---

### 3.20 Enum-Based State Identification (Automatic State Variable Discovery)

**Principle**: Protocol implementations routinely use `enum`-typed variables
with named constants (`INIT`, `READY`, `WAITING`) to represent current state.
By analyzing the source code at compile time, these state variables can be
automatically identified. During fuzzing, the sequence of values assigned to
these variables is tracked, producing a "map" of the explored state space —
again with zero manual annotation.

**Key insight**: An empirical analysis of the top-50 most widely used
open-source protocol implementations found that **every implementation** uses
state variables assigned named constants to represent state. This is a
universal coding pattern that can be exploited automatically.

**State representation**: For each identified enum variable, the sequence of
assigned values. State = (variable_name → current_value) tuple.

**Novel state detection**: New (variable_name → value) pair, or new transition
between known states.

**HMDFS fit: 3/5**

| Strength | Weakness |
|----------|----------|
| Universally applicable to any codebase with enum state variables | HMDFS state is partially in bit flags (inode state, stash status), not always enum |
| Fully automatic — no manual annotation | Requires source-level static analysis of HMDFS code |
| Discovered several CVEs in prominent protocol implementations | May miss state encoded in non-enum data structures (linked lists, counters) |

**Data needed**: Static analysis of HMDFS source to identify enum state
variables. Runtime instrumentation to track value assignments. Complementary
to our eBPF trace.

**References**: Ba, Böhme, Mirzamomen, Roychoudhury, "Stateful Greybox
Fuzzing," USENIX Security 2022 (arXiv:2204.02545).

---

### 3.21 Load Variance-Guided Fuzzing (Themis, DFS-Specific)

**Principle**: Specifically designed for distributed file systems. Models both
client requests AND system configuration inputs as operation sequences. Uses
*load variance* as the feedback signal — the fuzzer actively tries to make
different nodes as differently loaded as possible, since imbalance is a
primary source of bugs in DFS deployments. A load detector monitors per-node
resource usage (CPU, memory, I/O) and identifies imbalances.

**Key insight**: DFS bugs often manifest as *imbalance* — one node overloaded,
one node idle. Maximizing load variance systematically surfaces load-sensitive
bugs (hang-ups, crashes, data inconsistencies due to overloaded coordinators).

**State representation**: The system's load distribution vector across nodes
plus the operation/configuration sequence that produced it.

**Novel state detection**: New load distribution pattern (different skew across
nodes) or a new operation sequence that achieves extreme variance.

**HMDFS fit: 4/5**

| Strength | Weakness |
|----------|----------|
| DFS-specific — designed for filesystem testing | Load variance is one dimension of "interesting behaviour," not exhaustive |
| Combines client requests + system configuration as input space | Requires per-node resource monitoring (CPU, memory, I/O) — new data collection |
| Found 10 new bugs in 4 real-world DFSes including CephFS and GlusterFS | Load probing may interfere with fuzzer timing |

**Data needed**: Per-node resource metrics during execution (CPU, memory,
I/O), plus the operation sequences (our eBPF trace). Configuration space
modelling (node count, replica count, stripe size) — design artifact.

**References**: Chen et al., "Themis: Finding Imbalance Failures in Distributed
File Systems via a Load Variance Model," SOSP 2025.

---

## 4. Comparative Analysis (All 21 Approaches)

| Approach | State Richness | Concurrency | FS Semantics | HMDFS Fit | Complexity |
|----------|:---:|:---:|:---:|:---:|:---:|
| 1. Lamport Timelines | ★★ | ★★★★ | ★ | ★★★ | Medium |
| 2. Vector Clocks + Snapshots | ★★★ | ★★★★★ | ★★★ | ★★★★ | High |
| 3. Operation DAG | ★★★★ | ★★★★★ | ★★★★ | **★★★★★** | High |
| 4. CRDT Models | ★★★ | ★★★★ | ★★ | ★★ | Very High |
| 5. File Tree Hash | ★★★ | ★ | **★★★★★** | ★★★★ | Low |
| 6. Predicate Mining | ★★★★ | ★★★★ | **★★★★★** | **★★★★★** | Medium |
| 7. Graph-Based State | **★★★★★** | ★★★ | ★★★★ | ★★★★ | Very High |
| 8. Commutativity Equiv | ★★★ | **★★★★★** | ★★★ | ★★★★ | Very High |
| 9. Hybrid (6+5) | **★★★★★** | ★★★★ | **★★★★★** | **★★★★★** | Medium-Low |
| 10. Deterministic Simulation | **★★★★★** | **★★★★★** | **★★★★★** | ★ | — (inapplicable) |
| 11. Invariant Mining (Daikon) | ★★★ | ★★★ | ★★★ | ★★★ | High |
| 12. Span-Based Tracing | ★★★★ | ★★★★ | ★★★ | ★★★★ | Medium |
| 13. Property-Based Testing | ★★★ | ★★★ | ★★★★ | ★★★ | Very High |
| 14. Delta-Based State | ★★★★ | ★ | **★★★★★** | ★★★★ | Low |
| 15. TSC-Based Happens-Before | ★★★★ | **★★★★★** | ★★ | **★★★★★** | Low-Medium |
| 16. Merkle DAG State | ★★★★ | ★ | **★★★★★** | **★★★★★** | Medium |
| 17. Hybrid (6+14+15+16) | **★★★★★** | **★★★★★** | **★★★★★** | **★★★★★** | Medium |
| 18. Model-Guided Fuzzing (TLA+) | ★★★★ | ★★★ | ★★★ | ★★ | Very High |
| 19. Memory-Based State (LSH) | ★★★★ | ★★★ | ★★★ | ★★★ | High |
| 20. Enum-Based State ID | ★★★ | ★★★★ | ★★ | ★★★ | Medium |
| 21. Load Variance-Guided (Themis) | ★★★ | ★★ | ★★★★ | ★★★★ | Medium |

---

## 5. Recommendation & Evolution Path

### 5.1 Primary Recommendation: Code Coverage + Operation DAG

Two dimensions cover the core questions of distributed filesystem fuzzing:

| Feedback Dimension | Approach | What It Captures | Data Status |
|---|---|---|---|
| Code-level exploration | Edge Coverage (existing) | *What code was executed* — new code paths | Done (KCOV) |
| Causal structure | Operation DAG (Approach 3) | *How operations causally relate* — new interleavings, new conflict patterns | Data ready (eBPF trace) |

**Why two dimensions suffice**:

- **Edge coverage** drives *exploration* — the fuzzer discovers which syscalls
  and arguments reach new code regions. This is AFL's proven feedback loop.
- **Operation DAG** drives *behaviour discovery* — the fuzzer discovers which
  causal patterns (write-before-read, concurrent-mkdir, writepage-barrier)
  have been exercised. This is the "distributed systems equivalent of new
  code edges" — Mallory's core insight adapted to filesystems.

Both dimensions are implemented as **separate feedback channels**: code
coverage feeds the existing `maxSignal` set, while the Operation DAG feeds a
dedicated `maxDagSignal` set with its own dashboard statistics (`dag pair
signal`, `dag schedule signal`, `dag corpus`). The channels were kept apart so
that DAG feedback can be analysed independently of coverage. Both answer the
same question — *has this bit been seen before?* — but they never mix: a DAG
bit is not reported as coverage, and a coverage bit is not reported as a DAG
pattern.

Additional dimensions (tree structure abstraction, operation distribution
vectors, predicate mining, span tracing, etc.) are not independent feedback
signals; they are *different encodings of the same underlying data*. The
Operation DAG, properly constructed, already captures the causal structure
that these alternatives attempt to approximate.

### 5.2 Phase Plan

**Phase 1 (data already collected, algorithm needed)**:

```
  Code Coverage (existing, KCOV → AFL-style edge hashing → maxSignal)
+ Operation DAG (eBPF trace → ino-based causal rules → DAG → DAG hash → maxDagSignal)
  ────────────────────────────────────────────────────────
  Two independent feedback channels (coverage vs. DAG)
```

Operation DAG construction from eBPF trace data uses these causal rules
(already enumerated in Approach 3.3):

| Rule | Derivation from eBPF Data |
|------|--------------------------|
| Read-after-Write (same ino, overlapping range) | `FuncID ∈ {WRITE, READ}`, same `Ino`, `args[offset]`/`args[len]` overlap |
| Write-after-Write (same ino, overlapping range) | `FuncID ∈ {WRITE, WRITE}`, same `Ino`, overlapping range |
| Fsync barrier (same ino, write before fsync) | `FuncID ∈ {WRITE} → FuncID ∈ {FSYNC}`, same `Ino` |
| WRITEPAGE_CB anchor (ino, writeback completion before read) | `FuncID ∈ {WRITEPAGE_CB}` → `FuncID ∈ {READ}`, same `Ino` |
| Namespace: create before open | `FuncID ∈ {CREATE, MKDIR} → OPEN`, path relationship via `Ino` chain |
| Namespace: delete / create conflict | `FuncID ∈ {UNLINK, RMDIR}` overlaps `FuncID ∈ {CREATE, MKDIR}`, same path/ino |
| No relation | Different `Ino`, no parent-child naming relationship |

This requires zero new data collection — all fields (`FuncID`, `Ino`, `Args`,
`Timestamp`) are already present in `HmdfsTraceEvent`.

**Phase 2 (optional enhancements if needed later)**:

```
+ Predicate Mining (semantic predicates layered on top of DAG)
+ Span-Based Tracing (hierarchical structure, alternative DAG perspective)
+ Property-Based Testing (requires formal HMDFS state model)
```

### 5.3 Design Principle: Two Signals, One Question

Monarch keeps code coverage and the operation DAG in **two separate signal
sets** (`maxSignal` and `maxDagSignal`), but they answer the same question:
"is this signal bit new?" The fuzzer never mixes them — DAG novelty drives
its own corpus entries and statistics, and coverage statistics stay
unpolluted. The separation is a deliberate analysis choice (see the
`DAG_KNOWN_ISSUES.md` appendix); the two channels can be re-merged into a
single `maxSignal` at any time if a unified corpus budget is preferred.

Mallory's key lesson is that *abstraction* is what makes feedback practical.
Our choice of Operation DAG as the sole behaviour-level dimension is based
on the observation that filesystem causality is finite and enumerable — unlike
general distributed systems where causal relationships must be inferred from
message passing. A filesystem's state space is bounded by its namespace and
the set of defined operations — making a DAG-based feedback signal both
tractable and comprehensive.

---

## 6. Operation DAG Feedback Design

This section transitions from *survey* to *design*: given the recommended
feedback strategy (Code Coverage + Operation DAG from §5.1), how exactly is
the Operation DAG constructed from Monarch's existing data pipeline, and how
is it reduced to actionable feedback bits?

### 6.1 Data Pipeline

```
eBPF events → HmdfsTraceEvent → ino → path mapping (fsMd)
     │                                    │
     │                                    ▼
     │                             Individual vertices:
     │                        (func_id, path, ret_bucket)
     │                                    │
     ▼                                    ▼
  TSC global timeline              Path resolution
  (total order: stime/etime)       (§6.2.1)
           │                            │
           └────────────┬───────────────┘
                        │
              ┌─────────┼─────────┐
              │                   │
         HB DAG edges        Concurrent pairs
    (path relation +      (overlapping intervals)
     modifier + interval
     not overlapping)
              │                   │
              └─────────┬─────────┘
                        │
                   Pair set (both HB + concurrent)
                        │
                  ┌─────┴─────┐
                  │           │
             per-pair hash   schedule hash
             (type-level     (combination-level
              novelty)        novelty)
                  │           │
                  └─────┬─────┘
                        ▼
              maxDagSignal (+ schedule bit →
              maxDagSchedSignal): independent
              channel, never merged into the
              coverage maxSignal
```

**Key distinction**: The *TSC global timeline* is the raw input — all eBPF
events sorted by their hardware timestamp (corrected to host TSC via
`tsc_offset`). It represents total temporal order. The *HB DAG* is an
extraction from it — a partial order containing only the edges that satisfy
both path relation and modifier/observer rules (§6.3). The DAG edges are
exclusively happens-before edges. Concurrent operations that overlap in
time do not appear in the DAG; they are captured separately as concurrent
pairs in the signature (§6.3.1).

### 6.2 Vertex Definition

A vertex represents a single eBPF-captured merge-view VFS function return.

| Field | Source | Notes |
|-------|--------|-------|
| `func_id` | `HmdfsTraceEvent.FuncID` | 16 operation types (MKDIR, WRITE, WRITEPAGE_CB, ...) |
| `path` | `HmdfsTraceEvent.Ino` → fsMd reverse lookup | `merge_view/...` path; stable across nodes and schedules |
| `ret_bucket` | `HmdfsTraceEvent.Ret` → bucket mapping | `SUCCESS / EEXIST / ENOENT / FAILURE / WRITEPAGE_DONE / WRITEPAGE_ERR` |
| `off` | write/read: `kiocb->ki_pos`; truncate: `iattr->ia_size` (gated by `ATTR_SIZE`); else 0 | Input to `offset_bucket` (end position — see #22) |
| `size` | fsMd `StatMd.Size` (post-exec) | Reference for `offset_bucket` (TAIL/BEYOND boundaries); **not a feature itself** |

**Why `path` instead of `ino`**: HMDFS merge_view inode numbers differ across
client nodes (see §2). The `path`, resolved from fsMd's `StatMd.Ino`,
provides a cross-node stable file identity.

**`ino → path` resolution**:

- kretprobe events: `BPF_CORE_READ(inode, i_ino)` gives the full merge_view
  inode → matches `fsMd[path].StatMd.Ino` directly.
- writepage events: the tracepoint's `ino_raw` field (low 32 bits) matches
  `fsMd[path].StatMd.Ino & 0xFFFFFFFF`.
- Transient files (created then deleted within one schedule): the `path` is
  extracted from the `creat`/`mkdir` arguments (`args[0]`) in the eBPF event.

### 6.2.1 Path Matching Strategy

Rather than relying exclusively on `ino → path` reverse lookup via fsMd,
vertex paths are resolved from the test program's call sequence wherever
possible. The system call arguments already contain the paths the fuzzer
generated — matching eBPF events to calls by timestamp window is both
simpler and more precise than inode-based lookup.

**Three tiers of path resolution**:

| Tier | Operation Types | Path Source | Method |
|:--:|------|------|------|
| 1 — Direct argument | `mkdir`, `creat`, `unlink`, `rmdir` | `call.Args[0]` (the path string itself) | `extractPathFromCall(call)` |
| 2 — FD resolution | `write`, `read`, `fsync`, `open`, `release`, `getattr`, `setattr`, `iterate` | Same-prog `open`/`creat` call's path | `resolveFdToPath(prog, call)` — scan backward through the program for the `open`/`creat` that returned this fd |
| 3 — Ino fallback | `writepage_cb`, unmatched events | fsMd: `{path → StatMd.Ino}` | `ino → path` reverse lookup (current design) |

**Matching algorithm**:

```
for each call in ps[progIdx]:
    path = resolve_via_tier_1_or_2(call, prog)
    if path == "": continue

    candidates = eBPF events in [call.stime, call.etime]
                 with matching func_id

    if len(candidates) == 1:
        assign(candidate, path)

    elif len(candidates) > 1:
        // Name collision fallback — test programs use unique names
        for candidate in candidates:
            d_name = last component of candidate's path (from ino → path or args)
            if d_name == last component of path:
                assign; break
        if still unmatched: fallback to first candidate in window

For rename: old_path and new_path are obtained directly from
`call.Args[0]` and `call.Args[1]`. No reverse lookup needed.
```

**Rationale**: Since Monarch generates unique file names per test (via
`randomSuffix`), the d_name matching is highly reliable. The fallback chain
(timestamp window → d_name → first candidate) ensures correctness even if
name uniqueness is violated by a mutation.

Vertices are NOT merged — every eBPF event becomes its own vertex. The
abstraction layer handles de-duplication at signature time (§6.5).

### 6.3 HB Edges

A directed edge A→B exists when **three** conditions hold simultaneously:

1. **Causal dependency**: A and B share a path relation (see below).
2. **A is a modifier that succeeded** (§6.3.1). A modifier that fails
   (e.g., `mkdir /A` returning `EEXIST`, `write` returning `ENOSPC`) does
   not change filesystem state and cannot causally influence later operations.
   Its outgoing edges are suppressed.
3. **Temporal ordering**: A.etime < B.stime (intervals do not overlap).

The HB DAG is a *partial order* derived from the *total order* of the TSC
global timeline (§6.1). It contains only **direct** edges — every edge is
established by pairwise comparison of two vertices, never via transitive
closure. Transitive information (e.g., `A→B→C` implying `A` causally
precedes `C`) is implicitly encoded by the schedule hash (§6.4) which
captures the full set of pairs occurring together. Not every adjacent
pair in the timeline becomes an edge — only those satisfying all three
conditions. Concurrent operations (time intervals overlapping) produce no
DAG edge; they are captured separately as concurrent pairs (§6.3.1).

| Path Relation | Condition | Example |
|--------------|-----------|---------|
| **SAME_PATH** | `A.path == B.path` | `mkdir /A` → `rmdir /A` |
| **SAME_INODE** | `A.ino == B.ino` AND A succeeded; if both have offset/length then overlap required, otherwise same ino suffices | `write /f(0→128)` → `read /f(64→192)`, `setattr /f` → `read /f` |
| **BARRIER** | A or B is `fsync`/`fdatasync`. Note: `writepage_cb` extends write's effective etime, after which normal SAME_INODE handles interaction — BARRIER does not produce independent relation pairs | `write /f` → `fsync /f` |
| **PARENT_CHILD** | One path is a prefix of the other AND the prefix path is a directory | `rmdir /A` → `mkdir /A/B` (direct), `mkdir /A` → `creat /A/B/C/file.txt` (indirect) |
| **SAME_PARENT** | Paths share the same parent directory | `mkdir /A/X`, `mkdir /A/Y` |

**SAME_INODE refinement for operations without offset/length**: `WRITE`
and `READ` carry `offset`/`length` in `Args[1]`/`Args[2]`, enabling precise
overlap detection. Operations like `SETATTR` (truncate, chmod) and
`WRITEPAGE_CB` do not. When either operation lacks offset/length, any
same-inode relationship is considered causal — the modifier changed the
file, and all subsequent same-inode operations are affected.

**PARENT_CHILD covers any ancestor/descendant relationship**, not just
direct parent-child. If `/A` is created and then `/A/B/C/file.txt` is
created, there is a causal edge even though `/A/B` was not explicitly
operated on. The DAG's transitive closure cannot capture this through
intermediate operations that never happened. The prefix check covers
all levels. Declaring only direct parents would leave gaps.

Because only directories can have children in a filesystem, the prefix
path must be a directory node type. Two regular files with a common path
prefix (e.g., `/A/f1` and `/A/f2`) do NOT generate PARENT_CHILD — they
fall under SAME_PARENT or no relation.

**Rename dual-path edge rule**: a `RENAME` vertex carries both `old_path` and
`new_path`, obtained directly from the call arguments (§6.2.1). For
happens-before edge determination, the rename vertex's role is directional:

| Comparison Direction | Path Used | Rationale |
|---|---|---|
| Other operation → RENAME | `old_path` | The earlier operation affected the file at its old location |
| RENAME → Other operation | `new_path` | The rename has moved the file to a new location |

For concurrent pairs, both paths are checked independently against the
other operation's path. A concurrent pair may be recorded for `old_path`,
`new_path`, both, or neither.

**SAME_PARENT does NOT produce an HB edge** — sibling files have no direct
causal dependency. It DOES produce a concurrent pair in the signature
(see §6.4).

**Concurrent operations** (intervals overlap) → no HB edge, regardless of
path relation. They are captured in the signature as concurrent pairs.

### 6.3.1 Modifier vs. Observer Classification

Not all VFS operations are equal in their causal influence. Operations fall
into two categories:

**Modifiers** — change filesystem state on success and can causally
influence later operations when they succeed. A modifier whose return code
indicates failure (e.g., `-EEXIST`, `-ENOSPC`, `-EIO`) does not produce
outgoing HB edges: `WRITE`, `MKDIR`, `CREAT`, `RMDIR`, `UNLINK`, `RENAME`,
`SETATTR`, `FSYNC`. (Note: `WRITEPAGE_CB` is not an independent modifier —
it extends the write operation's effective completion time; the extended
write interval then interacts with other operations via normal SAME_INODE.)

**Observers** — read filesystem state but do not change it. They cannot
causally influence later operations: `READ`, `LOOKUP`, `ITERATE`, `GETATTR`,
`OPEN`, `RELEASE`.

**HB edge rule (refined)**:

A→B exists iff:
1. A path relation exists between A and B.
2. **A is a modifier that succeeded.** (A.ret_bucket ∈ {SUCCESS, WRITEPAGE_DONE})
3. A.etime < B.stime (intervals do not overlap).

| A | B | HB edge? | Rationale |
|---|---|---|---|:--:|------|
| Modifier (succeeded) | Modifier | ✅ | A's successful change affects B's execution |
| Modifier (succeeded) | Observer | ✅ | A's successful change affects what B observes |
| Modifier (failed) | Any | ❌ | Failed modifier changed nothing; no causal influence |
| Observer | Any | ❌ | Observer changed nothing; no causal influence |

Example: `WRITE /f` → `READ /f` produces an HB edge because the write
changes the file content that the read observes. But `LOOKUP /f` → `READ /f`
does NOT produce an HB edge — the lookup result may be cached, but it does
not change the data that the read observes.

**Concurrent pair rule (refined)**:

A concurrent pair is recorded in the signature for vertex pairs that share
a path relation AND have overlapping execution intervals AND at least one
of the two is a modifier. Two observers happening concurrently on related
paths (e.g., two concurrent `LOOKUP /f` events) produce no signature entry
— their concurrency has no behavioural significance.

### 6.4 Abstract Signature Layer

The feedback signature is derived from two independent sources, both
rooted in the TSC global timeline (§6.1):

- **HB pairs**: extracted from the HB DAG — vertex pairs connected by a
  *direct* causal edge. The DAG contains only direct edges (§6.3);
  transitive relationships are implicitly encoded by the schedule hash.
- **Concurrent pairs**: computed directly from the TSC timeline — vertex
  pairs whose execution intervals overlap, satisfy the modifier/observer
  rules (§6.3.1), and share a path relation. They do NOT require or use
  the DAG.

Both are abstracted through the same pipeline: abstract each vertex to its
5-feature tuple, determine the relationship type, hash, and insert into
`maxDagSignal` (the dedicated DAG channel; see §5.3).

For every vertex pair (A, B) that shares a path relation and satisfies
the modifier/observer rules (§6.3.1, including A succeeded for HB edges):

1. **Determine `temporal_rel`**:
   - A→B reachable in the HB DAG → `HB`
   - B→A reachable in the HB DAG → `HB`
   - Neither reachable AND intervals overlap → `CONCURRENT`
   - Neither reachable AND NOT overlapping → this pair produces no signature entry (A occurred before B but no causal dependency exists)

2. **Determine `path_rel`**: one of `{SAME_PATH, SAME_INODE, PARENT_CHILD, SAME_PARENT}`.

3. **Abstract each vertex** to a 6-feature tuple:

   | Feature | Values | Rationale |
   |---------|--------|-----------|
   | `func_id` | 16 types | Operation kind |
   | `ret_bucket` | SUCCESS/EEXIST/ENOENT/FAILURE/WRITEPAGE_DONE/WRITEPAGE_ERR | Different outcomes = different code paths |
   | `depth_bucket` | 0 / 1 / 2-4 / 5+ | Affects lookup iteration count and comrade search depth |
   | `node_type` | FILE / DIR | Routes to `file_ops` vs `dir_ops` VFS tables |
   | `is_persist` | true / false | Routes through stash/restore vs. normal writeback |
   | `offset_bucket` | NA / 0 / MID / TAIL / BEYOND | Distinguishes same-position concurrent writes (real data races) from disjoint-region writes; TAIL = last partial page (size % 4096), BEYOND = pos ≥ size (sparse write / expansion) — mirrors HMDFS writeback (file_remote.c `hmdfs_get_writecount`) |

   **Dropped features and why**:
   - `is_initial` — pre-existing remote files and newly created files both go
     through HMDFS remote RPC paths; the distinction is captured by `func_id`
     (OPEN vs. CREAT).
    - `TMP_DIR` — a Monarch test-framework classification, not an HMDFS
      behavioural distinction.
    - `length` — write length affects writeback merging, but its primary
      behavioural impact (crossing the file-size / page boundary) is already
      captured by the `offset_bucket` (the kretprobe reads the end position
      ki_pos ≈ pos+len, so boundary-crossing writes fall into BEYOND).
      Encoding raw length separately would add noise without new signal.
      (Note: the original design rejected `offset` too — as parameter-level
      noise with no signal. That is revisited: `offset_bucket` is not a
      parameter-level distinction but a **concurrency-semantics** one — it
      separates true data races (same position) from disjoint-region writes,
      which is exactly the missing dimension for data-race feedback. See
      `DAG_KNOWN_ISSUES.md` #22.)

4. **Compute signature hash**:

   ```
   sig_hash = hash(
       func_id_A, ret_bucket_A, depth_bucket_A, node_type_A, is_persist_A, offset_bucket_A,
       func_id_B, ret_bucket_B, depth_bucket_B, node_type_B, is_persist_B, offset_bucket_B,
       temporal_rel, path_rel
   )
   ```

5. **Insert into `maxDagSignal`**: the hash is truncated from uint64 to
   uint32 and merged into the dedicated DAG signal set (`maxDagSignal`),
   which is **separate** from the code-coverage `maxSignal` (§5.3). The
   fuzzer never mixes the two channels: DAG novelty drives its own corpus
   entries (`dag corpus` statistic) and is reported via `dag pair signal` /
   `dag schedule signal` statistics, leaving coverage statistics pure.

6. **Schedule-level hash (dual granularity)**:

    ```
    all_pair_hashes = sorted({hash(pair) for all pairs})
    schedule_hash = hash(all_pair_hashes)  → uint64 → uint32 → maxDagSchedSignal
    ```

    The per-pair hash (§5 above) rewards the discovery of new *pair types*
    — the first time a `(MKDIR, RMDIR, CONCURRENT)` pair appears, it
    contributes a new signal bit. Once all pairwise types have been
    discovered, however, a schedule that combines three previously-seen
    pair types in a single execution would produce zero new bits. The
    schedule-level hash closes this gap: it encodes the *combination* of
    all pairs occurring together, rewarding schedules whose global
    structure is novel even if every constituent pair type has been seen
    individually.

    The schedule hash is stable across repeated executions of the same
    schedule: per-pair hashes are already abstracted (vertex features have
    collapsed away concrete paths and offsets), so the sorted set is
    deterministic for a given causal pattern. No additional noise is
    introduced.

    **Corpus-driving role**: only the per-pair bits drive corpus entries
    (each novel pair earns the execution a triage slot); the schedule bit
    is statistics-only. Schedule novelty implies a new pair combination but
    not necessarily a new pair (§6.5/`DAG_KNOWN_ISSUES.md` appendix), and
    the combination space is exponential — driving the corpus from it would
    admit almost every execution.

**Why both granularities are needed (not just schedule hash)**:

If only the schedule hash were used, every schedule that produces a new
combination of pairs would receive exactly one new signal bit — regardless
of whether it discovered three entirely new pair types or merely recombined
fifty previously-seen ones. The per-pair bits quantify "how much was
discovered" for corpus entry and statistics; the schedule hash rewards
"this combination is novel" as a single statistical bit.

**Why concurrent pairs matter**: `(MKDIR, RMDIR, HB, SAME_PATH)` and
`(MKDIR, RMDIR, CONCURRENT, SAME_PATH)` are semantically distinct — the
former is a sequential create-then-delete, the latter is a concurrent
conflict. Both must produce distinguishable signature bits.

**Automatic de-duplication via set semantics**: If 3 WRITE vertices all form
the same relation pair with 1 READ vertex, each produces an identical hash →
the sorted set collapses them to one entry. The signature captures *whether*
a pattern occurred, not *how many times*.

### 6.5 Relationship to the Mutator

The mutation design (DCT weights, path selection via the file tree, group
mutation, time-aligned concurrent insertion — §6.5.2) generates diverse
causal patterns. The Op DAG feedback is now **integrated into the mutation
loop**: novel DAG pairs are fed back into the DCT tables (each novel pair is
mapped to its `(rootCall, variant)` combo and marked as yielding signal).
Triage successes enter the corpus without any static attribution — DCT
learning is driven exclusively by the dynamic pair feedback (see
`DAG_KNOWN_ISSUES.md` #20). The feedback loop is:

```
chooseVariant picks (root, variant) → NoYield++；unexplored combos preferred；≥ threshold → down-weight
ChooseTemporal picks the combo's insertion form (concurrent / causal) → execute
→ novel DAG pair → MarkExplored + MarkYield + weight +1
→ pair temporal (CONCURRENT/HB) → UpdateTemporalWeight (second layer, per form)
```

Of the three enhancement directions identified in the original design, two
are implemented and one is deferred:

| Direction | Status | Implementation |
|-----------|:--:|------|
| Path-relation exploration tracking | **Implemented** | DCT keeps an `Explored` map per `(rootCall, variant)`; `chooseVariant` picks exclusively from combos that never produced signal and are still within their exploration budget (`NoYield < 20`). |
| Adaptive weight damping | **Implemented** | DCT keeps a `NoYield` counter per combo: each selection bumps it, a yield (`MarkYield` from novel DAG pairs) resets it, and reaching 20 consecutive no-yield selections drops the weight by 5 (floor 1). |
| Temporal form layer | **Implemented** | A second layer per combo: `TemporalWeights` (concurrent vs causal/HB form, 50/50 start). `insertCallFromDCT` picks the form (`ChooseTemporal`); the causal form inserts at `firstBoundaryAfter` (after the root finishes, favoring HB pairs). `feedbackDagPairs` updates the weights by the actually produced pair temporal; every novel pair — concurrent or HB — also drives direction 1/2 (`MarkYield`: the combo is rewarded for its combined output, whatever form it took; see `DAG_KNOWN_ISSUES.md` #16). |
| Targeted mutation | Deferred | "I need a WRITE→READ SAME_INODE HB pair on a depth-3+ DIR but haven't seen one" → construct it specifically. Requires a synthesiser; revisit after evaluating the first two directions. See also `DAG_KNOWN_ISSUES.md` #17 for the broader modeling research (structure for concurrent/causal relations). |

Both implemented directions use the same signal source: `feedbackDagPairs`
(fuzzer-side mapping of novel DAG pairs to DCT combos).

**Direction 1 — Path-relation exploration tracking (implemented)**

- Mechanism: the DCT table keeps an `Explored` flag per `(rootCall, variant)`;
  `chooseVariant` collects the combos that never produced signal and are
  still within their exploration budget (`NoYield < 20`) and, when this pool
  is non-empty, picks **only from it** (exclusive bias — see
  `DAG_KNOWN_ISSUES.md` #7).
- Signal source: `MarkYield` (from novel DAG pairs via `feedbackDagPairs`)
  sets `Explored = true`.
- Parameters: exploration budget 20 (shared `NoYield` counter with
  Direction 2).
- Implementation: `distributed_choice.go` (`Explored`, `chooseVariant`),
  `proc.go` (`feedbackDagPairs`), `dag.go` (`DagPairToVariant` mapping).
- Granularity: DCT combos (`callName` + `pathRel`), coarser than the DAG
  feature buckets — the bias acts on generation/mutation combo selection,
  not on the pair space itself.

**Direction 2 — Adaptive weight damping (implemented)**

- Mechanism: a `NoYield` counter per combo (bumped on every selection);
  reaching 20 consecutive no-yield selections drops the weight by 5
  (floor 1), recomputes `TotalWeights`, and resets the counter (to avoid
  continuous downgrades).
- Signal source: `MarkYield` resets `NoYield = 0`.
- Parameters: `noYieldThreshold = 20`, `noYieldDelta = 5`.
- Per-proc semantics: DCT tables are per-proc, so counters and downgrades
  evolve independently per worker.
- Implementation: `distributed_choice.go` (`noYieldTick`, `MarkYield`).

**Direction 3 — Targeted mutation (deferred)**

- Concept: flip from "act first, observe later" (exploration: random
  modification → execute → see what signal appears) to "set the target,
  then construct" (prescription: query missing patterns → build a program
  that produces one).
- Example: `(WRITE→READ, HB, SAME_INODE, depth≥3)` has never been seen →
  pick a depth-3+ path + an `open/write/read` fd chain + sequential timing
  (guaranteeing HB rather than CONCURRENT) → execute and verify.
- Three prerequisites: ① missing-pattern tracking (infer unseen combos from
  the known `maxDagSignal` space — an extension of Direction 1); ② a
  synthesizer from abstract features to concrete programs (a program
  synthesis problem; ready-made building blocks: `generateCallByName`, fd
  chain construction, barrier/nanosleep timing control); ③ a verification
  loop (execute, check whether the target pair fired, adjust if not).
- A weak form already exists: §6.5.3 call movement reorders existing calls
  toward a target; the full form constructs from scratch.
- Priority: deferred — it depends on all three prerequisites and the first
  two directions' experimental data should first confirm that missing
  patterns are actually the exploration bottleneck.

### 6.5.1 PathCross: Extending DCT for Cross-Node Path References

The current DCT uses a star topology — all concurrent nodes resolve their
call paths relative to node 0's root call path. In the DAG, every
cross-node pair is `(node0, nodeN)`. Horizontal interactions between peer
nodes — `(node1, node2)` — are never directly generated, limiting the
diversity of PARENT_CHILD and SAME_PARENT patterns.

**PathCross** (`PathRelation=7`) allows a non-node0 call's path to be
resolved relative to **another peer node's** already-resolved path.

**Generation guarantee**: Nodes are generated sequentially (0 → 1 → 2 → …).
When PathCross is selected for node N, its peer is randomly chosen from
nodes {0..N-1} — which already have resolved paths. This guarantees a
valid reference at any node count.

**Design**: Option A — embed an `IsCross` flag directly into `CallVariant`,
giving each `(rootCall, variantCall, PathRelation, IsCross)` combination
its own weight in the DCT table. This enables independent weight tuning
for cross-node vs. same-node variants (e.g., `PathCross+PathChild` at weight
8 vs. same-node `PathChild` at weight 15).

| Variant Field | Value | Meaning |
|:---|:--:|------|
| `PathRelation` | any | The relation type used to resolve the path (Child, Same, Sibling, …) |
| `IsCross` | false | Path is relative to node 0's root call (existing behavior) |
| `IsCross` | true | Path is relative to a peer node's resolved path (new) |

**Chaining example (5 nodes)**:

```
node0: mkdir /A
node1: IsCross=false, PathChild → mkdir /A/B
node2: IsCross=false, PathSibling → creat /A/X
node3: IsCross=true, peer=node1, PathChild → mkdir /A/B/C
node4: IsCross=true, peer=node2, PathSame → write /A/X

DAG pairs:
  (node1, node0, PARENT_CHILD, HB)
  (node2, node0, SAME_PARENT, CONCURRENT)
  (node3, node1, PARENT_CHILD, HB)  ← cross-node pair
  (node4, node2, SAME_INODE, CONCURRENT)  ← cross-node pair
```

The first two pairs already exist with the star topology. The latter two
— directly linking peer nodes — require PathCross.

**Implementation checklist**:

| File | Change |
|------|--------|
| `distributed_choice.go` (enum) | Add `PathCross = 7` to `PathRelation` |
| `distributed_choice.go` (variant) | Add `IsCross bool` to `CallVariant` |
| `distributed_choice.go` (table) | Add `IsCross` variant loop in `initDefaultConfig`; set initial weights |
| `distributed_choice.go` (info) | Add `ResolvedPaths map[int]string` to `DistributedChoiceInfo` |
| `rand.go` (generation) | When `IsCross`, pick peer from {0..N-1} and use its resolved path |
| `mutation.go` (MutateGroupPath) | Handle `IsCross` when re-resolving group paths |
| `mutation.go` (insertCallFromDCT) | Same |

**Status: not implemented (deferred).** The DAG feedback loop (integrated in
§6.5) and its saturation characteristics are being evaluated first; PathCross
is the next candidate if the star topology shows up as a bottleneck in the
observed pair space.

### 6.5.2 Time-Aligned Concurrent Insertion

Concurrent calls inserted by the pattern/DCT mutation paths used to be
aligned by **call-array index** (`min(insertPos, len(p.Calls))` or the
reference insert position): every prog inserts at the same index. Because
programs differ in call count and per-call duration, equal indices do not
mean equal execution times — the "concurrent" calls actually execute at
different moments and rarely overlap.

Since every executed call already carries its last execution window
(`Call.CheckInfo.Stime/Etime`, raw guest TSC), insertion positions are now
aligned by **execution time** instead:

```
refTime = 0                                     if insertPos == 0        (program-start alignment)
        = Etime(Calls[insertPos-1]) − tscoff[0] if the predecessor call has timing info
        = −1                                     otherwise (no reference → fall back to index alignment)

for each other prog p:
    j* = argmin_j | boundaryTime(p, j) − refTime |     boundaryTime(p, j) = Etime(Calls[j-1]) − tscoff[p]
    insert the concurrent call at j*                    (fall back to index alignment when no timing info)
```

Details:

- **Timing source**: `CheckInfo` of the **most recent execution** (shared
  pointer copied by `Prog.Clone`, refreshed on every execution). Using the
  same seed repeatedly (e.g., 100 smash rounds) keeps one stable reference
  layout; corpus re-executions (triage) refresh it.
- **Cross-VM normalization**: windows are raw per-VM TSC; they are compared
  in the global domain by subtracting each VM's `tsc_offset` — the same
  normalization the DAG global timeline uses.
- **fd constraints**: `findInsertPosition` aligns within the chosen
  open/close fd range; `insertCallFromDCT` falls back to the index-aligned
  position when time alignment would place an fd-required call before its
  open.
- **Coverage**: `insertCallFromPattern` (ops and verification calls) and
  `insertCallFromDCT` (concurrent calls) both align; the generation path
  (`generateFromDistributedChoiceTable`) is untouched — freshly generated
  programs have no execution history to align against.

### 6.5.3 Time-Aligned Call Movement

**Status: not implemented (planned).** The design below is the implementation
spec for `MoveCallTimeAligned`; see also `DAG_KNOWN_ISSUES.md` #11.

§6.5.2 builds concurrency by *inserting* freshly generated calls; §6.5.3
moves *existing* calls instead. Moving is cheaper in two ways: the call count
stays unchanged (no new calls to validate), and the moved call has already
executed successfully, so it only fails when its dependencies were broken by
the move.

**What movement creates**: the pair hash is sensitive to both the A/B order
and the temporal relation, so reordering existing calls yields new bits:

- **Concurrent pair**: move A into B's execution window — the temporal
  relation flips from HB to CONCURRENT.
- **Reversed HB pair**: move A right after B — the direction flips
  (hash(A→B) ≠ hash(B→A)).

Both modes target positions via the same time-aligned machinery as §6.5.2
(reference time in the global TSC domain, `TimeAlignedInsertPos` on the
predecessor-boundary times):

```
mode = CONCURRENT (60%)   refTime = Stime(B) − tscoff[dst]      → insert into B's window
mode = REVERSED   (40%)   refTime = Etime(B) − tscoff[dst] + δ  → insert right after B
```

The mode split (60/40) is a tunable constant in `MoveCallTimeAligned`.

**Candidate filtering (`callIsMovable`)**: a call is movable when it is not
fd-required (`!IsFdRequiredCall`), not `open`/`close`, has a path argument,
has timing info (`CheckInfo != nil`), and nothing references its result
(`Ret.uses` empty). The `syz_failure` pseudo-calls carry no path argument
and are excluded automatically.

**Dependency check**: the moved call's path must exist in `lcs.FileTree`
(approximation of the merge-view state; skipped when `lcs == nil`). If the
path vanished at the target moment, the call fails and yields a FAILURE-bucket
pair — low value but harmless.

**Integration**: `mutateHmdfs` runs the type-specific mutators
(WithDCT/Stash/Dcache) first; if they fail, it tries `MoveCallTimeAligned`
before falling back to the standard `Mutate`.

**Comparison with §6.5.2**:

| Dimension | Insertion (§6.5.2) | Movement (§6.5.3) |
|---|---|---|
| Call source | freshly generated | existing (already executed) |
| Call count | +1..n | unchanged |
| Targets | concurrency | concurrency + reversed HB |
| fd handling | align inside fd range / fallback | only non-fd calls are moved |
| Failure cost | new call invalid | existing call broken (low probability) |

### 6.5.4 Dynamic Group Mutation (lazy grouping, GroupID-free)

**Status: implemented.** Group-level mutations no longer use static group IDs
assigned at generation time (they go stale as later mutations change the
concurrency/causality structure; the whole feedback chain was already dynamic).
Groups are computed **lazily from the execution timeline** when a mutation
needs them, and nothing is persisted:

```
pickAnchor(ps, r, wantReadWrite)       ps[0] random call with path + timing
  -> findGroupCalls(ps, anchor)         other progs whose last-execution windows
                                       overlap the anchor's (s1<e2 && s2<e1) ∪
                                       the direct causal successors (per prog,
                                       the earliest call starting after the
                                       anchor finishes) — normalized to the
                                       global TSC domain via tscoffs; same-prog
                                       calls never overlap
  -> pathRelBetween(anchorPath, cPath)  geometric relation (Same/Child/Parent/
                                       Sibling/NoRel), computed on the spot
```

The unified group = anchor + concurrent calls + direct causal successors
(see `DAG_KNOWN_ISSUES.md` #18), so all four mutations act on causal pairs
exactly as on concurrent ones: data sharing yields sequential write→read on
the same offset (consistency checks), path migration moves causal-chain
members together, and removal deletes them with the group.

**The four dynamic mutations** (invoked from `MutateInodeOpsWithDCT` /
`MutateFileopsWithDCT`, distribution removeGroup 20% / removeOneInGroup 10% /
path migrate 10% / data mutate 10% / insert 50%):

- `MutateGroupPathDynamic`: anchor -> new base path (`pickNewBasePath`);
  concurrent peers resolve against it with their on-the-spot relations;
  fd-required calls backtrack via `resolveFdTarget`; the non-concurrent
  backbone stays in place (no second-pass follower logic needed).
- `RemoveGroupDynamic`: deletes the anchor and its whole concurrent set.
- `RemoveOneInGroupDynamic`: deletes one fd-safe call of the set
  (`AnalyzeProgFds`, failure-injection pseudo calls kept).
- `MutateGroupDataDynamic`: the anchor must be a read/write call; all
  read/write calls of the set share one random offset (and one write length
  with `updateWriteDataBuf`) - the deterministic counterpart of the removed
  probabilistic `OffsetSame` insertions. Range comes from the anchor path's
  file size when available (1MB fallback).

**Why keep the relations on path migration (P1 decision)**: the path mutator
generalizes a proven pattern to other path shapes, it does not reshape the
relations - generating new rel combos is the job of `insertCallFromDCT`/
`ChooseVariant`. Re-randomizing rels would (a) overlap with the generation
mechanism, (b) tear apart concurrent same-path pairs (the core hmdfs conflict
scenario), and (c) pollute DCT weight learning (path-dimension feedback would
be attributed to the rel dimension).

**What was removed**: `CallProps.GroupID/PathRel/IsFromDCT/OffsetRel/LengthRel`
(5 serialization keys), `Prog.Groups/LastGroupID`, `GroupMeta`/`GroupSourceType`,
`AllocGroupID/renumberGroups/GetGroupPositions/RemoveGroup/collectAll*/...`,
~55 `SetGroupID` call sites, and the probabilistic offset/length selection
(`ChooseOffsetRel`/`ChooseLengthRel`/weights) - shared offsets are now
produced deterministically by `MutateGroupDataDynamic`. Serialization stays
backward compatible: unknown keys are ignored on deserialization. See
`DAG_KNOWN_ISSUES.md` #13/#14/#15.

**Regression guard**: `prog/path_mutation_test.go` (`TestPathRelBetween`,
`TestFindConcurrentCalls`, `TestPickAnchor`) covers the relation classifier,
window-overlap detection and anchor selection.


---

## 7. Data Dependencies

```
                     Monarch Data Pipeline
                            │
         ┌──────────────────┼──────────────────┐
         │                  │                   │
    eBPF kretprobe      code coverage     callReply
    (15 merge funcs      (KCOV)           (Stime/Etime
     + writepage_cb)         │             + tsc_offset)
         │                  │                   │
         │                  ▼                   │
         │            Edge Coverage             │
         │            (existing)                │
         │                  │                   │
         └──────────┬───────┘                   │
                    │                           │
                    ▼                           ▼
             Operation DAG               TSC Timeline
          (ino-based causal            (global ordering
           rules → DAG hash)             reference)
                    │                           │
                    └──────────┬────────────────┘
                               │
                maxDagSignal / maxDagSchedSignal
      (novel DAG pair = corpus entry + DCT feedback;
       schedule bit = statistics only)
      — separate channel, never merged into the coverage maxSignal
```

---

## 8. References

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
