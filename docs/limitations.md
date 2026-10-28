# Limitations

What TraceLens measures, and where those measurements stop meaning what they sound like.

## What the numbers are

- **Syscall duration is entry-to-exit time of the syscall.** A slow `read` means the thread spent that long inside `read`, whether that was waiting on a disk, a pipe, a lock in the kernel, or being preempted halfway through. It isn't device latency.
- **Bytes are what `read`/`write` returned.** Page-cache hits, pipes and sockets all count. This is not disk I/O.
- **`connect` duration is the syscall, not the TCP handshake.** For a non-blocking socket, `connect` returns EINPROGRESS immediately and the handshake finishes later, outside any traced syscall.
- **`fsync` covers both `fsync` and `fdatasync`.**
- **Read wait** is the time from a `read` returning EAGAIN to the next `read` on the same fd returning something else. On a request/response connection where the reader only reads when it expects a reply, that's the peer's response time. But Go's HTTP server and client both keep a read pending on idle keep-alive connections, so there it also includes the gap between requests. In a test with a 50 ms handler and a client sleeping 100 ms between requests, both sides reported about 155 ms with keep-alive and the client reported 51 ms without it.
- **Run-queue delay** is the time from wakeup (or preemption) until the thread is switched in. A `sched_wakeup` for a task that is already queued moves its timestamp forward, so waits can be slightly undercounted.
- **p99 in `top`, the agent's trigger, and `/metrics` comes from log2 buckets.** It is the upper bound of the bucket holding the 99th-percentile sample, so it can be up to 2x the real value. `trace` reports exact durations.

## Go programs and other non-blocking I/O

Go's runtime puts every socket and pipe into non-blocking mode. A read that would block returns EAGAIN in a few hundred nanoseconds, and the goroutine then parks in the netpoller (`epoll_pwait`). So for network I/O in Go and anything else built on epoll, the `read` and `write` ops stay near zero no matter how slow the peer is.

The `readwait` op covers part of that gap: reads that hit EAGAIN and then succeed on the same fd. It has holes:

- Only `read(2)`. Programs that use `recv`, `recvfrom` or `recvmsg` on sockets don't get it.
- A wait only starts with an EAGAIN. If the data is already there when the program reads, there's nothing to measure, which is fine. But a program that calls `epoll_wait` first and reads only once the fd is ready never hits EAGAIN, so its waits are invisible.
- Writes that block on a full socket buffer (EAGAIN on `write`) aren't tracked.
- An fd closed by something other than `close(2)` (`dup2` over it, `close_range`, process exit) leaves its entry until LRU evicts it. If that number gets reused in the meantime, the first completed read on the new file reports a wait measured from the old file's EAGAIN.

File I/O (regular files are always blocking), `fsync` and run-queue delay show up for these programs as usual. The demo uses `fdatasync` for this reason.

## Coverage

- Only `read`, `write`, `openat`, `connect`, `fsync` and `fdatasync`. `pread64`/`pwrite64`, `readv`/`writev`, `recvmsg`/`sendmsg`, `io_uring` and `epoll_wait` are not traced. `close` is traced only to clean up read-wait state.
- TCP retransmits were considered and left out. The `tcp_retransmit_skb` tracepoint mostly fires in softirq context, where the current task is whatever happened to be running and not the socket's owner, so attributing it to a process would be wrong.
- `trace` and triggered capture follow one process at a time. There is a single target slot in the kernel config.

## Process identity

- Processes are keyed by TGID as seen from the initial PID namespace. The agent looks up names in `/proc/<pid>/comm`, so it has to run in the host PID namespace (`--pid=host` in a container) or the names come out as `?`.
- A PID that is reused within one sampling interval looks like a continuation of the old process.
- If a process's stats key is deleted and recreated with a larger count than before, `probe.Delta` can't tell, and the interval merges the two.

## Resource limits

- `stats` holds 8192 keys, at most seven per process (one per op). Each value is 352 bytes per CPU. When it fills, new processes go unrecorded and `tracelens_stats_dropped_total` counts the misses.
- The ring buffer drops events when the consumer falls behind; the count is reported by `trace`, captures and `tracelens_events_dropped_total`. [benchmarks.md](benchmarks.md) shows where that starts.

## Kernel requirements

- BTF (`/sys/kernel/btf/vmlinux`, `CONFIG_DEBUG_INFO_BTF=y`) for the `tp_btf` scheduler hooks and CO-RE relocations.
- Linux 5.18 or later, because the `sched_switch` program assumes the `(preempt, prev, next, prev_state)` argument order introduced there. Ring buffers need 5.8.
- Developed and tested only on Ubuntu 24.04 (kernel 6.8, arm64) in a Lima VM.
