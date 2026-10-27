// Command tracelens shows per-process syscall activity and latency using eBPF.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage: tracelens <command> [flags]

commands:
  top     per-process syscall rates and p99 latency, refreshed every interval
  trace   record every syscall of one process for a while and summarize it
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "top":
		err = runTop(ctx, os.Args[2:])
	case "trace":
		err = runTrace(ctx, os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tracelens:", err)
		os.Exit(1)
	}
}
