// SPDX-License-Identifier: GPL-2.0
/*
 * glusterfs_trace_test.cc — standalone GlusterFS BPF tracer smoke harness.
 *
 * This file is a test-only translation unit. It includes the production
 * glusterfs_trace.cc so it can inspect the same ctl/path/final state without
 * adding any test hook or behavior change to the executor sources.
 *
 * Build:
 *   make glusterfs-trace-test
 *
 * Run (in the target VM, as root):
 *   ./glusterfs-trace-test --fuse-so <fuse.so> --trace-root eifuzz_root --raw
 *
 * Commands:
 *   Enter / c : drain rings, print raw events (with --raw) and final events
 *   q / quit  : exit
 */

#include <algorithm>
#include <cerrno>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <iostream>
#include <string>
#include <vector>

#include "glusterfs_trace.cc"

/*
 * The production tracing unit expects these executor-owned symbols. The
 * standalone harness does not serialize to the executor stream; the stubs are
 * only here to satisfy the linker when glusterfs_trace.cc is included.
 */
static std::vector<unsigned char> g_wire_output;

static void wire_append(const void *data, size_t n)
{
	const unsigned char *p = static_cast<const unsigned char *>(data);
	g_wire_output.insert(g_wire_output.end(), p, p + n);
}

uint32 *write_output(uint32 v)
{
	static uint32 ret;
	wire_append(&v, sizeof(v));
	return &ret;
}

uint32 *write_output_64(uint64 v)
{
	static uint32 ret;
	wire_append(&v, sizeof(v));
	return &ret;
}

uint64_t tsc_ns_to_global(uint64_t ns)
{
	return ns;
}

signed long long int tsc_offset = 0;
uint64_t g_prog_end_raw = 0;
double tsc_ns_ratio = 1.0;
uint64_t tsc_best_gap = 0;

static std::string field_text(const char *p, unsigned int len,
			      unsigned int maxlen)
{
	if (len > maxlen)
		len = maxlen;
	return std::string(p, len);
}

static const char *func_name(unsigned int func_id)
{
	switch (func_id) {
	case GF_FUNC_MKDIR:
		return "MKDIR";
	case GF_FUNC_CREATE:
		return "CREATE";
	case GF_FUNC_RMDIR:
		return "RMDIR";
	case GF_FUNC_UNLINK:
		return "UNLINK";
	case GF_FUNC_RENAME:
		return "RENAME";
	case GF_FUNC_WRITE:
		return "WRITE";
	case GF_FUNC_READ:
		return "READ";
	case GF_FUNC_FILE_OPEN:
		return "FILE_OPEN";
	case GF_FUNC_DIR_OPEN:
		return "DIR_OPEN";
	case GF_FUNC_GETATTR:
		return "GETATTR";
	case GF_FUNC_SETATTR:
		return "SETATTR";
	case GF_FUNC_FSYNC:
		return "FSYNC";
	case GF_FUNC_RELEASE:
		return "RELEASE";
	case GF_FUNC_LOOKUP:
		return "LOOKUP";
	case GF_FUNC_ITERATE:
		return "ITERATE";
	case GF_FUNC_LINK:
		return "LINK";
	case GF_FUNC_SYMLINK:
		return "SYMLINK";
	case GF_FUNC_READLINK:
		return "READLINK";
	case GF_FUNC_GETXATTR:
		return "GETXATTR";
	case GF_FUNC_SETXATTR:
		return "SETXATTR";
	default:
		return "UNKNOWN";
	}
}

static const char *mode_type(unsigned int mode)
{
	switch (mode & 0xf000) {
	case 0x8000:
		return "file";
	case 0x4000:
		return "dir";
	case 0xa000:
		return "symlink";
	default:
		return "unknown";
	}
}

static void dump_ctl(const struct glusterfs_ctl_event &e)
{
	printf("[ctl %c] ts=%llu unique=%llu opcode=%u nodeid=%llu pid=%u "
	       "error=%d mode=0x%x\n",
	       e.phase == 0 ? 'D' : 'S', e.ts, e.unique, e.opcode, e.nodeid,
	       e.pid, e.error, e.mode);
}

static void dump_path(const struct glusterfs_path_event &e)
{
	std::string path = field_text(e.path, e.path_len,
				      GLUSTERFS_TRACE_PATH_MAX);
	std::string path2 = field_text(e.path2, e.path2_len,
				       GLUSTERFS_TRACE_PATH_MAX);
	std::string xattr = field_text(e.xattr_name, e.xattr_name_len,
				       GLUSTERFS_TRACE_XATTR_MAX);
	printf("[path] ts=%llu unique=%llu off=%llu size=%llu mode=0x%x "
	       "trunc=0x%x path=\"%s\"",
	       e.ts, e.unique, e.off, e.size, e.mode, e.truncated,
	       path.c_str());
	if (!path2.empty())
		printf(" path2=\"%s\"", path2.c_str());
	if (!xattr.empty())
		printf(" xattr=\"%s\"", xattr.c_str());
	printf("\n");
}

