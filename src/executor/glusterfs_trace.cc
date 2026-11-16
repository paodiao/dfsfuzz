// SPDX-License-Identifier: GPL-2.0
/*
 * glusterfs_trace.cc — GlusterFS FUSE uprobe tracer: libelf attach,
 * ctl/path ring collection, D/R/S aggregation, and wire serialization.
 *
 * The final output is the same [ratio][offset][gap][count] header as HMDFS,
 * followed by fixed 1228-byte glusterfs_trace_event records.  Server
 * executors never attach but still write a count=0 header so the fuzzer's
 * [program output][trace][fsMd] stream order is preserved.
 */

#include <algorithm>
#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>

#include <fcntl.h>
#include <gelf.h>
#include <libelf.h>
#include <unistd.h>

#include <bpf/libbpf.h>

#include "glusterfs_trace.h"
#include "glusterfs_trace.skel.h"

/* ── Executor output/time helpers (provided by executor.cc) ───── */
typedef unsigned int uint32;
typedef unsigned long long uint64;

extern uint32 *write_output(uint32 v);
extern uint32 *write_output_64(uint64 v);
extern uint64_t tsc_ns_to_global(uint64_t ns);
extern signed long long int tsc_offset;
extern uint64_t g_prog_end_raw;
extern double tsc_ns_ratio;
extern uint64_t tsc_best_gap;

/* ── Attach state ─────────────────────────────────────────────── */
static struct glusterfs_trace_bpf *skel;
static struct ring_buffer *ctl_rb;
static struct ring_buffer *path_rb;
static struct bpf_link *link_dispatch;
static struct bpf_link *link_resume;
static struct bpf_link *link_send_entry;
static struct bpf_link *link_send_ret;

static std::string g_trace_root;

/* ── Raw ring buffers ─────────────────────────────────────────── */
static std::vector<struct glusterfs_ctl_event> ctl_read_buf;
static std::vector<struct glusterfs_path_event> path_read_buf;

/* ── Aggregation state (parent process only) ──────────────────── */
struct pending_req {
	bool has_d = false;
	bool has_r = false;
	bool out_of_root = false;
	uint64_t d_ts = 0;
	uint32_t opcode = 0;
	uint64_t nodeid = 0;
	uint64_t off = 0;
	uint64_t size = 0;
	uint32_t mode = 0;
	uint32_t truncated = 0;
	std::string path;
	std::string path2;
	std::string path2_raw;
	std::string xattr_name;
	std::string xattr_raw;
};

static std::unordered_map<uint64_t, struct pending_req> g_pending;
static std::unordered_map<uint64_t, std::string> g_nodeid_path;

/* ── Ring callbacks ───────────────────────────────────────────── */
static int handle_ctl(void *ctx, void *data, size_t data_sz)
{
	(void)ctx;
	if (data_sz != sizeof(struct glusterfs_ctl_event))
		return 0;
	ctl_read_buf.push_back(*(const struct glusterfs_ctl_event *)data);
	return 0;
}

static int handle_path(void *ctx, void *data, size_t data_sz)
{
	(void)ctx;
	if (data_sz != sizeof(struct glusterfs_path_event))
		return 0;
	path_read_buf.push_back(*(const struct glusterfs_path_event *)data);
	return 0;
}

/* ── Opcode classification ────────────────────────────────────── */
static uint32_t func_id_for_opcode(uint32_t opcode)
{
	switch (opcode) {
	case GF_OP_LOOKUP:
		return GF_FUNC_LOOKUP;
	case GF_OP_GETATTR:
		return GF_FUNC_GETATTR;
	case GF_OP_SETATTR:
		return GF_FUNC_SETATTR;
	case GF_OP_READLINK:
		return GF_FUNC_READLINK;
	case GF_OP_SYMLINK:
		return GF_FUNC_SYMLINK;
	case GF_OP_MKDIR:
		return GF_FUNC_MKDIR;
	case GF_OP_UNLINK:
		return GF_FUNC_UNLINK;
	case GF_OP_RMDIR:
		return GF_FUNC_RMDIR;
	case GF_OP_RENAME:
		return GF_FUNC_RENAME;
	case GF_OP_LINK:
		return GF_FUNC_LINK;
	case GF_OP_OPEN:
		return GF_FUNC_FILE_OPEN;
	case GF_OP_READ:
		return GF_FUNC_READ;
	case GF_OP_WRITE:
		return GF_FUNC_WRITE;
	case GF_OP_RELEASE:
		return GF_FUNC_RELEASE;
	case GF_OP_FSYNC:
		return GF_FUNC_FSYNC;
	case GF_OP_SETXATTR:
		return GF_FUNC_SETXATTR;
	case GF_OP_GETXATTR:
		return GF_FUNC_GETXATTR;
	case GF_OP_OPENDIR:
		return GF_FUNC_DIR_OPEN;
	case GF_OP_READDIR:
	case GF_OP_READDIRPLUS:
		return GF_FUNC_ITERATE;
	case GF_OP_CREATE:
		return GF_FUNC_CREATE;
	}
	return GF_FUNC_COUNT;
}

