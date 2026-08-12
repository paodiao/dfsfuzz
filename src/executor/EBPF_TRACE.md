# eBPF Merge Function Tracing for Monarch

## Overview

Monarch uses **eBPF kretprobes** to capture the invocation timeline of all
merge-view VFS operations inside HMDFS at the **internal function level**,
plus a **perf tracepoint** (`hmdfs_writepage_cb_exit`) for the asynchronous
writeback completion path. This provides a behaviour-level feedback signal
distinct from traditional code coverage, enabling grey-box fuzzing of the
distributed filesystem's causal structure.

```
                         ┌─── BPF handler (kretprobe) ───┐
                         │  capture:                      │
  merge_view operation   │    timestamp (TSC ns)         │
  ────────────────────▶  │    func_id   (which op)       │
                         │    ret       (success/fd/errno)│
  e.g. mkdir, write,     │    ino       (target inode)   │
  open, unlink, ...      │    args[6]   (raw registers)  │
                         │    off       (ki_pos/ia_size) │
                         └──────────┬─────────────────────┘
                                    │
                         ┌──────────▼─────────────────────┐
                         │   BPF ring buffer (256 KB)      │
                         │   lock-free, per-CPU, ~3200     │
                         │   events capacity (80 B each)   │
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
| 0  | hmdfs_mkdir_merge       | BPF kretprobe | Create directory |
| 1  | hmdfs_create_merge      | BPF kretprobe | Create regular file |
| 2  | hmdfs_rmdir_merge       | BPF kretprobe | Remove directory |
| 3  | hmdfs_unlink_merge      | BPF kretprobe | Remove regular file |
| 4  | hmdfs_rename_merge      | BPF kretprobe | Rename file/directory |
| 5  | hmdfs_merge_write_iter  | BPF kretprobe | Write data |
| 6  | hmdfs_merge_read_iter   | BPF kretprobe | Read data |
| 7  | hmdfs_file_open_merge   | BPF kretprobe | Open regular file |
| 8  | hmdfs_dir_open_merge    | BPF kretprobe | Open directory |
| 9  | hmdfs_getattr_merge     | BPF kretprobe | Get file attributes (stat) |
| 10 | hmdfs_setattr_merge     | BPF kretprobe | Set file attributes (truncate, chmod) |
| 11 | hmdfs_fsync_local       | BPF kretprobe | Flush file data (called by merge) |
| 12 | hmdfs_file_release_local| BPF kretprobe | Close file (called by merge) |
| 13 | hmdfs_lookup_merge      | BPF kretprobe | Lookup directory entry |
| 14 | hmdfs_iterate_merge     | BPF kretprobe | Read directory entries (getdents) |
| 15 | *(writepage_cb)*        | **perf tracepoint** `hmdfs_writepage_cb_exit` | **Async writeback completion** (not a kretprobe) |

### Captured Fields per Event (80 bytes total)

| Field     | Size  | Source                  | Description                              |
|-----------|:-----:|-------------------------|-------------------------------------------|
| timestamp | u64   | bpf_ktime_get_ns()      | TSC nanoseconds, cross-VM comparable      |
| func_id   | u32   | predefined enum         | Which VFS function fired                  |
| ret       | s32   | PT_REGS_RC (rax)        | Return value: 0=success, <0=-errno, >0=fd |
| ino       | u64   | BPF_CORE_READ from args | Target inode number (for fsMd matching)   |
| args[6]   | u64×6 | PT_REGS_PARM1..6        | Raw argument registers (rdi..r9)          |
| off       | u64   | kiocb->ki_pos / iattr->ia_size | write/read offset; 0 otherwise     |

`off` is captured for `FUNC_WRITE_ITER`/`FUNC_READ_ITER` (kiocb `ki_pos`)
and `FUNC_SETATTR` (iattr `ia_size` when `ATTR_SIZE` set). It feeds the DAG
`offsetBucketOf` semantics (same-position vs different-position concurrent
writes). The struct layout must stay in sync between
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

**Cross-round latency note**: writeback is scheduled with a delay, so a
completion event may arrive *after* the current round's
`stop_collect_hmdfs_trace()` — it will be drained (and attributed) in a
later round. Writepage vertices are built without call-window matching
(`dag.go`), so the impact of such mis-attribution is limited.

---

## Step 1: Export vmlinux.h

**When**: Once per kernel image (re-export only when the kernel is updated).

**Who**: Developer, either manually or via the Makefile `vmlinux.h` target.

### Method A — Manual (Recommended)

Run inside the VM:
```bash
bpftool btf dump file /sys/kernel/btf/vmlinux format c > /tmp/vmlinux.h
```

Copy out to the build host:
```bash
scp <vm>:/tmp/vmlinux.h tools/vmlinux.h
```

### Method B — Automated via SSH

Add to `src/Makefile` (the existing `VMLINUX_H`/`HMDFS_BPF_OBJ`/`HMDFS_BPF_SKEL`
targets in `src/Makefile` already automate this):
```makefile
$(HMDFS_BPF_SKEL): $(HMDFS_BPF_OBJ)
	bpftool gen skeleton $< > $@
