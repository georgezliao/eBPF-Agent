# Architecture

TraceLens answers one question about a Linux process: where is it waiting? It times five syscalls (`read`, `write`, `openat`, `connect`, and `fsync`/`fdatasync` counted together) plus the time each thread spends runnable but not running and how long non-blocking reads wait for data, keeps cheap per-process histograms of those all the time, and records individual calls only for one process at a time and only when asked.

## Data flow

```
 kernel                                              userspace (Go)
 ──────                                              ──────────────
 sys_enter_{read,write,openat,connect,         ┌──►  Stats() every interval ──► Delta() ──► top table
            fsync,fdatasync}                   │                                    └──► agent totals ──► /metrics
     │ start ts by TID                         │
     ▼                                         │
  active (LRU hash) ──► sys_exit_* ──► deliver ┼──►  stats (per-CPU hash, key tgid+op)
                                               │
 sched_wakeup / wakeup_new / switch (tp_btf)   │
     │ runnable-since ts by TID                └──►  events ring buffer ──► trace / agent capture
     ▼                                                   (only target_tgid, or everything in raw mode)
  runnable (LRU hash) ──► sched_switch ──► deliver
```

`deliver()` in `bpf/tracelens.bpf.c` is the one place every finished measurement goes through. It reads the single-entry `config` map and decides whether to aggregate, emit an event, or both.

## Kernel side

**Syscall timing.** Each traced syscall has an enter and an exit tracepoint. Enter stores `bpf_ktime_get_ns()` in the `active` map keyed by TID; exit looks it up, deletes it and computes the duration. TID is the right key because a thread can only be inside one syscall at a time. The map is an LRU hash so entries left behind by threads that died mid-syscall get evicted instead of filling it.

The tracepoint context structs are written by hand from `/sys/kernel/tracing/events/syscalls/*/format` instead of coming from `vmlinux.h`. Every syscall exit tracepoint has the same layout (8-byte common header, `int __syscall_nr`, `long ret` at offset 16), so one struct covers all of them.

**Run-queue delay.** `sched_wakeup` and `sched_wakeup_new` record when a thread became runnable. `sched_switch` records the same for the outgoing thread if it is still runnable (preempted, or `prev_state == 0`), and for the incoming thread computes how long it waited. These hooks use `tp_btf` because the classic tracepoint format only gives the TID of the woken task, and per-process attribution needs its TGID. A three-field `struct task_struct` marked `preserve_access_index` is enough: clang emits CO-RE relocations for `pid`, `tgid` and `comm`, and the loader resolves them against the running kernel's BTF.

**Read wait.** A non-blocking read with no data returns EAGAIN right away, and the program waits in `epoll_wait` (or `poll`, or Go's netpoller) before reading again. Neither read is slow, so the `read` op can't see the wait. The `read` enter probe also saves the fd, and at exit a read that returned EAGAIN stores its timestamp in `read_wait` keyed by `(tgid, fd)`, unless an earlier one is already there. The next read on that fd that returns anything else removes the entry and delivers the gap as the `readwait` pseudo-op, with the second read's return value. The key is the process and fd, not the thread, because the retry often runs on a different thread. `sys_enter_close` deletes the entry so a reused fd number doesn't inherit it.

**Aggregation.** `stats` is a per-CPU hash keyed by `(tgid, op)` holding a count, error count, byte count, total time and a 40-slot log2 histogram of durations. Being per-CPU means the hot path does plain increments with no atomics; the cost is memory (each value is 352 bytes per CPU) and a userspace sum across CPUs on every read.

EAGAIN and EINPROGRESS are not counted as errors. Non-blocking I/O returns them as a normal step, and Go's runtime does it for every socket and pipe read, so counting them made healthy Go processes look like they were failing.

**Detailed events.** `events` is a ring buffer (256 KiB by default, sizable at load). An event is emitted when the finished call belongs to `config.target_tgid` and is at least `config.min_duration_ns` long. Userspace changes the target by writing the config map; no program is reattached. If `bpf_ringbuf_reserve` fails, the `lost` map counts it rather than blocking or retrying, since the hook runs in the traced thread's syscall path.

`config.export_all` is a benchmark mode: every call from every process goes to the ring buffer and the kernel skips aggregation, so the agent has to aggregate in Go. It exists to compare the two designs on the same code path.

## Userspace side

`internal/probe` loads the bpf2go-generated objects, attaches every program by its ELF section name, and wraps the maps: `Stats()` (summed across CPUs), `SetTarget()`, `Events()`, `Lost()`, `ForgetExited()`.

**Snapshots and deltas.** The kernel map holds cumulative counters. `top` and the agent read it every interval and diff consecutive snapshots with `probe.Delta`. A key whose counters went backwards was deleted and recreated, so it counts from zero.

**Process lifetime.** The kernel never deletes stats keys. After userspace has used a snapshot, `ForgetExited` deletes the keys of processes whose `/proc/<pid>` is gone. An earlier version deleted them in a `sched_process_exit` program, which threw away everything a process did since the last sample and never counted processes shorter than one interval.

**Commands** (all in `cmd/tracelens`):

- `top` prints per-process rates and p99 from each interval's delta.
- `trace` sets the target, reads the ring buffer for a fixed duration, and prints exact percentiles per op plus the longest syscalls. Events can also be written as JSON lines.
- `agent` serves `/metrics` (per-op totals, no PID labels) and, with `-trigger`, starts a capture of whichever process's interval p99 for one op crossed the threshold. Captures use the same code as `trace`. There is one target slot in the kernel config, so one capture runs at a time.

## Why the two modes

Always-on monitoring keeps the data that leaves the kernel to a few hundred map entries per interval, whatever the syscall rate. Detailed events cost a ring buffer reservation per call plus decoding in Go, which is fine for one process for a few seconds and not fine for every process all the time. [benchmarks.md](benchmarks.md) measures that difference.