static bool target_inode_opcode(uint32_t opcode)
{
	switch (opcode) {
	case GF_OP_GETATTR:
	case GF_OP_SETATTR:
	case GF_OP_OPEN:
	case GF_OP_OPENDIR:
	case GF_OP_SETXATTR:
	case GF_OP_GETXATTR:
		return true;
	}
	return false;
}

static bool user_xattr(const std::string &name)
{
	return name.rfind("user.", 0) == 0;
}

/* Strip the optional eifuzz_root layer from a FUSE path.  Return false for
 * mount root/.glusterfs/out-of-root.  An empty input is valid (path unknown;
 * the caller may use the nodeid map). */
static bool normalize_trace_path(const std::string &raw, std::string &out)
{
	out.clear();
	if (raw.empty())
		return true;

	std::string p = raw;
	if (p[0] == '/')
		p.erase(0, 1);
	if (p.empty())
		return false;
	if (p == ".glusterfs" || p.rfind(".glusterfs/", 0) == 0)
		return false;

	if (!g_trace_root.empty()) {
		const std::string root = g_trace_root;
		if (p == root)
			return true;
		if (p.rfind(root + "/", 0) == 0)
			p.erase(0, root.size() + 1);
	}
	out = p;
	return true;
}

static std::string raw_string(const char *buf, uint32_t len, uint32_t max)
{
	if (len > max)
		len = max;
	return std::string(buf, buf + len);
}

/* ── Merged ring event ────────────────────────────────────────── */
struct merged_event {
	uint64_t ts;
	int order; /* D=0, R=1, S=2 */
	bool is_path;
	struct glusterfs_ctl_event ctl;
	struct glusterfs_path_event path;
};

static bool merged_before(const struct merged_event &a,
			  const struct merged_event &b)
{
	if (a.ts != b.ts)
		return a.ts < b.ts;
	return a.order < b.order;
}

/* ── Final event collection ───────────────────────────────────── */
static std::vector<struct glusterfs_trace_event> g_final_events;
static uint64_t g_dropped_events;
static uint64_t g_missing_d;
static uint64_t g_missing_r;
static uint64_t g_missing_s;
static uint64_t g_drop_xattr;
static uint64_t g_out_of_root;
static uint64_t g_path2_out_of_root;
static uint64_t g_unmapped_opcode;

static void append_event(struct pending_req &p, const struct glusterfs_ctl_event &s)
{
	uint32_t fid = func_id_for_opcode(p.opcode);
	if (fid == GF_FUNC_COUNT) {
		g_unmapped_opcode++;
		return;
	}
	if ((p.opcode == GF_OP_SETXATTR || p.opcode == GF_OP_GETXATTR) &&
	    !user_xattr(p.xattr_name)) {
		g_drop_xattr++;
		return;
	}
	if (p.out_of_root) {
		g_out_of_root++;
		return;
	}
	if (g_final_events.size() >= MAX_GLUSTERFS_TRACE_EVENTS) {
		g_dropped_events++;
		return;
	}

	struct glusterfs_trace_event ev = {};
	ev.timestamp = tsc_ns_to_global(s.ts);
	if (p.has_d)
		ev.entry_ts = tsc_ns_to_global(p.d_ts);
	ev.func_id = fid;
	ev.ret = s.error;
	ev.nodeid = p.nodeid;
	ev.unique = s.unique;
	ev.off = p.off;
	ev.size = p.size;
	ev.mode = p.mode;

	if (p.path.size() > GLUSTERFS_TRACE_PATH_MAX) {
		p.truncated |= 1;
		p.path.resize(GLUSTERFS_TRACE_PATH_MAX);
	}
	if (p.path2.size() > GLUSTERFS_TRACE_PATH_MAX) {
		p.truncated |= 2;
		p.path2.resize(GLUSTERFS_TRACE_PATH_MAX);
	}
	if (p.xattr_name.size() > GLUSTERFS_TRACE_XATTR_MAX) {
		p.truncated |= 4;
		p.xattr_name.resize(GLUSTERFS_TRACE_XATTR_MAX);
	}
	ev.path_len = (uint32_t)p.path.size();
	ev.path2_len = (uint32_t)p.path2.size();
	ev.xattr_name_len = (uint32_t)p.xattr_name.size();
	ev.truncated = p.truncated;
	if (ev.path_len)
		memcpy(ev.path, p.path.data(), ev.path_len);
	if (ev.path2_len)
		memcpy(ev.path2, p.path2.data(), ev.path2_len);
	if (ev.xattr_name_len)
		memcpy(ev.xattr_name, p.xattr_name.data(), ev.xattr_name_len);
	g_final_events.push_back(ev);
}

