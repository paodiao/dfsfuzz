// SPDX-License-Identifier: GPL-2.0
/*
 * hmdfs_trace.bpf.c — eBPF HMDFS Merge View Function Tracer
 *
 * 15 kretprobes (one per merge-view VFS function) plus 15 paired entry
 * kprobes. (func_id 15/writepage is an async perf tracepoint handled in
 * hmdfs_trace.cc, not here.)
 *
 * All argument-derived state is captured at FUNCTION ENTRY. A kretprobe
 * pt_regs holds the registers the function body left behind at its return
 * (the kernel return trampoline saves the return-time state; see
 * arch/x86/kernel/rethook.c and kernel/kprobes.c), so PT_REGS_PARMx must
 * never be read there. The return handler uses only:
 *   - the return value (PT_REGS_RC — the one register the ABI makes
 *     meaningful at return),
 *   - the per-task {pid_tgid, func_id} entry-state map (position, size,
 *     inode, mode captured at entry),
 *   - for mkdir/create/rename/lookup, the dentry POINTER stored at entry
 *     and dereferenced at return: the operation's own entry inode is
 *     created (d_add) or spliced during the call, so it can only be read
 *     after the function has run.
 *
 * Events are written to a lock-free BPF ring buffer, consumed by the
 * executor after each test schedule completes.
 */

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

/* vmlinux.h carries types but not macros; ATTR_SIZE gates ia_size in setattr. */
#ifndef ATTR_SIZE
#define ATTR_SIZE (1 << 10)
#endif
/* O_APPEND from uapi/fcntl.h (not exported by vmlinux.h): an O_APPEND
 * write starts at EOF, not at the caller's file position. */
#ifndef HMDFS_O_APPEND
#define HMDFS_O_APPEND 0x400
#endif
/* TASK_COMM_LEN is a kernel macro that vmlinux.h may not export. */
#ifndef TASK_COMM_LEN
#define TASK_COMM_LEN 16
#endif

/* ── Event structure (112 bytes) ─────────────────────────────── */
struct merge_trace_event {
	u64 timestamp;    /* bpf_ktime_get_ns() — TSC nanoseconds  */
	u32 func_id;      /* operation type (0=MKDIR .. 14=ITERATE) */
	s32 ret;          /* return value: 0=OK, <0=-errno, >0=fd   */
	u64 ino;          /* target inode captured at ENTRY; for
			     LOOKUP it is the parent's inode, for
			     RENAME the old parent's inode           */
	u64 args[6];      /* reserved; always 0 (argument registers
			     are not valid at kretprobe time)        */
	u64 off;          /* ENTRY position: write/read entry ki_pos
			     (O_APPEND resolved to entry i_size);
			     truncate: ia_size; else 0               */
	u64 size;         /* ENTRY-time file size for write/read/
			     truncate (entry kprobe); else 0         */
	u64 entry_ino;    /* the operation's own entry inode, read at
			     return through the dentry pointer stored
			     at entry: mkdir/create dentry, rename
			     new_dentry, lookup child; else 0        */
	u32 i_mode;       /* entry inode i_mode (raw; the fuzzer masks
			     S_IFMT), 0 when unknown (negative lookup
			     or dropped dentry); the four entry-dentry
			     functions read it at return              */
	u64 entry_ts;     /* bpf_ktime_get_ns() at entry: this event's own
			     execution window start (return timestamp is
			     the window end); 0 when the entry state was
			     lost                                     */
};

/* ── Ring buffer (256 KB) ───────────────────────────────────── */
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

/* ── Entry-state map (entry kprobe → return kretprobe pairing) ─ */
/*
 * Paired by (pid_tgid, func_id): the entry kprobe stores the
 * operation's START state, the return handler consumes and deletes
 * it. func_id is a compile-time constant in both probes, so the
 * pairing never depends on registers read at return. hmdfs merge
 * functions do not recurse into themselves, so one slot per
 * task+function is enough; a missed return leaves an entry that the
 * next update for the same key overwrites.
 */
struct entry_key {
	u64 pid;          /* bpf_get_current_pid_tgid()              */
	u64 fid;          /* func_id constant of the paired probes   */
};

struct entry_val {
	u64 ino;          /* target inode captured at entry          */
	u64 mode;         /* entry i_mode; u64 so the struct has no
			     padding — the verifier requires every
			     byte of a map-update value initialized  */
	u64 off;          /* entry position / ia_size                */
	u64 size;         /* entry-time file size                    */
	u64 ptr;          /* dentry* for mkdir/create/rename/lookup,
			     dereferenced at return; else 0           */
	u64 ts;           /* bpf_ktime_get_ns() at entry             */
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8192);
	__type(key, struct entry_key);
	__type(value, struct entry_val);
} entry_state SEC(".maps");

