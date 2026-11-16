# eBPF Merge Function Tracing for Monarch

## Overview

Monarch uses **eBPF kretprobes plus paired entry kprobes** to capture the
invocation timeline of all merge-view VFS operations inside HMDFS at the
**internal function level**, plus a **perf tracepoint**
(`hmdfs_writepage_cb_exit`) for the asynchronous writeback completion path.
This provides a behaviour-level feedback signal distinct from traditional
code coverage, enabling grey-box fuzzing of the distributed filesystem's
causal structure.

```
                         ┌─── BPF handlers ──────────────┐
                         │  entry kprobe (15 funcs):     │
  merge_view operation   │    pos/size/ino/i_mode        │
  ────────────────────▶  │    dentry ptr for mkdir/      │
                         │    create/rename/lookup       │
  e.g. mkdir, write,     │  kretprobe:                   │
  open, unlink, ...      │    timestamp (TSC ns)         │
                         │    func_id   (which op)       │
                         │    ret       (success/fd/errno)│
                         │    + entry-state map fields   │
                         └──────────┬─────────────────────┘
                                    │
                         ┌──────────▼─────────────────────┐
                         │   BPF ring buffer (256 KB)      │
                         │   lock-free, per-CPU, ~2500     │
                         │   events capacity (104 B each)  │
                         └──────────┬─────────────────────┘
                                    │
                         ┌──────────▼─────────────────────┐
                         │  executor: stop_collect_hmdfs_  │
                         │  trace() → drain ring buffer    │
                         │  → serialize to output_data     │
                         └──────────┬─────────────────────┘
                                    │
                         ┌──────────▼─────────────────────┐
                         │  fuzzer: parseHmdfsTraceEvents  │
                         │  → build timeline               │
                         │  → match ino → fsMd → path      │
                         │  → compute happens-before hash  │
                         └─────────────────────────────────┘
```

### Tracked Functions (16 func_ids)

| ID | Function | Source | Description |
|:--:|----------|--------|-------------|
| 0  | hmdfs_mkdir_merge       | BPF kretprobe + entry kprobe | Create directory (entry dentry) |
| 1  | hmdfs_create_merge      | BPF kretprobe + entry kprobe | Create regular file (entry dentry) |
| 2  | hmdfs_rmdir_merge       | BPF kretprobe + entry kprobe | Remove directory |
| 3  | hmdfs_unlink_merge      | BPF kretprobe + entry kprobe | Remove regular file |
| 4  | hmdfs_rename_merge      | BPF kretprobe + entry kprobe | Rename file/directory (new dentry) |
| 5  | hmdfs_merge_write_iter  | BPF kretprobe + entry kprobe | Write data (entry pos/size) |
| 6  | hmdfs_merge_read_iter   | BPF kretprobe + entry kprobe | Read data (entry pos/size) |
| 7  | hmdfs_file_open_merge   | BPF kretprobe + entry kprobe | Open regular file |
| 8  | hmdfs_dir_open_merge    | BPF kretprobe + entry kprobe | Open directory |
| 9  | hmdfs_getattr_merge     | BPF kretprobe + entry kprobe | Get file attributes (stat) |
| 10 | hmdfs_setattr_merge     | BPF kretprobe + entry kprobe | Set file attributes (truncate, chmod; entry size) |
| 11 | hmdfs_fsync_local       | BPF kretprobe + entry kprobe | Flush file data (called by merge) |
| 12 | hmdfs_file_release_local| BPF kretprobe + entry kprobe | Close file (called by merge) |
| 13 | hmdfs_lookup_merge      | BPF kretprobe + entry kprobe | Lookup directory entry (child dentry) |
| 14 | hmdfs_iterate_merge     | BPF kretprobe + entry kprobe | Read directory entries (getdents) |
| 15 | *(writepage_cb)*        | **perf tracepoint** `hmdfs_writepage_cb_exit` | **Async writeback completion** (not a kretprobe) |

### Captured Fields per Event (112 bytes total)