static void process_merged_events(void)
{
	std::vector<struct merged_event> merged;
	merged.reserve(ctl_read_buf.size() + path_read_buf.size());

	for (const auto &c : ctl_read_buf) {
		struct merged_event m = {};
		m.ts = c.ts;
		m.order = c.phase == 0 ? 0 : 2;
		m.ctl = c;
		merged.push_back(m);
	}
	for (const auto &r : path_read_buf) {
		struct merged_event m = {};
		m.ts = r.ts;
		m.order = 1;
		m.is_path = true;
		m.path = r;
		merged.push_back(m);
	}
	std::sort(merged.begin(), merged.end(), merged_before);

	for (const auto &m : merged) {
		if (m.is_path) {
			const auto &r = m.path;
			auto &p = g_pending[r.unique];
			p.has_r = true;
			p.off = r.off;
			p.size = r.size;
			p.mode = r.mode;
			p.truncated = r.truncated;

			std::string norm;
			if (!normalize_trace_path(raw_string(r.path, r.path_len,
							    GLUSTERFS_TRACE_PATH_MAX),
						  norm)) {
				p.out_of_root = true;
			} else {
				p.path = norm;
			}

			/* Keep path2/xattr raw until S tells us the opcode:
			 * R can be drained before its D, and xattr needs the
			 * user.* check before a final event is emitted. */
			p.path2_raw = raw_string(r.path2, r.path2_len,
						 GLUSTERFS_TRACE_PATH_MAX);
			p.xattr_raw = raw_string(r.xattr_name, r.xattr_name_len,
						 GLUSTERFS_TRACE_XATTR_MAX);
			continue;
		}

		const auto &c = m.ctl;
		if (c.phase == 0) {
			auto &p = g_pending[c.unique];
			p.has_d = true;
			p.d_ts = c.ts;
			p.opcode = c.opcode;
			p.nodeid = c.nodeid;
			continue;
		}

		auto it = g_pending.find(c.unique);
		if (it == g_pending.end()) {
			struct pending_req p;
			p.has_d = false;
			it = g_pending.emplace(c.unique, std::move(p)).first;
		}
		auto &p = it->second;
		/* S is authoritative for the opcode when D was lost. */
		p.opcode = c.opcode;
		p.nodeid = c.nodeid;
		if (!p.has_d)
			g_missing_d++;

		if (!p.has_r) {
			if (p.opcode == GF_OP_SETXATTR || p.opcode == GF_OP_GETXATTR) {
				/* no name/path: cannot emit */
				g_drop_xattr++;
				g_pending.erase(it);
				continue;
			}
			if (p.opcode != GF_OP_RELEASE)
				g_missing_r++;
		}
		/* Missing or out-of-root event path: prefer the nodeid map. */
		if (p.path.empty() || p.out_of_root) {
			auto nip = g_nodeid_path.find(p.nodeid);
			if (nip != g_nodeid_path.end()) {
				p.path = nip->second;
				p.out_of_root = false;
				p.truncated &= ~1u;
			}
		}

		/* Interpret the raw R fields once the opcode is known. */
		if (p.opcode == GF_OP_RENAME || p.opcode == GF_OP_LINK) {
			std::string norm2;
			if (!normalize_trace_path(p.path2_raw, norm2)) {
				p.path2.clear();
				g_path2_out_of_root++;
			} else {
				p.path2 = norm2;
			}
		} else if (p.opcode == GF_OP_SYMLINK) {
			p.path2 = p.path2_raw;
		} else {
			p.path2.clear();
		}
		if (p.opcode == GF_OP_SETXATTR || p.opcode == GF_OP_GETXATTR) {
			p.xattr_name = p.xattr_raw;
			if (!user_xattr(p.xattr_name)) {
				g_drop_xattr++;
				g_pending.erase(it);
				continue;
			}
		} else {
			p.xattr_name.clear();
		}
		if (target_inode_opcode(p.opcode) && !p.path.empty() &&
		    !p.out_of_root)
			g_nodeid_path[p.nodeid] = p.path;
		append_event(p, c);
		g_pending.erase(it);
	}

	for (const auto &kv : g_pending) {
		(void)kv;
		g_missing_s++;
	}

	fprintf(stderr,
		"glusterfs_trace: merged=%zu final=%zu missing_d=%llu missing_r=%llu "
		"missing_s=%llu drop_xattr=%llu out_root=%llu path2_out_root=%llu "
		"unmapped=%llu dropped=%llu\n",
		merged.size(), g_final_events.size(),
		(unsigned long long)g_missing_d, (unsigned long long)g_missing_r,
		(unsigned long long)g_missing_s, (unsigned long long)g_drop_xattr,
		(unsigned long long)g_out_of_root,
		(unsigned long long)g_path2_out_of_root,
		(unsigned long long)g_unmapped_opcode,
		(unsigned long long)g_dropped_events);

	g_pending.clear();
	g_nodeid_path.clear();
	ctl_read_buf.clear();
	path_read_buf.clear();
}