struct lookup_check {
	int total;
	int positive_mode_zero;
	int negative_mode_nonzero;
};

static const char *errno_text(int ret)
{
	if (ret >= 0)
		return "";
	return strerror(-ret);
}

static void dump_final(const struct glusterfs_trace_event &e)
{
	std::string path = field_text(e.path, e.path_len,
				      GLUSTERFS_TRACE_PATH_MAX);
	std::string path2 = field_text(e.path2, e.path2_len,
				       GLUSTERFS_TRACE_PATH_MAX);
	std::string xattr = field_text(e.xattr_name, e.xattr_name_len,
				       GLUSTERFS_TRACE_XATTR_MAX);
	printf("[final] func=%s(%u) ret=%d", func_name(e.func_id), e.func_id,
	       e.ret);
	if (e.ret < 0)
		printf("/%s", errno_text(e.ret));
	printf(" mode=0x%x(%s) unique=%llu nodeid=%llu off=%llu size=%llu "
	       "path=\"%s\"",
	       e.mode, mode_type(e.mode), e.unique, e.nodeid, e.off, e.size,
	       path.c_str());
	if (!path2.empty())
		printf(" path2=\"%s\"", path2.c_str());
	if (!xattr.empty())
		printf(" xattr=\"%s\"", xattr.c_str());
	if (e.truncated)
		printf(" truncated=0x%x", e.truncated);
	if (e.entry_ts == 0)
		printf(" entry_ts=missing");
	printf("\n");
}

static void collect(bool raw)
{
	int nc = ring_buffer__consume(ctl_rb);
	int np = ring_buffer__consume(path_rb);
	if (nc < 0)
		fprintf(stderr, "ctl ring consume: %d\n", nc);
	if (np < 0)
		fprintf(stderr, "path ring consume: %d\n", np);

	if (raw) {
		for (const auto &e : ctl_read_buf)
			dump_ctl(e);
		for (const auto &e : path_read_buf)
			dump_path(e);
	}

	process_merged_events();

	struct lookup_check lk = {};
	printf("[collect] final=%zu\n", g_final_events.size());
	for (const auto &e : g_final_events) {
		dump_final(e);
		if (e.func_id == GF_FUNC_LOOKUP) {
			lk.total++;
			if (e.ret == 0 && (e.mode & 0xf000) == 0)
				lk.positive_mode_zero++;
			if (e.ret < 0 && (e.mode & 0xf000) != 0)
				lk.negative_mode_nonzero++;
		}
	}
	printf("[lookup check] total=%d positive_mode_zero=%d "
	       "negative_mode_nonzero=%d\n",
	       lk.total, lk.positive_mode_zero, lk.negative_mode_nonzero);
	fflush(stdout);
	g_final_events.clear();
}

static void usage(const char *argv0)
{
	fprintf(stderr,
		"usage: %s [--fuse-so PATH] [--trace-root PATH] [--raw]\n"
		"  default fuse.so: "
		"/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so\n"
		"  default trace-root: eifuzz_root (relative to mount root)\n",
		argv0);
}

int main(int argc, char **argv)
{
	std::string fuse_so =
		"/usr/local/lib/glusterfs/11dev/xlator/mount/fuse.so";
	// trace_root is relative to the FUSE mount root; normalize_trace_path
	// strips the leading '/' from the raw event path before comparing.
	std::string trace_root = "eifuzz_root";
	bool raw = false;

	for (int i = 1; i < argc; i++) {
		if (!strcmp(argv[i], "--fuse-so") && i + 1 < argc) {
			fuse_so = argv[++i];
		} else if (!strcmp(argv[i], "--trace-root") && i + 1 < argc) {
			trace_root = argv[++i];
		} else if (!strcmp(argv[i], "--raw")) {
			raw = true;
		} else if (!strcmp(argv[i], "-h") || !strcmp(argv[i], "--help")) {
			usage(argv[0]);
			return 0;
		} else {
			usage(argv[0]);
			return 2;
		}
	}

	init_glusterfs_trace(fuse_so.c_str(), trace_root.c_str(), 1);
	if (!skel || !ctl_rb || !path_rb) {
		fprintf(stderr,
			"glusterfs-trace-test: attach/load failed; see "
			"glusterfs_trace diagnostics above\n");
		destroy_attach();
		return 1;
	}
	start_glusterfs_trace();

	printf("glusterfs-trace-test: ready (fuse_so=%s trace_root=%s)\n",
	       fuse_so.c_str(), trace_root.c_str());
	printf("Enter/c=collect, q=quit\n");
	fflush(stdout);

	std::string line;
	while (std::getline(std::cin, line)) {
		if (line == "q" || line == "quit")
			break;
		if (line.empty() || line == "c" || line == "collect") {
			collect(raw);
			continue;
		}
		printf("commands: Enter/c=collect, q=quit\n");
		fflush(stdout);
	}

	destroy_attach();
	return 0;
}
