# Demo: a write-ahead log slowed by someone else's writes

`scripts/demo.sh` runs a small service that commits every request to a log file with `write` + `fdatasync`, then starts an unrelated 3 GB buffered write on the same disk. The question is whether TraceLens points at the right thing without knowing what was injected.

```
make build
scripts/linux.sh scripts/demo.sh
```

`bin/demo` handles 200 requests per second and prints its own per-second p50/p99. The agent runs alongside with `-trigger 20ms -trigger-op fsync -capture 5s`.

The output below is from one run in the Lima VM described in [benchmarks.md](benchmarks.md) (Ubuntu 24.04, kernel 6.8, 4 vCPUs on an M4 MacBook Air). Timings vary a lot from run to run. All four runs while building the demo showed the fsync jump; the worst per-second p99 in the demo's own log ranged from about 200 to 700 ms.

## Before the bulk write

```
   PID             COMM  READ/s  WRITE/s  OPEN/s  CONN/s  FSYNC/s  ERR/s  READ p99  WRITE p99  FSYNC p99  RUNQ p99
  6714             demo       0      201       0       0      200    0.0         -      131µs      4.2ms      66µs
```

## During it

```
   PID             COMM  READ/s  WRITE/s  OPEN/s  CONN/s  FSYNC/s  ERR/s  READ p99  WRITE p99  FSYNC p99  RUNQ p99
  6714             demo       0      197       0       0      196    0.0         -      262µs     16.8ms      66µs
```

`top`'s p99 is a log2 bucket bound, so 4.2 ms means "between 2.1 and 4.2 ms". The jump is in `FSYNC p99`. Run-queue delay doesn't move, so the demo isn't waiting for a CPU. The demo's own log for the same period:

```
01:31:36  requests 201  p50 936µs  p99 2.384ms
01:31:37  requests 200  p50 870µs  p99 6.152ms
01:31:38  requests 91  p50 1.982ms  p99 324.619ms
01:31:39  requests 30  p50 2.691ms  p99 361.44ms
01:31:40  requests 182  p50 783µs  p99 41.662ms
01:31:41  requests 199  p50 600µs  p99 2.982ms
```

## The triggered capture

```
2026/10/27 01:31:40 pid 6714 (demo): fsync p99 67.1ms >= 20ms, capturing 5s to /tmp/tmp.RDvrh6nFrf/tracelens-6714-20261027-013140.jsonl
     OP  CALLS  ERRORS    p50    p99     MAX
  write    975       0   20µs   76µs   1.2ms
  fsync    970       0  738µs  2.9ms  80.1ms
   runq  11554       0    4µs   34µs     2ms
events lost: 0

longest syscalls:
  80.1ms  tid 6714  fsync  ret 0
  56.3ms  tid 6714  fsync  ret 0
  16.5ms  tid 6718  fsync  ret 0
  16.2ms  tid 6714  fsync  ret 0
  12.7ms  tid 6714  fsync  ret 0
  ...
```

The capture confirms the slow calls are `fdatasync`, not the `write` before it (write p99 76 µs), and the JSON lines file has every call with its timestamp for lining up against other evidence.

## What the capture missed

The worst seconds were 01:31:38 and 01:31:39. The agent only sees an interval after it ends, so the trigger fired at 01:31:40, and by then the worst of the writeback was over: the capture's max is 80 ms, while the demo saw 361 ms. A triggered capture records the aftermath of a spike, up to one sampling interval late. The always-on histogram is what saw the peak (the 67.1 ms bucket that fired the trigger), which is the argument for keeping both.

Two smaller things this run shows:

- The fsyncs come from TIDs 6714 and 6718. The Go runtime moves the goroutine between OS threads, which is why stats are keyed by TGID and not TID.
- An earlier version of the capture wrote one JSON line per `write(2)`, and in that run the capturing agent was the busiest writer in `top`. It's buffered now.