/* ── Funciton ID enum (must match ipc.go MergeFuncNames map) ── */
#define FUNC_MKDIR_MERGE     0
#define FUNC_CREATE_MERGE    1
#define FUNC_RMDIR_MERGE     2
#define FUNC_UNLINK_MERGE    3
#define FUNC_RENAME_MERGE    4
#define FUNC_WRITE_ITER      5
#define FUNC_READ_ITER       6
#define FUNC_FILE_OPEN       7
#define FUNC_DIR_OPEN        8
#define FUNC_GETATTR         9
#define FUNC_SETATTR        10
#define FUNC_FSYNC          11
#define FUNC_RELEASE        12
#define FUNC_LOOKUP         13
#define FUNC_ITERATE        14
#define FUNC_WRITEPAGE_CB   15

/* ── Shared comm filter ─────────────────────────────────────── */
/*
 * Only the executor's own hmdfs activity is meaningful: other
 * processes in the VM (agent, daemons, mount-time traversal) would
 * otherwise flood the event stream with window-less background reads
 * (e.g. ~3000 events before the program starts on the persistence
 * node). comm is "syz-executor" (executed by its plain binary name).
 */
static __always_inline int hmdfs_trace_comm_ok(void)
{
	char comm[TASK_COMM_LEN];

	bpf_get_current_comm(&comm, sizeof(comm));
	if (comm[0] != 's' || comm[1] != 'y' || comm[2] != 'z' ||
	    comm[3] != '-' || comm[4] != 'e' || comm[5] != 'x' ||
	    comm[6] != 'e' || comm[7] != 'c' || comm[8] != 'u' ||
	    comm[9] != 't' || comm[10] != 'o' || comm[11] != 'r')
		return 0;
	return 1;
}

/* ── Handler macros ─────────────────────────────────────────── */
/*
 * Entry kprobe: capture the operation's START state into the map.
 *
 * name       — function name (string, for attach)
 * fid_val    — func_id constant (also the pairing key)
 * ino_expr   — target inode (u64) read at entry
 * mode_expr  — entry i_mode (u32); 0 when the entry only comes into
 *              existence during the call (mkdir/create/rename/lookup)
 * off_expr   — entry position for write/read, ia_size for truncate, else 0
 * size_expr  — entry-time file size for write/read/setattr, else 0
 * ptr_expr   — dentry pointer (u64) to dereference at return, else 0
 *
 * The handler body is the SAME for all 15 functions; only the
 * extraction expressions differ.
 */
