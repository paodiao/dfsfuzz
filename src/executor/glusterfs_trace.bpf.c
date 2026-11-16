// SPDX-License-Identifier: GPL-2.0
/*
 * glusterfs_trace.bpf.c — GlusterFS FUSE uprobe tracer.
 *
 * Three functions in the GlusterFS client's fuse.so are attached by offset
 * from userspace (bpf_program__attach_uprobe):
 *
 *   fuse_dispatch    entry       -> D (fuse_in_header only)
 *   fuse_fop_resume  entry       -> R (path/path2/xattr/off/size/mode)
 *   send_fuse_iov    entry       -> save finh/iov_out/error
 *   send_fuse_iov    uretprobe   -> S (timestamp only from the return)
 *
 * ctl ring carries D/S; path ring carries R.  The two rings are independent,
 * so userspace merges both by timestamp before building final events.
 */

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

#include "glusterfs_trace.h"

#define GF_S_IFREG 0x8000
#define GF_S_IFDIR 0x4000
#define GF_S_IFLNK 0xA000

#define GF_PHASE_D 0
#define GF_PHASE_S 1

#define GF_TRUNC_PATH  1
#define GF_TRUNC_PATH2 2
#define GF_TRUNC_XATTR 4

/* One request can be in flight per task; the map value is saved at the
 * send_fuse_iov entry and consumed by its uretprobe. */
struct gf_send_key {
	u64 pid_tgid;
};

struct gf_send_val {
	u64 unique;
	u64 nodeid;
	u32 opcode;
	u32 pid;
	s32 error;
	u32 pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 * 1024 * 1024);
} gf_ctl_events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 4 * 1024 * 1024);
} gf_path_events SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8192);
	__type(key, struct gf_send_key);
	__type(value, struct gf_send_val);
} gf_send_state SEC(".maps");

static __always_inline int gf_opcode_allowed(u32 opcode)
{
	switch (opcode) {
	case GF_OP_LOOKUP:
	case GF_OP_GETATTR:
	case GF_OP_SETATTR:
	case GF_OP_READLINK:
	case GF_OP_SYMLINK:
	case GF_OP_MKDIR:
	case GF_OP_UNLINK:
	case GF_OP_RMDIR:
	case GF_OP_RENAME:
	case GF_OP_LINK:
	case GF_OP_OPEN:
	case GF_OP_READ:
	case GF_OP_WRITE:
	case GF_OP_RELEASE:
	case GF_OP_FSYNC:
	case GF_OP_SETXATTR:
	case GF_OP_GETXATTR:
	case GF_OP_OPENDIR:
	case GF_OP_READDIR:
	case GF_OP_CREATE:
	case GF_OP_READDIRPLUS:
		return 1;
	}
	return 0;
}

static __always_inline int gf_read_finh(u64 finh, u32 *opcode, u64 *unique,
					u64 *nodeid, u32 *pid)
{
	u64 uq = 0;
	u64 nid = 0;
	u32 op = 0;
	u32 p = 0;

	if (!finh)
		return -1;
	if (bpf_probe_read_user(&op, sizeof(op), (void *)(finh + 4)))
		return -1;
	if (bpf_probe_read_user(&uq, sizeof(uq), (void *)(finh + 8)))
		return -1;
	if (bpf_probe_read_user(&nid, sizeof(nid), (void *)(finh + 16)))
		return -1;
	if (bpf_probe_read_user(&p, sizeof(p), (void *)(finh + 32)))
		return -1;

	*opcode = op;
	*unique = uq;
	*nodeid = nid;
	*pid = p;
	return 0;
}

static __always_inline int gf_user_xattr(u64 name_ptr)
{
	char prefix[5];

	if (!name_ptr)
		return 0;
	if (bpf_probe_read_user(prefix, sizeof(prefix), (void *)name_ptr))
		return 0;
	return prefix[0] == 'u' && prefix[1] == 's' && prefix[2] == 'e' &&
	       prefix[3] == 'r' && prefix[4] == '.';
}

static __always_inline u32 gf_mode_from_ia(u32 ia_type)
{
	switch (ia_type) {
	case 1:
		return GF_S_IFREG;
	case 2:
		return GF_S_IFDIR;
	case 3:
		return GF_S_IFLNK;
	}
	return 0;
}

