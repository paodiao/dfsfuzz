// SPDX-License-Identifier: GPL-2.0
/*
 * dfs_runtime.cc — per-DFS executor hook tables (B2: trace hooks only).
 */

#include "dfs_runtime.h"

#include <cstring>

#include "glusterfs_trace.h"
#include "hmdfs_trace.h"

#define GLUSTERFS_FUSE_SO \
	"/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so"

/* ── HMDFS ─────────────────────────────────────────────────────── */

static bool hmdfs_trace_enabled(bool is_client)
{
	(void)is_client;
	return true;
}

static void hmdfs_init_trace(bool is_client, const char *trace_root)
{
	(void)is_client;
	(void)trace_root;
	init_hmdfs_trace();
}

static void hmdfs_start_trace(void)
{
	start_hmdfs_trace();
}

static void hmdfs_stop_trace(void)
{
	stop_collect_hmdfs_trace();
}

/* ── GlusterFS ─────────────────────────────────────────────────── */

static bool glusterfs_trace_enabled(bool is_client)
{
	/* Both roles emit output: client events, or a count=0 server header. */
	(void)is_client;
	return true;
}

static void glusterfs_init_trace(bool is_client, const char *trace_root)
{
	init_glusterfs_trace(GLUSTERFS_FUSE_SO, trace_root, is_client ? 1 : 0);
}

static void glusterfs_start_trace(void)
{
	start_glusterfs_trace();
}

static void glusterfs_stop_trace(void)
{
	stop_collect_glusterfs_trace();
}

/* Field order mirrors struct DfsRuntimeOps.  B2 leaves the non-trace hooks
 * null; the D phase fills them. */
static const DfsRuntimeOps hmdfs_runtime = {
	"hmdfs",                    /* name */
	hmdfs_trace_enabled,        /* trace_enabled_for */
	hmdfs_init_trace,           /* init_trace */
	hmdfs_start_trace,          /* start_trace */
	hmdfs_stop_trace,           /* stop_trace */
	nullptr,                    /* restore_initial */
	nullptr,                    /* needs_startup_barrier */
	nullptr,                    /* needs_restore_barrier */
	nullptr,                    /* client_cwd */
	nullptr,                    /* server_cwd */
	nullptr,                    /* metadata_root */
	nullptr,                    /* client_metadata_enabled */
	nullptr,                    /* server_metadata_enabled */
	nullptr,                    /* config_command */
	nullptr,                    /* node_up_command */
	nullptr,                    /* node_down_command */
};

static const DfsRuntimeOps glusterfs_runtime = {
	"glusterfs",                /* name */
	glusterfs_trace_enabled,    /* trace_enabled_for */
	glusterfs_init_trace,       /* init_trace */
	glusterfs_start_trace,      /* start_trace */
	glusterfs_stop_trace,       /* stop_trace */
	nullptr,                    /* restore_initial */
	nullptr,                    /* needs_startup_barrier */
	nullptr,                    /* needs_restore_barrier */
	nullptr,                    /* client_cwd */
	nullptr,                    /* server_cwd */
	nullptr,                    /* metadata_root */
	nullptr,                    /* client_metadata_enabled */
	nullptr,                    /* server_metadata_enabled */
	nullptr,                    /* config_command */
	nullptr,                    /* node_up_command */
	nullptr,                    /* node_down_command */
};

static const DfsRuntimeOps *const runtime_ops[] = {
	&hmdfs_runtime,
	&glusterfs_runtime,
};

const DfsRuntimeOps *dfs_runtime_ops(const char *dfs_name)
{
	if (!dfs_name)
		return nullptr;
	for (const DfsRuntimeOps *rt : runtime_ops) {
		if (!strcmp(rt->name, dfs_name))
			return rt;
	}
	return nullptr;
}