/* ── Output serialization ─────────────────────────────────────── */
static void write_bytes(const void *data, size_t n)
{
	const unsigned char *p = (const unsigned char *)data;
	size_t i = 0;
	for (; i + 8 <= n; i += 8) {
		uint64_t v;
		memcpy(&v, p + i, 8);
		write_output_64(v);
	}
	for (; i < n; i++)
		write_output((uint32)p[i]);
}

static void write_header(uint32_t count)
{
	write_output_64((uint64_t)(tsc_ns_ratio * 1000000000.0));
	write_output_64((uint64_t)tsc_offset);
	write_output_64(tsc_best_gap);
	write_output(count);
}

static void write_events(void)
{
	write_header((uint32_t)g_final_events.size());
	for (const auto &ev : g_final_events) {
		write_output_64(ev.timestamp);
		write_output_64(ev.entry_ts);
		write_output(ev.func_id);
		write_output((uint32)ev.ret);
		write_output_64(ev.nodeid);
		write_output_64(ev.unique);
		write_output_64(ev.off);
		write_output_64(ev.size);
		write_output(ev.mode);
		write_output(ev.path_len);
		write_output(ev.path2_len);
		write_output(ev.xattr_name_len);
		write_output(ev.truncated);
		write_bytes(ev.path, sizeof(ev.path));
		write_bytes(ev.path2, sizeof(ev.path2));
		write_bytes(ev.xattr_name, sizeof(ev.xattr_name));
	}
	g_final_events.clear();
}

/* ── libelf symbol -> file offset ─────────────────────────────── */
struct symbol_offset {
	const char *name;
	uint64_t st_value;
	uint64_t file_offset;
	bool found;
};

