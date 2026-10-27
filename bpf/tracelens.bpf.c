// SPDX-License-Identifier: GPL-2.0 OR MIT
//
// Syscall latency per process. Each syscall entry stores a timestamp keyed by TID; the
// matching exit computes the duration and folds it into per-CPU stats keyed by
// (tgid, op). Detailed per-call events only leave the kernel for the one TGID that
// userspace selects in `config`.

#include "headers/types.h"

#define HIST_SLOTS 40 // log2(ns) buckets; slot 39 starts at ~9 minutes
#define EAGAIN 11
#define EINPROGRESS 115

enum op { OP_READ, OP_WRITE, OP_OPENAT, OP_CONNECT, NUM_OPS };

// Context layouts from /sys/kernel/tracing/events/syscalls/sys_{enter,exit}_*/format.
// Every syscall tracepoint has the same 8-byte common header and int syscall number.
struct sys_exit_ctx {
	__u64 common;
	__s32 nr;
	__s64 ret;
};

struct trace_config {
	__u32 target_tgid;      // 0 = no detailed events
	__u32 pad;
	__u64 min_duration_ns;  // detailed events shorter than this are dropped in-kernel
};

struct stats_key {
	__u32 tgid;
	__u32 op;
};

struct op_stats {
	__u64 count;
	__u64 errors; // excludes EAGAIN and EINPROGRESS, see record()
	__u64 bytes; // bytes the syscall returned (read/write only), not device I/O
	__u64 total_ns;
	__u64 hist[HIST_SLOTS];
};

struct event {
	__u64 ts_ns;
	__u64 duration_ns;
	__s64 ret;
	__u32 tgid;
	__u32 tid;
	__u32 op;
	char comm[16];
};
// Ring buffers carry untyped bytes; this keeps struct event in BTF for bpf2go.
const struct event *unused_event __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct trace_config);
} config SEC(".maps");

// In-flight syscalls by TID. LRU so threads that die mid-syscall can't fill it.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u32);
	__type(value, __u64);
} active SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct stats_key);
	__type(value, struct op_stats);
} stats SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024); // userspace may resize before load
} events SEC(".maps");

// Events that didn't fit in the ring buffer.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, __u64);
} drops SEC(".maps");

static __always_inline __u32 log2_slot(__u64 v)
{
	__u32 r = 0;
	if (v >> 32) { v >>= 32; r += 32; }
	if (v >> 16) { v >>= 16; r += 16; }
	if (v >> 8)  { v >>= 8;  r += 8; }
	if (v >> 4)  { v >>= 4;  r += 4; }
	if (v >> 2)  { v >>= 2;  r += 2; }
	if (v >> 1)  { r += 1; }
	return r < HIST_SLOTS ? r : HIST_SLOTS - 1;
}

static __always_inline int on_enter(void)
{
	__u32 tid = (__u32)bpf_get_current_pid_tgid();
	__u64 ts = bpf_ktime_get_ns();
	bpf_map_update_elem(&active, &tid, &ts, BPF_ANY);
	return 0;
}

static __always_inline void record(__u32 tgid, __u32 op, __u64 dur, __s64 ret)
{
	struct stats_key key = { .tgid = tgid, .op = op };
	struct op_stats *st = bpf_map_lookup_elem(&stats, &key);
	if (!st) {
		static const struct op_stats zero;
		bpf_map_update_elem(&stats, &key, &zero, BPF_NOEXIST);
		st = bpf_map_lookup_elem(&stats, &key);
		if (!st)
			return; // map full
	}
	// Per-CPU value: no other CPU writes this copy, so plain increments are safe.
	st->count++;
	st->total_ns += dur;
	st->hist[log2_slot(dur)]++;
	// Non-blocking I/O returns EAGAIN/EINPROGRESS as a normal step (Go's netpoller does
	// this for every socket and pipe), so they aren't failures.
	if (ret < 0) {
		if (ret != -EAGAIN && ret != -EINPROGRESS)
			st->errors++;
	} else if (op == OP_READ || op == OP_WRITE) {
		st->bytes += ret;
	}
}

static __always_inline void emit(__u32 tgid, __u32 tid, __u32 op, __u64 now, __u64 dur, __s64 ret)
{
	__u32 zero = 0;
	struct trace_config *cfg = bpf_map_lookup_elem(&config, &zero);
	if (!cfg || cfg->target_tgid != tgid || dur < cfg->min_duration_ns)
		return;

	struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__u64 *n = bpf_map_lookup_elem(&drops, &zero);
		if (n)
			(*n)++;
		return;
	}
	e->ts_ns = now;
	e->duration_ns = dur;
	e->ret = ret;
	e->tgid = tgid;
	e->tid = tid;
	e->op = op;
	bpf_get_current_comm(e->comm, sizeof(e->comm));
	bpf_ringbuf_submit(e, 0);
}

static __always_inline int on_exit(__u32 op, __s64 ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tid = (__u32)id, tgid = id >> 32;

	__u64 *start = bpf_map_lookup_elem(&active, &tid);
	if (!start)
		return 0; // entry happened before we attached
	__u64 now = bpf_ktime_get_ns();
	__u64 dur = now - *start;
	bpf_map_delete_elem(&active, &tid);

	record(tgid, op, dur, ret);
	emit(tgid, tid, op, now, dur, ret);
	return 0;
}

#define SYSCALL_PROBES(name, op)                                         \
	SEC("tracepoint/syscalls/sys_enter_" #name)                      \
	int enter_##name(void *ctx) { return on_enter(); }               \
	SEC("tracepoint/syscalls/sys_exit_" #name)                       \
	int exit_##name(struct sys_exit_ctx *ctx) { return on_exit(op, ctx->ret); }

SYSCALL_PROBES(read, OP_READ)
SYSCALL_PROBES(write, OP_WRITE)
SYSCALL_PROBES(openat, OP_OPENAT)
SYSCALL_PROBES(connect, OP_CONNECT)

// Drop a process's stats when its main thread exits, so the stats map only holds live
// processes. If the leader exits before other threads, their later syscalls recreate
// the entry and it stays until the agent stops; rare enough to accept.
SEC("tracepoint/sched/sched_process_exit")
int process_exit(void *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tid = (__u32)id, tgid = id >> 32;

	bpf_map_delete_elem(&active, &tid);
	if (tid != tgid)
		return 0;
	for (__u32 op = 0; op < NUM_OPS; op++) {
		struct stats_key key = { .tgid = tgid, .op = op };
		bpf_map_delete_elem(&stats, &key);
	}
	return 0;
}

char LICENSE[] SEC("license") = "Dual MIT/GPL";
