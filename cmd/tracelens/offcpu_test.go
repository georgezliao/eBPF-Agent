package main

import (
	"strings"
	"testing"

	"github.com/georgezliao/tracelens/internal/probe"
)

func TestSymbols(t *testing.T) {
	syms, err := readKsyms(strings.NewReader(`ffff000000002000 T schedule
ffff000000001000 t __schedule
0000000000000000 t hidden
ffff000000003000 T ep_poll	[kernel]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := symbol(syms, 0xffff000000002010); got != "schedule" {
		t.Errorf("mid-function address resolved to %q", got)
	}
	if got := symbol(syms, 0xffff000000000fff); got != "0xffff000000000fff" {
		t.Errorf("address below every symbol resolved to %q", got)
	}
	got := sleepFrames([]uint64{0xffff000000000100, 0xffff000000001010, 0xffff000000002000, 0xffff000000003000}, syms)
	if strings.Join(got, " ") != "schedule ep_poll" {
		t.Errorf("sleepFrames = %v", got)
	}

	stacks := []probe.OffCPUStack{
		{Frames: []uint64{0xffff000000001010, 0xffff000000003000}},
		{Frames: []uint64{0xffff000000002000}},
		{},
	}
	if kept := matching(stacks, syms, "poll"); len(kept) != 1 || len(kept[0].Frames) != 2 {
		t.Errorf("matching kept %+v", kept)
	}
}
