//go:build linux

package integration

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf/ringbuf"
	"github.com/georgezliao/tracelens/internal/probe"
	"golang.org/x/sys/unix"
)

// drain returns the events that arrive within 300ms. It can't wait for the stream to go
// quiet: when the test traces itself, each time the reader wakes up to poll, that wakeup
// is itself a new run-queue event for the traced process.
func drain(t *testing.T, p *probe.Probe) []probe.Event {
	t.Helper()
	rd, err := p.Events()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	time.AfterFunc(300*time.Millisecond, func() { rd.Flush() })
	var out []probe.Event
	for {
		rec, err := rd.Read()
		if errors.Is(err, ringbuf.ErrFlushed) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		e, err := probe.Decode(rec.RawSample)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
}

func badReads(n int) {
	buf := make([]byte, 1)
	for range n {
		syscall.Read(-1, buf)
	}
}

func TestTargetSwitchesWithoutReattach(t *testing.T) {
	p := openProbe(t)
	me := uint32(os.Getpid())

	// Off: nothing from us.
	badReads(10)
	if ev := drain(t, p); len(ev) != 0 {
		t.Fatalf("got %d events with no target", len(ev))
	}

	if err := p.SetTarget(me, 0); err != nil {
		t.Fatal(err)
	}
	badReads(10)
	// Another process doing syscalls at the same time must not leak in.
	if err := exec.Command("sh", "-c", "head -c 4096 /dev/zero > /dev/null").Run(); err != nil {
		t.Fatal(err)
	}
	ev := drain(t, p)
	var reads int
	for _, e := range ev {
		if e.Tgid != me {
			t.Fatalf("event from pid %d (%s) leaked into trace of %d", e.Tgid, e.CommString(), me)
		}
		if probe.Op(e.Op) == probe.OpRead && e.Ret == -int64(syscall.EBADF) {
			reads++
		}
	}
	if reads != 10 {
		t.Errorf("got %d EBADF read events, want 10", reads)
	}

	if err := p.SetTarget(0, 0); err != nil {
		t.Fatal(err)
	}
	drain(t, p) // syscalls that finished between the last drain and SetTarget
	badReads(10)
	if ev := drain(t, p); len(ev) != 0 {
		t.Errorf("got %d events after clearing target", len(ev))
	}
}

func TestMinDurationFiltersInKernel(t *testing.T) {
	p := openProbe(t)
	if err := p.SetTarget(uint32(os.Getpid()), 2*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	badReads(100) // sub-microsecond
	slowReads(t, 5, 5*time.Millisecond)

	var slow int
	for _, e := range drain(t, p) {
		if e.DurationNs < uint64(2*time.Millisecond) {
			t.Errorf("%s event of %v passed a 2ms filter", probe.Op(e.Op), time.Duration(e.DurationNs))
		}
		if probe.Op(e.Op) == probe.OpRead && e.DurationNs >= uint64(4*time.Millisecond) {
			slow++
		}
	}
	if slow != 5 {
		t.Errorf("got %d slow read events, want 5", slow)
	}
}

func TestFullRingBufferCountsDrops(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	p, err := probe.Open(uint32(os.Getpagesize())) // room for ~50 events
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.SetTarget(uint32(os.Getpid()), 0); err != nil {
		t.Fatal(err)
	}

	const n = 1000
	badReads(n) // nobody is consuming
	drops, _, err := p.Lost()
	if err != nil {
		t.Fatal(err)
	}
	got := uint64(len(drain(t, p)))
	if drops == 0 {
		t.Fatal("no drops recorded with a full ring buffer")
	}
	// Other syscalls from this process (Go runtime) may add a few events on top of ours.
	if got+drops < n {
		t.Errorf("delivered %d + dropped %d < %d calls made", got, drops, n)
	}
	t.Logf("ring %d bytes: delivered %d, dropped %d", os.Getpagesize(), got, drops)
}

// Events arrive without a wakeup unless the ring fills up, so at a low rate they depend
// on the reader's polling. They must still arrive, and not much later than one poll.
func TestTrickleOfEventsArrivesByPolling(t *testing.T) {
	p := openProbe(t)
	rd, err := p.Events()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	if err := p.SetTarget(uint32(os.Getpid()), 0); err != nil {
		t.Fatal(err)
	}
	defer p.SetTarget(0, 0)

	const n = 50
	time.AfterFunc(5*time.Second, func() { rd.Close() })
	go func() {
		for range n {
			badReads(1)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	var delays []time.Duration
	for len(delays) < n {
		rec, err := rd.Read()
		if errors.Is(err, ringbuf.ErrClosed) {
			t.Fatalf("got %d of %d events in 5s", len(delays), n)
		}
		if err != nil {
			t.Fatal(err)
		}
		var now unix.Timespec
		unix.ClockGettime(unix.CLOCK_MONOTONIC, &now)
		e, err := probe.Decode(rec.RawSample)
		if err != nil {
			t.Fatal(err)
		}
		if probe.Op(e.Op) == probe.OpRead && e.Ret == -int64(syscall.EBADF) { // badReads, not the runtime
			delays = append(delays, time.Duration(now.Nano()-int64(e.TsNs)))
		}
	}
	slices.Sort(delays)
	t.Logf("delay from event to read: p50 %v, max %v", delays[n/2], delays[n-1])
	if delays[n-1] > 50*time.Millisecond {
		t.Errorf("an event took %v to arrive, want well under 50ms with a 10ms poll", delays[n-1])
	}
}
