// SPDX-License-Identifier: GPL-2.0
/*
 * dfs_runtime.h — per-DFS executor runtime hook table.
 *
 * B2 fills the trace hooks only.  The remaining restore/cwd/metadata/config
 * hooks are declared here for the D phase and stay nullptr until then.
 */

#ifndef DFS_RUNTIME_H
#define DFS_RUNTIME_H

#include <cstdint>
#include <string>

struct DfsRuntimeOps {
	const char *name;

	/* true: this role emits trace output. GlusterFS server returns true
	 * too: it writes the count=0 header even though it does not attach. */
	bool (*trace_enabled_for)(bool is_client);
	/* trace_root is non-null only for GlusterFS client ("eifuzz_root"). */
	void (*init_trace)(bool is_client, const char *trace_root);
	void (*start_trace)();
	void (*stop_trace)();

	bool (*restore_initial)(bool is_restarting);
	bool (*needs_startup_barrier)();
	bool (*needs_restore_barrier)();

	const char *(*client_cwd)();
	const char *(*server_cwd)();
	const char *(*metadata_root)(bool is_client);

	bool (*client_metadata_enabled)();
	bool (*server_metadata_enabled)();

	std::string (*config_command)(const char *init_ip,
				      const char *setup_params,
				      uint64_t index);
	std::string (*node_up_command)(const char *init_ip,
				       const char *setup_params,
				       uint64_t index);
	std::string (*node_down_command)(uint64_t index);
};

const DfsRuntimeOps *dfs_runtime_ops(const char *dfs_name);

#endif /* DFS_RUNTIME_H */
