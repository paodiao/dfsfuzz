// SPDX-License-Identifier: GPL-2.0
/*
 * hmdfs_trace.cc — Executor-side HMDFS BPF + perf trace loading, control,
 * and collection.
 *
 * Collects two independent streams of per-function events:
 *   1. BPF kretprobes on 15 merge-view VFS functions (hmdfs_trace.bpf.c)
 *   2. perf_event on the hmdfs_writepage_cb_exit kernel tracepoint
 *
 * Both streams are merged into a single sorted event list, serialised to
 * the executor output stream.  The fuzzer-side parser sees a single flat
 * timeline of func_id-labelled events with identical struct layout.
 *
 * Compiled as C++ for compat with the executor's write_output helpers.
 */

#include <algorithm>
#include <cstdint>
#include <string.h>
#include <stdio.h>
#include <stdlib.h>
#include <fcntl.h>
#include <unistd.h>
#include <sys/mman.h>
#include <sys/ioctl.h>
#include <sys/syscall.h>
#include <linux/perf_event.h>

#include "hmdfs_trace.skel.h"
#include "hmdfs_trace.h"

/* ── Local event buffer (matches BPF layout, no name collision) ─ */
struct hmdfs_event {
	unsigned long long timestamp;
	unsigned int func_id;
	int ret;
	unsigned long long ino;
	unsigned long long args[6];
	unsigned long long off;
};

/* ── Per‑tracepoint field descriptor (from /sys/.../format) ──── */
struct tp_field {
	char name[64];
	int offset;
	int size;
};

/* ── Perf event for a single tracepoint ──────────────────────── */
struct perf_state {
	int fd;                     // perf_event_open fd
	void *buf;                  // mmap'd ring buffer (2 pages)
	int page_cnt;
	struct tp_field fields[10]; // parsed format fields
	int field_count;
	int off_remote_ino;
	int off_page_index;
	int off_device_id;
	int off_err;
	int off_ino_raw;
};

static struct perf_state wb_perf;
/* ── Executor output helpers (provided by executor.cc) ──────── */

typedef unsigned int uint32;
typedef unsigned long long uint64;

extern uint32 *write_output(uint32 v);
extern uint32 *write_output_64(uint64 v);
extern uint64_t tsc_ns_to_global(uint64_t ns);

static struct hmdfs_trace_bpf *skel;
static struct ring_buffer *rb;
static struct hmdfs_event collected[MAX_HMDFS_TRACE_EVENTS];
static int collected_count;

/* ── Ring buffer callback (BPF kretprobe events) ─────────────── */
static int handle_event(void *ctx, void *data, size_t data_sz) __attribute__((unused));
static int handle_event(void *ctx, void *data, size_t data_sz)
{
	if (collected_count >= MAX_HMDFS_TRACE_EVENTS)
		return 0;
	memcpy(&collected[collected_count], data,
	       sizeof(struct hmdfs_event));
	collected_count++;
	return 0;
}

/* ── Perf helper: parse format file for field offsets ────────── */
static int parse_tp_format(const char *event_name, struct tp_field *fields,
			   int max_fields)
{
	char path[256];
	snprintf(path, sizeof(path),
		 "/sys/kernel/debug/tracing/events/hmdfs/%s/format",
		 event_name);
	FILE *f = fopen(path, "r");
	if (!f)
		return -1;

	int count = 0;
	char line[256];
	while (fgets(line, sizeof(line), f) && count < max_fields) {
		/* lines look like:
		 *   field:u64 ino_raw; offset:168; size:8; signed:0;
		 * skip "field:" prefix, then parse type name offset size.
		 */
		char *p = strstr(line, "field:");
		if (!p)
			continue;
		p += 6; // skip "field:"

		/* find the last space before ';' — that's the field name */
		char *semi = strchr(p, ';');
		if (!semi)
			continue;
		*semi = '\0';
		char *last_space = strrchr(p, ' ');
		strncpy(fields[count].name,
			last_space ? last_space + 1 : p,
			sizeof(fields[count].name) - 1);
		fields[count].name[sizeof(fields[count].name) - 1] = '\0';

		int offset = 0, size = 0;
		semi[0] = ';'; // restore
		// sscanf against line (it contains the "field:" prefix; p points
		// past it) with a leading space in the format to skip the tab the
		// format file lines start with.
		if (sscanf(line, " field:%*[^;]; offset:%d; size:%d;",
			   &offset, &size) != 2)
			continue;

		fields[count].offset = offset;
		fields[count].size = size;
		count++;
	}
	fclose(f);
	return count;
}

