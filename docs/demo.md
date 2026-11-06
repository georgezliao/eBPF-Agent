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

The output below is from one run in the Lima VM described in [benchmarks.md](benchmarks.md). All three runs are in [`benchmarks/results/demo/`](../benchmarks/results/demo/), and each fault produced the same pattern in all of them, though the sizes vary, especially for faults 2 and 3. In the worst run, the client completed a single request in one second of fault 3.

## What `top` showed

These columns are trimmed from the real output. `top`'s p99 is a log2 bucket bound, so 4.2 ms means "between 2.1 and 4.2 ms".

```
                          FSYNC p99  RESP p99  RETRANS/s
healthy      store            4.2ms    67.1ms        0.0
             client               -    33.6ms        0.0
fault 1      store            4.2ms     8.4ms        0.0
             client               -    67.1ms        0.0
fault 2      store            4.2ms   268.4ms       11.0
             client               -   268.4ms       26.3
fault 3      store           33.6ms    67.1ms        0.0
             client               -   536.9ms        1.7
```

The client's own p50 and p99 for the same run:

| phase | p50 | p99 |
| --- | --- | --- |
| healthy | 9–11 ms | 18–27 ms |
| fault 1 | 37–39 ms | 43–54 ms |
| fault 2 | 9.5–18 ms | 242–894 ms |
| fault 3 | 66–172 ms | 271–550 ms |

Reading the faults off `top`:

- **Fault 1:** the client's `RESP` moves up one or two log2 buckets, which alone wouldn't mean much, but its own p50 rises by the injected 30 ms. `FSYNC` doesn't move and there are no retransmits, so it's neither the disk nor the network. The time is inside the store, and `tracelens offcpu` on it would be the next step.
- **Fault 2:** `RETRANS/s` appears on both processes. The client's p50 barely moves and its p99 goes to hundreds of milliseconds, since only the requests that lost a packet wait for a retransmit. `RESP` shows that tail. Fsync doesn't move.
- **Fault 3:** the store's `FSYNC p99` jumps. That's the only fault where it moves, and retransmits stay near zero. They weren't exactly zero, 0 to 2.3/s across the runs, and 0.3/s in one healthy period; I don't know what caused those. The client reported no failed requests in a later run with failure counting added.

Ignore the store's `RESP` column. It's the gap between the store's reply and the next request on that connection, so it measures the client's schedule, not the store's speed. [limitations.md](limitations.md) has more on why.

## Why the fsyncs were slow

`top` says fsync got slow, and `offcpu` says what it slept on and who asked. The script runs it during fault 3 with `-match ext4_sync_file`, because Go's idle threads otherwise fill the top of the list:

```
$ tracelens offcpu -pid 66367 -duration 3s -n 1 -match ext4_sync_file
1.035s total  602 sleeps  0 still asleep  max 210.3ms
  schedule
  jbd2_log_wait_commit
  jbd2_complete_transaction
  ext4_fc_commit
  ext4_sync_file
  vfs_fsync_range
  do_fsync
  __arm64_sys_fdatasync
  ...
  --
  internal/runtime/syscall/linux.Syscall6.abi0
  syscall.Syscall
  syscall.Fdatasync
  main.main.func1
  net/http.HandlerFunc.ServeHTTP
  ...
```

Above the `--` is the kernel stack: `fdatasync` is waiting for an ext4 journal commit. Below it is the user stack, which leads to the store's HTTP handler (`main.main.func1`). The top stack was the same in all three runs. Nothing in it mentions `dd`: it shows the store waiting on the journal, not who else was using it.

## The triggered capture

```
2026/11/06 14:48:11 pid 66367 (store): fsync p99 134.2ms >= 20ms, capturing 3s to ...
        OP  CALLS  ERRORS     p50      p99      MAX
      read   5796       0     2µs     31µs    423µs
     write   1278       0    51µs    265µs    5.6ms
     fsync    638       0   927µs    5.4ms  210.7ms
      runq   7934       0     7µs    492µs    2.2ms
  readwait    697       0  39.2ms  296.4ms  488.7ms
  response    637       0  31.5ms   41.1ms   43.5ms
events lost: 0

longest syscalls:
  210.7ms  tid 66372  fsync  ret 0
  192.2ms  tid 66378  fsync  ret 0
  82.7ms   tid 66372  fsync  ret 0
  ...
```

The trigger fired once in each run, on fault 3, because it only watches fsync. The capture confirms that the slow calls are `fdatasync`, not the `write` before it, and run-queue delay stays low.

## What this doesn't show

- The client's p99 reached 550 ms during fault 3 while the longest fsync in the capture was 211 ms. The store commits one request at a time, so requests queue behind a slow fsync, and the client measures from when each was due.
- A triggered capture records what follows a spike, up to one sampling interval late, so it can miss the peak. The always-on histogram is what fired the trigger.
- Fault 1 needed tuning to show up. At a 10 ms delay, the change only moved `RESP` by one log2 bucket, and that overlapped healthy runs. A smaller real slowdown could be hard to spot in `top`.
- Every fault here was injected alone, on one machine, over loopback.
