package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/georgezliao/tracelens/internal/probe"
)

// metrics holds system-wide per-op totals for the Prometheus endpoint. Totals are built
// from interval deltas rather than read straight from the kernel map, because the map
// forgets a process when it exits and the sums would go backwards.
//
// Labels are op only. Per-PID labels would create a new series for every process that
// ever ran; use `top` or `trace` for per-process detail.
type metrics struct {
	mu     sync.Mutex
	totals [probe.NumOps]probe.Stats
	drops  func() (uint64, error)
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

func (m *metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	drops, err := m.drops()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.mu.Lock()
	totals := m.totals
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	writeMetrics(w, &totals, drops)
}

// writeMetrics renders the Prometheus text format. Syscall ops and run-queue delay go in
// separate families since runq isn't a syscall.
func writeMetrics(w io.Writer, totals *[probe.NumOps]probe.Stats, drops uint64) {
	fmt.Fprintln(w, "# HELP tracelens_syscalls_total Completed syscalls.")
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
		writeHist(w, "tracelens_syscall_duration_seconds", fmt.Sprintf("op=%q", op), &totals[op])
	}
	fmt.Fprintln(w, "# HELP tracelens_runq_delay_seconds Time a thread spent runnable before getting a CPU.")
	fmt.Fprintln(w, "# TYPE tracelens_runq_delay_seconds histogram")
	writeHist(w, "tracelens_runq_delay_seconds", "", &totals[probe.OpRunq])
	fmt.Fprintln(w, "# HELP tracelens_events_dropped_total Trace events lost because the ring buffer was full.")
	fmt.Fprintln(w, "# TYPE tracelens_events_dropped_total counter")
	fmt.Fprintf(w, "tracelens_events_dropped_total %d\n", drops)
}

func writeHist(w io.Writer, name, labels string, s *probe.Stats) {
	braces := func(extra string) string {
		if l := strings.Trim(labels+","+extra, ","); l != "" {
			return "{" + l + "}"
		}
		return ""
	}
	var cum uint64
	for i, n := range s.Hist[:len(s.Hist)-1] {
		cum += n
		le := float64(uint64(1)<<(i+1)) / 1e9 // slot i holds [2^i, 2^(i+1)) ns
		fmt.Fprintf(w, "%s_bucket%s %d\n", name, braces(fmt.Sprintf("le=\"%g\"", le)), cum)
	}
	fmt.Fprintf(w, "%s_bucket%s %d\n", name, braces(`le="+Inf"`), s.Count)
	fmt.Fprintf(w, "%s_sum%s %g\n", name, braces(""), float64(s.TotalNs)/1e9)
	fmt.Fprintf(w, "%s_count%s %d\n", name, braces(""), s.Count)
}

func syscallOps() []probe.Op {
	return probe.Ops[:probe.OpRunq]
}