static int find_field(const struct tp_field *fields, int count,
		      const char *name)
{
	for (int i = 0; i < count; i++)
		if (!strcmp(fields[i].name, name))
			return fields[i].offset;
	return -1;
}

/* ── Perf: open writepage tracepoint ─────────────────────────── */
static int open_wb_tracepoint(struct perf_state *ps)
{
	/* read tracepoint id */
	char id_path[256];
	snprintf(id_path, sizeof(id_path),
		 "/sys/kernel/debug/tracing/events/hmdfs/"
		 "hmdfs_writepage_cb_exit/id");
	FILE *f = fopen(id_path, "r");
	if (!f)
		return -1;
	int tp_id = 0;
	fscanf(f, "%d", &tp_id);
	fclose(f);
	if (tp_id <= 0)
		return -1;

	/* parse format file for field offsets */
	int fc = parse_tp_format("hmdfs_writepage_cb_exit",
				 ps->fields, 10);
	if (fc <= 0)
		return -1;
	ps->field_count = fc;
	ps->off_remote_ino = find_field(ps->fields, fc, "remote_ino");
	ps->off_page_index = find_field(ps->fields, fc, "page_index");
	ps->off_device_id  = find_field(ps->fields, fc, "device_id");
	ps->off_err        = find_field(ps->fields, fc, "err");
	ps->off_ino_raw    = find_field(ps->fields, fc, "ino_raw");

	/* open perf event */
	struct perf_event_attr attr = {};
	attr.type = PERF_TYPE_TRACEPOINT;
	attr.size = sizeof(attr);
	attr.sample_period = 1;
	attr.sample_type = PERF_SAMPLE_TIME | PERF_SAMPLE_RAW;
	attr.disabled = 1;
	attr.config = tp_id;

	ps->fd = syscall(__NR_perf_event_open, &attr, -1, 0, -1, 0);
	if (ps->fd < 0)
		return -1;

	ps->page_cnt = 5; /* (1+4) pages: 1 metadata + 4 data — perf ring requires (1+2^n) */
	ps->buf = mmap(NULL, ps->page_cnt * 4096, PROT_READ | PROT_WRITE,
		       MAP_SHARED, ps->fd, 0);
	if (ps->buf == MAP_FAILED) {
		close(ps->fd);
		ps->fd = -1;
		return -1;
	}

	ioctl(ps->fd, PERF_EVENT_IOC_DISABLE, 0); // enabled later in start_hmdfs_trace
	return 0;
}

static void close_wb_tracepoint(struct perf_state *ps) __attribute__((unused));
static void close_wb_tracepoint(struct perf_state *ps)
{
	/* reserved for executor shutdown cleanup — not currently called;
	 * kernel reclaims fd/mmap on process exit */
	if (ps->fd >= 0) {
		ioctl(ps->fd, PERF_EVENT_IOC_DISABLE, 0);
		munmap(ps->buf, ps->page_cnt * 4096);
		close(ps->fd);
		ps->fd = -1;
	}
}

/* ── Perf: drain ring buffer into collected[] ────────────────── */
static void drain_wb_events(struct perf_state *ps)
{
	if (ps->fd < 0)
		return;

	/* The perf ring mmap is not inherited across fork() (the kernel
	 * marks the VMA VM_DONTCOPY), so the loop process (a child of
	 * do_sandbox_none) has no mapping for wb_perf.buf even though it
	 * inherited the perf fd. Re-establish the mapping here when it is
	 * gone; if that fails, skip writepage events for this round. */
	if (ps->buf && ps->page_cnt > 0) {
		unsigned char v;
		if (mincore(ps->buf, 4096, &v) != 0) {
			void *nb = mmap(NULL, (size_t)ps->page_cnt * 4096,
					PROT_READ | PROT_WRITE, MAP_SHARED,
					ps->fd, 0);
			if (nb != MAP_FAILED)
				ps->buf = nb;
			else
				return;
		}
	}

	struct perf_event_mmap_page *mp = (struct perf_event_mmap_page *)ps->buf;
	uint64_t head = __atomic_load_n(&mp->data_head, __ATOMIC_ACQUIRE);
	uint64_t tail = mp->data_tail;

	uint64_t base = (uint64_t)ps->buf + mp->data_offset;
	uint64_t size = mp->data_size;

	while (tail < head) {
		if (collected_count >= MAX_HMDFS_TRACE_EVENTS)
			break;

		uint64_t pos = base + (tail % size);
		struct perf_event_header *hdr =
			(struct perf_event_header *)pos;

		if (hdr->size == 0 || tail + hdr->size > head)
			break;

		/* advance past header to sample data */
		unsigned char *data = (unsigned char *)(pos + sizeof(*hdr));
		uint64_t sample_time = *(uint64_t *)data;
		data += 8;

		uint32_t raw_sz = *(uint32_t *)data;
		data += 4;
		(void)raw_sz;
		/* data now points at raw trace buffer (common +
		 * custom fields) — use format offsets */

		collected[collected_count].timestamp = sample_time;
		collected[collected_count].func_id = FUNC_WRITEPAGE_CB;
		collected[collected_count].ret =
			ps->off_err >= 0
			? *(int *)(data + ps->off_err) : 0;
		collected[collected_count].ino =
			ps->off_ino_raw >= 0
			? *(uint64_t *)(data + ps->off_ino_raw) : 0;
		collected[collected_count].args[0] =
			ps->off_remote_ino >= 0
			? *(uint64_t *)(data + ps->off_remote_ino) : 0;
		collected[collected_count].args[1] =
			ps->off_device_id >= 0
			? *(uint64_t *)(data + ps->off_device_id) : 0;
		collected[collected_count].args[2] =
			ps->off_page_index >= 0
			? *(unsigned long *)(data + ps->off_page_index)
			: 0;
		collected[collected_count].args[3] = 0;
		collected[collected_count].args[4] = 0;
		collected[collected_count].args[5] = 0;

		collected_count++;

		tail += hdr->size;
	}
	__atomic_store_n(&mp->data_tail, tail, __ATOMIC_RELEASE);
}

