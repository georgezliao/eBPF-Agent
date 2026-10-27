package main

import (
	"testing"

	"github.com/georgezliao/tracelens/internal/probe"
)

func BenchmarkAddEvent(b *testing.B) {
	m := &metrics{}
	e := probe.Event{Op: uint32(probe.OpRead), DurationNs: 250, Ret: 1}
	for b.Loop() {
		m.addEvent(&e)
	}
}
