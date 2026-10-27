package main

import (
	"testing"

	"github.com/georgezliao/tracelens/internal/probe"
)

func TestInterval(t *testing.T) {
	read := probe.Key{Tgid: 10, Op: uint32(probe.OpRead)}
	write := probe.Key{Tgid: 10, Op: uint32(probe.OpWrite)}
	other := probe.Key{Tgid: 20, Op: uint32(probe.OpRead)}

	prev := map[probe.Key]probe.Stats{
		read:  {Count: 100, Errors: 1},
		other: {Count: 50},
	}
	cur := map[probe.Key]probe.Stats{
		read:  {Count: 300, Errors: 5},
		write: {Count: 20}, // new since prev
		other: {Count: 50}, // idle: no row
	}

	rows := interval(prev, cur, 2)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.tgid != 10 || r.perSec[probe.OpRead] != 100 || r.perSec[probe.OpWrite] != 10 || r.errSec != 2 {
		t.Errorf("unexpected row %+v", r)
	}
}

func TestIntervalPIDReuse(t *testing.T) {
	k := probe.Key{Tgid: 7, Op: uint32(probe.OpRead)}
	prev := map[probe.Key]probe.Stats{k: {Count: 1000}}
	cur := map[probe.Key]probe.Stats{k: {Count: 40}}
	rows := interval(prev, cur, 1)
	if len(rows) != 1 || rows[0].perSec[probe.OpRead] != 40 {
		t.Fatalf("reused PID should restart from zero, got %+v", rows)
	}
}
