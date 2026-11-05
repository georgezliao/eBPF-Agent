package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/georgezliao/tracelens/internal/probe"
)

// maxFrames limits how much of each stack is printed. The frames nearest the sleep
// usually say why; the rest is the syscall entry path.
const maxFrames = 12

type ksym struct {
	addr uint64
	name string
}

// readKsyms parses /proc/kallsyms lines ("addr type name [module]"), sorted by address.
func readKsyms(r io.Reader) ([]ksym, error) {
	var syms []ksym
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		addr, err := strconv.ParseUint(f[0], 16, 64)
		if err != nil {
			return nil, err
		}
		if addr != 0 { // kptr_restrict hides addresses from unprivileged readers
			syms = append(syms, ksym{addr, f[2]})
		}
	}
	slices.SortFunc(syms, func(a, b ksym) int { return cmp.Compare(a.addr, b.addr) })
	return syms, sc.Err()
}

// symbol names the function containing addr: the last symbol at or below it.
func symbol(syms []ksym, addr uint64) string {
	i, found := slices.BinarySearchFunc(syms, addr, func(s ksym, a uint64) int { return cmp.Compare(s.addr, a) })
	if !found {
		i--
	}
	if i < 0 {
		return fmt.Sprintf("%#x", addr)
	}
	return syms[i].name
}

func printOffCPU(w io.Writer, stacks []probe.OffCPUStack, syms []ksym, limit int, elapsed time.Duration) {
	slices.SortFunc(stacks, func(a, b probe.OffCPUStack) int { return cmp.Compare(b.Total, a.Total) })
	var total time.Duration
	for _, s := range stacks {
		total += s.Total
	}
	fmt.Fprintf(w, "%v asleep across all threads in %v, %d distinct stacks\n", total.Round(time.Millisecond), elapsed.Round(time.Millisecond), len(stacks))
	for i, s := range stacks {
		if i == limit {
			break
		}
		fmt.Fprintf(w, "\n%v total  %d sleeps  avg %s  max %s\n", s.Total.Round(time.Millisecond), s.Count,
			fmtDur(s.Total/time.Duration(s.Count)), fmtDur(s.Max))
		if len(s.Frames) == 0 {
			fmt.Fprintln(w, "  [stack not captured]")
		}
		for j, pc := range s.Frames {
			if j == maxFrames {
				fmt.Fprintf(w, "  ... %d more\n", len(s.Frames)-j)
				break
			}
			fmt.Fprintf(w, "  %s\n", symbol(syms, pc))
		}
	}
}

func runOffCPU(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("offcpu", flag.ExitOnError)
	pid := fs.Int("pid", 0, "process to follow")
	dur := fs.Duration("duration", 10*time.Second, "how long to collect")
	limit := fs.Int("n", 5, "stacks to print")
	fs.Parse(args)
	if *pid == 0 {
		return errors.New("-pid is required")
	}
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		return err
	}
	defer f.Close()
	syms, err := readKsyms(f)
	if err != nil {
		return err
	}
	p, err := probe.Open(0)
	if err != nil {
		return err
	}
	defer p.Close()
	if err := p.SetOffCPUTarget(uint32(*pid)); err != nil {
		return err
	}

	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(*dur):
	}
	stacks, err := p.OffCPU()
	if err != nil {
		return err
	}
	printOffCPU(os.Stdout, stacks, syms, *limit, time.Since(start))
	return nil
}
