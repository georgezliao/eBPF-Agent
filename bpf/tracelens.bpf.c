// SPDX-License-Identifier: GPL-2.0 OR MIT
//
// Syscall latency and run-queue delay per process. Each syscall entry stores a timestamp
// keyed by TID; the matching exit computes the duration and folds it into per-CPU stats
// keyed by (tgid, op). Pseudo-ops go into the same map: OP_RUNQ is run-queue delay (wakeup
// to running), OP_READ_WAIT the time from a read returning EAGAIN until a read on the same
// fd returns something else, OP_RESPONSE the part of that wait after the last write on the
// fd, and OP_RETRANS counts TCP retransmits. Detailed per-call events only leave the kernel
// for the one TGID that userspace selects in `config`. Off-CPU time by stack is
// collected for one TGID only, since taking a stack on every sleep isn't free.

#include "headers/types.h"

#define HIST_SLOTS 40 // log2(ns) buckets; slot 39 starts at ~9 minutes
#define EAGAIN 11
#define EINPROGRESS 115
#define TCP_SYN_SENT 2
#define TCP_CLOSE 7
#define MAX_STACK_DEPTH 32 // the frames nearest the sleep are the useful ones
#define NO_STACK -1
#define BPF_F_USER_STACK (1ULL << 8)
#define TASK_DEAD 0x80

enum op { OP_READ, OP_WRITE, OP_OPENAT, OP_CONNECT, OP_FSYNC, OP_RUNQ, OP_READ_WAIT, OP_RESPONSE,
       OP_RETRANS, NUM_OPS };

// Context layouts from /sys/kernel/tracing/events/syscalls/sys_{enter,exit}_*/format.
// Every syscall tracepoint has the same 8-byte common header and int syscall number.
// read, write and close all take the fd as their first argument.
struct sys_enter_fd_ctx {
	__u64 common;
	__s32 nr;
	__u64 fd;
};

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
	// Benchmark "raw" mode: send every call of every process to userspace and skip
	// in-kernel aggregation, so userspace has to do it instead.
	__u32 export_all;
	__u64 min_duration_ns;  // detailed events shorter than this are dropped in-kernel
	__u32 offcpu_tgid;      // 0 = no off-CPU stacks
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

struct inflight {
	__u64 ts;
	__u64 fd; // set for read and write, 0 for other syscalls
};

// In-flight syscalls by TID. LRU so entries left by threads that died mid-syscall get
// evicted; a reused TID overwrites its stale entry at its first syscall entry.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u32);
	__type(value, struct inflight);
} active SEC(".maps");

struct fd_key {
	__u32 tgid;
	__u32 fd;
};

struct pending_read {
	__u64 since;      // first EAGAIN
	__u64 last_write; // last write on the fd since then, 0 if none
};

// First EAGAIN read per (tgid, fd) that hasn't been followed by a completed read yet.
// Keyed by process and fd rather than thread, because Go (and any epoll loop with a
// thread pool) may retry the read from a different thread. close() deletes the entry so
// a reused fd number doesn't inherit it; LRU covers fds closed some other way.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, struct fd_key);
	__type(value, struct pending_read);
} read_wait SEC(".maps");

// Runnable-since timestamps by TID, for run-queue delay.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u32);
	__type(value, __u64);
} runnable SEC(".maps");

// Up to NUM_OPS keys per process (kernel threads included, via runq). Userspace deletes a
// process's keys after it has read their final values and seen the process is gone; the
// kernel never deletes them, so short-lived processes still get counted. The value is 352
// bytes per CPU, so 8192 entries is ~2.9 MB per CPU. When it's full, new processes aren't
// tracked and lost[LOST_STATS] counts the misses.
struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct stats_key);
	__type(value, struct op_stats);
} stats SEC(".maps");

// Ids in kernel_stacks and user_stacks, or NO_STACK.
struct offcpu_key {
	__s64 kernel;
	__s64 user;
};

struct sleep_start {
	__u64 ts;
	struct offcpu_key stack;
};

// Threads of the off-CPU target that are asleep, by TID. Userspace also reads this, to
// count sleeps that haven't ended yet.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, __u32);
	__type(value, struct sleep_start);
} sleeping SEC(".maps");

// Preallocated, so each costs about 4 MiB on every command. Without a flag, a new stack
// that hashes to a taken bucket isn't stored, so losses start around 100 distinct stacks.
// Separate maps so that user stacks, which vary more, can't crowd out kernel stacks.
struct {
	__uint(type, BPF_MAP_TYPE_STACK_TRACE);
	__uint(max_entries, 8192);
	__type(key, __u32);
	__type(value, __u64[MAX_STACK_DEPTH]);
} kernel_stacks SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_STACK_TRACE);
	__uint(max_entries, 8192);
	__type(key, __u32);
	__type(value, __u64[MAX_STACK_DEPTH]);
} user_stacks SEC(".maps");

struct offcpu_stat {
	__u64 count;
	__u64 total_ns;
	__u64 max_ns;
};