| Field     | Size  | Source                  | Description                              |
|-----------|:-----:|-------------------------|-------------------------------------------|
| timestamp | u64   | bpf_ktime_get_ns()      | TSC nanoseconds at return (window end), cross-VM comparable |
| entry_ts  | u64   | entry kprobe, bpf_ktime_get_ns() | The event's own execution window is `[entry_ts, timestamp]`; 0 when the entry state was lost |
| func_id   | u32   | predefined enum         | Which VFS function fired                  |
| ret       | s32   | PT_REGS_RC (rax)        | Return value: 0=success, <0=-errno, >0=fd |
| ino       | u64   | entry kprobe            | Target inode read at entry; for mkdir/create/rename/lookup it is the parent (old parent) directory |
| args[6]   | u64×6 | reserved                | Always 0 — argument registers are not valid at kretprobe time |
| off       | u64   | entry kprobe            | **START** position: write/read entry ki_pos (O_APPEND resolved to EOF), truncate ia_size; 0 otherwise |
| size      | u64   | entry kprobe            | **entry-time** file size for write/read/truncate; 0 otherwise |
| entry_ino | u64   | entry dentry ptr → read at return | The operation's own entry inode: mkdir/create dentry, rename OLD dentry (the moved source inode), lookup child; 0 otherwise |
| i_mode    | u32   | entry kprobe / entry dentry ptr → read at return | Entry inode mode (raw `i_mode`; the fuzzer masks with `S_IFMT`); 0 when unknown (negative lookup or dropped dentry). For rename the entry value is the **source** inode's type (a rename preserves the type; a fresh destination has no upper inode until VFS's post-rename `d_move`, which runs after the kretprobe), overwritten from `new_dentry` when one is attached |

**Why everything is captured at entry.** A kretprobe handler's `pt_regs`
holds the registers the function body left behind at its return — the kernel
return trampoline saves the return-time state (`arch/x86/kernel/rethook.c`,
`kernel/kprobes.c:__kretprobe_trampoline_handler`), so `PT_REGS_PARMx` there
are leftovers, not arguments. Reading them produced all-zero `entry_ino` and
fallback offset buckets on the test VM regardless of the kernel's
`ZERO_CALL_USED_REGS` setting. Therefore every argument-derived field is
captured by a paired entry kprobe and handed to the return handler through an
`entry_state` hash map keyed by `(pid_tgid, func_id)` (both values are not
register-derived at return; func_id is a compile-time constant). The return
handler uses only the return value (`PT_REGS_RC`), the map, and for
mkdir/create/rename/lookup a dentry POINTER stored at entry: the operation's
own entry inode is created (`d_add` in `do_create_merge`/`do_mkdir_merge`) or
spliced (`d_splice_alias` in `hmdfs_lookup_merge`) during the call, so it is
read through the stored pointer *after* the function has run. There are no
inode (`entry_ino`) fallbacks — a missed pairing yields zeros, which the
`inoFunc` diagnostic in
`dag marg` reports (`zeroOK` is the capture-failure alarm for successful
create/mkdir). **Rename stores the old dentry**: its inode still exists at
kretprobe time (VFS `d_move` runs *after* `hmdfs_rename_merge` returns), so
`entry_ino` is the moved source inode — the identity the rename vertex needs;
storing `new_dentry` would have yielded 0 for a fresh destination and the
replaced (destroyed) inode for an overwrite. The entry kprobe also fills
`i_mode` from the source dentry, so the event carries the exact type. The
struct layout must stay in sync between
`hmdfs_trace.bpf.c` (`struct merge_trace_event`), `hmdfs_trace.cc`
(`struct hmdfs_event`) and `ipc.go` (`prog.HmdfsTraceEvent`).

---

## Dependencies & Prerequisites

### Build Host

| Tool        | Min Version | Install                            |
|-------------|:----------:|-------------------------------------|
| clang       | ≥ 12       | `apt install clang`                 |
| libbpf-dev  | ≥ 0.7      | `apt install libbpf-dev`            |
| bpftool     | —          | `apt install linux-tools-common`    |
| linux-libc-dev | —       | `apt install linux-libc-dev`        |

