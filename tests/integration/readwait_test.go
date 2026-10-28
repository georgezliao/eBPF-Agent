//go:build linux

package integration

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/georgezliao/tracelens/internal/probe"
)

// Through the netpoller, each slow read is a fast EAGAIN read plus a fast successful one,
// so the read op never sees the delay. The read-wait op should.
func TestNetpollerReadsShowUpAsReadWait(t *testing.T) {
	p := openProbe(t)
	r, w, err := os.Pipe() // non-blocking, registered with the netpoller
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	const n, delay = 20, 5 * time.Millisecond
	before := readStats(t, p, probe.OpReadWait)
	go func() {
		defer w.Close()
		for range n {
			time.Sleep(delay)
			w.Write([]byte{1})
		}
	}()
	buf := make([]byte, 1)
	for range n {
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	after := readStats(t, p, probe.OpReadWait)

	if got := after.Count - before.Count; got < n {
		t.Errorf("read waits = %d, want >= %d", got, n)
	}
	// Same bucket check as TestSlowReadsLandInRightBucket: slot 22 starts at 4.19 ms.
	var slow uint64
	for i := 22; i < len(after.Hist); i++ {
		slow += after.Hist[i] - before.Hist[i]
	}
	if slow < n {
		t.Errorf("read waits >= 4ms = %d, want >= %d", slow, n)
	}
}

// After an EAGAIN on fd N, closing N and reading from a new file that got the same number
// must not report a wait that started on the old file.
func TestCloseForgetsPendingReadWait(t *testing.T) {
	p := openProbe(t)
	for _, closeFirst := range []bool{false, true} {
		var fds [2]int
		if err := syscall.Pipe2(fds[:], syscall.O_NONBLOCK); err != nil {
			t.Fatal(err)
		}
		rfd, wfd := fds[0], fds[1]
		buf := make([]byte, 1)
		if _, err := syscall.Read(rfd, buf); err != syscall.EAGAIN {
			t.Fatalf("want EAGAIN, got %v", err)
		}

		if closeFirst {
			syscall.Close(rfd)
			syscall.Close(wfd)
			if err := syscall.Pipe2(fds[:], syscall.O_NONBLOCK); err != nil {
				t.Fatal(err)
			}
			// The kernel hands out the lowest free number, so the new read end usually
			// lands on the old one already. If not, move it there with dup3, which
			// doesn't go through close().
			if fds[0] != rfd {
				if err := syscall.Dup3(fds[0], rfd, 0); err != nil {
					t.Fatal(err)
				}
				syscall.Close(fds[0])
			}
			wfd = fds[1]
		}

		before := readStats(t, p, probe.OpReadWait)
		syscall.Write(wfd, []byte{1})
		if _, err := syscall.Read(rfd, buf); err != nil {
			t.Fatal(err)
		}
		got := readStats(t, p, probe.OpReadWait).Count - before.Count
		syscall.Close(rfd)
		syscall.Close(wfd)

		if closeFirst && got != 0 {
			t.Errorf("read on a reused fd reported %d waits from the closed file", got)
		}
		if !closeFirst && got != 1 {
			t.Errorf("EAGAIN then data on the same fd: %d waits, want 1", got)
		}
	}
}
