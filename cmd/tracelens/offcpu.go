package main

import (
	"bufio"
	"cmp"
	"context"
	"debug/elf"
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
// usually say why.
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

// sleepFrames symbolizes a kernel stack and drops everything up to and including
// __schedule: the BPF program and the tracepoint plumbing are the tracer, and identical on
// every stack.
func sleepFrames(frames []uint64, syms []ksym) []string {
	names := make([]string, len(frames))
	start := 0
	for i, pc := range frames {
		names[i] = symbol(syms, pc)
		if names[i] == "__schedule" && start == 0 {
			start = i + 1
		}
	}
	return names[start:]
}

// userImage is the target's executable, for naming user stack frames.
type userImage struct {
	syms   []ksym // link-time addresses
	bias   uint64 // runtime address - link-time address
	lo, hi uint64 // link-time range the symbols cover
}

// loadBias finds where a position-independent executable was loaded, from the text of
// /proc/<pid>/maps: the start of its first mapping, less that mapping's file offset.
func loadBias(maps, exe string) (uint64, error) {
	for line := range strings.Lines(maps) {
		f := strings.Fields(line)
		if len(f) < 6 || f[5] != exe {
			continue
		}
		start, _, _ := strings.Cut(f[0], "-")
		addr, err := strconv.ParseUint(start, 16, 64)
		if err != nil {
			return 0, err
		}
		off, err := strconv.ParseUint(f[2], 16, 64)
		if err != nil {
			return 0, err
		}
		return addr - off, nil
	}
	return 0, fmt.Errorf("%s not mapped", exe)
}

// readUserImage loads the function symbols of a process's executable. A stripped binary
// isn't an error: its frames just print as addresses.
func readUserImage(pid int) (userImage, error) {
	var img userImage
	dir := fmt.Sprintf("/proc/%d", pid)
	exe, err := os.Readlink(dir + "/exe")
	if err != nil {
		return img, err
	}
	f, err := elf.Open(dir + "/exe")
	if err != nil {
		return img, err
	}
	defer f.Close()
	if f.Type == elf.ET_DYN {
		maps, err := os.ReadFile(dir + "/maps")
		if err != nil {
			return img, err
		}
		if img.bias, err = loadBias(string(maps), exe); err != nil {
			return img, err
		}
	}
	syms, err := f.Symbols()
	if err != nil {
		return img, nil // no symbol table
	}
	for _, s := range syms {
		if elf.ST_TYPE(s.Info) == elf.STT_FUNC && s.Value != 0 {
			img.syms = append(img.syms, ksym{s.Value, s.Name})
			img.hi = max(img.hi, s.Value+s.Size)
		}
	}
	slices.SortFunc(img.syms, func(a, b ksym) int { return cmp.Compare(a.addr, b.addr) })
	if len(img.syms) > 0 {
		img.lo = img.syms[0].addr
	}
	return img, nil
}

// frames names a user stack. Addresses outside the executable, such as shared libraries,
// stay as addresses.
func (img userImage) frames(pcs []uint64) []string {
	names := make([]string, len(pcs))
	for i, pc := range pcs {
		if a := pc - img.bias; a >= img.lo && a < img.hi {
			names[i] = symbol(img.syms, a)
		} else {
			names[i] = fmt.Sprintf("%#x", pc)
		}
	}
	return names
}

// matching keeps the stacks with a frame whose name contains sub. Go keeps idle threads in
// futex and ep_poll, and those stacks otherwise top the list on a healthy process.
func matching(stacks []probe.OffCPUStack, syms []ksym, img userImage, sub string) []probe.OffCPUStack {
	var out []probe.OffCPUStack
	has := func(name string) bool { return strings.Contains(name, sub) }
	for _, s := range stacks {
		if slices.ContainsFunc(sleepFrames(s.Kernel, syms), has) || slices.ContainsFunc(img.frames(s.User), has) {
			out = append(out, s)
		}
	}
	return out
}

func printFrames(w io.Writer, names []string) {
	for i, name := range names {
		if i == maxFrames {
			fmt.Fprintf(w, "  ... %d more\n", len(names)-i)
			break
		}
		fmt.Fprintf(w, "  %s\n", name)
	}
}

func printOffCPU(w io.Writer, stacks []probe.OffCPUStack, syms []ksym, img userImage, limit int, elapsed time.Duration, scope string) {
	slices.SortFunc(stacks, func(a, b probe.OffCPUStack) int { return cmp.Compare(b.Total, a.Total) })
	var total time.Duration
	for _, s := range stacks {
		total += s.Total
	}
	fmt.Fprintf(w, "%v asleep in %s in %v, %d distinct stacks\n", total.Round(time.Millisecond), scope, elapsed.Round(time.Millisecond), len(stacks))
	for i, s := range stacks {
		if i == limit {
			break
		}
		fmt.Fprintf(w, "\n%v total  %d sleeps  %d still asleep  max %s\n", s.Total.Round(time.Millisecond), s.Count, s.Asleep, fmtDur(s.Max))
		if len(s.Kernel) == 0 {
			fmt.Fprintln(w, "  [kernel stack not captured]")
		}
		printFrames(w, sleepFrames(s.Kernel, syms))
		if len(s.User) == 0 {
			fmt.Fprintln(w, "  [user stack not captured]")
			continue
		}
		fmt.Fprintln(w, "  --")
		printFrames(w, img.frames(s.User))
	}
}

func runOffCPU(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("offcpu", flag.ExitOnError)
	pid := fs.Int("pid", 0, "process to follow")
	dur := fs.Duration("duration", 10*time.Second, "how long to collect")
	limit := fs.Int("n", 5, "stacks to print")
	match := fs.String("match", "", "only stacks with a frame containing this")
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
	img, err := readUserImage(*pid)
	if err != nil { // kernel threads have no executable; kernel stacks are still useful
		fmt.Fprintln(os.Stderr, "tracelens: user frames stay as addresses:", err)
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
	scope := "all threads"
	if *match != "" {
		stacks, scope = matching(stacks, syms, img, *match), "matching stacks"
	}
	printOffCPU(os.Stdout, stacks, syms, img, *limit, time.Since(start), scope)
	return nil
}