// Off-CPU time per pair of stacks. Updated from whichever CPU runs the wakeup, so unlike
// `stats` it needs atomic adds.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, struct offcpu_key);
	__type(value, struct offcpu_stat);
} offcpu SEC(".maps");

// Which process owns a TCP socket, by struct sock pointer. Retransmits mostly fire in
// softirq, where the current task is unrelated, so the owner is recorded earlier from
// process context.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, __u32);
} sock_owner SEC(".maps");

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

static __always_inline int on_enter(__u64 fd)
{
	__u32 tid = (__u32)bpf_get_current_pid_tgid();
	struct inflight in = { .ts = bpf_ktime_get_ns(), .fd = fd };
	bpf_map_update_elem(&active, &tid, &in, BPF_ANY);
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

static __always_inline void emit(__u32 tgid, __u32 tid, __u32 op, __u64 now, __u64 dur, __s64 ret,
				 struct task_struct *task)
{
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

// deliver handles one finished call or run-queue wait: aggregate it, and send it to
// userspace if it belongs to the traced process. task names the process when it isn't
// the current one (sched_switch runs on behalf of the task being switched out); NULL
// means current.
static __always_inline void deliver(__u32 tgid, __u32 tid, __u32 op, __u64 now, __u64 dur,
				    __s64 ret, struct task_struct *task)
{
	__u32 zero = 0;
	struct trace_config *cfg = bpf_map_lookup_elem(&config, &zero);
	if (!cfg)
		return;
	if (cfg->export_all) {
		emit(tgid, tid, op, now, dur, ret, task);
		return;
	}
	record(tgid, op, dur, ret);
	if (cfg->target_tgid == tgid && dur >= cfg->min_duration_ns)
		emit(tgid, tid, op, now, dur, ret, task);
}

// track_read_wait handles a finished read on fd. With non-blocking I/O the read itself
// is fast either way; the wait is the gap between the first EAGAIN and the read that
// finally returns data, EOF or a real error.
static __always_inline void track_read_wait(__u32 tgid, __u32 tid, __u32 fd, __u64 now, __s64 ret)
{
	struct fd_key key = { .tgid = tgid, .fd = fd };
	if (ret == -EAGAIN) {
		// NOEXIST keeps the first attempt when the reader retries more than once.
		struct pending_read pending = { .since = now };
		bpf_map_update_elem(&read_wait, &key, &pending, BPF_NOEXIST);
		return;
	}
	// Lookup and delete aren't atomic: two threads finishing reads on the same fd at
	// once can both report the wait. Rare, and not worth a lock in this path.
	struct pending_read *p = bpf_map_lookup_elem(&read_wait, &key);
	if (!p)
		return;
	__u64 since = p->since, last_write = p->last_write;
	bpf_map_delete_elem(&read_wait, &key);
	deliver(tgid, tid, OP_READ_WAIT, now, now - since, ret, 0);
	if (last_write && ret > 0)
		deliver(tgid, tid, OP_RESPONSE, now, now - last_write, ret, 0);
}

// note_write stamps a successful write on an fd that has a read pending. Only those fds:
// a pending read is how a request/response connection looks in Go, where the reader
// parks on the connection before the next request goes out, and it keeps file writes
// and logging out of the map.
static __always_inline void note_write(__u32 tgid, __u32 fd, __u64 now)
{
	struct fd_key key = { .tgid = tgid, .fd = fd };
	struct pending_read *p = bpf_map_lookup_elem(&read_wait, &key);
	if (p)
		p->last_write = now;
}

static __always_inline int on_exit(__u32 op, __s64 ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tid = (__u32)id, tgid = id >> 32;

	struct inflight *in = bpf_map_lookup_elem(&active, &tid);
	if (!in)
		return 0; // entry happened before we attached
	__u64 now = bpf_ktime_get_ns();
	__u64 dur = now - in->ts;
	__u32 fd = in->fd;
	bpf_map_delete_elem(&active, &tid);

	deliver(tgid, tid, op, now, dur, ret, 0);
	if (op == OP_READ)
		track_read_wait(tgid, tid, fd, now, ret);
	else if (op == OP_WRITE && ret > 0)
		note_write(tgid, fd, now);
	return 0;
}

#define SYSCALL_PROBES(name, op)                                         \
	SEC("tracepoint/syscalls/sys_enter_" #name)                      \
	int enter_##name(void *ctx) { return on_enter(0); }              \
	SEC("tracepoint/syscalls/sys_exit_" #name)                       \
	int exit_##name(struct sys_exit_ctx *ctx) { return on_exit(op, ctx->ret); }

#define FD_SYSCALL_PROBES(name, op)                                      \
	SEC("tracepoint/syscalls/sys_enter_" #name)                      \
	int enter_##name(struct sys_enter_fd_ctx *ctx) { return on_enter(ctx->fd); } \
	SEC("tracepoint/syscalls/sys_exit_" #name)                       \
	int exit_##name(struct sys_exit_ctx *ctx) { return on_exit(op, ctx->ret); }

FD_SYSCALL_PROBES(read, OP_READ)
FD_SYSCALL_PROBES(write, OP_WRITE)

SEC("tracepoint/syscalls/sys_enter_close")
int enter_close(struct sys_enter_fd_ctx *ctx)
{
	struct fd_key key = { .tgid = bpf_get_current_pid_tgid() >> 32, .fd = ctx->fd };
	bpf_map_delete_elem(&read_wait, &key);
	return 0;
}

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

static __always_inline __u32 offcpu_target(void)
{
	__u32 zero = 0;
	struct trace_config *cfg = bpf_map_lookup_elem(&config, &zero);
	return cfg ? cfg->offcpu_tgid : 0;
}

// Runs in sched_switch, where current is still the thread going to sleep, so the stacks
// are the ones that led it there.
static __always_inline void mark_sleeping(void *ctx, __u32 tid)
{
	long kernel = bpf_get_stackid(ctx, &kernel_stacks, 0);
	long user = bpf_get_stackid(ctx, &user_stacks, BPF_F_USER_STACK);
	struct sleep_start s = {
		.ts = bpf_ktime_get_ns(),
		.stack = { .kernel = kernel < 0 ? NO_STACK : kernel, .user = user < 0 ? NO_STACK : user },
	};
	bpf_map_update_elem(&sleeping, &tid, &s, BPF_ANY);
}

// Off-CPU time here ends at wakeup, not when the thread gets a CPU: the rest is run-queue
// delay, which has its own op.
static __always_inline void mark_awake(__u32 tid)
{
	struct sleep_start *s = bpf_map_lookup_elem(&sleeping, &tid);
	if (!s)
		return;
	__u64 dur = bpf_ktime_get_ns() - s->ts;
	struct offcpu_key stack = s->stack;
	bpf_map_delete_elem(&sleeping, &tid);

	struct offcpu_stat *st = bpf_map_lookup_elem(&offcpu, &stack);
	if (!st) {
		// Insert the first sleep whole: userspace reads this map while it's live, and
		// must never see an entry with a zero count.
		struct offcpu_stat first = { .count = 1, .total_ns = dur, .max_ns = dur };
		if (bpf_map_update_elem(&offcpu, &stack, &first, BPF_NOEXIST) == 0)
			return;
		st = bpf_map_lookup_elem(&offcpu, &stack);
		if (!st)
			return;
	}
	__sync_fetch_and_add(&st->count, 1);
	__sync_fetch_and_add(&st->total_ns, dur);
	if (dur > st->max_ns) // racy, but only a concurrent wakeup on the same stack loses
		st->max_ns = dur;
}

// tp_btf programs get the raw tracepoint arguments as an array of u64.
SEC("tp_btf/sched_wakeup")
int sched_wakeup(__u64 *ctx)
{
	struct task_struct *p = (void *)ctx[0];
	if (p->tgid == offcpu_target())
		mark_awake(p->pid);
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
	// Known undercount: a sched_wakeup for a task that's already queued (ttwu_runnable)
	// moves its timestamp forward.
	bool preempt = ctx[0];
	if (preempt || prev_state == 0)
		mark_runnable(prev->pid);
	else if (!(prev_state & TASK_DEAD) && prev->tgid == offcpu_target())
		mark_sleeping(ctx, prev->pid);

	__u32 tid = next->pid;
	__u64 *since = bpf_map_lookup_elem(&runnable, &tid);
	if (!since)
		return 0;
	__u64 now = bpf_ktime_get_ns();
	__u64 dur = now - *since;
	bpf_map_delete_elem(&runnable, &tid);

	__u32 tgid = next->tgid;
	deliver(tgid, tid, OP_RUNQ, now, dur, 0, next);
	return 0;
}

static __always_inline void own_socket(__u64 sk)
{
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	__u32 *owner = bpf_map_lookup_elem(&sock_owner, &sk);
	if (!owner || *owner != tgid) // skip the write on the common path
		bpf_map_update_elem(&sock_owner, &sk, &tgid, BPF_ANY);
}

// tcp_sendmsg(struct sock *sk, ...), in the sender's context.
SEC("fentry/tcp_sendmsg")
int tcp_send(__u64 *ctx)
{
	own_socket(ctx[0]);
	return 0;
}

// inet_sock_set_state(sk, oldstate, newstate). SYN_SENT comes from connect() in process
// context, which covers a SYN retransmitted before anything was sent. CLOSE drops the
// entry so a recycled struct sock doesn't inherit the old owner.
SEC("tp_btf/inet_sock_set_state")
int sock_set_state(__u64 *ctx)
{
	__u64 sk = ctx[0];
	int state = (int)ctx[2];
	if (state == TCP_SYN_SENT)
		own_socket(sk);
	else if (state == TCP_CLOSE)
		bpf_map_delete_elem(&sock_owner, &sk);
	return 0;
}

// tcp_retransmit_skb(sk, skb). Retransmits on a socket with no known owner go to tgid 0.
SEC("tp_btf/tcp_retransmit_skb")
int tcp_retransmit(__u64 *ctx)
{
	__u64 sk = ctx[0];
	__u32 *owner = bpf_map_lookup_elem(&sock_owner, &sk);
	record(owner ? *owner : 0, OP_RETRANS, 0, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual MIT/GPL";
