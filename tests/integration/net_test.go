//go:build linux

package integration

import (
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/georgezliao/tracelens/internal/probe"
)

// A client whose reader is parked on a keep-alive connection, idle for 30ms before each
// request to a server that takes 5ms. Read wait covers the idle time; response doesn't.
func TestResponseExcludesIdleTimeBeforeTheWrite(t *testing.T) {
	p := openProbe(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	const n, idle, work = 20, 30 * time.Millisecond, 5 * time.Millisecond
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
			time.Sleep(work)
			c.Write(buf)
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	replies := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
			replies <- struct{}{}
		}
	}()
	// Let the reader park and the first read hit EAGAIN.
	time.Sleep(idle)

	resp0, wait0 := readStats(t, p, probe.OpResponse), readStats(t, p, probe.OpReadWait)
	for range n {
		c.Write([]byte{1})
		<-replies
		time.Sleep(idle)
	}
	resp1, wait1 := readStats(t, p, probe.OpResponse), readStats(t, p, probe.OpReadWait)

	count := resp1.Count - resp0.Count
	if count < n-1 {
		t.Fatalf("responses = %d, want >= %d", count, n-1)
	}
	avg := time.Duration((resp1.TotalNs - resp0.TotalNs) / count)
	if avg < work || avg >= idle {
		t.Errorf("average response = %v, want between %v and %v", avg, work, idle)
	}
	// The client's waits include the idle time, and the server's are mostly idle too.
	if got := wait1.TotalNs - wait0.TotalNs; got < uint64(n-1)*uint64(idle) {
		t.Errorf("total read wait = %v, want it to include the idle time", time.Duration(got))
	}
}

// Packets dropped on loopback force retransmits. They run from timers and softirq, so the
// count should still land on the sending process, which is a child here so that "current"
// can't be right by accident.
func TestRetransmitsCountAgainstTheSender(t *testing.T) {
	if addr := os.Getenv("TRACELENS_SEND_TO"); addr != "" {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(1)
		}
		chunk := make([]byte, 64*1024)
		for range 64 {
			c.Write(chunk)
		}
		c.Close()
		os.Exit(0)
	}

	p := openProbe(t)
	if out, err := exec.Command("tc", "qdisc", "add", "dev", "lo", "root", "netem", "loss", "20%").CombinedOutput(); err != nil {
		t.Skipf("can't add netem to lo: %v: %s", err, out)
	}
	defer exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c)
	}()

	child := exec.Command(os.Args[0], "-test.run=^TestRetransmitsCountAgainstTheSender$")
	child.Env = append(os.Environ(), "TRACELENS_SEND_TO="+ln.Addr().String())
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	<-done

	all, err := p.Stats()
	if err != nil {
		t.Fatal(err)
	}
	pid := uint32(child.Process.Pid)
	got := all[probe.Key{Tgid: pid, Op: uint32(probe.OpRetrans)}].Count
	if got == 0 {
		t.Errorf("no retransmits for sender %s", strconv.Itoa(int(pid)))
	}
	var others uint64
	for k, s := range all {
		if probe.Op(k.Op) == probe.OpRetrans && k.Tgid != pid {
			others += s.Count
		}
	}
	t.Logf("%d retransmits on the sender, %d elsewhere", got, others)
	if others > got/4 {
		t.Errorf("%d retransmits landed on other processes, %d on the sender", others, got)
	}
}
