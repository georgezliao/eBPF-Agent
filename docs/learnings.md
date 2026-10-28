# Things I learned building TraceLens

These are notes on what surprised me while building this, roughly in the order it happened. The numbers are from the Lima VM described in [benchmarks.md](benchmarks.md) unless I say otherwise.

## The Go program I wanted to debug is mostly invisible to this tool

The original idea was the classic story: a Go API server goes from 15 ms to 250 ms per request, you run the tracer, and read syscall p99 jumps. So one of the first integration tests did 20 one-byte reads from a pipe where the writer sleeps 5 ms before each byte. Every read should block for about 5 ms.

With `os.File.Read` the probe saw 40 reads, 20 of them failing, and zero reads over 4 ms. With raw `syscall.Read` on a blocking fd, it saw 20 reads, all in the 4–8 ms bucket.

What's going on is that Go puts pipes and sockets in non-blocking mode. A read with no data returns EAGAIN right away, the goroutine parks in the netpoller, and the actual waiting happens in `epoll_pwait`, which I'm not tracing. Then a second read gets the data instantly. So for socket I/O in a Go server, read latency is basically always tiny, however slow the client is. Same for anything else built on epoll.

Two things came out of that. EAGAIN and EINPROGRESS stopped counting as errors, because otherwise every healthy Go process looked like it was failing half its reads. And the demo ended up being about `fdatasync` instead of network reads, since regular file I/O in Go is still plain blocking syscalls. I think the "real" fix for network services would be off-CPU time or tracing `epoll_wait`, but I haven't tried either.

## Measuring the wait between two fast reads

Later I came back to this. Off-CPU time and `epoll_wait` both have the same problem: one `epoll_wait` covers every fd the program is waiting on, and an idle server is "off CPU" most of the time anyway, so neither tells you which connection was slow. What I ended up doing is simpler. A read that returns EAGAIN records a timestamp keyed by (process, fd), and the next read on that fd that actually returns something closes it out. It's keyed by fd and not by thread because Go retries the read from whatever thread picks up the goroutine. The 20-pipe-read test that used to show nothing now shows 20 waits of about 5 ms, which is what was really happening.

Then I tried it on a Go HTTP client calling a server with a 50 ms handler, with the client sleeping 100 ms between requests. Both processes reported a read wait of about 155 ms, the full cycle and not the 50 ms I expected. I think it's because Go's HTTP server and client both keep a read pending on a keep-alive connection, so the wait includes the idle gap between requests. With keep-alive turned off, the client showed 51 ms and the server under 1 ms, which matches. So read wait is the response time only when nothing reads the connection while it's idle, and that's not the case for most Go HTTP code. It's still more than zero information, but it's not the clean "your backend got slow" number I was hoping for.

Also, fds get reused. If a program gets EAGAIN on fd 7, closes it, and opens something new that lands on 7, the first read on the new file would report a wait that started on the old one. So `close` is traced now too, just to delete the entry. My test for this first failed with `EINVAL` from `dup3`, because I was trying to move the new pipe onto the old fd number and the kernel had already given it that number.

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

That guess sat in the docs for a while, with a plan item to check it on bare metal with `perf stat`. Then I reran the paced benchmark for an unrelated reason and got 85 ns. So I ran it nine more times. The aggregated path went anywhere from 79 to 174 ns per run, and the raw path from 210 to 341 ns, so raw was the slower one in every run. The 248 was one unlucky run, and I'd built a cache theory on top of a single sample. Lesson for me: run anything surprising at least three times before explaining it.

## Idle CPUs make syscalls slower, which wrecked my latency numbers

The weirdest result: at paced rates, the workload's read p99 was *lower* with raw tracing than with no tracing at all (1.0 µs vs 1.4 µs at 1M reads/s). That obviously can't mean tracing makes reads faster.