```

---

## Step 2: Build the BPF Object

```bash
# Compile BPF program to ELF object
clang -g -O2 -target bpf -D__TARGET_ARCH_x86 \
      -I tools \
      -c src/executor/hmdfs_trace.bpf.c \
      -o src/executor/hmdfs_trace.bpf.o

# Generate libbpf skeleton header (C code for executor)
bpftool gen skeleton src/executor/hmdfs_trace.bpf.o \
      > src/executor/hmdfs_trace.skel.h
```

The skeleton header provides type-safe C functions to load, attach, and
manage the BPF program from the executor. The main `src/Makefile` `executor`
target builds `hmdfs_trace.skel.h` automatically (requires `tools/vmlinux.h`).

---

## Step 3: Executor Integration

### `hmdfs_trace.cc` / `hmdfs_trace.h`

```c
void init_hmdfs_trace(void);       // load BPF skeleton + attach 15 kretprobes + open writepage perf event
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

Serialization (per round, after fsMd): `u32 count` + per event
`u64 ts, u32 fid, u32 ret, u64 ino, u64 args[6], u64 off` (80 B/event).

---

## Step 4: Fuzzer-Side Parsing

### `ipc.go` — parseHmdfsTraceEvents

```go
func parseHmdfsTraceEvents(data *[]byte) []prog.HmdfsTraceEvent {
    // count (u32) prefix, then per event:
    //   u64 ts, u32 fid, u32 ret, u64 ino, u64 args[6], u64 off
}
```

`prog.HmdfsTraceEvent` mirrors the 80-byte layout (Timestamp/FuncID/Ret/Ino/
Args[6]/Off) plus `ProgIdx` filled on the Go side. The parser must stay
aligned with `stop_collect_hmdfs_trace`'s write order — including `off`.

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
passed to the executor as `-TSCOFF` (argv[15]). The executor's
`tsc_ns_to_global()` converts event timestamps (and per-call window
timestamps) into one global raw-TSC domain. For happens-before ordering,
only the relative ordering within the same frequency domain matters.

---

## Performance Overhead

| Metric          | Estimate                     |
|-----------------|------------------------------|
| Per kretprobe   | ~300-500 ns (BPF handler + ring buffer submit) |
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
| `src/executor/hmdfs_trace.bpf.c` | BPF program — 15 kretprobe handlers (80 B events incl. `off`) |
| `tools/vmlinux.h` | Generated: kernel BTF type definitions (export once) |
| `src/executor/hmdfs_trace.skel.h` | Generated: libbpf skeleton for executor |
| `src/executor/hmdfs_trace.cc` / `.h` | Executor-side load/control/collect logic (BPF ring + perf writepage) |
| `src/executor/executor.cc` | Integration: init/start/stop call sites |
| `src/pkg/ipc/ipc.go` | Fuzzer-side: `parseHmdfsTraceEvents` (80 B layout) |
| `src/prog/prog.go` | `HmdfsTraceEvent` struct (incl. `Off`) |
| `src/prog/dag.go` | DAG timeline construction, writepage vertices, offset buckets |
| `src/executor/EBPF_TRACE.md` | This document |
