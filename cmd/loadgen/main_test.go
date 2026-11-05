package main

import (
	"slices"
	"testing"
	"time"
)

// One 50ms stall at 10k calls/s delays about 500 calls. Service time should show it as a
// single outlier, and response time as a whole backlog.
func TestStallCountsAgainstEveryDelayedCall(t *testing.T) {
	const stall = 50 * time.Millisecond
	calls := 0
	read := func() {
		calls++
		if calls == 1000 {
			time.Sleep(stall)
		}
	}
	_, s := run(time.Now().Add(300*time.Millisecond), 10000, read)

	over := func(ds []time.Duration) (n int) {
		for _, d := range ds {
			if d > stall/2 {
				n++
			}
		}
		return n
	}
	// sampleEvery means at most one service sample holds the stall, and it may be missed.
	if n := over(s.service); n > 1 {
		t.Errorf("service samples over 25ms = %d, want at most 1", n)
	}
	// Calls delayed by more than 25ms: about 250 of them, sampled 1 in 16.
	if n := over(s.response); n < 8 {
		t.Errorf("response samples over 25ms = %d, want >= 8", n)
	}
	// The stall hits between samples, so the worst sample can be up to 16 calls late.
	if max := slices.Max(s.response); max < stall*9/10 {
		t.Errorf("worst response = %v, want about %v", max, stall)
	}
}