So I ran the baseline twice at 500k reads/s, once normally and once with an unrelated `while :; do :; done` loop on another vCPU. p50 went from about 295 ns to about 225 ns, and p99 from about 1.4 µs to 0.6 µs. Just having *something* busy in the VM makes everything faster, I guess because vCPUs and host cores aren't dropping into idle states. Raw mode's consumer is exactly that busy thing.

That basically means the paced latency comparisons between modes are measuring how idle the VM is. I kept them in the CSV but the writeup leans on unthrottled throughput and BPF run time instead. Doing this properly would need isolated, pinned CPUs with a fixed frequency, which I can't get in a VM on a laptop. I also noticed the demo's p50 going *down* during the bulk write, which might be the same effect, but I didn't test that.

## A bigger ring buffer doesn't fix a slow consumer

At 500k and 1M events/s, going from 16 KiB to 256 KiB took drops from 8.5% and 14% down to zero. Unthrottled, drops were 40–48% at every size up to 16 MiB. The consumer was only getting through about 2.2M events/s, and if events come in faster than that forever, a bigger buffer just fills up a few milliseconds later. Obvious once I saw it, but I kind of expected the curve to at least bend.

Later I went back and benchmarked the consumer's two suspects separately. `binary.Read` took about 220 ns and two allocations per event, and the mutex-guarded update took 6 ns, so it was basically all decoding. Reading the fields by offset got decode down to 7.5 ns. After that, the consumer kept up with about 3.8M events/s instead of 2.2M, and unthrottled drops went from 40-48% to about 6%. It's a bit funny that half the "cost of exporting events to userspace" in my first benchmark was just a slow decoding function.

## Triggered capture sees the aftermath

In the demo, the worst seconds were when the service only managed 30 requests with a p99 of 361 ms. The agent checks once per second, so it only noticed after that interval ended, and by the time the 5-second capture started the worst writeback was over. The capture's max was 80 ms.

The always-on histogram did catch the spike, since that's what fired the trigger. So I think the two modes really are complementary: the cheap histogram tells you it happened and roughly how bad, and the capture tells you which calls in what order, as long as the problem keeps going. In an earlier run the trigger fired on writeback left over from the *previous* run, before I'd even injected anything, and the one-minute cooldown then skipped the real spike. The demo does a `sync` first now.

## Smaller things

- Docker Desktop was unusable here: image pulls hung on `docker-credential-desktop`, and containers with a bind mount from `~/Desktop` sat in "Created" forever, I think because of macOS folder permissions. A Lima Ubuntu VM ended up being better anyway, since it's a normal kernel with BTF.
- Apple's clang has no BPF backend; Homebrew's LLVM does.
- bpf2go got confused when a map and a struct were both named `config`. It also won't generate a Go type for a struct that's only used in `sizeof()` for a ring buffer reservation, so there's a dummy `const struct event *unused_event` to keep it in BTF.
- Replacing cilium's example `common.h` (a mini `vmlinux.h` with XDP structs I didn't use) with a 30-line header produced a byte-identical `.o`. That's how I found out bpf2go output is deterministic, which made `make check-generated` possible.
- Unit tests run on macOS, and one caught `unix.EAGAIN` being 35 there instead of 11. The return values come from a Linux kernel, so the Linux values are hardcoded now.
- `dd` writing a big file had a couple of writes take about 210 ms while p50 was 21 µs. I'd bet that's dirty-page throttling, but I'd need writeback tracepoints or stacks to know.
- I dropped TCP retransmits. The tracepoint mostly fires in softirq context, so whatever process happens to be "current" isn't the socket's owner.

## Open questions

- Is there a cheap way to separate idle keep-alive time from real response time in read wait? Maybe timing from the last `write` on the same fd instead of from the EAGAIN, but that assumes request/response.
- Per-CPU stats values are 352 bytes per CPU. On a 64-core machine the map would be around 180 MB. At some core count a shared hash with atomics is probably the better trade, but I haven't measured where.
