# TraceLens

TraceLens is a Linux eBPF tool that shows, per process, how long syscalls take and how long threads wait for a CPU. It traces `read`, `write`, `openat`, `connect`, `fsync`/`fdatasync`, run-queue delay and how long non-blocking reads wait for data, keeps per-process latency histograms inside the kernel, and only streams individual calls to userspace for one process at a time, when asked or when a latency threshold trips.

It's written in Go and C with [cilium/ebpf](https://github.com/cilium/ebpf) and `bpf2go`. The output below is from the [demo](docs/demo.md) while a bulk write was slowing its fsyncs; the `trace` part was printed by the agent's triggered capture, which runs the same code as `tracelens trace`.

```
$ tracelens top
   PID             COMM  READ/s  WRITE/s  OPEN/s  CONN/s  FSYNC/s  ERR/s  READ p99  WRITE p99  FSYNC p99  RUNQ p99
  6714             demo       0      197       0       0      196    0.0         -      262µs     16.8ms      66µs
   302  systemd-journal      12        0       9       0        0    1.0      66µs          -          -     131µs

$ tracelens trace -pid 6714 -duration 5s
     OP  CALLS  ERRORS    p50    p99     MAX
  write    975       0   20µs   76µs   1.2ms
  fsync    970       0  738µs  2.9ms  80.1ms
   runq  11554       0    4µs   34µs     2ms
events lost: 0

longest syscalls:
  80.1ms  tid 6714  fsync  ret 0
  56.3ms  tid 6714  fsync  ret 0
  ...
```

This output was captured before the `RWAIT p99` column (read wait) was added to `top`.

## Commands

```
tracelens top   [-interval 2s] [-n 15]
tracelens trace -pid PID [-duration 10s] [-min 0] [-out events.jsonl]
tracelens agent [-listen :9090] [-interval 1s]
                [-trigger 20ms -trigger-op fsync -capture 10s -dir /tmp]
```

- **`top`** prints syscall rates, error rates and p99 per process every interval, sorted by syscall rate.
- **`trace`** follows one process for a fixed time and reports exact p50/p99/max per op, the longest calls, and how many events were dropped. `-min` filters short calls in the kernel; `-out` writes every call as a JSON line.
- **`agent`** serves Prometheus metrics at `/metrics`: per-op call, error and byte counters, latency histograms, run-queue delay and read wait histograms, and drop counters. Labels are per op only, with no PID label, so the number of series stays fixed. With `-trigger`, it captures whichever process's interval p99 for `-trigger-op` reaches the threshold, using the same code as `trace`, and writes the capture to `-dir`.

Errors exclude EAGAIN and EINPROGRESS, which non-blocking I/O returns as a normal step. Read wait (`readwait`, `RWAIT`) is the time from a `read` returning EAGAIN until the next `read` on the same fd returns; for Go sockets and pipes, which never block in `read`, it's the only place their waits show up.

## How it works

Each syscall's enter tracepoint stores a timestamp keyed by thread ID and the exit tracepoint turns it into a duration. Scheduler hooks (`tp_btf/sched_wakeup`, `sched_switch`) do the same for the time between becoming runnable and getting a CPU. Every measurement goes to `deliver()`, which adds it to a per-CPU hash keyed by `(tgid, op)` with a 40-slot log2 histogram, and also pushes it into a ring buffer if it belongs to the traced process. Userspace changes the traced process by writing a config map, without reattaching anything.

`top` and the agent read the stats map every interval and diff it against the previous snapshot. The kernel never deletes a process's keys; userspace does once it has read the final values and `/proc/<pid>` is gone, so short-lived processes still get counted.

There's no `vmlinux.h`. Syscall tracepoint contexts are hand-written from their `format` files, and the scheduler hooks use a three-field `task_struct` with CO-RE relocations. [docs/architecture.md](docs/architecture.md) has the details.

## Requirements

- Linux 5.18+ with BTF (`/sys/kernel/btf/vmlinux`), run as root. `CAP_BPF` + `CAP_PERFMON` should be enough but hasn't been tried.
- Go 1.25+ to build
- clang and llvm-strip with the BPF target, only to regenerate the BPF objects (`make generate`). The generated `.o` and Go bindings are committed.

Only tested on Ubuntu 24.04 (kernel 6.8, arm64) in a Lima VM.

## Build and test

```
make build            # bin/tracelens, bin/loadgen, bin/demo for linux/$GOARCH
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

`scripts/linux.sh scripts/demo.sh` runs a service that does `write` + `fdatasync` for every request, then starts an unrelated 3 GB buffered write on the same disk. The agent notices the service's fsync p99 jump and captures it. [docs/demo.md](docs/demo.md) walks through one run, including what the triggered capture missed.

## Overhead

Measured in a 4-vCPU Lima VM on an M4 MacBook Air with a worst-case workload: back-to-back 1-byte reads from `/dev/zero`, about 200 ns each. Full method and caveats are in [docs/benchmarks.md](docs/benchmarks.md).

| | reads/s, 2 threads unthrottled | agent CPU at 1M reads/s | BPF time per read, unthrottled (upper bound) |
| --- | --- | --- | --- |
| no tracing | 10.0M | | |
| in-kernel aggregation (default) | 5.81M | ~0% | ≤120 ns |
| every event to userspace (`-export-all`) | 4.18M, ~8.7M events dropped per 5 s run* | 30% of a core* | ≤234 ns |

\* Measured with the original `binary.Read` decoder. After switching to offset-based decoding, unthrottled drops fell to 5-6% (about 3.8M events/s delivered); agent CPU hasn't been remeasured.

For syscalls that take microseconds or more, the per-call cost is a much smaller fraction. Latency comparisons at paced rates turned out to depend on how idle the VM was, so the writeup doesn't rely on them.

## Limitations

The full list is in [docs/limitations.md](docs/limitations.md). The ones most likely to matter:

- **Syscall duration isn't device latency**, and read/write bytes aren't disk bytes.
- **Socket waits in epoll-based programs only show up as read wait**, and only for `read(2)` calls that hit EAGAIN first. `recv`, writes and programs that wait in `epoll_wait` before their first read aren't covered. On keep-alive connections, read wait includes the idle time between requests.
- **p99 in `top`, `/metrics` and the trigger is a log2 bucket upper bound**, up to 2x the true value. `trace` is exact.
- **Triggered captures start up to one interval after the spike**, so they can miss its peak.
- **One traced process at a time**, and PIDs are from the host PID namespace.

## Notes

[docs/learnings.md](docs/learnings.md) is a less formal write-up of what turned up while building this: Go's netpoller hiding socket latency, tracing yourself, the bugs code review caught, and why the latency benchmarks had to be thrown out.

## Layout

```
bpf/tracelens.bpf.c        all BPF programs and maps
bpf/headers/               libbpf helper headers + a minimal types.h
internal/probe/            load/attach, map access, bpf2go output
internal/hist/             percentiles from log2 histograms
cmd/tracelens/             top, trace, agent
cmd/loadgen/, cmd/demo/    benchmark workload, demo service
benchmarks/                benchmark scripts, results/ with CSVs and plots
scripts/linux.sh           run as root on Linux or in the Lima VM
tests/integration/         tests that load the real programs
```
