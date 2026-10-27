package main

import (
	"testing"
	"time"

	"github.com/georgezliao/tracelens/internal/probe"
)

func TestSlowest(t *testing.T) {
	read := uint32(probe.OpRead)
	delta := map[probe.Key]probe.Stats{
		{Tgid: 1, Op: read}:                  {Count: 100, Hist: [40]uint64{10: 100}},        // ~2µs
		{Tgid: 2, Op: read}:                  {Count: 100, Hist: [40]uint64{10: 90, 23: 10}}, // p99 ~16ms
		{Tgid: 3, Op: read}:                  {Count: 5, Hist: [40]uint64{25: 5}},            // too few calls
		{Tgid: 4, Op: uint32(probe.OpWrite)}: {Count: 100, Hist: [40]uint64{25: 100}},        // wrong op
		{Tgid: 5, Op: read}:                  {Count: 100, Hist: [40]uint64{24: 100}},        // skipped
	}
	skip := func(pid uint32) bool { return pid == 5 }

	pid, p99, ok := slowest(delta, probe.OpRead, 10*time.Millisecond, skip)
	if !ok || pid != 2 || p99 != 1<<24 {
		t.Errorf("slowest = %d, %v, %v; want pid 2", pid, p99, ok)
	}
	if _, _, ok := slowest(delta, probe.OpRead, time.Second, skip); ok {
		t.Error("nothing should reach 1s")
	}
}
