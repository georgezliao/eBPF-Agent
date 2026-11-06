# TraceLens

TraceLens is a Linux eBPF tool that shows, per process, how long syscalls take and how long threads wait for a CPU. It traces `read`, `write`, `openat`, `connect`, `fsync`/`fdatasync`, run-queue delay, how long non-blocking reads wait for data, and TCP retransmits. It keeps per-process latency histograms inside the kernel and only streams individual calls to userspace for one process at a time, when asked or when a latency threshold trips. For one process it can also show which kernel stacks its threads slept in.

It's written in Go and C with [cilium/ebpf](https://github.com/cilium/ebpf) and `bpf2go`. The output below is from the [demo](docs/demo.md), where a client's requests to a small service get slow for three different reasons. The columns are trimmed from `tracelens top`:

```
                          FSYNC p99  RESP p99  RETRANS/s
healthy      store            4.2ms   134.2ms        0.0
             client               -    16.8ms        0.0
slow service store            4.2ms    16.8ms        0.0
             client               -    67.1ms        0.0
packet loss  store            4.2ms   536.9ms       13.7
             client               -   536.9ms       26.7
busy disk    store           67.1ms     4.2ms        0.0
             client               -   268.4ms        0.3
```

The client's response time rises every time, and the other columns say why. A slow service leaves fsync and retransmits alone, packet loss shows up as retransmits, and only the busy disk moves fsync. (The store's `RESP` is the client's pause between requests, not the store's speed.) `offcpu` then shows what the slow fsyncs slept on:

```
$ tracelens offcpu -pid 61533 -duration 3s -n 1 -match ext4_sync_file
2.205s total  546 sleeps  avg 4ms  max 111.1ms
  schedule
  jbd2_log_wait_commit
  jbd2_complete_transaction
  ext4_fc_commit
  ext4_sync_file
  ...
```

## Commands

```
tracelens top   [-interval 2s] [-n 15]
tracelens trace -pid PID [-duration 10s] [-min 0] [-out events.jsonl]
tracelens offcpu -pid PID [-duration 10s] [-n 5] [-match NAME]
tracelens agent [-listen :9090] [-interval 1s]
                [-trigger 20ms -trigger-op fsync -capture 10s -dir /tmp]
```

- **`top`** prints syscall rates, error rates and p99 per process every interval, sorted by syscall rate.
- **`trace`** follows one process for a fixed time and reports exact p50/p99/max per op, the longest calls, and how many events were dropped. `-min` filters short calls in the kernel; `-out` writes every call as a JSON line.
- **`offcpu`** groups one process's sleeps by kernel stack and prints the stacks with the most time asleep. `-match` keeps stacks with a frame containing that name, since Go's idle threads otherwise fill the list. This is how to find out why a thread was waiting: `balance_dirty_pages` under a slow `write`, `ep_poll` for an idle Go runtime.
- **`agent`** serves Prometheus metrics at `/metrics`: per-op call, error and byte counters, latency histograms, run-queue delay, read wait and response histograms, TCP retransmits, and drop counters. Labels are per op only, with no PID label, so the number of series stays fixed. With `-trigger`, it captures whichever process's interval p99 for `-trigger-op` reaches the threshold, using the same code as `trace`, and writes the capture to `-dir`.

Errors exclude EAGAIN and EINPROGRESS, which non-blocking I/O returns as a normal step. Read wait (`readwait`, `RWAIT`) is the time from a `read` returning EAGAIN until the next `read` on the same fd returns; for Go sockets and pipes, which never block in `read`, it's the main place their waits show up. Response (`response`, `RESP`) is the part of that wait after the last `write` on the fd, so on a keep-alive connection the client sees the server's response time without the idle gap before the request.

## How it works

Each syscall's enter tracepoint stores a timestamp keyed by thread ID and the exit tracepoint turns it into a duration. Scheduler hooks (`tp_btf/sched_wakeup`, `sched_switch`) do the same for the time between becoming runnable and getting a CPU. Every measurement goes to `deliver()`, which adds it to a per-CPU hash keyed by `(tgid, op)` with a 40-slot log2 histogram, and also pushes it into a ring buffer if it belongs to the traced process. Userspace changes the traced process by writing a config map, without reattaching anything.

`top` and the agent read the stats map every interval and diff it against the previous snapshot. The kernel never deletes a process's keys; userspace does once it has read the final values and `/proc/<pid>` is gone, so short-lived processes still get counted.

There's no `vmlinux.h`. Syscall tracepoint contexts are hand-written from their `format` files, and the scheduler hooks use a three-field `task_struct` with CO-RE relocations. [docs/architecture.md](docs/architecture.md) has the details.

## Requirements

- Linux 5.18+ with BTF (`/sys/kernel/btf/vmlinux`), run as root. On arm64, 6.0+ for `fentry`. `CAP_BPF` + `CAP_PERFMON` should be enough but hasn't been tried.
- Go 1.25+ to build
- clang and llvm-strip with the BPF target, only to regenerate the BPF objects (`make generate`). The generated `.o` and Go bindings are committed.

Only tested on Ubuntu 24.04 (kernel 6.8, arm64) in a Lima VM.

## Build and test

```
make build            # bin/tracelens, bin/loadgen, bin/store, bin/client for linux/$GOARCH
make check            # go vet + unit tests (runs on macOS too)
make itest            # integration tests: load the real programs, as root
make check-generated  # regenerate BPF objects and fail if they differ from the committed ones
```

On macOS, everything that needs a kernel goes through `scripts/linux.sh`, which runs the command as root in a Lima VM:

