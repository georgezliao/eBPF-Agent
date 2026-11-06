package main

import (
	"slices"
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
		{Kernel: []uint64{0xffff000000001010, 0xffff000000003000}},
		{Kernel: []uint64{0xffff000000002000}, User: []uint64{0x1010}},
		{},
	}
	img := userImage{syms: []ksym{{0x1000, "main.commit"}}, lo: 0x1000, hi: 0x1100}
	if kept := matching(stacks, syms, img, "poll"); len(kept) != 1 || len(kept[0].Kernel) != 2 {
		t.Errorf("matching on a kernel frame kept %+v", kept)
	}
	if kept := matching(stacks, syms, img, "commit"); len(kept) != 1 || len(kept[0].User) != 1 {
		t.Errorf("matching on a user frame kept %+v", kept)
	}
}

func TestUserImage(t *testing.T) {
	img := userImage{syms: []ksym{{0x1000, "main.a"}, {0x2000, "main.b"}}, bias: 0x500000, lo: 0x1000, hi: 0x2100}
	got := img.frames([]uint64{0x501010, 0x502050, 0x7f0000001000})
	want := []string{"main.a", "main.b", "0x7f0000001000"}
	if !slices.Equal(got, want) {
		t.Errorf("frames = %v, want %v", got, want)
	}

	maps := `aaaad0000000-aaaad0001000 r-xp 00001000 fe:01 42 /usr/bin/app
aaaad0010000-aaaad0011000 rw-p 00010000 fe:01 42 /usr/bin/app
ffff80000000-ffff80001000 r-xp 00000000 fe:01 43 /lib/libc.so.6
`
	bias, err := loadBias(maps, "/usr/bin/app")
	if err != nil || bias != 0xaaaacffff000 {
		t.Errorf("loadBias = %#x, %v", bias, err)
	}
	if _, err := loadBias(maps, "/usr/bin/other"); err == nil {
		t.Error("loadBias found an unmapped executable")
	}
}