/* ── Comparator for std::sort — by timestamp ascending ───────── */
static bool event_before(const struct hmdfs_event &a,
			 const struct hmdfs_event &b)
{
	return a.timestamp < b.timestamp;
}

/* ── Public API ──────────────────────────────────────────────── */

void init_hmdfs_trace(void)
{
	/* 1. load BPF kretprobe skeleton */
	skel = hmdfs_trace_bpf__open_and_load();
	if (!skel) {
		fprintf(stderr, "hmdfs_trace: BPF load failed\n");
	} else if (hmdfs_trace_bpf__attach(skel) != 0) {
		fprintf(stderr, "hmdfs_trace: BPF attach failed\n");
		hmdfs_trace_bpf__destroy(skel);
		skel = NULL;
	} else {
		rb = ring_buffer__new(bpf_map__fd(skel->maps.events),
				      handle_event, NULL, NULL);
		if (!rb) {
			fprintf(stderr, "hmdfs_trace: ring buffer failed\n");
			hmdfs_trace_bpf__destroy(skel);
			skel = NULL;
		} else {
			fprintf(stderr, "hmdfs_trace: %d kretprobes attached\n", 15);
		}
	}

	/* 2. open perf event for writepage tracepoint */
	if (open_wb_tracepoint(&wb_perf) == 0) {
		fprintf(stderr, "hmdfs_trace: writepage tracepoint attached\n");
	} else {
		fprintf(stderr, "hmdfs_trace: writepage tracepoint unavailable\n");
	}
}

void start_hmdfs_trace(void)
{
	collected_count = 0;
	if (wb_perf.fd >= 0)
		ioctl(wb_perf.fd, PERF_EVENT_IOC_ENABLE, 0);
}

void stop_collect_hmdfs_trace(void)
{
	/* 1. drain BPF kretprobe events */
	if (skel && rb) {
		ring_buffer__consume(rb);
	}

	/* 2. drain perf writepage events */
	if (wb_perf.fd >= 0)
		ioctl(wb_perf.fd, PERF_EVENT_IOC_DISABLE, 0);
	drain_wb_events(&wb_perf);

	/* 3. sort merged events by timestamp */
	if (collected_count > 0) {
		std::sort(collected, collected + collected_count,
			  event_before);
		/* convert bpf_ktime_get_ns() timestamps to the global
		 * raw-TSC domain (shared with per-call windows) */
		for (int i = 0; i < collected_count; i++)
			collected[i].timestamp = tsc_ns_to_global(collected[i].timestamp);
	}
	fprintf(stderr, "executor %d hmdfs trace collected=%d\n", (int)getpid(), collected_count);

	/* 4. write to output */
	write_output((uint32)collected_count);
	for (int i = 0; i < collected_count; i++) {
		write_output_64(collected[i].timestamp);
		write_output(collected[i].func_id);
		write_output((uint32)collected[i].ret);
		write_output_64(collected[i].ino);
		for (int j = 0; j < 6; j++)
			write_output_64(collected[i].args[j]);
		write_output_64(collected[i].off);
	}

	/* 5. reset for the next round: this runs in the parent (loop) process,
	 * whose collected_count is never reset by the forked child's
	 * start_hmdfs_trace (COW copy), so without this every round re-emits
	 * all historical events and repeatedly re-converts their timestamps. */
	collected_count = 0;
}
