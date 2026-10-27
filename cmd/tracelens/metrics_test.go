package main

import (
	"strings"
	"testing"

	"github.com/georgezliao/tracelens/internal/probe"
)

func TestMetricsAccumulateAcrossProcesses(t *testing.T) {
	m := &metrics{}
	m.add(map[probe.Key]probe.Stats{
		{Tgid: 1, Op: uint32(probe.OpRead)}: {Count: 3, Bytes: 30, Hist: [40]uint64{10: 3}},
		{Tgid: 2, Op: uint32(probe.OpRead)}: {Count: 2, Errors: 1, Hist: [40]uint64{20: 2}},
	})
	// pid 1 exited: later intervals just don't mention it, totals stay.
	m.add(map[probe.Key]probe.Stats{
		{Tgid: 2, Op: uint32(probe.OpRead)}: {Count: 1, TotalNs: 2e9, Hist: [40]uint64{20: 1}},
	})

	var b strings.Builder
	writeMetrics(&b, &m.totals, 7, 3)
	out := b.String()
	for _, want := range []string{
		`tracelens_syscalls_total{op="read"} 6`,
		`tracelens_syscall_errors_total{op="read"} 1`,
		`tracelens_syscall_bytes_total{op="read"} 30`,
		`tracelens_syscall_duration_seconds_bucket{op="read",le="2.048e-06"} 3`,
		`tracelens_syscall_duration_seconds_bucket{op="read",le="0.002097152"} 6`,
		`tracelens_syscall_duration_seconds_bucket{op="read",le="+Inf"} 6`,
		`tracelens_syscall_duration_seconds_sum{op="read"} 2`,
		`tracelens_runq_delay_seconds_count{op="runq"} 0`,
		`tracelens_events_dropped_total 7`,
		`tracelens_stats_dropped_total 3`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing line %q", want)
		}
	}
	if strings.Contains(out, `tracelens_syscalls_total{op="runq"}`) {
		t.Error("runq should not appear as a syscall op")
	}
}