static __always_inline u32 gf_copy_user_str(char *dst, u32 size, u64 src,
					    u32 *truncated, u32 bit)
{
	int n;

	if (!src)
		return 0;
	n = bpf_probe_read_user_str(dst, size, (void *)src);
	if (n <= 0)
		return 0;
	if ((u32)n >= size) {
		*truncated |= bit;
		return size - 1;
	}
	return (u32)n - 1;
}

/* D: fuse_dispatch(void *async_ptr) where async_ptr = &fasync->async.
 * fasync = async_ptr - 32, fasync->finh = fasync + 8 = async_ptr - 24. */
SEC("uprobe")
int gf_dispatch(struct pt_regs *ctx)
{
	u64 async = (u64)PT_REGS_PARM1(ctx);
	u64 finh = 0;
	u32 opcode = 0;
	u32 pid = 0;
	u64 unique = 0;
	u64 nodeid = 0;
	struct glusterfs_ctl_event *e;

	if (!async)
		return 0;
	if (bpf_probe_read_user(&finh, sizeof(finh), (void *)(async - 24)))
		return 0;
	if (gf_read_finh(finh, &opcode, &unique, &nodeid, &pid))
		return 0;
	if (!pid || !gf_opcode_allowed(opcode))
		return 0;

	e = bpf_ringbuf_reserve(&gf_ctl_events, sizeof(*e), 0);
	if (!e)
		return 0;
	e->ts = bpf_ktime_get_ns();
	e->unique = unique;
	e->phase = GF_PHASE_D;
	e->opcode = opcode;
	e->nodeid = nodeid;
	e->pid = pid;
	e->error = 0;
	bpf_ringbuf_submit(e, 0);
	return 0;
}

/* R: fuse_fop_resume(fuse_state_t *state). */
SEC("uprobe")
int gf_resume(struct pt_regs *ctx)
{
	u64 state = (u64)PT_REGS_PARM1(ctx);
	u64 finh = 0;
	u64 loc_path = 0, loc2_path = 0, name = 0;
	u64 loc_ino = 0, loc2_ino = 0;
	u64 off = 0, size = 0;
	u64 inode = 0;
	u32 opcode = 0, pid = 0, ia_type = 0, mode = 0;
	u32 truncated = 0;
	u64 unique = 0, nodeid = 0;
	u64 path_src = 0, path2_src = 0;
	struct glusterfs_path_event *e;

	if (!state)
		return 0;
	if (bpf_probe_read_user(&finh, sizeof(finh), (void *)(state + 160)))
		return 0;
	if (gf_read_finh(finh, &opcode, &unique, &nodeid, &pid))
		return 0;
	if (!pid || !gf_opcode_allowed(opcode))
		return 0;

	bpf_probe_read_user(&loc_path, sizeof(loc_path), (void *)(state + 32));
	bpf_probe_read_user(&loc_ino, sizeof(loc_ino), (void *)(state + 48));
	bpf_probe_read_user(&loc2_path, sizeof(loc2_path), (void *)(state + 96));
	bpf_probe_read_user(&loc2_ino, sizeof(loc2_ino), (void *)(state + 112));
	bpf_probe_read_user(&off, sizeof(off), (void *)(state + 176));
	bpf_probe_read_user(&size, sizeof(size), (void *)(state + 200));
	bpf_probe_read_user(&name, sizeof(name), (void *)(state + 248));

	if (opcode == GF_OP_SETXATTR || opcode == GF_OP_GETXATTR) {
		if (!gf_user_xattr(name))
			return 0;
	}

	path_src = loc_path;
	if (opcode == GF_OP_RENAME || opcode == GF_OP_LINK)
		path2_src = loc2_path;
	else if (opcode == GF_OP_SYMLINK)
		path2_src = name;

	/* link reads the source inode from loc2; rename uses the source loc.
	 * The other ops use loc; mkdir/create/symlink may have a fresh inode
	 * (0 at R), leaving mode 0 for userspace defaults. */
	inode = loc_ino;
	if (opcode == GF_OP_LINK)
		inode = loc2_ino;
	if (inode) {
		bpf_probe_read_user(&ia_type, sizeof(ia_type), (void *)(inode + 92));
		mode = gf_mode_from_ia(ia_type);
	}

	e = bpf_ringbuf_reserve(&gf_path_events, sizeof(*e), 0);
	if (!e)
		return 0;
	e->ts = bpf_ktime_get_ns();
	e->unique = unique;
	e->off = 0;
	e->size = 0;
	e->mode = mode;
	e->path_len = gf_copy_user_str(e->path, GLUSTERFS_TRACE_PATH_MAX,
				       path_src, &truncated, GF_TRUNC_PATH);
	e->path2_len = gf_copy_user_str(e->path2, GLUSTERFS_TRACE_PATH_MAX,
					path2_src, &truncated, GF_TRUNC_PATH2);
	e->xattr_name_len = 0;
	e->xattr_name[0] = 0;
	if (opcode == GF_OP_SETXATTR || opcode == GF_OP_GETXATTR) {
		e->xattr_name_len = gf_copy_user_str(
			e->xattr_name, GLUSTERFS_TRACE_XATTR_MAX, name,
			&truncated, GF_TRUNC_XATTR);
	}

	switch (opcode) {
	case GF_OP_READ:
	case GF_OP_WRITE:
	case GF_OP_READDIR:
	case GF_OP_READDIRPLUS:
		e->off = off;
		e->size = size;
		break;
	case GF_OP_SETATTR:
		/* truncate: off = new size (fsi->size), size = 0 */
		e->off = off;
		e->size = 0;
		break;
	case GF_OP_SETXATTR:
	case GF_OP_GETXATTR:
		e->off = 0;
		e->size = size;
		break;
	default:
		break;
	}
	e->truncated = truncated;
	bpf_ringbuf_submit(e, 0);
	return 0;
}

