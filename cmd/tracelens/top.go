package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/georgezliao/tracelens/internal/hist"
	"github.com/georgezliao/tracelens/internal/probe"
)

// procRow is one process's activity over a sampling interval.
type procRow struct {
	tgid     uint32
	perSec   [probe.NumOps]float64
	errSec   float64
	readP99  time.Duration
	writeP99 time.Duration
}

func (r procRow) total() float64 {
	var t float64
	for _, v := range r.perSec {
		t += v
	}
	return t
}

// interval turns two cumulative snapshots into per-process rates. Keys missing from prev
// are new since the last sample and count in full.
func interval(prev, cur map[probe.Key]probe.Stats, secs float64) []procRow {
	byPID := map[uint32]*procRow{}
	for k, c := range cur {
		p := prev[k]
		if c.Count < p.Count || c.Errors < p.Errors { // process exited and its PID was reused
			p = probe.Stats{}
		}
		n := c.Count - p.Count
		if n == 0 {
			continue
		}
		r := byPID[k.Tgid]
		if r == nil {
			r = &procRow{tgid: k.Tgid}
			byPID[k.Tgid] = r
		}
		r.perSec[k.Op] = float64(n) / secs
		r.errSec += float64(c.Errors-p.Errors) / secs
		p99 := hist.Percentile(hist.Sub(c.Hist[:], p.Hist[:]), 0.99)
		switch probe.Op(k.Op) {
		case probe.OpRead:
			r.readP99 = p99
		case probe.OpWrite:
			r.writeP99 = p99
		}
	}
	rows := make([]procRow, 0, len(byPID))
	for _, r := range byPID {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if a, b := rows[i].total(), rows[j].total(); a != b {
			return a > b
		}
		return rows[i].tgid < rows[j].tgid // stable order between refreshes
	})
	return rows
}

func comm(pid uint32) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

func printTop(w io.Writer, rows []procRow, limit int) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "PID\tCOMM\tREAD/s\tWRITE/s\tOPEN/s\tCONNECT/s\tERR/s\tREAD p99\tWRITE p99\t")
	for i, r := range rows {
		if i == limit {
			break
		}
		fmt.Fprintf(tw, "%d\t%s\t%.0f\t%.0f\t%.0f\t%.0f\t%.1f\t%s\t%s\t\n",
			r.tgid, comm(r.tgid), r.perSec[probe.OpRead], r.perSec[probe.OpWrite],
			r.perSec[probe.OpOpenat], r.perSec[probe.OpConnect], r.errSec,
			fmtDur(r.readP99), fmtDur(r.writeP99))
	}
	tw.Flush()
}

// fmtDur prints "-" for no samples, otherwise a short rounded duration.
func fmtDur(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Millisecond:
		return d.Round(time.Microsecond).String()
	default:
		return d.Round(100 * time.Microsecond).String()
	}
}

func runTop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	every := fs.Duration("interval", 2*time.Second, "sampling interval")
	limit := fs.Int("n", 15, "processes to show")
	fs.Parse(args)

	p, err := probe.Open(0)
	if err != nil {
		return err
	}
	defer p.Close()

	fmt.Println("p99 columns are log2 bucket upper bounds (up to 2x the true value).")
	prev, err := p.Stats()
	if err != nil {
		return err
	}
	tick := time.NewTicker(*every)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-tick.C:
			cur, err := p.Stats()
			if err != nil {
				return err
			}
			fmt.Printf("\n%s\n", now.Format("15:04:05"))
			printTop(os.Stdout, interval(prev, cur, now.Sub(last).Seconds()), *limit)
			prev, last = cur, now
		}
	}
}
