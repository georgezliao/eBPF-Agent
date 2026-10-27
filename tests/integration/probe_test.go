//go:build linux

// Integration tests load the real BPF programs, so they need Linux and root.
// Run with `make itest`.
package integration

import (
	"os"
	"os/exec"
	"runtime"
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
	if err := syscall.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
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
		t.Errorf("reads >= 4ms = %d, want >= %d", slow, n)
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

func TestWouldBlockIsNotAnError(t *testing.T) {
	p := openProbe(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fd := int(r.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}

	before := readStats(t, p, probe.OpRead)
	buf := make([]byte, 1)
	for range 5 {
		if _, err := syscall.Read(fd, buf); err != syscall.EAGAIN {
			t.Fatalf("want EAGAIN, got %v", err)
		}
	}
	after := readStats(t, p, probe.OpRead)
	if after.Count-before.Count < 5 {
		t.Errorf("count delta = %d, want >= 5", after.Count-before.Count)
	}
	if after.Errors != before.Errors {
		t.Errorf("EAGAIN counted as error: delta %d", after.Errors-before.Errors)
	}
	if after.Bytes != before.Bytes {
		t.Errorf("EAGAIN changed byte count: %d -> %d", before.Bytes, after.Bytes)
	}
}

func TestFsyncAndFdatasyncShareAnOp(t *testing.T) {
	p := openProbe(t)
	f, err := os.CreateTemp("", "tracelens-fsync")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	before := readStats(t, p, probe.OpFsync)
	for range 3 {
		f.Write([]byte("x"))
		if err := f.Sync(); err != nil { // fsync
			t.Fatal(err)
		}
		if err := syscall.Fdatasync(int(f.Fd())); err != nil {
			t.Fatal(err)
		}
	}
	after := readStats(t, p, probe.OpFsync)
	if got := after.Count - before.Count; got != 6 {
		t.Errorf("fsync count delta = %d, want 6", got)
	}
}

func TestRunqDelayUnderCPUContention(t *testing.T) {
	p := openProbe(t)

	// Twice as many busy processes as CPUs: each should spend about half its time
	// runnable but waiting for a CPU.
	n := 2 * runtime.NumCPU()
	const spin = time.Second
	var spinners []*exec.Cmd
	for range n {
		c := exec.Command("sh", "-c", "while :; do :; done")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		spinners = append(spinners, c)
	}
	time.Sleep(spin)

	all, err := p.Stats()
	if err != nil {
		t.Fatal(err)
	}
	var waited time.Duration
	for _, c := range spinners {
		waited += time.Duration(all[probe.Key{Tgid: uint32(c.Process.Pid), Op: uint32(probe.OpRunq)}].TotalNs)
		c.Process.Kill()
		c.Wait()
	}
	// Expect ~n*spin/2 in total; a quarter of that leaves room for a noisy VM.
	if want := time.Duration(n) * spin / 4; waited < want {
		t.Errorf("spinners waited %v in the run queue, want >= %v", waited, want)
	}
	t.Logf("%d spinners on %d CPUs for %v: %v total run-queue wait", n, runtime.NumCPU(), spin, waited)
}