/* S entry: send_fuse_iov(xlator_t *this, fuse_in_header_t *finh,
 *                        struct iovec *iov_out, ...). */
SEC("uprobe")
int gf_send_entry(struct pt_regs *ctx)
{
	u64 finh = (u64)PT_REGS_PARM2(ctx);
	u64 iov_out = (u64)PT_REGS_PARM3(ctx);
	u64 iov_base = 0;
	u32 opcode = 0, pid = 0;
	u64 unique = 0, nodeid = 0;
	s32 error = 0;
	struct gf_send_key key = {0};
	struct gf_send_val val = {0};

	if (!finh || !iov_out)
		return 0;
	if (gf_read_finh(finh, &opcode, &unique, &nodeid, &pid))
		return 0;
	if (!pid || !gf_opcode_allowed(opcode))
		return 0;
	if (bpf_probe_read_user(&iov_base, sizeof(iov_base), (void *)iov_out))
		return 0;
	if (iov_base)
		bpf_probe_read_user(&error, sizeof(error), (void *)(iov_base + 4));

	key.pid_tgid = bpf_get_current_pid_tgid();
	val.unique = unique;
	val.nodeid = nodeid;
	val.opcode = opcode;
	val.pid = pid;
	val.error = error;
	val.pad = 0;
	bpf_map_update_elem(&gf_send_state, &key, &val, BPF_ANY);
	return 0;
}

/* S return: only take the timestamp; error/finh were captured at entry. */
SEC("uprobe")
int gf_send_ret(struct pt_regs *ctx)
{
	struct gf_send_key key = {0};
	struct gf_send_val *val;
	struct glusterfs_ctl_event *e;

	key.pid_tgid = bpf_get_current_pid_tgid();
	val = bpf_map_lookup_elem(&gf_send_state, &key);
	if (!val)
		return 0;

	e = bpf_ringbuf_reserve(&gf_ctl_events, sizeof(*e), 0);
	if (!e) {
		bpf_map_delete_elem(&gf_send_state, &key);
		return 0;
	}
	e->ts = bpf_ktime_get_ns();
	e->unique = val->unique;
	e->phase = GF_PHASE_S;
	e->opcode = val->opcode;
	e->nodeid = val->nodeid;
	e->pid = val->pid;
	e->error = val->error;
	bpf_ringbuf_submit(e, 0);
	bpf_map_delete_elem(&gf_send_state, &key);
	return 0;
}

char LICENSE[] SEC("license") = "GPL";
