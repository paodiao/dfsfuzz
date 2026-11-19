// SPDX-License-Identifier: GPL-2.0
/*
 * glusterfs_trace.h — GlusterFS FUSE uprobe trace wire events.
 *
 * The final wire event is a fixed 1228-byte record. The Go parser
 * (parseGlusterfsTraceEvents) reads fixed 512/512/128 arrays and slices them
 * by the length fields; it must not treat the record as variable-length.
 *
 * This header is shared by the BPF program and the executor userspace code, so
 * it uses plain C integer types and no libc headers.
 */

#ifndef GLUSTERFS_TRACE_H
#define GLUSTERFS_TRACE_H

#define GLUSTERFS_TRACE_PATH_MAX 512
#define GLUSTERFS_TRACE_XATTR_MAX 128
#define MAX_GLUSTERFS_TRACE_EVENTS 8192

#define GLUSTERFS_TRACE_CTL_EVENT_SIZE 44
#define GLUSTERFS_TRACE_PATH_EVENT_SIZE 1204
#define GLUSTERFS_TRACE_EVENT_SIZE 1228

/* func_id constants — keep in sync with dag.go Func* and the Go parser. */
enum glusterfs_func_id {
	GF_FUNC_MKDIR = 0,
	GF_FUNC_CREATE = 1,
	GF_FUNC_RMDIR = 2,
	GF_FUNC_UNLINK = 3,
	GF_FUNC_RENAME = 4,
	GF_FUNC_WRITE = 5,
	GF_FUNC_READ = 6,
	GF_FUNC_FILE_OPEN = 7,
	GF_FUNC_DIR_OPEN = 8,
	GF_FUNC_GETATTR = 9,
	GF_FUNC_SETATTR = 10,
	GF_FUNC_FSYNC = 11,
	GF_FUNC_RELEASE = 12,
	GF_FUNC_LOOKUP = 13,
	GF_FUNC_ITERATE = 14,
	GF_FUNC_LINK = 16,
	GF_FUNC_SYMLINK = 17,
	GF_FUNC_READLINK = 18,
	GF_FUNC_GETXATTR = 19,
	GF_FUNC_SETXATTR = 20,
	GF_FUNC_COUNT = 21,
};

/* FUSE opcodes mapped by v1. FUSE_RENAME2 45 and COPY_FILE_RANGE 47 are not
 * emitted by this build. */
enum glusterfs_fuse_opcode {
	GF_OP_LOOKUP = 1,
	GF_OP_GETATTR = 3,
	GF_OP_SETATTR = 4,
	GF_OP_READLINK = 5,
	GF_OP_SYMLINK = 6,
	GF_OP_MKDIR = 9,
	GF_OP_UNLINK = 10,
	GF_OP_RMDIR = 11,
	GF_OP_RENAME = 12,
	GF_OP_LINK = 13,
	GF_OP_OPEN = 14,
	GF_OP_READ = 15,
	GF_OP_WRITE = 16,
	GF_OP_RELEASE = 18,
	GF_OP_FSYNC = 20,
	GF_OP_SETXATTR = 21,
	GF_OP_GETXATTR = 22,
	GF_OP_OPENDIR = 27,
	GF_OP_READDIR = 28,
	GF_OP_CREATE = 35,
	GF_OP_READDIRPLUS = 44,
};

/* ctl ring: D (phase 0) and S (phase 1). */
struct __attribute__((packed)) glusterfs_ctl_event {
	unsigned long long ts;     /* 0 */
	unsigned long long unique; /* 8 */
	unsigned int phase;        /* 16: D=0, S=1 */
	unsigned int opcode;       /* 20 */
	unsigned long long nodeid; /* 24 */
	unsigned int pid;          /* 32 */
	int error;                 /* 36: S only; D/R 0 */
	unsigned int mode;         /* 40: S only; LOOKUP reply entry type */
};

/* path ring: R events. */
struct __attribute__((packed)) glusterfs_path_event {
	unsigned long long ts;             /* 0 */
	unsigned long long unique;         /* 8 */
	unsigned long long off;            /* 16 */
	unsigned long long size;           /* 24 */
	unsigned int mode;                 /* 32 */
	unsigned int path_len;             /* 36 */
	unsigned int path2_len;            /* 40 */
	unsigned int xattr_name_len;       /* 44 */
	unsigned int truncated;            /* 48 */
	char path[GLUSTERFS_TRACE_PATH_MAX];        /* 52 */
	char path2[GLUSTERFS_TRACE_PATH_MAX];       /* 564 */
	char xattr_name[GLUSTERFS_TRACE_XATTR_MAX]; /* 1076 */
};

/* final event written to the executor output stream. */
struct __attribute__((packed)) glusterfs_trace_event {
	unsigned long long timestamp;      /* 0:  send_fuse_iov uretprobe time */
	unsigned long long entry_ts;       /* 8:  fuse_dispatch entry time, 0 if missing */
	unsigned int func_id;              /* 16 */
	int ret;                           /* 20: fuse_out_header.error */
	unsigned long long nodeid;         /* 24: inode_t * (diagnostic; not Ino) */
	unsigned long long unique;         /* 32: D/R/S correlation key */
	unsigned long long off;            /* 40: read/write offset; truncate new size */
	unsigned long long size;           /* 48: read/write length; truncate 0 */
	unsigned int mode;                 /* 56: ia_type mapped to S_IFMT */
	unsigned int path_len;             /* 60 */
	unsigned int path2_len;            /* 64 */
	unsigned int xattr_name_len;       /* 68 */
	unsigned int truncated;            /* 72: bit0 path, bit1 path2, bit2 xattr */
	char path[GLUSTERFS_TRACE_PATH_MAX];        /* 76 */
	char path2[GLUSTERFS_TRACE_PATH_MAX];       /* 588 */
	char xattr_name[GLUSTERFS_TRACE_XATTR_MAX]; /* 1100 */
};

#ifdef __cplusplus
static_assert(sizeof(struct glusterfs_ctl_event) == GLUSTERFS_TRACE_CTL_EVENT_SIZE,
              "glusterfs_ctl_event wire size must be 44");
static_assert(sizeof(struct glusterfs_path_event) == GLUSTERFS_TRACE_PATH_EVENT_SIZE,
              "glusterfs_path_event wire size must be 1204");
static_assert(sizeof(struct glusterfs_trace_event) == GLUSTERFS_TRACE_EVENT_SIZE,
              "glusterfs_trace_event wire size must be 1228");
#else
_Static_assert(sizeof(struct glusterfs_ctl_event) == GLUSTERFS_TRACE_CTL_EVENT_SIZE,
               "glusterfs_ctl_event wire size must be 44");
_Static_assert(sizeof(struct glusterfs_path_event) == GLUSTERFS_TRACE_PATH_EVENT_SIZE,
               "glusterfs_path_event wire size must be 1204");
_Static_assert(sizeof(struct glusterfs_trace_event) == GLUSTERFS_TRACE_EVENT_SIZE,
               "glusterfs_trace_event wire size must be 1228");
#endif

/* attach=0 means "server executor": emit only the empty trace header.
 * Returns 0 when a client attach cannot be initialized. */
int init_glusterfs_trace(const char *fuse_so, const char *trace_root, int attach);
void start_glusterfs_trace(void);
void stop_collect_glusterfs_trace(void);

#endif /* GLUSTERFS_TRACE_H */
