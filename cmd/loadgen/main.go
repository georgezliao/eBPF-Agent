// Command loadgen issues 1-byte read syscalls on /dev/zero at a fixed total rate (or as
// fast as possible) and prints achieved throughput and per-call latency as one JSON line.
// It's the workload for the overhead benchmarks: each call is one traced syscall, and
// /dev/zero makes the call itself cheap, so probe overhead is a large share of it.
package main

import (
	"encoding/json"
	"flag"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"
)

// Every sampleEvery-th call is timed; timing every call would add two clock reads to a
// ~150ns syscall.
const sampleEvery = 16

type result struct {
	Workers   int     `json:"workers"`
	TargetOps int     `json:"target_ops_per_sec"`
	Ops       uint64  `json:"ops"`
	OpsPerSec float64 `json:"ops_per_sec"`
	P50Ns     int64   `json:"p50_ns"`
	P99Ns     int64   `json:"p99_ns"`
}

func main() {
	workers := flag.Int("workers", 2, "goroutines issuing reads, each locked to its own thread")
	rate := flag.Int("rate", 0, "total reads per second (0 = as fast as possible)")
	dur := flag.Duration("duration", 5*time.Second, "how long to run")
	flag.Parse()

	var (
		mu      sync.Mutex
		total   uint64
		samples []time.Duration
		wg      sync.WaitGroup
	)
	start := time.Now()
	deadline := start.Add(*dur)
	for range *workers {
		wg.Go(func() {
			n, s := worker(deadline, *rate / *workers)
			mu.Lock()
			total += n
			samples = append(samples, s...)
			mu.Unlock()
		})
	}
	wg.Wait()
	elapsed := time.Since(start)

	slices.Sort(samples)
	r := result{Workers: *workers, TargetOps: *rate, Ops: total, OpsPerSec: float64(total) / elapsed.Seconds()}
	if len(samples) > 0 {
		r.P50Ns = int64(samples[len(samples)/2])
		r.P99Ns = int64(samples[len(samples)*99/100])
	}
	json.NewEncoder(os.Stdout).Encode(r)
}

// worker reads until deadline. With a rate, it works in 1ms slots and issues the slot's
// share of calls back to back, then sleeps to the next slot; a slot that falls behind
// isn't made up later, so the achieved rate shows when the target is out of reach.
func worker(deadline time.Time, rate int) (uint64, []time.Duration) {
	fd, err := syscall.Open("/dev/zero", syscall.O_RDONLY, 0)
	if err != nil {
		panic(err)
	}
	defer syscall.Close(fd)
	buf := make([]byte, 1)
	var (
		n       uint64
		samples []time.Duration
	)
	call := func() {
		if n%sampleEvery == 0 {
			t := time.Now()
			syscall.Read(fd, buf)
			samples = append(samples, time.Since(t))
		} else {
			syscall.Read(fd, buf)
		}
		n++
	}

	const slot = time.Millisecond
	perSlot := rate / int(time.Second/slot)
	next := time.Now()
	for time.Now().Before(deadline) {
		if rate == 0 {
			for range 1000 {
				call()
			}
			continue
		}
		for range perSlot {
			call()
		}
		next = next.Add(slot)
		if d := time.Until(next); d > 0 {
			time.Sleep(d)
		} else {
			next = time.Now()
		}
	}
	return n, samples
}
