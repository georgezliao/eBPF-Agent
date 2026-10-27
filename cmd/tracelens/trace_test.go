package main

import (
	"testing"
	"time"

	"github.com/georgezliao/tracelens/internal/probe"
)

func TestQuantile(t *testing.T) {
	d := make([]time.Duration, 100)
	for i := range d {
		d[i] = time.Duration(i + 1)
	}
	if got := quantile(d, 0.5); got != 50 {
		t.Errorf("p50 = %v, want 50", got)
	}
	if got := quantile(d, 0.99); got != 99 {
		t.Errorf("p99 = %v, want 99", got)
	}
	if got := quantile(d[:1], 0.99); got != 1 {
		t.Errorf("single sample p99 = %v, want 1", got)
	}
}

func TestSummaryKeepsLongest(t *testing.T) {
	s := newSummary()
	for i := range 50 {
		s.add(probe.Event{DurationNs: uint64(i), Op: uint32(probe.OpRead), Ret: -1})
	}
	if len(s.longest) != keepLongest || s.longest[0].DurationNs != 49 || s.longest[keepLongest-1].DurationNs != 40 {
		t.Errorf("longest = %v", s.longest)
	}
	if s.errs[probe.OpRead] != 50 || len(s.durs[probe.OpRead]) != 50 {
		t.Errorf("errs=%d durs=%d", s.errs[probe.OpRead], len(s.durs[probe.OpRead]))
	}
}

func TestSummaryLongestSkipsRunq(t *testing.T) {
	s := newSummary()
	s.add(probe.Event{DurationNs: 100, Op: uint32(probe.OpRunq)})
	s.add(probe.Event{DurationNs: 5, Op: uint32(probe.OpRead)})
	if len(s.longest) != 1 || probe.Op(s.longest[0].Op) != probe.OpRead {
		t.Errorf("longest = %+v, want only the read", s.longest)
	}
	if len(s.durs[probe.OpRunq]) != 1 {
		t.Error("runq should still count in the per-op table")
	}
}
