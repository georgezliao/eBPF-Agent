package main

import (
	"testing"

	"github.com/georgezliao/tracelens/internal/probe"
)

func TestRates(t *testing.T) {
	delta := map[probe.Key]probe.Stats{
		{Tgid: 10, Op: uint32(probe.OpRead)}:     {Count: 200, Errors: 4},
		{Tgid: 10, Op: uint32(probe.OpWrite)}:    {Count: 20},
		{Tgid: 10, Op: uint32(probe.OpRunq)}:     {Count: 500},
		{Tgid: 10, Op: uint32(probe.OpReadWait)}: {Count: 100, Errors: 2},
		{Tgid: 20, Op: uint32(probe.OpRead)}:     {Count: 300},
	}
	rows := rates(delta, 2)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	// Sorted by syscall rate; runq and read waits don't count.
	if rows[0].tgid != 20 {
		t.Errorf("first row is pid %d, want 20", rows[0].tgid)
	}
	r := rows[1]
	// The two failed read waits are among the four failed reads, so errSec stays 2.
	if r.perSec[probe.OpRead] != 100 || r.perSec[probe.OpWrite] != 10 || r.errSec != 2 {
		t.Errorf("unexpected row %+v", r)
	}
}
