// SPDX-License-Identifier: GPL-2.0
/*
 * hmdfs_trace.bpf.c — eBPF HMDFS Merge View Function Tracer
 *
 * Attaches kretprobes to 15 merge-view VFS functions in hmdfs. On each
 * function return, captures: timestamp, func_id, return value, target
 * inode, and all argument registers.
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

/* bpf_tracing.h defines PT_REGS_PARM1..5 only; x86_64's 6th arg is r9. */
#ifndef PT_REGS_PARM6
#define PT_REGS_PARM6(x) ((__u64)(x)->r9)
#endif

/* ── Event structure (80 bytes) ──────────────────────────────── */
struct merge_trace_event {
	u64 timestamp;    /* bpf_ktime_get_ns() — TSC nanoseconds  */
	u32 func_id;      /* operation type (0=MKDIR .. 14=ITERATE) */
	s32 ret;          /* return value: 0=OK, <0=-errno, >0=fd   */
	u64 ino;          /* target inode number (from BPF_CORE_READ)*/
	u64 args[6];      /* rdi, rsi, rdx, rcx, r8, r9              */
	u64 off;          /* write/read: kiocb->ki_pos; truncate:
			     iattr->ia_size (only when ATTR_SIZE set);
			     0 otherwise (no offset semantics)        */
};

/* ── Ring buffer (256 KB) ───────────────────────────────────── */
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

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

/* ── Common handler macro ───────────────────────────────────── */
/*
 * name       — function name (string, for kretprobe attach)
 * fid        — func_id constant
 * ino_expr   — C expression yielding the target inode (u64)
 *               executed inside the handler body; all parameter
 *               pointers declared as local variables below.
 *
 * The handler body is the SAME for all 15 functions. Only the
 * inode extraction expression differs.
 */
