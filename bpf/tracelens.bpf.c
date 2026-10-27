// SPDX-License-Identifier: GPL-2.0 OR MIT
//
// Syscall latency and run-queue delay per process. Each syscall entry stores a timestamp
// keyed by TID; the matching exit computes the duration and folds it into per-CPU stats
// keyed by (tgid, op). Run-queue delay (wakeup to running) goes into the same map as the
// pseudo-op OP_RUNQ. Detailed per-call events only leave the kernel for the one TGID that
// userspace selects in `config`.

#include "headers/types.h"

#define HIST_SLOTS 40 // log2(ns) buckets; slot 39 starts at ~9 minutes
#define EAGAIN 11
#define EINPROGRESS 115

enum op { OP_READ, OP_WRITE, OP_OPENAT, OP_CONNECT, OP_FSYNC, OP_RUNQ, NUM_OPS };

// Context layouts from /sys/kernel/tracing/events/syscalls/sys_{enter,exit}_*/format.
// Every syscall tracepoint has the same 8-byte common header and int syscall number.
struct sys_exit_ctx {
	__u64 common;
	__s32 nr;
	__s64 ret;
};

// Only the fields we read. preserve_access_index makes clang emit CO-RE relocations, so
// the loader patches in the running kernel's real offsets from its BTF.
struct task_struct {
	int pid;
	int tgid;
	char comm[16];
} __attribute__((preserve_access_index));

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

// Runnable-since timestamps by TID, for run-queue delay.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u32);
	__type(value, __u64);
} runnable SEC(".maps");

// Up to NUM_OPS keys per live process (kernel threads included, via runq). The value is
// 352 bytes per CPU, so 8192 entries is ~2.9 MB per CPU. When it's full, new processes
// aren't tracked and lost[LOST_STATS] counts the misses.
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

enum { LOST_EVENTS, LOST_STATS, NUM_LOST };

// What couldn't be recorded: events that didn't fit in the ring buffer, and updates for
// keys that didn't fit in the stats map.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, NUM_LOST);
	__type(key, __u32);
	__type(value, __u64);
} lost SEC(".maps");

static __always_inline void count_lost(__u32 kind)
{
	__u64 *n = bpf_map_lookup_elem(&lost, &kind);
	if (n)
		(*n)++;
}

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
		if (!st) {
			count_lost(LOST_STATS);
			return;
		}
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

// task names the process the event is about when it isn't the current one (sched_switch
// runs on behalf of the task being switched out); NULL means current.
static __always_inline void emit(__u32 tgid, __u32 tid, __u32 op, __u64 now, __u64 dur, __s64 ret,
				 struct task_struct *task)
{
	__u32 zero = 0;
	struct trace_config *cfg = bpf_map_lookup_elem(&config, &zero);
	if (!cfg || cfg->target_tgid != tgid || dur < cfg->min_duration_ns)
		return;

	struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		count_lost(LOST_EVENTS);
		return;
	}
	e->ts_ns = now;
	e->duration_ns = dur;
	e->ret = ret;
	e->tgid = tgid;
	e->tid = tid;
	e->op = op;
	if (task)
		bpf_probe_read_kernel_str(e->comm, sizeof(e->comm), task->comm);
	else
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
	emit(tgid, tid, op, now, dur, ret, 0);
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
SYSCALL_PROBES(fsync, OP_FSYNC)
SYSCALL_PROBES(fdatasync, OP_FSYNC)

static __always_inline void mark_runnable(__u32 tid)
{
	if (tid == 0) // idle task
		return;
	__u64 ts = bpf_ktime_get_ns();
	bpf_map_update_elem(&runnable, &tid, &ts, BPF_ANY);
}

// tp_btf programs get the raw tracepoint arguments as an array of u64.
SEC("tp_btf/sched_wakeup")
int sched_wakeup(__u64 *ctx)
{
	struct task_struct *p = (void *)ctx[0];
	mark_runnable(p->pid);
	return 0;
}

SEC("tp_btf/sched_wakeup_new")
int sched_wakeup_new(__u64 *ctx)
{
	struct task_struct *p = (void *)ctx[0];
	mark_runnable(p->pid);
	return 0;
}

// sched_switch(bool preempt, prev, next, unsigned int prev_state)
SEC("tp_btf/sched_switch")
int sched_switch(__u64 *ctx)
{
	struct task_struct *prev = (void *)ctx[1], *next = (void *)ctx[2];
	__u32 prev_state = (__u32)ctx[3];

	// prev stays on the run queue if it was preempted or is still TASK_RUNNING (0).
	// Checking prev_state alone misses a task preempted between setting
	// TASK_INTERRUPTIBLE and calling schedule(): non-zero state, still queued.
	bool preempt = ctx[0];
	if (preempt || prev_state == 0)
		mark_runnable(prev->pid);

	__u32 tid = next->pid;
	__u64 *since = bpf_map_lookup_elem(&runnable, &tid);
	if (!since)
		return 0;
	__u64 now = bpf_ktime_get_ns();
	__u64 dur = now - *since;
	bpf_map_delete_elem(&runnable, &tid);

	__u32 tgid = next->tgid;
	record(tgid, OP_RUNQ, dur, 0);
	emit(tgid, tid, OP_RUNQ, now, dur, 0, next);
	return 0;
}

// Drop a process's stats when its main thread exits, so the stats map only holds live
// processes. If the leader exits before other threads, their later syscalls recreate
// the entry and it stays until the agent stops; rare enough to accept.
SEC("tracepoint/sched/sched_process_exit")
int process_exit(void *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tid = (__u32)id, tgid = id >> 32;

	bpf_map_delete_elem(&active, &tid);
	bpf_map_delete_elem(&runnable, &tid);
	if (tid != tgid)
		return 0;
	for (__u32 op = 0; op < NUM_OPS; op++) {
		struct stats_key key = { .tgid = tgid, .op = op };
		bpf_map_delete_elem(&stats, &key);
	}
	return 0;
}

char LICENSE[] SEC("license") = "Dual MIT/GPL";