static bool find_symbol_offsets(const char *path, struct symbol_offset *syms,
				size_t nsyms)
{
	int fd = -1;
	Elf *elf = nullptr;
	bool ok = false;

	if (elf_version(EV_CURRENT) == EV_NONE)
		return false;
	fd = open(path, O_RDONLY);
	if (fd < 0) {
		fprintf(stderr, "glusterfs_trace: open %s failed: %s\n", path,
			strerror(errno));
		return false;
	}
	elf = elf_begin(fd, ELF_C_READ, nullptr);
	if (!elf) {
		fprintf(stderr, "glusterfs_trace: elf_begin %s failed\n", path);
		goto out;
	}

	Elf_Scn *symtab = nullptr;
	GElf_Shdr symshdr;
	Elf_Scn *scn = nullptr;
	while ((scn = elf_nextscn(elf, scn)) != nullptr) {
		GElf_Shdr shdr;
		if (!gelf_getshdr(scn, &shdr))
			continue;
		if (shdr.sh_type == SHT_SYMTAB) {
			symtab = scn;
			symshdr = shdr;
			break;
		}
	}
	if (!symtab) {
		fprintf(stderr, "glusterfs_trace: %s has no .symtab\n", path);
		goto out;
	}

	Elf_Data *symdata = elf_getdata(symtab, nullptr);
	if (!symdata) {
		fprintf(stderr, "glusterfs_trace: no symtab data\n");
		goto out;
	}
	size_t entsize = symshdr.sh_entsize;
	if (!entsize)
		entsize = gelf_fsize(elf, ELF_T_SYM, 1, EV_CURRENT);
	if (!entsize)
		goto out;
	size_t count = symshdr.sh_size / entsize;

	for (size_t i = 1; i < count; i++) {
		GElf_Sym sym;
		if (!gelf_getsym(symdata, (int)i, &sym))
			continue;
		if (sym.st_shndx == SHN_UNDEF)
			continue;
		const char *name =
			elf_strptr(elf, symshdr.sh_link, sym.st_name);
		if (!name)
			continue;
		for (size_t j = 0; j < nsyms; j++) {
			if (syms[j].found || strcmp(name, syms[j].name))
				continue;
			syms[j].st_value = sym.st_value;
			syms[j].found = true;
		}
	}

	size_t phnum = 0;
	if (elf_getphdrnum(elf, &phnum) != 0)
		goto out;

	for (size_t j = 0; j < nsyms; j++) {
		if (!syms[j].found)
			continue;
		bool mapped = false;
		for (size_t p = 0; p < phnum; p++) {
			GElf_Phdr phdr;
			if (!gelf_getphdr(elf, (int)p, &phdr))
				continue;
			if (phdr.p_type != PT_LOAD)
				continue;
			if (syms[j].st_value < phdr.p_vaddr ||
			    syms[j].st_value >= phdr.p_vaddr + phdr.p_filesz)
				continue;
			syms[j].file_offset = syms[j].st_value - phdr.p_vaddr +
					      phdr.p_offset;
			mapped = true;
			break;
		}
		if (!mapped)
			fprintf(stderr,
				"glusterfs_trace: %s st_value=0x%llx outside PT_LOAD\n",
				syms[j].name,
				(unsigned long long)syms[j].st_value);
		syms[j].found = mapped;
	}

	ok = true;
	for (size_t j = 0; j < nsyms; j++) {
		if (syms[j].found)
			continue;
		fprintf(stderr, "glusterfs_trace: symbol %s not found or not mapped\n",
			syms[j].name);
		ok = false;
	}
out:
	if (elf)
		elf_end(elf);
	if (fd >= 0)
		close(fd);
	return ok;
}

/* ── Attach ───────────────────────────────────────────────────── */
static struct bpf_link *attach_one(struct bpf_program *prog, bool ret,
				   const char *path, uint64_t offset)
{
	struct bpf_link *link;

	if (!prog) {
		fprintf(stderr, "glusterfs_trace: missing BPF program\n");
		return nullptr;
	}
	link = bpf_program__attach_uprobe(prog, ret, -1, path, offset);
	if (!link) {
		fprintf(stderr,
			"glusterfs_trace: attach %s ret=%d off=0x%llx failed: %s\n",
			path, ret ? 1 : 0, (unsigned long long)offset,
			strerror(errno));
		return nullptr;
	}
	fprintf(stderr,
		"glusterfs_trace: attached %s ret=%d off=0x%llx link=%p\n",
		path, ret ? 1 : 0, (unsigned long long)offset, (void *)link);
	return link;
}

static void destroy_attach(void)
{
	if (link_dispatch) {
		bpf_link__destroy(link_dispatch);
		link_dispatch = nullptr;
	}
	if (link_resume) {
		bpf_link__destroy(link_resume);
		link_resume = nullptr;
	}
	if (link_send_entry) {
		bpf_link__destroy(link_send_entry);
		link_send_entry = nullptr;
	}
	if (link_send_ret) {
		bpf_link__destroy(link_send_ret);
		link_send_ret = nullptr;
	}
	if (ctl_rb) {
		ring_buffer__free(ctl_rb);
		ctl_rb = nullptr;
	}
	if (path_rb) {
		ring_buffer__free(path_rb);
		path_rb = nullptr;
	}
	if (skel) {
		glusterfs_trace_bpf__destroy(skel);
		skel = nullptr;
	}
}

