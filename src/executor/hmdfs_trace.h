// SPDX-License-Identifier: GPL-2.0
/*
 * hmdfs_trace.h — Public API for HMDFS function tracing
 */

#ifndef HMDFS_TRACE_H
#define HMDFS_TRACE_H

#define MAX_HMDFS_TRACE_EVENTS 4096

/* func_id constants — keep in sync with hmdfs_trace.bpf.c and HmdfsFuncName */
#define FUNC_WRITEPAGE_CB 15

void init_hmdfs_trace(void);
void start_hmdfs_trace(void);
void stop_collect_hmdfs_trace(void);

#endif /* HMDFS_TRACE_H */
