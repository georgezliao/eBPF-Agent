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
	tgid   uint32
	perSec [probe.NumOps]float64
	p99    [probe.NumOps]time.Duration
	errSec float64
}

// syscalls is the row's syscall rate, used for sorting.
func (r procRow) syscalls() float64 {
	var t float64
	for op, v := range r.perSec {
		if probe.Op(op).IsSyscall() {
			t += v
		}
	}
	return t
}

// rates turns one sampling interval's activity into per-process rates.
func rates(delta map[probe.Key]probe.Stats, secs float64) []procRow {
	byPID := map[uint32]*procRow{}
	for k, d := range delta {
		r := byPID[k.Tgid]
		if r == nil {
			r = &procRow{tgid: k.Tgid}
			byPID[k.Tgid] = r
		}
		r.perSec[k.Op] = float64(d.Count) / secs
		if probe.Op(k.Op).IsSyscall() { // a failed read wait was already a failed read
			r.errSec += float64(d.Errors) / secs
		}
		r.p99[k.Op] = hist.Percentile(d.Hist[:], 0.99)
	}
	rows := make([]procRow, 0, len(byPID))
	for _, r := range byPID {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if a, b := rows[i].syscalls(), rows[j].syscalls(); a != b {
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
	fmt.Fprintln(tw, "PID\tCOMM\tREAD/s\tWRITE/s\tOPEN/s\tCONN/s\tFSYNC/s\tERR/s\tREAD p99\tWRITE p99\tFSYNC p99\tRWAIT p99\tRUNQ p99\t")
	for i, r := range rows {
		if i == limit {
			break
		}
		fmt.Fprintf(tw, "%d\t%s\t%.0f\t%.0f\t%.0f\t%.0f\t%.0f\t%.1f\t%s\t%s\t%s\t%s\t%s\t\n",
			r.tgid, comm(r.tgid), r.perSec[probe.OpRead], r.perSec[probe.OpWrite],
			r.perSec[probe.OpOpenat], r.perSec[probe.OpConnect], r.perSec[probe.OpFsync], r.errSec,
			fmtDur(r.p99[probe.OpRead]), fmtDur(r.p99[probe.OpWrite]), fmtDur(r.p99[probe.OpFsync]),
			fmtDur(r.p99[probe.OpReadWait]), fmtDur(r.p99[probe.OpRunq]))
	}
	tw.Flush()
}

// fmtDur prints "-" for no samples, otherwise a short rounded duration.
func fmtDur(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < time.Microsecond:
		return d.String()
	case d < time.Millisecond:
		return d.Round(time.Microsecond).String()
	default:
		return d.Round(100 * time.Microsecond).String()
	}
}

func runTop(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	interval := fs.Duration("interval", 2*time.Second, "sampling interval")
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
	tick := time.NewTicker(*interval)
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
			printTop(os.Stdout, rates(probe.Delta(prev, cur), now.Sub(last).Seconds()), *limit)
			if err := p.ForgetExited(cur); err != nil {
				return err
			}
			prev, last = cur, now
		}
	}
}
