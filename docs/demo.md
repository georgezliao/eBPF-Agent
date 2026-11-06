# Demo: telling three slowdowns apart

A client's requests get slow. There are three possible reasons here, and from the client's side they look similar. `scripts/demo.sh` injects each one in turn, and the question is whether TraceLens points at the right cause without being told which was injected.

```
make build
scripts/linux.sh scripts/demo.sh
```

`bin/client` sends 200 requests/s over eight keep-alive connections to `bin/store`, an HTTP service that appends each 32 KiB request to a log with `write` + `fdatasync`. The client prints its own per-second latency, measured from when each request was due, so there's a number to check TraceLens against. The agent runs alongside with `-trigger 20ms -trigger-op fsync -capture 3s`.

| fault | what the script does |
| --- | --- |
| 1, slow service | `store` sleeps 30 ms per request, as if a dependency got slow |
| 2, packet loss | `tc netem` drops 5% of packets on loopback |
| 3, disk contention | `dd` writes 3 GB of buffered data to the same disk |

The output below is from one run in the Lima VM described in [benchmarks.md](benchmarks.md). All three runs are in [`benchmarks/results/demo/`](../benchmarks/results/demo/), and each fault produced the same pattern in all of them, though the sizes vary, especially for faults 2 and 3.

## What `top` showed

These columns are trimmed from the real output. `top`'s p99 is a log2 bucket bound, so 4.2 ms means "between 2.1 and 4.2 ms".

```
                          FSYNC p99  RESP p99  RETRANS/s
healthy      store            4.2ms   134.2ms        0.0
             client               -    16.8ms        0.0
fault 1      store            4.2ms    16.8ms        0.0
             client               -    67.1ms        0.0
fault 2      store            4.2ms   536.9ms       13.7
             client               -   536.9ms       26.7
fault 3      store           67.1ms     4.2ms        0.0
             client               -   268.4ms        0.3
```

The client's own p50 and p99 for the same run:

| phase | p50 | p99 |
| --- | --- | --- |
| healthy | 6–7 ms | 13–15 ms |
| fault 1 | 35–36 ms | 39–41 ms |
| fault 2 | 6.5–23 ms | 429–811 ms |
| fault 3 | 71 ms–1.3 s | 183 ms–1.5 s |

Reading the faults off `top`:

- **Fault 1:** the client's `RESP` jumps and its p50 rises by the injected 30 ms. `FSYNC` doesn't move and there are no retransmits, so it's neither the disk nor the network. The time is inside the store, and syscall tracing can't say where. `tracelens offcpu` would be the next step.
- **Fault 2:** `RETRANS/s` appears on both processes. The client's p50 barely moves and its p99 goes to hundreds of milliseconds, since only the requests that lost a packet wait for a retransmit. `RESP` shows that tail. Fsync doesn't move.
- **Fault 3:** the store's `FSYNC p99` jumps. That's the only fault where it moves, and retransmits stay near zero.

Ignore the store's `RESP` column. It's the gap between the store's reply and the next request on that connection, so it measures the client's schedule, not the store's speed. [limitations.md](limitations.md) has more on why.

## Why the fsyncs were slow

`top` says fsync got slow, and `offcpu` says what it slept on. The script runs it during fault 3 with `-match ext4_sync_file`, because Go's idle threads otherwise fill the top of the list:

```
$ tracelens offcpu -pid 61533 -duration 3s -n 1 -match ext4_sync_file
2.838s asleep across all threads in 3.005s, 3 distinct stacks

2.205s total  546 sleeps  avg 4ms  max 111.1ms
  schedule
  jbd2_log_wait_commit
  jbd2_complete_transaction
  ext4_fc_commit
  ext4_sync_file
  vfs_fsync_range
  do_fsync
  __arm64_sys_fdatasync
  ...
```

`fdatasync` is waiting for an ext4 journal commit. The top stack was the same in all three runs. Nothing in this stack mentions `dd`: it shows the store waiting on the journal, not who else was using it.

## The triggered capture

```
2026/11/06 11:19:01 pid 61533 (store): fsync p99 33.6ms >= 20ms, capturing 3s to ...
        OP  CALLS  ERRORS    p50      p99      MAX
      read   4837       0    1µs     32µs    216µs
     write   1074       0   44µs    901µs    4.8ms
     fsync    535       0  3.2ms   39.7ms  132.5ms
      runq   8378       0    8µs    657µs    4.7ms
  readwait    580       0   31ms  194.7ms  279.5ms
  response    535       0  268µs    2.8ms    4.9ms
events lost: 0

longest syscalls:
  132.5ms  tid 61543  fsync  ret 0
  115.1ms  tid 61543  fsync  ret 0
  113.5ms  tid 61542  fsync  ret 0
  ...
```

The trigger fired once in each run, on fault 3, because it only watches fsync. The capture confirms that the slow calls are `fdatasync`, not the `write` before it, with run-queue delay staying in the milliseconds.

## What this doesn't show

- The client's p99 reached 1.5 s during fault 3 while the longest fsync in the capture was 132 ms. The store commits one request at a time, so requests queue behind a slow fsync, and the client measures from when each was due. The capture also ran for only 3 s of a stall that lasted about 6 s.
- A triggered capture records what follows a spike, up to one sampling interval late, so it can miss the peak. The always-on histogram is what fired the trigger.
- Fault 1 needed tuning to show up. At a 10 ms delay, the change only moved `RESP` by one log2 bucket, and that overlapped healthy runs. A smaller real slowdown could be hard to spot in `top`.
- Every fault here was injected alone, on one machine, over loopback.
