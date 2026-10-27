package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/cilium/ebpf/ringbuf"
	"github.com/georgezliao/tracelens/internal/probe"
)

const keepLongest = 10

// summary accumulates exact per-op durations from trace events.
type summary struct {
	durs    map[probe.Op][]time.Duration
	errs    map[probe.Op]int
	longest []probe.Event // sorted, longest first
}

func newSummary() *summary {
	return &summary{durs: map[probe.Op][]time.Duration{}, errs: map[probe.Op]int{}}
}

func (s *summary) add(e probe.Event) {
	op := probe.Op(e.Op)
	s.durs[op] = append(s.durs[op], time.Duration(e.DurationNs))
	if probe.Failed(e.Ret) {
		s.errs[op]++
	}
	i := sort.Search(len(s.longest), func(i int) bool { return s.longest[i].DurationNs < e.DurationNs })
	if i < keepLongest {
		s.longest = slices.Insert(s.longest, i, e)
		if len(s.longest) > keepLongest {
			s.longest = s.longest[:keepLongest]
		}
	}
}

// quantile returns the q-th value of sorted durations by nearest rank.
func quantile(sorted []time.Duration, q float64) time.Duration {
	i := int(q*float64(len(sorted))+0.5) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

func (s *summary) print(w io.Writer, drops uint64) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "OP\tCALLS\tERRORS\tp50\tp99\tMAX\t")
	for _, op := range probe.Ops {
		d := s.durs[op]
		if len(d) == 0 {
			continue
		}
		slices.Sort(d)
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t\n", op, len(d), s.errs[op],
			fmtDur(quantile(d, 0.5)), fmtDur(quantile(d, 0.99)), fmtDur(d[len(d)-1]))
	}
	tw.Flush()
	fmt.Fprintf(w, "events lost: %d\n", drops)

	if len(s.longest) == 0 {
		return
	}
	fmt.Fprintln(w, "\nlongest calls:")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range s.longest {
		fmt.Fprintf(tw, "  %s\ttid %d\t%s\tret %d\n", fmtDur(time.Duration(e.DurationNs)),
			e.Tid, probe.Op(e.Op), e.Ret)
	}
	tw.Flush()
}

// jsonEvent is the --out line format.
type jsonEvent struct {
	TimeNs     uint64 `json:"ts_ns"` // CLOCK_MONOTONIC at syscall exit
	DurationNs uint64 `json:"duration_ns"`
	Op         string `json:"op"`
	Ret        int64  `json:"ret"`
	PID        uint32 `json:"pid"`
	TID        uint32 `json:"tid"`
	Comm       string `json:"comm"`
}

func runTrace(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	pid := fs.Uint("pid", 0, "process to trace (required)")
	dur := fs.Duration("duration", 10*time.Second, "how long to trace")
	minDur := fs.Duration("min", 0, "only record calls at least this long")
	out := fs.String("out", "", "write every event as JSON lines to this file")
	fs.Parse(args)
	if *pid == 0 {
		return errors.New("trace: -pid is required")
	}

	p, err := probe.Open(0)
	if err != nil {
		return err
	}
	defer p.Close()
	rd, err := p.Events()
	if err != nil {
		return err
	}
	defer rd.Close()

	var enc *json.Encoder
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		enc = json.NewEncoder(f)
	}

	if err := p.SetTarget(uint32(*pid), *minDur); err != nil {
		return err
	}
	fmt.Printf("Tracing PID %d (%s) for %s...\n", *pid, comm(uint32(*pid)), *dur)

	ctx, cancel := context.WithTimeout(ctx, *dur)
	defer cancel()
	go func() { <-ctx.Done(); rd.Close() }()

	s := newSummary()
	for {
		rec, err := rd.Read()
		if errors.Is(err, ringbuf.ErrClosed) {
			break
		}
		if err != nil {
			return err
		}
		e, err := probe.Decode(rec.RawSample)
		if err != nil {
			return err
		}
		s.add(e)
		if enc != nil {
			enc.Encode(jsonEvent{e.TsNs, e.DurationNs, probe.Op(e.Op).String(), e.Ret, e.Tgid, e.Tid, e.CommString()})
		}
	}

	drops, err := p.Drops()
	if err != nil {
		return err
	}
	fmt.Println()
	s.print(os.Stdout, drops)
	return nil
}