### VM Kernel Requirements

#### A. eBPF kretprobes (merge-view functions)

| Config Item                 | Purpose                                   |
|-----------------------------|-------------------------------------------|
| `CONFIG_BPF=y`              | BPF infrastructure                        |
| `CONFIG_BPF_SYSCALL=y`      | `bpf()` syscall to load programs          |
| `CONFIG_BPF_EVENTS=y`       | **kprobe-type BPF programs (required)**   |
| `CONFIG_KPROBES=y`          | kprobe infrastructure                     |
| `CONFIG_KALLSYMS=y`         | symbol resolution for hmdfs module fns    |
| `CONFIG_DEBUG_INFO_BTF=y`   | BTF type info for BPF CO-RE               |
| `CONFIG_DEBUG_FS=y`         | `/sys/kernel/debug` (kprobe management)   |
| kernel ≥ 5.8                | `BPF_MAP_TYPE_RINGBUF` support            |
| kernel ≥ 5.7                | full BPF CO-RE / BTF support              |
| `/sys/kernel/debug` mounted | debugfs mounted                           |
| `kptr_restrict ≤ 1`         | allow BPF to read kernel pointers         |
| hmdfs module loaded         | kretprobe attach targets exist            |

#### B. perf tracepoint (async writeback — `hmdfs_writepage_cb_exit`)

| Config Item                 | Purpose                                   |
|-----------------------------|-------------------------------------------|
| `CONFIG_TRACEPOINTS=y`      | **tracepoint infrastructure (required)**  |
| `CONFIG_PERF_EVENTS=y`      | `perf_event_open` for the tracepoint      |
| `CONFIG_TRACING=y`          | **tracefs — `/sys/kernel/debug/tracing/events`** (executor reads the tracepoint `id` + `format` files) |
| `CONFIG_FTRACE=y`           | ftrace event framework                    |
| `CONFIG_DEBUG_FS=y`         | debugfs for `/sys/kernel/debug`           |
| hmdfs module loaded         | tracepoints registered (`events/hmdfs/`)  |

Verify in the VM:
```bash
cat /proc/sys/kernel/kptr_restrict                 # must be 0 or 1
ls /sys/kernel/debug/tracing/events/hmdfs/         # hmdfs tracepoints registered
ls /sys/kernel/debug/tracing/events/hmdfs/hmdfs_writepage_cb_exit/   # id + format files
```

---

## Async Writeback Tracepoint Activation

The `hmdfs_writepage_cb_exit` tracepoint fires on **asynchronous writeback
completion**: a dirty page written to the remote device triggers
`F_WRITEPAGE`, and the completion callback (`hmdfs_writepage_cb` in
`hmdfs_client.c`) emits the trace event. "Activating" it requires three
layers:

1. **Compile/load layer** — `CONFIG_TRACEPOINTS` builds the `trace_*`
   functions into the module (`CREATE_TRACE_POINTS` in `hmdfs/main.c`);
   loading the module registers them under `events/hmdfs/`.
2. **Listener layer** — the executor opens the tracepoint with
   `perf_event_open(PERF_TYPE_TRACEPOINT)` (`open_wb_tracepoint` in
   `hmdfs_trace.cc`). This arms the tracepoint's jump label — the runtime
   "activation" step. Executor log:
   `hmdfs_trace: writepage tracepoint attached`.
   **Note**: the perf ring mmap must be `(1 + 2^n)` pages (`page_cnt = 5`;
   4 pages fails `perf_mmap` on stock kernels and yields
   `writepage tracepoint unavailable` with no events at all).
3. **Event production layer** — async writeback must actually happen:
   dirty pages → delayed workqueue (`mod_delayed_work` in
   `client_writeback.c`) → remote `F_WRITEPAGE` → completion callback.
   Small writes may stay in the page cache; writeback is deferred by design.