#define DEFINE_MERGE_KRETPROBE(name, fid, ino_expr)                \
SEC("kretprobe/hmdfs_" #name)                                      \
int BPF_KRETPROBE(hmdfs_##name##_exit, int ret)                    \
{                                                                   \
	struct merge_trace_event *e;                                 \
	e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);             \
	if (!e)                                                      \
		return 0;                                            \
	e->timestamp = bpf_ktime_get_ns();                           \
	e->func_id   = (fid);                                        \
	e->ret       = ret;                                          \
	e->args[0]   = PT_REGS_PARM1(ctx);                           \
	e->args[1]   = PT_REGS_PARM2(ctx);                           \
	e->args[2]   = PT_REGS_PARM3(ctx);                           \
	e->args[3]   = PT_REGS_PARM4(ctx);                           \
	e->args[4]   = PT_REGS_PARM5(ctx);                           \
	e->args[5]   = PT_REGS_PARM6(ctx);                           \
	e->ino       = (ino_expr);                                    \
	if (fid == FUNC_WRITE_ITER || fid == FUNC_READ_ITER) {       \
		struct kiocb *iocb = (struct kiocb *)PT_REGS_PARM1(ctx); \
		e->off = BPF_CORE_READ(iocb, ki_pos);                \
	} else if (fid == FUNC_SETATTR) {                            \
		struct iattr *attr = (struct iattr *)PT_REGS_PARM3(ctx);\
		if (BPF_CORE_READ(attr, ia_valid) & ATTR_SIZE)         \
			e->off = BPF_CORE_READ(attr, ia_size);        \
		else                                                   \
			e->off = 0;                                    \
	} else {                                                     \
		e->off = 0;                                          \
	}                                                            \
	bpf_ringbuf_submit(e, 0);                                    \
	return 0;                                                    \
}

/*
 * Inode extraction helper — declared once and reused.
 *
 * Each handler declares the relevant pointer(s) from PT_REGS_PARMx,
 * then uses BPF_CORE_READ to follow the pointer chain to i_ino.
 *
 * Function signatures (x86_64 calling convention):
 *
 *   hmdfs_mkdir_merge(idmap*, dir*, dentry*, umode_t)
 *   hmdfs_create_merge(idmap*, dir*, dentry*, umode_t, bool)
 *   hmdfs_rmdir_merge(dir*, dentry*)          ino: dentry->d_inode
 *   hmdfs_unlink_merge(dir*, dentry*)         ino: dentry->d_inode
 *   hmdfs_rename_merge(old_dir*, old_dentry*, new_dir*, new_dentry*, flags)
 *   hmdfs_merge_write_iter(kio*, iter*)
 *   hmdfs_merge_read_iter(kio*, iter*)
 *   hmdfs_file_open_merge(inode*, file*)
 *   hmdfs_dir_open_merge(inode*, file*)
 *   hmdfs_getattr_merge(idmap*, path*, kstat*, u32, u32)
 *   hmdfs_setattr_merge(idmap*, dentry*, iattr*)
 *   hmdfs_fsync_local(file*, loff_t, loff_t, int)
 *   hmdfs_file_release_local(inode*, file*)
 *   hmdfs_lookup_merge(dir*, dentry*, u32)
 *   hmdfs_iterate_merge(file*, dir_context*)
 */

/* ── 15 handler definitions ─────────────────────────────────── */

/*
 * Mkdir / Create — inode from the parent directory.
 * PTR2 = dir* (parent directory inode)
 */
DEFINE_MERGE_KRETPROBE(mkdir_merge, FUNC_MKDIR_MERGE, ({
	struct inode *d = (struct inode *)PT_REGS_PARM2(ctx);
	BPF_CORE_READ(d, i_ino);
}));

DEFINE_MERGE_KRETPROBE(create_merge, FUNC_CREATE_MERGE, ({
	struct inode *d = (struct inode *)PT_REGS_PARM2(ctx);
	BPF_CORE_READ(d, i_ino);
}));

/*
 * Rmdir / Unlink — inode from the removed entry itself.
 * PTR2 = dentry*  →  d_inode  →  i_ino
 */
DEFINE_MERGE_KRETPROBE(rmdir_merge, FUNC_RMDIR_MERGE, ({
	struct dentry *de = (struct dentry *)PT_REGS_PARM2(ctx);
	BPF_CORE_READ(de, d_inode, i_ino);
}));

DEFINE_MERGE_KRETPROBE(unlink_merge, FUNC_UNLINK_MERGE, ({
	struct dentry *de = (struct dentry *)PT_REGS_PARM2(ctx);
	BPF_CORE_READ(de, d_inode, i_ino);
}));

/*
 * Rename — inode from the old parent directory (idmap occupies PARM1,
 * so PARM2 is old_dir* — the parent inode, not the renamed entry).
 */
DEFINE_MERGE_KRETPROBE(rename_merge, FUNC_RENAME_MERGE, ({
	struct inode *d = (struct inode *)PT_REGS_PARM2(ctx);
	BPF_CORE_READ(d, i_ino);
}));

/*
 * Write / Read — inode from the kiocb's file.
 * PTR1 = kiocb*  →  kiocb->ki_filp  →  file->f_inode
 */
DEFINE_MERGE_KRETPROBE(merge_write_iter, FUNC_WRITE_ITER, ({
	struct kiocb *iocb = (struct kiocb *)PT_REGS_PARM1(ctx);
	BPF_CORE_READ(iocb, ki_filp, f_inode, i_ino);
}));

DEFINE_MERGE_KRETPROBE(merge_read_iter, FUNC_READ_ITER, ({
	struct kiocb *iocb = (struct kiocb *)PT_REGS_PARM1(ctx);
	BPF_CORE_READ(iocb, ki_filp, f_inode, i_ino);
}));

/*
 * Open — inode from the inode argument.
 * PTR1 = inode*
 */
DEFINE_MERGE_KRETPROBE(file_open_merge, FUNC_FILE_OPEN, ({
	struct inode *i = (struct inode *)PT_REGS_PARM1(ctx);
	BPF_CORE_READ(i, i_ino);
}));

DEFINE_MERGE_KRETPROBE(dir_open_merge, FUNC_DIR_OPEN, ({
	struct inode *i = (struct inode *)PT_REGS_PARM1(ctx);
	BPF_CORE_READ(i, i_ino);
}));

/*
 * Getattr / Setattr — inode from the path's dentry / dentry itself.
 *
 * hmdfs_getattr_merge: PTR2 = path*  →  path->dentry  →  d_inode
 * hmdfs_setattr_merge: PTR2 = dentry*  →  d_inode
 */
DEFINE_MERGE_KRETPROBE(getattr_merge, FUNC_GETATTR, ({
	struct path *p = (struct path *)PT_REGS_PARM2(ctx);
	BPF_CORE_READ(p, dentry, d_inode, i_ino);
}));

DEFINE_MERGE_KRETPROBE(setattr_merge, FUNC_SETATTR, ({
	struct dentry *de = (struct dentry *)PT_REGS_PARM2(ctx);
	BPF_CORE_READ(de, d_inode, i_ino);
}));

/*
 * Fsync — inode from the file argument.
 * PTR1 = file*  →  file->f_inode
 */
DEFINE_MERGE_KRETPROBE(fsync_local, FUNC_FSYNC, ({
	struct file *f = (struct file *)PT_REGS_PARM1(ctx);
	BPF_CORE_READ(f, f_inode, i_ino);
}));

/*
 * Release (close) — inode from the inode argument.
 * PTR1 = inode*
 */
DEFINE_MERGE_KRETPROBE(file_release_local, FUNC_RELEASE, ({
	struct inode *i = (struct inode *)PT_REGS_PARM1(ctx);
	BPF_CORE_READ(i, i_ino);
}));

/*
 * Lookup — inode from the parent directory.
 * PTR1 = dir*
 */
DEFINE_MERGE_KRETPROBE(lookup_merge, FUNC_LOOKUP, ({
	struct inode *d = (struct inode *)PT_REGS_PARM1(ctx);
	BPF_CORE_READ(d, i_ino);
}));

/*
 * Iterate (readdir) — inode from the file argument.
 * PTR1 = file*  →  file->f_inode
 */
DEFINE_MERGE_KRETPROBE(iterate_merge, FUNC_ITERATE, ({
	struct file *f = (struct file *)PT_REGS_PARM1(ctx);
	BPF_CORE_READ(f, f_inode, i_ino);
}));

/* ── Licence (required by BPF verifier) ─────────────────────── */
char LICENSE[] SEC("license") = "GPL";
