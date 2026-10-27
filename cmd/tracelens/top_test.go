package main

import (
	"testing"

	"github.com/georgezliao/tracelens/internal/probe"
)

func TestInterval(t *testing.T) {
	delta := map[probe.Key]probe.Stats{
		{Tgid: 10, Op: uint32(probe.OpRead)}:  {Count: 200, Errors: 4},
		{Tgid: 10, Op: uint32(probe.OpWrite)}: {Count: 20},
		{Tgid: 10, Op: uint32(probe.OpRunq)}:  {Count: 500},
		{Tgid: 20, Op: uint32(probe.OpRead)}:  {Count: 300},
	}
	rows := interval(delta, 2)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	// Sorted by syscall rate; runq doesn't count.
	if rows[0].tgid != 20 {
		t.Errorf("first row is pid %d, want 20", rows[0].tgid)
	}
	r := rows[1]
	if r.perSec[probe.OpRead] != 100 || r.perSec[probe.OpWrite] != 10 || r.errSec != 2 {
		t.Errorf("unexpected row %+v", r)
	}
}