**Capture-window and CPU scope (corrected 2026-09-28)**: the perf event is
disabled in `stop_collect_hmdfs_trace` (`PERF_EVENT_IOC_DISABLE`) and only
re-enabled in `start_hmdfs_trace`, so writeback completions that occur while
disabled are **lost**, not drained in a later round. The event is opened with
`pid=-1, cpu=0`, and perf trace/kprobe events are enqueued on the per-CPU
hlist of their event CPU (`perf_trace_add` → `this_cpu_ptr`; delivery reads
`this_cpu_ptr(event_call->perf_events)`), so **only CPU0 completions are
sampled**. The 15 BPF kprobes are unaffected: their programs attach to
`tp_event->prog_array` (`perf_event_attach_bpf_prog`) and run before the
per-CPU hlist gate (`kprobe_perf_func` / `kretprobe_perf_func`), i.e. on all
CPUs. Making writepage all-CPU would require attaching a BPF program to the
tracepoint instead of raw perf sampling (not implemented). Writepage vertices
are built without call-window matching (`dag.go`), and writepage does not feed
DCT/coverage-board/generation (see DAG_KNOWN_ISSUES P7 supplement), so these
limits affect observation statistics only.

---

## Step 1: Export vmlinux.h

**When**: Once per DFS kernel image (re-export only when the kernel is updated).

**Who**: Developer. `bpftool` runs on the build host; the VM only needs to
provide `/sys/kernel/btf/vmlinux`.

The two DFS profiles use **separate** headers:

```text
HMDFS:     tools/vmlinux.h              (from the HMDFS test kernel)
GlusterFS: tools/glusterfs/vmlinux.h    (from the GlusterFS 5.15 kernel)
```

Never overwrite one with the other.

### GlusterFS

Copy the raw BTF out of the GlusterFS VM (VM does not need `bpftool`):

```bash
VM=root@192.168.x.y
ssh "$VM" 'uname -r; ls -l /sys/kernel/btf/vmlinux'
scp "$VM":/sys/kernel/btf/vmlinux /tmp/glusterfs-5.15.btf
```

Dump it on the build host. Both the dump and build commands below run from
the repository's `src/` directory:

```bash
mkdir -p tools/glusterfs
bpftool btf dump file /tmp/glusterfs-5.15.btf format c \
    > tools/glusterfs/vmlinux.h
```

The result is a full BTF dump (approximately 7.6 MB / 358k lines), not a
hand-written minimal header.

### HMDFS

Keep the existing `tools/vmlinux.h`; it is generated from the HMDFS test
kernel. Do **not** replace it with the GlusterFS BTF.

---

## Step 2: Build the BPF Objects

Run from the repository's `src/` directory; each DFS uses its own include
directory:

```bash
# HMDFS: full HMDFS kernel BTF
clang -g -O2 -target bpf -D__TARGET_ARCH_x86 \
      -Itools \
      -c executor/hmdfs_trace.bpf.c \
      -o executor/hmdfs_trace.bpf.o

# GlusterFS: full GlusterFS 5.15 BTF
clang -g -O2 -target bpf -D__TARGET_ARCH_x86 \
      -Itools/glusterfs \
      -c executor/glusterfs_trace.bpf.c \
      -o executor/glusterfs_trace.bpf.o

# Skeleton headers
bpftool gen skeleton executor/hmdfs_trace.bpf.o \
      > executor/hmdfs_trace.skel.h
bpftool gen skeleton executor/glusterfs_trace.bpf.o \
      > executor/glusterfs_trace.skel.h
```

`src/Makefile` uses `HMDFS_BPF_INC=-Itools` and
`GLUSTERFS_BPF_INC=-Itools/glusterfs`; do not compile one DFS with the other
DFS's include path.

The skeleton header provides type-safe C functions to load and manage the BPF
program from the executor. The main `src/Makefile` `executor` target builds
both skeletons and links both trace `.cc` files.

### GlusterFS uprobe attach

The libbpf version used by the P0 build has `bpf_uprobe_opts` **without**
`func_name` (`bpf_uprobe_opts__last_field=retprobe`). Attach by offset:

```c
bpf_program__attach_uprobe(prog, /*retprobe=*/false, -1, fuse_so, file_offset);
```

Resolve the three LOCAL symbols from `fuse.so` `.symtab` with libelf, then
convert the symbol virtual address to the file offset through the containing
PT_LOAD:

```text
file_offset = st_value - p_vaddr + p_offset
```

The VM image must keep `.symtab`; a stripped `fuse.so` makes startup symbol
resolution fail. The P0 image (`BuildID e752dad97c4d6048387bd91117df351afb83c0c1`)
has the three symbols in LOAD1 (`p_offset=p_vaddr=0x15000`), so
`file_offset == st_value` for that build.

`send_fuse_iov` uses `retprobe=true`; `fuse_dispatch` and `fuse_fop_resume`
use `false`.

---

## Step 3: Executor Integration

### `hmdfs_trace.cc` / `hmdfs_trace.h`

```c
void init_hmdfs_trace(void);       // load BPF skeleton + attach 15 kretprobes + 15 entry kprobes + open writepage perf event
void start_hmdfs_trace(void);      // reset collected_count + enable perf event (forked child)
void stop_collect_hmdfs_trace(void);// drain ring buffers + write count+events + reset for next round (parent)
```

### `executor.cc` Call Sites

```c
// In main(), once at startup:
init_hmdfs_trace();                        // ~executor.cc:579

// In execute_one(), per round (forked child):
start_hmdfs_trace();                       // ~executor.cc:1705

// In reply_execute(), per round (parent, after write_metadata):
stop_collect_hmdfs_trace();                // ~executor.cc:1592
```

Serialization (per round, after fsMd): header
`u64 ratio, u64 tsc_offset, u64 best_gap, u32 count`, then per event
`u64 ts, u32 fid, u32 ret, u64 ino, u64 args[6], u64 off, u64 size,
u64 entry_ino, u32 i_mode, u64 entry_ts` (108 B/event on the wire; the
in-kernel event structure is 112 B with padding). `best_gap` is the accepted
calibration's smallest TSC read gap — an independent bound on the clock
conversion error, logged by the parser to `parse-dbg.log`.

---

## Step 4: Fuzzer-Side Parsing

### `ipc.go` — parseHmdfsTraceEvents

```go
func parseHmdfsTraceEvents(data *[]byte) []prog.HmdfsTraceEvent {
    // ratio/offset/gap (u64 each) + count (u32) prefix, then per event:
    //   u64 ts, u32 fid, u32 ret, u64 ino, u64 args[6], u64 off,
    //   u64 size, u64 entry_ino, u32 i_mode, u64 entry_ts
}
```

`prog.HmdfsTraceEvent` mirrors the 108-byte wire layout (Timestamp/
FuncID/Ret/Ino/Args[6]/Off/Size/EntryIno/Mode/EntryTs) plus `ProgIdx` filled
on the Go side. The parser must stay aligned with `stop_collect_hmdfs_trace`'s
write order. `Args` is reserved and always 0 (arguments are captured before
the call, not at return).

### `func_id` → Operation Name Mapping

```go
var MergeFuncNames = map[uint32]string{
    0:  "MKDIR", 1: "CREATE", 2: "RMDIR", 3: "UNLINK", 4: "RENAME",
    5:  "WRITE", 6: "READ", 7: "FILE_OPEN", 8: "DIR_OPEN", 9: "GETATTR",
    10: "SETATTR", 11: "FSYNC", 12: "RELEASE", 13: "LOOKUP", 14: "ITERATE",
    15: "WRITEPAGE",
}
```

---

## Clock Normalisation

`bpf_ktime_get_ns()` returns `CLOCK_MONOTONIC` nanoseconds. On x86_64 with
TSC clocksource, this is derived from the hardware TSC. All QEMU VMs on the
same host share the same TSC frequency (via `+invtsc` CPU flag).