/* ── Public API ───────────────────────────────────────────────── */
void init_glusterfs_trace(const char *fuse_so, const char *trace_root, int attach)
{
	const char *so = fuse_so ? fuse_so : "";
	g_trace_root = trace_root ? trace_root : "";

	if (!attach) {
		fprintf(stderr,
			"glusterfs_trace: server executor, no uprobe attach\n");
		return;
	}
	if (g_trace_root.empty()) {
		fprintf(stderr,
			"glusterfs_trace: client attach without trace_root; disabled\n");
		return;
	}

	struct symbol_offset syms[] = {
		{"fuse_dispatch", 0, 0, false},
		{"fuse_fop_resume", 0, 0, false},
		{"send_fuse_iov", 0, 0, false},
	};
	if (!find_symbol_offsets(so, syms, sizeof(syms) / sizeof(syms[0]))) {
		fprintf(stderr, "glusterfs_trace: symbol resolution failed\n");
		return;
	}
	for (const auto &s : syms) {
		fprintf(stderr,
			"glusterfs_trace: %s st_value=0x%llx file_offset=0x%llx\n",
			s.name, (unsigned long long)s.st_value,
			(unsigned long long)s.file_offset);
	}

	skel = glusterfs_trace_bpf__open_and_load();
	if (!skel) {
		fprintf(stderr, "glusterfs_trace: BPF load failed\n");
		return;
	}
	ctl_rb = ring_buffer__new(bpf_map__fd(skel->maps.gf_ctl_events),
				  handle_ctl, nullptr, nullptr);
	path_rb = ring_buffer__new(bpf_map__fd(skel->maps.gf_path_events),
				   handle_path, nullptr, nullptr);
	if (!ctl_rb || !path_rb) {
		fprintf(stderr, "glusterfs_trace: ring buffer creation failed\n");
		destroy_attach();
		return;
	}

	link_dispatch = attach_one(skel->progs.gf_dispatch, false, so,
				   syms[0].file_offset);
	link_resume = attach_one(skel->progs.gf_resume, false, so,
				 syms[1].file_offset);
	link_send_entry = attach_one(skel->progs.gf_send_entry, false, so,
				     syms[2].file_offset);
	link_send_ret = attach_one(skel->progs.gf_send_ret, true, so,
				   syms[2].file_offset);
	if (!link_dispatch || !link_resume || !link_send_entry || !link_send_ret) {
		fprintf(stderr, "glusterfs_trace: attach failed; tracing disabled\n");
		destroy_attach();
		return;
	}
	fprintf(stderr, "glusterfs_trace: 3 uprobes + 1 uretprobe attached\n");
}

void start_glusterfs_trace(void)
{
	/* Forked child: discard events left over after the previous
	 * stop_collect.  Do not clear pending/nodeid here: the parent owns
	 * that state and a child COW copy must not mutate it. */
	if (skel && ctl_rb)
		ring_buffer__consume(ctl_rb);
	if (skel && path_rb)
		ring_buffer__consume(path_rb);
}

void stop_collect_glusterfs_trace(void)
{
	if (skel && ctl_rb) {
		int n = ring_buffer__consume(ctl_rb);
		if (n < 0)
			fprintf(stderr, "glusterfs_trace: ctl ring consume: %d\n", n);
	}
	if (skel && path_rb) {
		int n = ring_buffer__consume(path_rb);
		if (n < 0)
			fprintf(stderr, "glusterfs_trace: path ring consume: %d\n", n);
	}

	g_final_events.clear();
	g_dropped_events = 0;
	g_missing_d = 0;
	g_missing_r = 0;
	g_missing_s = 0;
	g_drop_xattr = 0;
	g_out_of_root = 0;
	g_path2_out_of_root = 0;
	g_unmapped_opcode = 0;
	process_merged_events();
	write_events();

	/* Parent-only cleanup: inode pointers can be reused next round. */
	g_pending.clear();
	g_nodeid_path.clear();
	ctl_read_buf.clear();
	path_read_buf.clear();
}
