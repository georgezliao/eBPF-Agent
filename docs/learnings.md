# Things I learned building TraceLens

These are notes on what surprised me while building this, roughly in the order it happened. The numbers are from the Lima VM described in [benchmarks.md](benchmarks.md) unless I say otherwise.

## The Go program I wanted to debug is mostly invisible to this tool

The original idea was the classic story: a Go API server goes from 15 ms to 250 ms per request, you run the tracer, and read syscall p99 jumps. So one of the first integration tests did 20 one-byte reads from a pipe where the writer sleeps 5 ms before each byte. Every read should block for about 5 ms.

With `os.File.Read` the probe saw 40 reads, 20 of them failing, and zero reads over 4 ms. With raw `syscall.Read` on a blocking fd, it saw 20 reads, all in the 4–8 ms bucket.

What's going on is that Go puts pipes and sockets in non-blocking mode. A read with no data returns EAGAIN right away, the goroutine parks in the netpoller, and the actual waiting happens in `epoll_pwait`, which I'm not tracing. Then a second read gets the data instantly. So for socket I/O in a Go server, read latency is basically always tiny, however slow the client is. Same for anything else built on epoll.

Two things came out of that. EAGAIN and EINPROGRESS stopped counting as errors, because otherwise every healthy Go process looked like it was failing half its reads. And the demo ended up being about `fdatasync` instead of network reads, since regular file I/O in Go is still plain blocking syscalls. I think the "real" fix for network services would be off-CPU time or tracing `epoll_wait`, but I haven't tried either.

## Measuring the wait between two fast reads

Later I came back to this. Off-CPU time and `epoll_wait` both have the same problem: one `epoll_wait` covers every fd the program is waiting on, and an idle server is "off CPU" most of the time anyway, so neither tells you which connection was slow. What I ended up doing is simpler. A read that returns EAGAIN records a timestamp keyed by (process, fd), and the next read on that fd that actually returns something closes it out. It's keyed by fd and not by thread because Go retries the read from whatever thread picks up the goroutine. The 20-pipe-read test that used to show nothing now shows 20 waits of about 5 ms, which is what was really happening.