```
limactl start --name=tracelens template:ubuntu-24.04
make build
scripts/linux.sh bin/tracelens top
make itest
```

The integration tests in `tests/integration` drive known workloads (slow pipe reads, failing reads, CPU spinners, short-lived processes) and check what the maps and ring buffer report.

## Demo

`scripts/linux.sh scripts/demo.sh` runs a client calling a small service that commits every request with `write` + `fdatasync`, then injects a slow service, packet loss and a 3 GB buffered write in turn. [docs/demo.md](docs/demo.md) walks through one run, including what each view missed.

## Overhead

Measured in a 4-vCPU Lima VM on an M4 MacBook Air with a worst-case workload: back-to-back 1-byte reads from `/dev/zero`, about 160 ns each. Throughput and CPU are medians of three 5-second runs, BPF time of five. Full method and caveats are in [docs/benchmarks.md](docs/benchmarks.md).

| | reads/s, 2 threads unthrottled | agent CPU at 1M reads/s | BPF time per read, unthrottled (upper bound) |
| --- | --- | --- | --- |
| no tracing | 12.3M | | |
| in-kernel aggregation (default) | 7.32M | ~0% | ≤105 ns |
| every event to userspace (`-export-all`) | 4.46M, about 3.5% of events dropped | 35% of a core | ≤278 ns |

At 1M reads/s, the workload's response p99 measured from each call's scheduled start was 2.6 µs with no tracing, 2.1 µs with aggregation and 7.9 µs with every event sent to userspace.

For syscalls that take microseconds or more, the per-call cost is a much smaller fraction. These numbers depend heavily on host load: with the Mac's load average around 28, the same tests gave very different results, so [docs/benchmarks.md](docs/benchmarks.md) records the conditions for each run.

## Limitations

The full list is in [docs/limitations.md](docs/limitations.md). The ones most likely to matter:

- **Syscall duration isn't device latency**, and read/write bytes aren't disk bytes.
- **Socket waits in epoll-based programs only show up as read wait and response**, and only for `read(2)` calls that hit EAGAIN first. `recv`, writes and programs that wait in `epoll_wait` before their first read aren't covered. On keep-alive connections, read wait includes the idle time between requests, and a server's `response` measures the client's pause, not its own speed.
- **p99 in `top`, `/metrics` and the trigger is a log2 bucket upper bound**, up to 2x the true value. `trace` is exact.
- **Triggered captures start up to one interval after the spike**, so they can miss its peak.
- **One traced process at a time**, and PIDs are from the host PID namespace.

## Notes

[docs/learnings.md](docs/learnings.md) is a less formal write-up of what turned up while building this: Go's netpoller hiding socket latency, tracing yourself, the bugs code review caught, and why the latency benchmarks had to be thrown out.

## Possible next steps

None of these are started.

- **User stacks in `offcpu`.** It only captures kernel stacks, which say what a thread slept in but not which code path called it. Go has frame pointers, so `BPF_F_USER_STACK` should work, but the addresses need symbolizing against the binary.
- **Writes blocked on a full socket buffer.** `write` returning EAGAIN could be timed the same way as read wait.
- **`recv`, `recvfrom` and `recvmsg`.** Read wait only sees `read(2)`, so programs that use these don't get it.
- **Per-CPU maps at high core counts.** Each stats value is 352 bytes per CPU, so a shared hash with atomics may become the better trade at some core count. Finding where needs a machine with many cores.

## Further reading

Concepts:

- [Off-CPU Analysis](https://brendangregg.com/offcpuanalysis.html), Brendan Gregg
- [How Not to Measure Latency](https://www.youtube.com/watch?v=0b3sR32m0nU), Gil Tene (talk, covers coordinated omission)

Talks on eBPF:

- [BPF Performance Analysis at Netflix](https://www.youtube.com/watch?v=16slh29iN1g), Brendan Gregg
- [BPF Internals](https://www.youtube.com/watch?v=_5Z2AU7QTH4), Brendan Gregg
- [A Beginner's Guide to eBPF Programming for Networking](https://www.youtube.com/watch?v=0p987hCplbk), Liz Rice

Failures worth examining:

- [The story of one latency spike](https://blog.cloudflare.com/the-story-of-one-latency-spike/) (Cloudflare): slow responses traced into kernel packet processing
- [SYN packet handling in the wild](https://blog.cloudflare.com/syn-packet-handling-in-the-wild/) (Cloudflare): how listen queues behave under load
- [Fsync Errors](https://wiki.postgresql.org/wiki/Fsync_Errors) (PostgreSQL wiki): the 2018 "fsyncgate" reliability issues
- [The Register on CrowdStrike and Red Hat](https://www.theregister.com/2024/09/26/grafana_labs_interview/): an eBPF sensor hitting a Linux kernel bug and panicking machines
- [Who Will Observe the Observability?](https://blog.zmalik.dev/p/who-will-observe-the-observability): eBPF performance at scale

## Layout

```
bpf/tracelens.bpf.c        all BPF programs and maps
bpf/headers/               libbpf helper headers + a minimal types.h
internal/probe/            load/attach, map access, bpf2go output
internal/hist/             percentiles from log2 histograms
cmd/tracelens/             top, trace, agent
cmd/loadgen/               benchmark workload
cmd/store/, cmd/client/    demo service and its caller
benchmarks/                benchmark scripts, results/ with CSVs and plots
scripts/linux.sh           run as root on Linux or in the Lima VM
tests/integration/         tests that load the real programs
```
