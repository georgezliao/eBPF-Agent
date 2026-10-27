// Package probe loads the TraceLens BPF programs, attaches them, and reads their maps.
package probe

//go:generate go tool bpf2go -target bpfel -type event -type stats_key -type op_stats -type trace_config tracelens ../../bpf/tracelens.bpf.c -- -I../../bpf

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// Op is a traced syscall, matching enum op in tracelens.bpf.c.
type Op uint32

const (
	OpRead Op = iota
	OpWrite
	OpOpenat
	OpConnect
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