#define DEFINE_MERGE_ENTRY_KPROBE(name, fid_val, ino_expr, mode_expr, off_expr, size_expr, ptr_expr) \
SEC("kprobe/hmdfs_" #name)                                                 \
int BPF_KPROBE(hmdfs_##name##_entry)                                       \
{                                                                          \
	if (!hmdfs_trace_comm_ok())                                        \
		return 0;                                                  \
	struct entry_key k = {.pid = bpf_get_current_pid_tgid(), .fid = (fid_val)}; \
	struct entry_val v = {                                             \
		.ino  = (ino_expr),                                        \
		.mode = (mode_expr),                                       \
		.off  = (off_expr),                                        \
		.size = (size_expr),                                       \
		.ptr  = (ptr_expr),                                        \
	};                                                                 \
	v.ts = bpf_ktime_get_ns(); \
	bpf_map_update_elem(&entry_state, &k, &v, BPF_ANY);                \
	return 0;                                                          \
}

/* Emit one event from a k(ret)probe pair: fill it from the entry-state map and
 * the (already normalized) return value. Shared by the two wrappers below; it
 * does not touch pt_regs, so it is valid for both int- and pointer-returning
 * functions. */
static __always_inline int merge_trace_emit(u32 fid, int ret)
{
	struct merge_trace_event *e;
	e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;
	e->timestamp = bpf_ktime_get_ns();
	e->func_id   = fid;
	e->ret       = ret;
	e->ino       = 0;
	e->args[0]   = 0;
	e->args[1]   = 0;
	e->args[2]   = 0;
	e->args[3]   = 0;
	e->args[4]   = 0;
	e->args[5]   = 0;
	e->off       = 0;
	e->size      = 0;
	e->entry_ino = 0;
	e->i_mode    = 0;
	e->entry_ts  = 0;
	struct entry_key k = {.pid = bpf_get_current_pid_tgid(), .fid = fid};
	struct entry_val *v = bpf_map_lookup_elem(&entry_state, &k);
	if (v) {
		e->ino       = v->ino;
		e->off       = v->off;
		e->size      = v->size;
		e->i_mode    = v->mode;
		e->entry_ts  = v->ts;
		/* The map value's ptr must be loaded into a local scalar first:
		 * a member access on the local struct entry_val inside a
		 * BPF_CORE_READ address expression is an unresolvable CO-RE
		 * relocation. */
		u64 dptr = v->ptr;
		if (dptr) {
			struct inode *in = BPF_CORE_READ((struct dentry *)dptr, d_inode);
			if (in) {
				e->entry_ino = BPF_CORE_READ(in, i_ino);
				e->i_mode    = BPF_CORE_READ(in, i_mode);
			}
		}
		bpf_map_delete_elem(&entry_state, &k);
	}
	bpf_ringbuf_submit(e, 0);
	return 0;
}
/*
 * Return kretprobe: fill the event from the entry-state map and the
 * return value. No PT_REGS_PARMx access (see the file header). For the
 * four entry-dentry functions the entry inode is read here through the
 * pointer stored at entry — d_add()/d_splice_alias() has run by now.
 */
#define DEFINE_MERGE_KRETPROBE(name, fid_val)                                  \
SEC("kretprobe/hmdfs_" #name)                                              \
int BPF_KRETPROBE(hmdfs_##name##_exit, int ret)                            \
{                                                                          \
	if (!hmdfs_trace_comm_ok())                                        \
		return 0;                                                  \
	return merge_trace_emit((fid_val), ret);                           \
}

/* Pointer-returning wrapper (lookup): BPF_KRETPROBE hands us the raw return
 * register, so classify it with IS_ERR -- a dentry alias means success (0) and
 * an ERR_PTR carries the errno; a plain int cast would keep only the low 32
 * bits of the pointer. */
#define DEFINE_MERGE_KRETPROBE_PTR(name, fid_val)                              \
SEC("kretprobe/hmdfs_" #name)                                              \
int BPF_KRETPROBE(hmdfs_##name##_exit, unsigned long fret)                 \
{                                                                          \
	if (!hmdfs_trace_comm_ok())                                        \
		return 0;                                                  \
	int ret = (fret >= (unsigned long)-4095) ? (int)(long)fret : 0;    \
	return merge_trace_emit((fid_val), ret);                           \
}

/*
 * Function signatures (x86_64 calling convention):
 *
 *   hmdfs_mkdir_merge(idmap*, dir*, dentry*, umode_t)
 *   hmdfs_create_merge(idmap*, dir*, dentry*, umode_t, bool)
 *   hmdfs_rmdir_merge(dir*, dentry*)          ino: dentry->d_inode
 *   hmdfs_unlink_merge(dir*, dentry*)         ino: dentry->d_inode
 *   hmdfs_rename_merge(idmap*, old_dir*, old_dentry*, new_dir*, new_dentry*, flags)
 *   hmdfs_merge_write_iter(kiocb*, iter*)
 *   hmdfs_merge_read_iter(kiocb*, iter*)
 *   hmdfs_file_open_merge(inode*, file*)
 *   hmdfs_dir_open_merge(inode*, file*)
 *   hmdfs_getattr_merge(idmap*, path*, kstat*, u32, u32)
 *   hmdfs_setattr_merge(idmap*, dentry*, iattr*)
 *   hmdfs_fsync_local(file*, loff_t, loff_t, int)
 *   hmdfs_file_release_local(inode*, file*)
 *   hmdfs_lookup_merge(dir*, dentry*, u32)
 *   hmdfs_iterate_merge(file*, dir_context*)
 */

/* ── 15 entry kprobes + 15 return kretprobes ────────────────── */

/* Mkdir / Create — target inode from the parent directory; the entry
 * dentry is stored so the new inode (d_add inside the call) can be read
 * at return. */
DEFINE_MERGE_ENTRY_KPROBE(mkdir_merge, FUNC_MKDIR_MERGE,
	BPF_CORE_READ((struct inode *)PT_REGS_PARM2(ctx), i_ino),
	0, 0, 0,
	(u64)(struct dentry *)PT_REGS_PARM3(ctx))
DEFINE_MERGE_KRETPROBE(mkdir_merge, FUNC_MKDIR_MERGE)

DEFINE_MERGE_ENTRY_KPROBE(create_merge, FUNC_CREATE_MERGE,
	BPF_CORE_READ((struct inode *)PT_REGS_PARM2(ctx), i_ino),
	0, 0, 0,
	(u64)(struct dentry *)PT_REGS_PARM3(ctx))
DEFINE_MERGE_KRETPROBE(create_merge, FUNC_CREATE_MERGE)

/* Rmdir / Unlink — the removed entry exists at entry. */
DEFINE_MERGE_ENTRY_KPROBE(rmdir_merge, FUNC_RMDIR_MERGE,
	BPF_CORE_READ((struct dentry *)PT_REGS_PARM2(ctx), d_inode, i_ino),
	BPF_CORE_READ((struct dentry *)PT_REGS_PARM2(ctx), d_inode, i_mode),
	0, 0, 0)
DEFINE_MERGE_KRETPROBE(rmdir_merge, FUNC_RMDIR_MERGE)

DEFINE_MERGE_ENTRY_KPROBE(unlink_merge, FUNC_UNLINK_MERGE,
	BPF_CORE_READ((struct dentry *)PT_REGS_PARM2(ctx), d_inode, i_ino),
	BPF_CORE_READ((struct dentry *)PT_REGS_PARM2(ctx), d_inode, i_mode),
	0, 0, 0)
DEFINE_MERGE_KRETPROBE(unlink_merge, FUNC_UNLINK_MERGE)

/* Rename: e->ino is the old parent (existing semantics); the OLD dentry
 * is stored so EntryIno is the moved SOURCE inode (d_move runs after this
 * kretprobe, so new_dentry would still be the replaced destination). The
 * entry i_mode is the SOURCE inode's type: a rename preserves the type, and
 * for a fresh destination the upper new_dentry has no inode until VFS's
 * post-rename d_move, so the source is the only event-time type available. */
DEFINE_MERGE_ENTRY_KPROBE(rename_merge, FUNC_RENAME_MERGE,
	BPF_CORE_READ((struct inode *)PT_REGS_PARM2(ctx), i_ino),
	BPF_CORE_READ((struct dentry *)PT_REGS_PARM3(ctx), d_inode, i_mode),
	0, 0,
	(u64)(struct dentry *)PT_REGS_PARM3(ctx))
DEFINE_MERGE_KRETPROBE(rename_merge, FUNC_RENAME_MERGE)

/* Write — start position/size at entry; O_APPEND resolves to entry EOF. */
DEFINE_MERGE_ENTRY_KPROBE(merge_write_iter, FUNC_WRITE_ITER,
	BPF_CORE_READ((struct kiocb *)PT_REGS_PARM1(ctx), ki_filp, f_inode, i_ino),
	BPF_CORE_READ((struct kiocb *)PT_REGS_PARM1(ctx), ki_filp, f_inode, i_mode),
	({ struct kiocb *io = (struct kiocb *)PT_REGS_PARM1(ctx);
	   u64 pos = BPF_CORE_READ(io, ki_pos);
	   u64 sz = BPF_CORE_READ(io, ki_filp, f_inode, i_size);
	   if (BPF_CORE_READ(io, ki_filp, f_flags) & HMDFS_O_APPEND)
		   pos = sz;
	   pos; }),
	BPF_CORE_READ((struct kiocb *)PT_REGS_PARM1(ctx), ki_filp, f_inode, i_size),
	0)
DEFINE_MERGE_KRETPROBE(merge_write_iter, FUNC_WRITE_ITER)

/* Read — entry position/size, no O_APPEND rewrite. */
DEFINE_MERGE_ENTRY_KPROBE(merge_read_iter, FUNC_READ_ITER,
	BPF_CORE_READ((struct kiocb *)PT_REGS_PARM1(ctx), ki_filp, f_inode, i_ino),
	BPF_CORE_READ((struct kiocb *)PT_REGS_PARM1(ctx), ki_filp, f_inode, i_mode),
	BPF_CORE_READ((struct kiocb *)PT_REGS_PARM1(ctx), ki_pos),
	BPF_CORE_READ((struct kiocb *)PT_REGS_PARM1(ctx), ki_filp, f_inode, i_size),
	0)
DEFINE_MERGE_KRETPROBE(merge_read_iter, FUNC_READ_ITER)

/* Open — inode exists at entry. */
DEFINE_MERGE_ENTRY_KPROBE(file_open_merge, FUNC_FILE_OPEN,
	BPF_CORE_READ((struct inode *)PT_REGS_PARM1(ctx), i_ino),
	BPF_CORE_READ((struct inode *)PT_REGS_PARM1(ctx), i_mode),
	0, 0, 0)
DEFINE_MERGE_KRETPROBE(file_open_merge, FUNC_FILE_OPEN)

DEFINE_MERGE_ENTRY_KPROBE(dir_open_merge, FUNC_DIR_OPEN,
	BPF_CORE_READ((struct inode *)PT_REGS_PARM1(ctx), i_ino),
	BPF_CORE_READ((struct inode *)PT_REGS_PARM1(ctx), i_mode),
	0, 0, 0)
DEFINE_MERGE_KRETPROBE(dir_open_merge, FUNC_DIR_OPEN)

/* Getattr — inode from path->dentry. */
DEFINE_MERGE_ENTRY_KPROBE(getattr_merge, FUNC_GETATTR,
	BPF_CORE_READ((struct path *)PT_REGS_PARM2(ctx), dentry, d_inode, i_ino),
	BPF_CORE_READ((struct path *)PT_REGS_PARM2(ctx), dentry, d_inode, i_mode),
	0, 0, 0)
DEFINE_MERGE_KRETPROBE(getattr_merge, FUNC_GETATTR)

/* Setattr — truncate ia_size and entry i_size captured at entry. */
DEFINE_MERGE_ENTRY_KPROBE(setattr_merge, FUNC_SETATTR,
	BPF_CORE_READ((struct dentry *)PT_REGS_PARM2(ctx), d_inode, i_ino),
	BPF_CORE_READ((struct dentry *)PT_REGS_PARM2(ctx), d_inode, i_mode),
	({ struct iattr *ia = (struct iattr *)PT_REGS_PARM3(ctx);
	   u64 off = 0;
	   if (BPF_CORE_READ(ia, ia_valid) & ATTR_SIZE)
		   off = BPF_CORE_READ(ia, ia_size);
	   off; }),
	BPF_CORE_READ((struct dentry *)PT_REGS_PARM2(ctx), d_inode, i_size),
	0)
DEFINE_MERGE_KRETPROBE(setattr_merge, FUNC_SETATTR)

/* Fsync — inode from the file argument. */
DEFINE_MERGE_ENTRY_KPROBE(fsync_local, FUNC_FSYNC,
	BPF_CORE_READ((struct file *)PT_REGS_PARM1(ctx), f_inode, i_ino),
	BPF_CORE_READ((struct file *)PT_REGS_PARM1(ctx), f_inode, i_mode),
	0, 0, 0)
DEFINE_MERGE_KRETPROBE(fsync_local, FUNC_FSYNC)

/* Release — inode argument. */
DEFINE_MERGE_ENTRY_KPROBE(file_release_local, FUNC_RELEASE,
	BPF_CORE_READ((struct inode *)PT_REGS_PARM1(ctx), i_ino),
	BPF_CORE_READ((struct inode *)PT_REGS_PARM1(ctx), i_mode),
	0, 0, 0)
DEFINE_MERGE_KRETPROBE(file_release_local, FUNC_RELEASE)

/* Lookup: parent inode at entry; child dentry stored for the post-lookup
 * entry inode (0 for negative lookups). The return value is a dentry pointer,
 * normalized in DEFINE_MERGE_KRETPROBE_PTR. */
DEFINE_MERGE_ENTRY_KPROBE(lookup_merge, FUNC_LOOKUP,
	BPF_CORE_READ((struct inode *)PT_REGS_PARM1(ctx), i_ino),
	0, 0, 0,
	(u64)(struct dentry *)PT_REGS_PARM2(ctx))
DEFINE_MERGE_KRETPROBE_PTR(lookup_merge, FUNC_LOOKUP)

/* Iterate (readdir) — inode from the file argument. */
DEFINE_MERGE_ENTRY_KPROBE(iterate_merge, FUNC_ITERATE,
	BPF_CORE_READ((struct file *)PT_REGS_PARM1(ctx), f_inode, i_ino),
	BPF_CORE_READ((struct file *)PT_REGS_PARM1(ctx), f_inode, i_mode),
	0, 0, 0)
DEFINE_MERGE_KRETPROBE(iterate_merge, FUNC_ITERATE)

/* ── Licence (required by BPF verifier) ─────────────────────── */
char LICENSE[] SEC("license") = "GPL";
