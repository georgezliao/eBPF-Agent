// Package probe loads the TraceLens BPF programs, attaches them, and reads their maps.
package probe

//go:generate go tool bpf2go -target bpfel -type event -type stats_key -type op_stats -type trace_config tracelens ../../bpf/tracelens.bpf.c -- -I../../bpf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// Op is a traced syscall, matching enum op in tracelens.bpf.c.
type Op uint32

const (
	OpRead Op = iota
	OpWrite
	OpOpenat
	OpConnect
	NumOps
)

var opNames = []string{"read", "write", "openat", "connect"}

func (o Op) String() string {
	if int(o) < len(opNames) {
		return opNames[o]
	}
	return fmt.Sprintf("op%d", uint32(o))
}

// Ops lists every traced syscall in display order.
var Ops = []Op{OpRead, OpWrite, OpOpenat, OpConnect}

type (
	Key   = tracelensStatsKey
	Stats = tracelensOpStats
	Event = tracelensEvent
)

// Probe owns the loaded BPF objects and their attachments.
type Probe struct {
	coll  *ebpf.Collection
	links []link.Link
}

// Open loads the BPF objects and attaches every program to the tracepoint named by its
// ELF section. ringBytes sizes the event ring buffer (a power of two multiple of the page
// size); 0 keeps the default.
func Open(ringBytes uint32) (*Probe, error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock limit: %w", err)
	}
	spec, err := loadTracelens()
	if err != nil {
		return nil, err
	}
	if ringBytes > 0 {
		spec.Maps["events"].MaxEntries = ringBytes
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("load BPF objects: %w", err)
	}
	p := &Probe{coll: coll}

	for name, ps := range spec.Programs {
		group, event, ok := strings.Cut(strings.TrimPrefix(ps.SectionName, "tracepoint/"), "/")
		if !ok {
			p.Close()
			return nil, fmt.Errorf("program %s: unexpected section %q", name, ps.SectionName)
		}
		l, err := link.Tracepoint(group, event, coll.Programs[name], nil)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("attach %s: %w", ps.SectionName, err)
		}
		p.links = append(p.links, l)
	}
	return p, nil
}

// Close detaches all programs and frees the maps.
func (p *Probe) Close() error {
	var errs []error
	for _, l := range p.links {
		errs = append(errs, l.Close())
	}
	p.coll.Close()
	return errors.Join(errs...)
}

// Stats returns the current totals for every (tgid, op), summed across CPUs. Values are
// cumulative since the process was first seen.
func (p *Probe) Stats() (map[Key]Stats, error) {
	out := make(map[Key]Stats)
	var (
		key    Key
		perCPU []Stats
	)
	it := p.coll.Maps["stats"].Iterate()
	for it.Next(&key, &perCPU) {
		var sum Stats
		for _, s := range perCPU {
			sum.Count += s.Count
			sum.Errors += s.Errors
			sum.Bytes += s.Bytes
			sum.TotalNs += s.TotalNs
			for i := range sum.Hist {
				sum.Hist[i] += s.Hist[i]
			}
		}
		out[key] = sum
	}
	return out, it.Err()
}

// SetTarget selects the process whose syscalls are sent as individual events, without
// reattaching anything. Calls shorter than minDur are filtered out in the kernel.
// tgid 0 turns detailed events off.
func (p *Probe) SetTarget(tgid uint32, minDur time.Duration) error {
	cfg := tracelensTraceConfig{TargetTgid: tgid, MinDurationNs: uint64(minDur)}
	return p.coll.Maps["config"].Put(uint32(0), cfg)
}

// Events opens a reader on the event ring buffer. Close it to unblock a pending Read.
func (p *Probe) Events() (*ringbuf.Reader, error) {
	return ringbuf.NewReader(p.coll.Maps["events"])
}

// Drops returns how many events the kernel discarded because the ring buffer was full.
func (p *Probe) Drops() (uint64, error) {
	var perCPU []uint64
	if err := p.coll.Maps["drops"].Lookup(uint32(0), &perCPU); err != nil {
		return 0, err
	}
	var n uint64
	for _, v := range perCPU {
		n += v
	}
	return n, nil
}

// Decode parses one ring buffer record.
func Decode(raw []byte) (Event, error) {
	var e Event
	err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &e)
	return e, err
}

// CommString returns the event's task name as a string.
func (e *Event) CommString() string {
	b := make([]byte, 0, len(e.Comm))
	for _, c := range e.Comm {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// Failed reports whether a syscall return value is a real failure, using the same rule as
// the kernel-side error counter: EAGAIN and EINPROGRESS are normal for non-blocking I/O.
func Failed(ret int64) bool {
	return ret < 0 && ret != -int64(unix.EAGAIN) && ret != -int64(unix.EINPROGRESS)
}
