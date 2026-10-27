package main

import (
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"sync"

	"github.com/georgezliao/tracelens/internal/probe"
)

// metrics holds system-wide per-op totals for the Prometheus endpoint. Totals are built
// from interval deltas rather than read straight from the kernel map, because the agent
// deletes exited processes from the map and the sums would go backwards.
//
// Labels are op only. Per-PID labels would create a new series for every process that
// ever ran; use `top` or `trace` for per-process detail.
type metrics struct {
	mu     sync.Mutex
	totals [probe.NumOps]probe.Stats
	probe  *probe.Probe
}

func (m *metrics) add(delta map[probe.Key]probe.Stats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, d := range delta {
		t := &m.totals[k.Op]
		t.Count += d.Count
		t.Errors += d.Errors
		t.Bytes += d.Bytes
		t.TotalNs += d.TotalNs
		for i := range t.Hist {
			t.Hist[i] += d.Hist[i]
		}
	}
}

// addEvent folds one event into the totals the same way the kernel's record() would, for
// raw mode where the kernel doesn't aggregate.
func (m *metrics) addEvent(e *probe.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &m.totals[e.Op]
	t.Count++
	t.TotalNs += e.DurationNs
	t.Hist[min(max(bits.Len64(e.DurationNs)-1, 0), len(t.Hist)-1)]++
	if probe.Failed(e.Ret) {
		t.Errors++
	} else if e.Ret > 0 && (probe.Op(e.Op) == probe.OpRead || probe.Op(e.Op) == probe.OpWrite) {
		t.Bytes += uint64(e.Ret)
	}
}

func (m *metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	events, stats, err := m.probe.Lost()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runs, runtime, err := m.probe.RunStats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.mu.Lock()
	totals := m.totals
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	writeMetrics(w, &totals, events, stats)
	fmt.Fprintln(w, "# HELP tracelens_bpf_runs_total BPF program runs (0 unless sysctl kernel.bpf_stats_enabled=1).")
	fmt.Fprintln(w, "# TYPE tracelens_bpf_runs_total counter")
	fmt.Fprintf(w, "tracelens_bpf_runs_total %d\n", runs)
	fmt.Fprintln(w, "# HELP tracelens_bpf_run_seconds_total Time spent in BPF programs (0 unless kernel.bpf_stats_enabled=1).")
	fmt.Fprintln(w, "# TYPE tracelens_bpf_run_seconds_total counter")
	fmt.Fprintf(w, "tracelens_bpf_run_seconds_total %g\n", runtime.Seconds())
}

// writeMetrics renders the Prometheus text format. Run-queue delay gets its own family
// (still labelled op="runq") since it isn't a syscall.
func writeMetrics(w io.Writer, totals *[probe.NumOps]probe.Stats, lostEvents, lostStats uint64) {
	fmt.Fprintln(w, "# HELP tracelens_syscalls_total Completed read, write, openat, connect and fsync (incl. fdatasync) syscalls.")
	fmt.Fprintln(w, "# TYPE tracelens_syscalls_total counter")
	for _, op := range syscallOps() {
		fmt.Fprintf(w, "tracelens_syscalls_total{op=%q} %d\n", op, totals[op].Count)
	}
	fmt.Fprintln(w, "# HELP tracelens_syscall_errors_total Syscalls that returned an error other than EAGAIN or EINPROGRESS.")
	fmt.Fprintln(w, "# TYPE tracelens_syscall_errors_total counter")
	for _, op := range syscallOps() {
		fmt.Fprintf(w, "tracelens_syscall_errors_total{op=%q} %d\n", op, totals[op].Errors)
	}
	fmt.Fprintln(w, "# HELP tracelens_syscall_bytes_total Bytes returned by read and write syscalls (not device I/O).")
	fmt.Fprintln(w, "# TYPE tracelens_syscall_bytes_total counter")
	for _, op := range []probe.Op{probe.OpRead, probe.OpWrite} {
		fmt.Fprintf(w, "tracelens_syscall_bytes_total{op=%q} %d\n", op, totals[op].Bytes)
	}
	fmt.Fprintln(w, "# HELP tracelens_syscall_duration_seconds Time from syscall entry to exit.")
	fmt.Fprintln(w, "# TYPE tracelens_syscall_duration_seconds histogram")
	for _, op := range syscallOps() {
		writeHist(w, "tracelens_syscall_duration_seconds", op, &totals[op])
	}
	fmt.Fprintln(w, "# HELP tracelens_runq_delay_seconds Time a thread spent runnable before getting a CPU.")
	fmt.Fprintln(w, "# TYPE tracelens_runq_delay_seconds histogram")
	writeHist(w, "tracelens_runq_delay_seconds", probe.OpRunq, &totals[probe.OpRunq])
	fmt.Fprintln(w, "# HELP tracelens_events_dropped_total Trace events lost because the ring buffer was full.")
	fmt.Fprintln(w, "# TYPE tracelens_events_dropped_total counter")
	fmt.Fprintf(w, "tracelens_events_dropped_total %d\n", lostEvents)
	fmt.Fprintln(w, "# HELP tracelens_stats_dropped_total Stats updates lost because the per-process stats map was full.")
	fmt.Fprintln(w, "# TYPE tracelens_stats_dropped_total counter")
	fmt.Fprintf(w, "tracelens_stats_dropped_total %d\n", lostStats)
}

func writeHist(w io.Writer, name string, op probe.Op, s *probe.Stats) {
	var cum uint64
	for i, n := range s.Hist[:len(s.Hist)-1] {
		cum += n
		le := float64(uint64(1)<<(i+1)) / 1e9 // slot i holds [2^i, 2^(i+1)) ns
		fmt.Fprintf(w, "%s_bucket{op=%q,le=\"%g\"} %d\n", name, op, le, cum)
	}
	fmt.Fprintf(w, "%s_bucket{op=%q,le=\"+Inf\"} %d\n", name, op, s.Count)
	fmt.Fprintf(w, "%s_sum{op=%q} %g\n", name, op, float64(s.TotalNs)/1e9)
	fmt.Fprintf(w, "%s_count{op=%q} %d\n", name, op, s.Count)
}

func syscallOps() []probe.Op {
	return probe.Ops[:probe.OpRunq]
}
