//go:build linux

package integration

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// A thread still asleep when the window ends has no finished sleep to report, and it may
// be the most interesting one.
func TestOffCPUCountsSleepsStillGoing(t *testing.T) {
	p := openProbe(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := syscall.SetNonblock(int(r.Fd()), false); err != nil {
		t.Fatal(err)
	}
	if err := p.SetOffCPUTarget(uint32(os.Getpid())); err != nil {
		t.Fatal(err)
	}
	go func() {
		syscall.Read(int(r.Fd()), make([]byte, 1)) // blocks until the deferred close
	}()
	const blocked = 200 * time.Millisecond
	time.Sleep(blocked)

	stacks, err := p.OffCPU()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stacks {
		if s.Asleep > 0 && s.Max >= blocked*3/4 {
			return
		}
	}
	t.Errorf("no sleep still going for about %v: %+v", blocked, stacks)
}

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
		if s.Count >= n && s.Total >= n*nap && len(s.Kernel) > 0 && len(s.User) > 0 {
			if s.Max < nap {
				t.Errorf("max sleep = %v, want >= %v", s.Max, nap)
			}
			return
		}
	}
	t.Errorf("no stack with %d sleeps of %v: %+v", n, nap, stacks)
}
