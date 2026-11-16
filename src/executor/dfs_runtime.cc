// SPDX-License-Identifier: GPL-2.0
/*
 * dfs_runtime.cc — per-DFS executor hook tables (B2: trace hooks only).
 */

#include "dfs_runtime.h"

#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <unistd.h>

#include "glusterfs_trace.h"
#include "hmdfs_trace.h"

#define GLUSTERFS_FUSE_SO \
	"/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so"
#define GLUSTERFS_CLIENT_ROOT "/root/glusterfs-client/eifuzz_root"
#define GLUSTERFS_SERVER_ROOT "/root"
#define GLUSTERFS_RESTORE_BASELINE "/home/baseline/glusterfs/"
#define GLUSTERFS_RESTORE_TARGET GLUSTERFS_CLIENT_ROOT "/"

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

static const char *glusterfs_client_cwd(void)
{
	return GLUSTERFS_CLIENT_ROOT;
}

static const char *glusterfs_server_cwd(void)
{
	return GLUSTERFS_SERVER_ROOT;
}

static const char *glusterfs_metadata_root(bool is_client)
{
	return is_client ? GLUSTERFS_CLIENT_ROOT : GLUSTERFS_SERVER_ROOT;
}

static bool glusterfs_needs_restore_barrier(void)
{
	return true;
}

/* The reset client rebuilds the fixed test root from the baseline; every
 * other client only participates in the barrier.  Clients leave the test
 * root first so the reset client can unlink it even if another client still
 * had it as its cwd. */
static bool glusterfs_restore_initial(bool is_restarting, bool is_client,
				      bool is_reset_client)
{
	if (is_restarting || !is_client)
		return true;
	if (chdir(GLUSTERFS_SERVER_ROOT) != 0) {
		fprintf(stderr,
			"glusterfs restore: chdir %s failed: %s\n",
			GLUSTERFS_SERVER_ROOT, strerror(errno));
		return false;
	}
	if (!is_reset_client)
		return true;

	fprintf(stderr, "glusterfs restore: rebuild %s\n",
		GLUSTERFS_CLIENT_ROOT);
	sync();
	char cmd[1024];
	snprintf(cmd, sizeof(cmd), "rm -rf %s", GLUSTERFS_CLIENT_ROOT);
	if (system(cmd) != 0) {
		fprintf(stderr, "glusterfs restore: rm failed: %s\n", cmd);
		return false;
	}
	snprintf(cmd, sizeof(cmd),
		 "timeout 60s rsync -a --numeric-ids --delete "
		 "--one-file-system --checksum %s %s",
		 GLUSTERFS_RESTORE_BASELINE, GLUSTERFS_RESTORE_TARGET);
	if (system(cmd) != 0) {
		fprintf(stderr, "glusterfs restore: rsync failed: %s\n", cmd);
		return false;
	}
	sync();
	return true;
}

/* Field order mirrors struct DfsRuntimeOps.  HMDFS keeps the legacy
 * executor paths; GlusterFS fills the D-phase runtime hooks. */
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
	glusterfs_restore_initial,  /* restore_initial */
	nullptr,                    /* needs_startup_barrier */
	glusterfs_needs_restore_barrier, /* needs_restore_barrier */
	glusterfs_client_cwd,       /* client_cwd */
	glusterfs_server_cwd,       /* server_cwd */
	glusterfs_metadata_root,    /* metadata_root */
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