Each VM has a per-instance TSC offset (read from KVM debugfs at boot),
passed to the executor as `-TSCOFF` (argv[15]). The offset is fixed for one
QEMU lifetime and the manager re-reads it on every fuzzer launch; there is no
in-process refresh. The executor's `tsc_ns_to_global()` (re-anchored every
round) converts event timestamps — including `entry_ts` — into the global
raw-TSC domain; call windows stay in raw guest TSC and the Go side subtracts
the same per-node tscoff set. The perf writepage event explicitly selects
`CLOCK_MONOTONIC` (`use_clockid`) so both streams share one clock. For
happens-before ordering, only the relative ordering within the same frequency
domain matters.

---

## Performance Overhead

| Metric          | Estimate                     |
|-----------------|------------------------------|
| Per kretprobe   | ~300-500 ns (BPF handler + ring buffer submit) |
| Per entry kprobe| ~150-300 ns (comm check + map update), all 15 functions |
| Per schedule    | ~0.05-0.25 ms (100-500 trace events) |
| Schedule length | ~1-10 ms typical             |
| Overhead ratio  | 5-20% of schedule time       |

The overhead is incurred ONLY when the BPF program is loaded and kretprobes
are attached (i.e., during execution loops). There is zero overhead when
disabled.

---

## Troubleshooting

### Check if kretprobes are attached
```bash
cat /sys/kernel/debug/kprobes/list | grep hmdfs
```
Expected output: one line per function starting with `r` (return probe).

### Check if the BPF program is loaded
```bash
bpftool prog list | grep hmdfs_trace
```
Expected output: program ID, name, and attached type (kprobe/kretprobe).

### Check the writepage tracepoint
```bash
ls /sys/kernel/debug/tracing/events/hmdfs/hmdfs_writepage_cb_exit/
```
Expected output: `enable`, `format`, `id`, `filter` files.
Executor log should show `hmdfs_trace: writepage tracepoint attached`.

### Check kernel log for registration errors
```bash
dmesg | tail -30 | grep -i "bpf\|kprobe\|trace"
```

### Common issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| `hmdfs_trace_bpf__open_and_load` fails | BTF not available | Check `CONFIG_DEBUG_INFO_BTF=y` |
| kretprobe registration fails | Function not exported / notrace | Verify function is in `/proc/kallsyms` |
| BPF program rejected by verifier | Invalid pointer access | Check `BPF_CORE_READ` chain matches struct layout |
| `ring_buffer__consume` returns 0 events | kretprobes not hit | Verify functions are actually called during execution |
| `writepage tracepoint unavailable` | perf ring mmap size or tracepoint missing | mmap must be 5 pages (1+4); check `CONFIG_TRACEPOINTS`/`CONFIG_PERF_EVENTS`; check `events/hmdfs/` exists |
| writepage events always 0 | async writeback not fired | Verify writes generate dirty pages and the delayed writeback workqueue runs |
| `kptr_restrict` blocks pointer read | Permission denied | `echo 1 > /proc/sys/kernel/kptr_restrict` |

---

## File Inventory

| File | Purpose |
|------|---------|
| `src/executor/hmdfs_trace.bpf.c` | BPF program — 15 kretprobe handlers + 15 entry kprobes (104 B events) |
| `tools/vmlinux.h` | Generated: kernel BTF type definitions (export once) |
| `src/executor/hmdfs_trace.skel.h` | Generated: libbpf skeleton for executor |
| `src/executor/hmdfs_trace.cc` / `.h` | Executor-side load/control/collect logic (BPF ring + perf writepage) |
| `src/executor/executor.cc` | Integration: init/start/stop call sites |
| `src/pkg/ipc/ipc.go` | Fuzzer-side: `parseHmdfsTraceEvents` (104 B layout) |
| `src/prog/prog.go` | `HmdfsTraceEvent` struct (incl. `Off`/`Size`/`EntryIno`/`Mode`) |
| `src/prog/dag.go` | DAG timeline construction, writepage vertices, offset buckets |
| `src/executor/EBPF_TRACE.md` | This document |
