// Integration tests load the real BPF programs, so they need Linux and root.
// Run with `make itest`.
package integration

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/georgezliao/tracelens/internal/probe"
)

func openProbe(t *testing.T) *probe.Probe {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	p, err := probe.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func readStats(t *testing.T, p *probe.Probe, op probe.Op) probe.Stats {
	t.Helper()
	all, err := p.Stats()
	if err != nil {
		t.Fatal(err)
	}
	return all[probe.Key{Tgid: uint32(os.Getpid()), Op: uint32(op)}]
}

// slowReads does n one-byte reads from a pipe whose writer sleeps delay before each byte,
// so every read blocks for roughly delay.
func slowReads(t *testing.T, n int, delay time.Duration) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() {
		defer w.Close()
		for range n {
			time.Sleep(delay)
			w.Write([]byte{1})
		}
	}()
	// Raw syscalls: os.File reads on a pipe go through the Go netpoller, which would
	// turn one blocking read into an EAGAIN read plus a later successful one.
	fd := int(r.Fd())
	syscall.SetNonblock(fd, false)
	buf := make([]byte, 1)
	for range n {
		if _, err := syscall.Read(fd, buf); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSlowReadsLandInRightBucket(t *testing.T) {
	p := openProbe(t)
	before := readStats(t, p, probe.OpRead)

	const n, delay = 20, 5 * time.Millisecond
	slowReads(t, n, delay)

	after := readStats(t, p, probe.OpRead)
	if got := after.Count - before.Count; got < n {
		t.Errorf("read count delta = %d, want >= %d", got, n)
	}
	if got := after.Bytes - before.Bytes; got < n {
		t.Errorf("read bytes delta = %d, want >= %d", got, n)
	}
	// 5 ms sits in slot 22, [4.19 ms, 8.39 ms). Allow slower reads on a loaded VM.
	var slow uint64
	for i := 22; i < len(after.Hist); i++ {
		slow += after.Hist[i] - before.Hist[i]
	}
	if slow < n {
		t.Errorf("reads >= 4ms = %d, want >= %d; hist delta %v", slow, n, after.Hist)
	}
}

func TestFailedReadsCountAsErrors(t *testing.T) {
	p := openProbe(t)
	before := readStats(t, p, probe.OpRead)
	buf := make([]byte, 1)
	for range 5 {
		syscall.Read(-1, buf) // EBADF
	}
	after := readStats(t, p, probe.OpRead)
	if got := after.Errors - before.Errors; got < 5 {
		t.Errorf("error delta = %d, want >= 5", got)
	}
}
