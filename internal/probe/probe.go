// Package probe loads the TraceLens BPF programs, attaches them, and reads their maps.
package probe

//go:generate go tool bpf2go -target bpfel -type event -type stats_key -type op_stats -type trace_config tracelens ../../bpf/tracelens.bpf.c -- -I../../bpf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// Op is a traced syscall, matching enum op in tracelens.bpf.c.
type Op uint32

const (
	OpRead Op = iota
	OpWrite
	OpOpenat
	OpConnect
	OpFsync // fsync and fdatasync
	OpRunq  // run-queue delay, not a syscall: wakeup or preemption until back on a CPU
	NumOps
)

var opNames = []string{"read", "write", "openat", "connect", "fsync", "runq"}

func (o Op) String() string {
	if int(o) < len(opNames) {
		return opNames[o]
	}
	return fmt.Sprintf("op%d", uint32(o))
}

// ParseOp returns the op with the given name, as printed by Op.String.
func ParseOp(name string) (Op, error) {
	for i, n := range opNames {
		if n == name {
			return Op(i), nil
		}
	}
	return 0, fmt.Errorf("unknown op %q (want one of %s)", name, strings.Join(opNames, ", "))
}

// Ops lists every op in display order, runq last.
var Ops = []Op{OpRead, OpWrite, OpOpenat, OpConnect, OpFsync, OpRunq}

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
// ELF section ("tracepoint/<group>/<event>" or "tp_btf/<event>"). ringBytes sizes the
// event ring buffer (a power-of-two multiple of the page size); 0 keeps the default.
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
		l, err := attach(ps.SectionName, coll.Programs[name])
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("attach %s: %w", ps.SectionName, err)
		}
		p.links = append(p.links, l)
	}
	return p, nil
}

func attach(section string, prog *ebpf.Program) (link.Link, error) {
	if strings.HasPrefix(section, "tp_btf/") {
		return link.AttachTracing(link.TracingOptions{Program: prog})
	}
	group, event, ok := strings.Cut(strings.TrimPrefix(section, "tracepoint/"), "/")
	if !ok {
		return nil, fmt.Errorf("unexpected section %q", section)
	}
	return link.Tracepoint(group, event, prog, nil)
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

// ExportAll switches to the benchmark's raw mode: every call of every process goes to the
// ring buffer and nothing is aggregated in the kernel.
func (p *Probe) ExportAll() error {
	return p.coll.Maps["config"].Put(uint32(0), tracelensTraceConfig{ExportAll: 1})
}

// RunStats returns the combined run count and run time of all BPF programs. The kernel
// only records them while the sysctl kernel.bpf_stats_enabled is 1; otherwise both are 0.
func (p *Probe) RunStats() (runs uint64, runtime time.Duration, err error) {
	for _, prog := range p.coll.Programs {
		st, err := prog.Stats()
		if err != nil {
			return 0, 0, err
		}
		runs += st.RunCount
		runtime += st.Runtime
	}
	return runs, runtime, nil
}

// Events opens a reader on the event ring buffer. Close it to unblock a pending Read.
func (p *Probe) Events() (*ringbuf.Reader, error) {
	return ringbuf.NewReader(p.coll.Maps["events"])
}

// Indexes into the lost map, matching LOST_* in tracelens.bpf.c.
const (
	lostEvents uint32 = iota
	lostStats
)

// Lost reports what the kernel couldn't record: trace events dropped because the ring
// buffer was full, and stats updates dropped because the stats map was full.
func (p *Probe) Lost() (events, stats uint64, err error) {
	sum := func(kind uint32) (uint64, error) {
		var perCPU []uint64
		if err := p.coll.Maps["lost"].Lookup(kind, &perCPU); err != nil {
			return 0, err
		}
		var n uint64
		for _, v := range perCPU {
			n += v
		}
		return n, nil
	}
	if events, err = sum(lostEvents); err != nil {
		return 0, 0, err
	}
	stats, err = sum(lostStats)
	return events, stats, err
}

// ForgetExited deletes the stats of processes that no longer exist, from the kernel map
// and from cur. Call it after the snapshot's values have been used: the kernel keeps an
// exited process's counters until then, so its last interval isn't lost.
func (p *Probe) ForgetExited(cur map[Key]Stats) error {
	for k := range cur {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", k.Tgid)); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := p.coll.Maps["stats"].Delete(k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return err
		}
		delete(cur, k)
	}
	return nil
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

// Linux errno values. Not unix.EAGAIN etc.: those follow the build host, and on macOS
// EAGAIN is 35.
const (
	eagain      = 11
	einprogress = 115
)

// Failed reports whether a syscall return value is a real failure, using the same rule as
// the kernel-side error counter: EAGAIN and EINPROGRESS are normal for non-blocking I/O.
func Failed(ret int64) bool {
	return ret < 0 && ret != -eagain && ret != -einprogress
}

// Delta returns the activity between two Stats snapshots, per key, skipping idle keys. A
// key whose counters went backwards was deleted and recreated in between (its process
// exited and the PID was reused), so its current values count in full.
func Delta(prev, cur map[Key]Stats) map[Key]Stats {
	out := make(map[Key]Stats, len(cur))
	for k, c := range cur {
		p, ok := prev[k]
		if ok && !covers(c, p) {
			p = Stats{}
		}
		if c.Count == p.Count {
			continue
		}
		d := Stats{
			Count:   c.Count - p.Count,
			Errors:  c.Errors - p.Errors,
			Bytes:   c.Bytes - p.Bytes,
			TotalNs: c.TotalNs - p.TotalNs,
		}
		for i := range d.Hist {
			d.Hist[i] = c.Hist[i] - p.Hist[i]
		}
		out[k] = d
	}
	return out
}

// covers reports whether every counter in c is at least the one in p.
func covers(c, p Stats) bool {
	if c.Count < p.Count || c.Errors < p.Errors || c.Bytes < p.Bytes || c.TotalNs < p.TotalNs {
		return false
	}
	for i := range c.Hist {
		if c.Hist[i] < p.Hist[i] {
			return false
		}
	}
	return true
}
