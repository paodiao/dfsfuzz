// SPDX-License-Identifier: GPL-2.0
/*
 * glusterfs_trace.h — GlusterFS FUSE uprobe trace wire event.
 *
 * The final wire event is a fixed 1228-byte record. The Go parser
 * (parseGlusterfsTraceEvents) reads fixed 512/512/128 arrays and slices them
 * by the length fields; it must not treat the record as variable-length.
 */

#ifndef GLUSTERFS_TRACE_H
#define GLUSTERFS_TRACE_H

#include <stdint.h>

#define GLUSTERFS_TRACE_PATH_MAX 512
#define GLUSTERFS_TRACE_XATTR_MAX 128
#define MAX_GLUSTERFS_TRACE_EVENTS 8192
#define GLUSTERFS_TRACE_EVENT_SIZE 1228

struct __attribute__((packed)) glusterfs_trace_event {
	uint64_t timestamp;      /* 0:  send_fuse_iov uretprobe time */
	uint64_t entry_ts;       /* 8:  fuse_dispatch entry time, 0 if missing */
	uint32_t func_id;        /* 16 */
	int32_t ret;             /* 20: fuse_out_header.error */
	uint64_t nodeid;         /* 24: inode_t * (diagnostic; not Ino) */
	uint64_t unique;         /* 32: D/R/S correlation key */
	uint64_t off;            /* 40: read/write offset; truncate new size */
	uint64_t size;           /* 48: read/write length; truncate 0 */
	uint32_t mode;           /* 56: ia_type mapped to S_IFMT */
	uint32_t path_len;       /* 60 */
	uint32_t path2_len;      /* 64 */
	uint32_t xattr_name_len; /* 68 */
	uint32_t truncated;      /* 72: bit0 path, bit1 path2, bit2 xattr */
	char path[GLUSTERFS_TRACE_PATH_MAX];       /* 76 */
	char path2[GLUSTERFS_TRACE_PATH_MAX];      /* 588 */
	char xattr_name[GLUSTERFS_TRACE_XATTR_MAX];/* 1100 */
};

#ifdef __cplusplus
static_assert(sizeof(struct glusterfs_trace_event) == GLUSTERFS_TRACE_EVENT_SIZE,
              "glusterfs_trace_event wire size must be 1228");
#else
_Static_assert(sizeof(struct glusterfs_trace_event) == GLUSTERFS_TRACE_EVENT_SIZE,
               "glusterfs_trace_event wire size must be 1228");
#endif

void init_glusterfs_trace(void);
void start_glusterfs_trace(void);
void stop_collect_glusterfs_trace(void);

#endif /* GLUSTERFS_TRACE_H */
