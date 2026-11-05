//go:build linux

package integration

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestOffCPUGroupsSleepsByStack(t *testing.T) {
	p := openProbe(t)
	if err := p.SetOffCPUTarget(uint32(os.Getpid())); err != nil {
		t.Fatal(err)
	}

	const n, nap = 20, 10 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		ts := syscall.NsecToTimespec(int64(nap))
		for range n {
			syscall.Nanosleep(&ts, nil)
		}
	}()
	<-done

	stacks, err := p.OffCPU()
	if err != nil {
		t.Fatal(err)
	}
	// Every nap has the same kernel stack, so one entry should hold them all.
	for _, s := range stacks {
		if s.Count >= n && s.Total >= n*nap && len(s.Frames) > 0 {
			if s.Max < nap {
				t.Errorf("max sleep = %v, want >= %v", s.Max, nap)
			}
			return
		}
	}
	t.Errorf("no stack with %d sleeps of %v: %+v", n, nap, stacks)
}