Then I tried it (in the same Lima VM, with a throwaway program I didn't keep) on a Go HTTP client calling a server with a 50 ms handler, with the client sleeping 100 ms between requests. Both processes reported a read wait of about 155 ms, the full cycle and not the 50 ms I expected. I think it's because Go's HTTP server and client both keep a read pending on a keep-alive connection, so the wait includes the idle gap between requests. With keep-alive turned off, the client showed 51 ms and the server under 1 ms, which matches. So read wait is the response time only when nothing reads the connection while it's idle, and that's not the case for most Go HTTP code. It's still more than zero information, but it's not the clean "your backend got slow" number I was hoping for.

So I added the obvious next thing: remember the last write on that fd during the wait, and also report the time since that write. On the same setup the client now shows 52 ms next to its 155 ms read wait, which is the handler plus a bit. The server shows 102 ms, and for a second that looked wrong until I realized it's the client's 100 ms sleep. From the server's side, "I wrote something and then waited for a reply" describes the gap between requests. The number is right, it's just answering a question nobody asked. I don't see a way to tell the two sides apart from syscalls alone, so the docs say to read it on the side that's waiting.

Also, fds get reused. If a program gets EAGAIN on fd 7, closes it, and opens something new that lands on 7, the first read on the new file would report a wait that started on the old one. So `close` is traced now too, just to delete the entry. My test for this first failed with `EINVAL` from `dup3`: I was moving the new pipe onto the old fd number, but the kernel always hands out the lowest free number, so it was already there.

## Tracing yourself never goes quiet

After adding run-queue delay, one of the integration tests started hanging forever. The test traces its own process and has a `drain()` helper that reads events until nothing arrives for 200 ms.

The problem: every time the test reads an event, the reader goroutine gets woken up, and that wakeup is a new run-queue event for the traced process. So reading events creates events, and the stream never goes silent. I changed `drain()` to read for a fixed window. It's a small thing, but it was a pretty direct example of the observer being part of what's observed.

The demo had a less cute version of the same thing. The triggered capture wrote one JSON line per `write(2)`, and during the capture the agent itself was the busiest writer in `top` at about 2,250 writes/s. Buffering the output fixed that.

## A second Claude as a reviewer was worth it

After each phase I ran a separate headless Claude session with read-only tools and asked it to review the diff like a manager who didn't write the code. I expected style nitpicks. Every round found at least one real bug:

- Right after I stopped counting EAGAIN as an error, the code path for `ret = -11` fell through to `bytes += ret`, adding a negative number to an unsigned counter.
- In `sched_switch` I only treated the outgoing task as still runnable if `prev_state == 0`. But a task can get preempted after setting `TASK_INTERRUPTIBLE` and before it calls `schedule()`, and then it has a non-zero state while staying on the run queue. The tracepoint has a `preempt` argument for exactly this, and I wasn't reading it. My spinner test didn't catch it, because spinners never sleep.
- The kernel was deleting a process's stats when it exited, which meant anything a process did since the last sample was lost, and a process shorter than one interval was never counted at all. Now userspace deletes the keys after it has read them.
- A new capture could read events left in the ring buffer by the previous capture of a different process.

It also had opinions I didn't take. It wanted me to drop the ring-size parameter to `probe.Open` as speculative, but it hadn't seen the commit where a test used it. The first review run actually came back empty: the reviewer session inherited this repo's Stop hook, which told it to go do the next item in the plan. Kind of funny in hindsight.

## In-kernel aggregation is cheaper, but not in the way I assumed

My mental model was that aggregating in BPF maps is cheap and shipping events to userspace is expensive. That's true for the agent: aggregated mode used about 0% of a core at every rate, while raw mode used about 30% of a core at 1M reads/s and saturated a full core unthrottled.

But the cost per syscall, which the traced program pays, was more complicated. With `kernel.bpf_stats_enabled` on and the workload running flat out, the aggregation path took about 60 ns per program run versus 117 ns for the ring buffer path. When the workload was paced to 500k reads/s, the aggregation path went up to 248 ns per run while the ring buffer path stayed at 123 ns. Same code, four times slower. My guess was cache: with 1 ms pauses between bursts, the per-CPU stats value and its hash bucket go cold, while the ring buffer keeps getting touched by the consumer on another CPU.

That guess sat in the docs for a while, with a plan item to check it on bare metal with `perf stat`. Then I reran the paced benchmark for an unrelated reason and got 85 ns. So I ran it nine more times. The aggregated path went anywhere from 79 to 174 ns per run, and the raw path from 210 to 341 ns, so raw was the slower one in every run. The 248 was one unlucky run, and I'd built a cache theory on top of a single sample. Lesson for me: run anything surprising at least three times before explaining it. Later, on a quiet host and with a load generator that spins instead of sleeping, aggregation took 55 ns per run paced versus 53 ns flat out. That doesn't fully kill the cache idea, because spinning removes the pauses it depended on, and I never reran the sleeping version on a quiet host. But I no longer have any evidence for it. Raw is the mode that gets worse when paced, nearly four times the BPF time per read, which I come back to below.

## Idle CPUs make syscalls slower, which wrecked my latency numbers

The weirdest result: at paced rates, the workload's read p99 was *lower* with raw tracing than with no tracing at all (1.0 µs vs 1.4 µs at 1M reads/s). That obviously can't mean tracing makes reads faster.

So I ran the baseline twice at 500k reads/s, once normally and once with an unrelated `while :; do :; done` loop on another vCPU. p50 went from about 295 ns to about 225 ns, and p99 from about 1.4 µs to 0.6 µs. Just having *something* busy in the VM makes everything faster, I guess because vCPUs and host cores aren't dropping into idle states. Raw mode's consumer is exactly that busy thing.

That basically means the paced latency comparisons between modes are measuring how idle the VM is. I kept them in the CSV but the writeup leans on unthrottled throughput and BPF run time instead. Doing this properly would need isolated, pinned CPUs with a fixed frequency, which I can't get in a VM on a laptop. I also noticed the demo's p50 going *down* during the bulk write, which might be the same effect, but I didn't test that.

## A bigger ring buffer doesn't fix a slow consumer

At 500k and 1M events/s, going from 16 KiB to 256 KiB took drops from 8.5% and 14% down to zero. Unthrottled, drops were 40–48% at every size up to 16 MiB. The consumer was only getting through about 2.2M events/s, and if events come in faster than that forever, a bigger buffer just fills up a few milliseconds later. Obvious once I saw it, but I kind of expected the curve to at least bend.

Later I went back and benchmarked the consumer's two suspects separately. `binary.Read` took about 220 ns and two allocations per event, and the mutex-guarded update took 6 ns, so it was basically all decoding. Reading the fields by offset got decode down to 7.5 ns. After that, the consumer kept up with about 3.8M events/s instead of 2.2M, and unthrottled drops went from 40-48% to about 6%. It's a bit funny that half the "cost of exporting events to userspace" in my first benchmark was just a slow decoding function.

## My load generator was hiding stalls, and then I broke it

`loadgen` turned out to have the problem Gil Tene calls coordinated omission. It timed each call from when the call started, so if the thread stalled for 5 ms, that showed up as one slow sample and the thousands of calls that should have run during those 5 ms just never counted. I changed it to a fixed schedule, where a call's response time is measured from when it was due.

The difference was kind of ridiculous. At 1M reads/s with no tracing at all, service p99 was 0.7 µs and response p99 was 4 ms. I couldn't use that to rank the tracing modes, since the ranges overlap and at 500k the untraced baseline came out worse than aggregated.

Then the unthrottled baseline came back as 5.3M reads/s instead of 10M, and for a moment that looked like a finding. It was my bug: the new loop read the clock on every call, and a clock read costs nearly as much as reading a byte from `/dev/zero`. After fixing that it was 8M, still not 10M, and aggregated cost looked like it had doubled. Three features had just gone in, so they were the obvious suspect. But the old build measured the same under the same conditions, and my Mac's load average was 28. After the 248 ns thing I'm glad I checked before writing that one down.

The real answer came from rerunning everything with the Mac mostly idle, at a load average of about 3 instead of 28. Baseline went to 12.3M reads/s, higher than anything I'd measured. Aggregated cost was 53 or 54 ns per run in all five runs, where before it had bounced between 85 and 146. The 4 ms response p99 at 1M became 2.6 µs. I think those stalls were mostly my laptop being busy, though it's one before and after, and an 18 ms stall still showed up in a quiet run. Service time was under a microsecond both times, so response time was seeing something service time wasn't, which is a pretty good argument for the new measurement.

Going from baseline to aggregated adds about 111 ns per read to each thread, and the BPF programs took about 105 ns, so BPF explains most of the loss. It's a rough match, since those came from separate runs. The old-vs-new comparison settled too: 53 ns against 53–55 ns, so the new hooks don't noticeably touch the read path.

I also wish I'd recorded host load from the start. Nothing else in this project moved the numbers as much, and for most of the early benchmarks I can't tell you what it was.

## Triggered capture sees the aftermath

In the first version of the demo, the worst seconds were when the service only managed 30 requests with a p99 of 361 ms. The agent checks once per second, so it only noticed after that interval ended, and by the time the 5-second capture started the worst writeback was over. The capture's max was 80 ms.

The always-on histogram did catch the spike, since that's what fired the trigger. So I think the two modes really are complementary: the cheap histogram tells you it happened and roughly how bad, and the capture tells you which calls in what order, as long as the problem keeps going. In an earlier run the trigger fired on writeback left over from the *previous* run, before I'd even injected anything, and the one-minute cooldown then skipped the real spike. The demo does a `sync` first now.

## What `dd` was actually waiting for

Early on, `dd` writing a big file had a couple of writes take about 210 ms while p50 was 21 µs, and I wrote that I'd bet it was dirty-page throttling. So once `tracelens offcpu` existed I actually checked, running `trace` and `offcpu` on the same `dd` three times in the Lima VM. I didn't keep the raw output, so these numbers are from reading it at the time.

Mostly I was right, but not completely. In two runs the longest writes were 55.7 and 54.5 ms and the longest sleeps were 54.6 and 52.4 ms, and those sleeps were in `balance_dirty_pages`, so the slowest writes basically are that one sleep. In the other run the top stack was something else: `rwsem_down_write_slowpath` under `ext4_da_map_blocks`, waiting on an ext4 lock for up to 47 ms. I guess that's contention with writeback, but all I actually know is the stack. And I never got 210 ms again, the worst write was about 56 ms, so I still can't say what those original writes were. Also, `dd` was asleep for only 0.2–0.6 s of each 3.3 s run, so this is a mostly on-CPU program with a few big stalls, which `top`'s p99 wouldn't have told me.

## Retransmits came back once I stopped asking softirq who it was

I'd dropped TCP retransmits because the tracepoint mostly fires in softirq, where "current" is whoever happened to be running. The fix is pretty simple in hindsight: don't ask at retransmit time. Remember which process owns each `struct sock *` when it calls `tcp_sendmsg` or `connect`, then look it up later. The test uses `netem` to drop 20% of packets on loopback while a child process sends 4 MiB, and the runs I looked at gave 20–36 retransmits on the child and none anywhere else.

## The demo that finally used everything together

The first demo only had one fault, so I rebuilt it as a client calling a service, with three different things going wrong. The nice part is that the client gets slower every time and you can't tell why from its side, but in `top` each fault moves a different column: `RESP` alone, then `RETRANS/s`, then `FSYNC p99`. It did that in all three runs, which I honestly wasn't sure about.

A few things went wrong getting there. My first slow-service fault was 40 ms, and with 4 workers on a 20 ms schedule the service just couldn't keep up, so latency climbed to 3 s and it looked like a totally different problem. At 10 ms it was too small to see, because `top`'s p99 is log2 buckets and 17 to 34 ms is one step that healthy runs land on anyway. That one bugs me a little, since a real 10 ms regression could hide there. And `top` caught a bug in my own demo: `CONN/s` was 73 on a client that was supposed to use keep-alive, because Go only keeps two idle connections per host by default and I had four workers.

`offcpu` was useless at first on a Go program, since the top of the list is always idle threads in `futex` and `ep_poll`. Filtering by a frame name fixed that, and then the store's slow fsyncs turned out to be waiting in `jbd2_log_wait_commit`, an ext4 journal commit, in all three runs. So that's one more layer than "fsync is slow", though it still doesn't say who else was filling the journal.

Adding user stacks made this feel a lot more real. The same sleep now continues down through `syscall.Fdatasync` to `main.main.func1` and `net/http`, so one stack goes all the way from the ext4 journal to the HTTP handler. Go made that easy, since it keeps frame pointers and a symbol table.

## The expensive part of the ring buffer was the doorbell

I'd been wondering why raw mode cost nearly four times more per read at 500k reads/s than flat out, when you'd think a quieter system would be cheaper. Per-program stats put it all in `exit_read`, 858 ns per run against 218. So I built a version that submits with `BPF_RB_NO_WAKEUP` and has the consumer poll every millisecond instead, and it went from about 1040 ns per read to about 260.

So the expensive part isn't writing the event, it's waking the reader. Flat out, the consumer is already awake and draining, so nobody needs waking. At a moderate rate it keeps falling asleep and every wakeup costs something. That's a little backwards from what I'd assumed, that a busy system pays more. It might be worse in a VM too, I haven't tried bare metal. And I only measured the BPF side, so I don't know what polling costs the agent.

## Benchmarking next to my own other work

The first attempt at measuring the other hook paths was garbage. Load went from 4 to 44 partway through because another project of mine was running on the same laptop, and baseline `read` ranged from 5.4M to 11.9M. One row said tracing made things 11 µs faster. I'd only checked the load at the start.

So now `benchmarks/quiet.sh` checks before and after each batch and throws the batch away if the load went up. That got 18 clean batches and 3 discards, and the numbers lined up. The new hooks made no measurable difference on any path: `write`, EAGAIN, `close`, sleeps or TCP.

One thing I can't explain yet: the pipe ping-pong workload got faster with `offcpu` attached, 96k ops/s against 83k with no tracing, in all three runs. I'd guess it's the same thing as the idle vCPU effect, since that workload sleeps constantly, but I haven't tested it.

## Smaller things

- Docker Desktop was unusable here: image pulls hung on `docker-credential-desktop`, and containers with a bind mount from `~/Desktop` sat in "Created" forever, I think because of macOS folder permissions. A Lima Ubuntu VM ended up being better anyway, since it's a normal kernel with BTF.
- Apple's clang has no BPF backend; Homebrew's LLVM does.
- bpf2go got confused when a map and a struct were both named `config`. It also won't generate a Go type for a struct that's only used in `sizeof()` for a ring buffer reservation, so there's a dummy `const struct event *unused_event` to keep it in BTF.
- Replacing cilium's example `common.h` (a mini `vmlinux.h` with XDP structs I didn't use) with a 30-line header produced a byte-identical `.o`. That's how I found out bpf2go output is deterministic, which made `make check-generated` possible.
- Unit tests run on macOS, and one caught `unix.EAGAIN` being 35 there instead of 11. The return values come from a Linux kernel, so the Linux values are hardcoded now.

## Open questions

- Per-CPU stats values are 352 bytes per CPU. On a 64-core machine the map would be around 180 MB. At some core count a shared hash with atomics is probably the better trade, but I haven't measured where.
