package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/georgezliao/tracelens/internal/hist"
	"github.com/georgezliao/tracelens/internal/probe"
)

// minTriggerCalls is the fewest calls in an interval for a process to trigger a capture.
// With nearest rank, p99 of fewer than 100 samples is just the slowest call.
const minTriggerCalls = 100

// recapture is how long a PID must wait before it can trigger another capture.
const recapture = time.Minute

// slowest returns the process with the highest p99 for op in this interval, if that p99
// reaches threshold. p99 is a log2 bucket upper bound, so a process can trigger with a
// true p99 as low as threshold/2.
func slowest(delta map[probe.Key]probe.Stats, op probe.Op, threshold time.Duration, skip func(uint32) bool) (uint32, time.Duration, bool) {
	var (
		pid   uint32
		worst time.Duration
	)
	for k, d := range delta {
		if probe.Op(k.Op) != op || d.Count < minTriggerCalls || skip(k.Tgid) {
			continue
		}
		if p99 := hist.Percentile(d.Hist[:], 0.99); p99 >= threshold && p99 > worst {
			pid, worst = k.Tgid, p99
		}
	}
	return pid, worst, worst > 0
}

func runAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	listen := fs.String("listen", ":9090", "address for the Prometheus /metrics endpoint")
	interval := fs.Duration("interval", time.Second, "how often to read the kernel stats")
	trigger := fs.Duration("trigger", 0, "capture a process whose interval p99 for -trigger-op reaches this (0 = off)")
	triggerOp := fs.String("trigger-op", "read", "op watched by -trigger")
	captureFor := fs.Duration("capture", 10*time.Second, "length of a triggered capture")
	dir := fs.String("dir", os.TempDir(), "directory for capture files")
	fs.Parse(args)
	op, err := probe.ParseOp(*triggerOp)
	if err != nil {
		return err
	}

	p, err := probe.Open(0)
	if err != nil {
		return err
	}
	defer p.Close()

	m := &metrics{probe: p}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m)
	srv := &http.Server{Addr: *listen, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server: %v", err)
		}
	}()
	defer srv.Close()
	log.Printf("serving metrics on %s/metrics", *listen)

	self := uint32(os.Getpid())
	lastCapture := map[uint32]time.Time{}
	captureDone := make(chan struct{}, 1)
	capturing := false

	prev, err := p.Stats()
	if err != nil {
		return err
	}
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if capturing {
				<-captureDone // the capture uses p, which is closed on return
			}
			return nil
		case <-captureDone:
			capturing = false
		case now := <-tick.C:
			cur, err := p.Stats()
			if err != nil {
				return err
			}
			delta := probe.Delta(prev, cur)
			m.add(delta)
			if err := p.ForgetExited(cur); err != nil {
				return err
			}
			prev = cur
			for pid, t := range lastCapture {
				if now.Sub(t) >= recapture {
					delete(lastCapture, pid)
				}
			}

			if *trigger == 0 || capturing {
				continue
			}
			skip := func(pid uint32) bool { return pid == self || now.Sub(lastCapture[pid]) < recapture }
			pid, p99, ok := slowest(delta, op, *trigger, skip)
			if !ok {
				continue
			}
			// One capture at a time: the BPF config has a single target slot.
			capturing = true
			lastCapture[pid] = now
			name := filepath.Join(*dir, fmt.Sprintf("tracelens-%d-%s.jsonl", pid, now.Format("20060102-150405")))
			log.Printf("pid %d (%s): %s p99 %s >= %s, capturing %s to %s",
				pid, comm(pid), op, fmtDur(p99), *trigger, *captureFor, name)
			go func() {
				defer func() { captureDone <- struct{}{} }()
				s, drops, err := captureCountingDrops(ctx, p, pid, *captureFor, 0, name)
				if err != nil {
					log.Printf("capture pid %d: %v", pid, err)
					return
				}
				s.print(log.Writer(), drops)
			}()
		}
	}
}
