// Command loadgen repeats an operation at a fixed total rate (or as fast as possible) and
// prints achieved throughput, service time (call start to return) and response time
// (scheduled start to return) as one JSON line. It's the workload for the overhead
// benchmarks. The default op is a 1-byte read of /dev/zero: one traced syscall that's cheap
// in itself, so probe overhead is a large share of it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"
)

// Every sampleEvery-th call is timed; timing every call would add two clock reads to a
// ~160ns syscall.
const sampleEvery = 16

type result struct {
	Workers   int     `json:"workers"`
	TargetOps int     `json:"target_ops_per_sec"`
	Ops       uint64  `json:"ops"`
	OpsPerSec float64 `json:"ops_per_sec"`
	P50Ns     int64   `json:"p50_ns"`
	P99Ns     int64   `json:"p99_ns"`
	RespP50Ns int64   `json:"response_p50_ns"`
	RespP99Ns int64   `json:"response_p99_ns"`
	RespMaxNs int64   `json:"response_max_ns"`
}

// samples holds the timings of every sampleEvery-th call.
type samples struct {
	service, response []time.Duration
}

func main() {
	workers := flag.Int("workers", 2, "goroutines issuing ops, each locked to its own thread")
	rate := flag.Int("rate", 0, "total ops per second (0 = as fast as possible)")
	name := flag.String("op", "read", "operation: read, write, eagain, openclose, pingpong or tcp")
	dur := flag.Duration("duration", 5*time.Second, "how long to run")
	flag.Parse()

	workload, ok := workloads[*name]
	if !ok {
		fmt.Fprintln(os.Stderr, "loadgen: unknown -op", *name)
		os.Exit(2)
	}

	var (
		mu    sync.Mutex
		total uint64
		all   samples
		wg    sync.WaitGroup
	)
	start := time.Now()
	deadline := start.Add(*dur)
	for range *workers {
		wg.Go(func() {
			n, s := worker(deadline, *rate / *workers, workload)
			mu.Lock()
			total += n
			all.service = append(all.service, s.service...)
			all.response = append(all.response, s.response...)
			mu.Unlock()
		})
	}
	wg.Wait()
	elapsed := time.Since(start)

	r := result{Workers: *workers, TargetOps: *rate, Ops: total, OpsPerSec: float64(total) / elapsed.Seconds()}
	if n := len(all.service); n > 0 {
		slices.Sort(all.service)
		slices.Sort(all.response)
		r.P50Ns = int64(all.service[n/2])
		r.P99Ns = int64(all.service[n*99/100])
		r.RespP50Ns = int64(all.response[n/2])
		r.RespP99Ns = int64(all.response[n*99/100])
		r.RespMaxNs = int64(all.response[n-1])
	}
	json.NewEncoder(os.Stdout).Encode(r)
}

// worker sets up the workload on its own locked thread and runs it.
func worker(deadline time.Time, rate int, workload func() (func(), error)) (uint64, samples) {
	runtime.LockOSThread()
	op, err := workload()
	if err != nil {
		panic(err)
	}
	return run(deadline, rate, op)
}

// run calls read until deadline. With a rate the load is open-loop: call n is due at
// start + n/rate whatever happened to earlier calls, so after a stall the calls it delayed
// run back to back and their response time includes the wait. Timing only the call itself
// would record one slow sample for the stall and hide the rest (coordinated omission).
func run(deadline time.Time, rate int, read func()) (uint64, samples) {
	if rate == 0 {
		return runUnthrottled(deadline, read)
	}
	var (
		n uint64
		s samples
	)
	start := time.Now()
	for {
		now := time.Now()
		if !now.Before(deadline) {
			return n, s
		}
		due := start.Add(time.Duration(float64(n) * float64(time.Second) / float64(rate)))
		// Spin: time.Sleep's wakeup delay would count as response time, and a sleeping
		// thread lets the vCPU go idle, which changes syscall latency.
		for now.Before(due) {
			now = time.Now()
		}
		if n%sampleEvery == 0 {
			read()
			end := time.Now()
			s.service = append(s.service, end.Sub(now))
			s.response = append(s.response, end.Sub(due))
		} else {
			read()
		}
		n++
	}
}

// runUnthrottled has no schedule, so response time equals service time. The clock is only
// read for samples and once per batch: reading it on every call cut the rate by about a
// third.
func runUnthrottled(deadline time.Time, read func()) (uint64, samples) {
	var (
		n uint64
		s samples
	)
	for time.Now().Before(deadline) {
		for range 1024 {
			if n%sampleEvery == 0 {
				start := time.Now()
				read()
				d := time.Since(start)
				s.service = append(s.service, d)
				s.response = append(s.response, d)
			} else {
				read()
			}
			n++
		}
	}
	return n, s
}
