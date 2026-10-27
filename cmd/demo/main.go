//go:build linux

// Command demo is a stand-in for a service that commits each request to a write-ahead
// log: it appends a record and calls fdatasync, at a fixed request rate, and prints its
// own request latency every second. It exists to give TraceLens a slowdown to find.
package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"
)

func main() {
	path := flag.String("wal", "/var/tmp/tracelens-demo.wal", "log file to append to")
	rate := flag.Int("rate", 100, "requests per second")
	size := flag.Int("record", 4096, "bytes appended per request")
	flag.Parse()

	f, err := os.OpenFile(*path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	record := make([]byte, *size)
	fmt.Printf("demo: pid %d, %d req/s, %d-byte records, %s\n", os.Getpid(), *rate, *size, *path)

	tick := time.NewTicker(time.Second / time.Duration(*rate))
	var lat []time.Duration
	window := time.Now()
	for range tick.C {
		start := time.Now()
		if _, err := f.Write(record); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := syscall.Fdatasync(int(f.Fd())); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		lat = append(lat, time.Since(start))

		if time.Since(window) >= time.Second {
			slices.Sort(lat)
			fmt.Printf("%s  requests %d  p50 %v  p99 %v\n", time.Now().Format("15:04:05"), len(lat),
				lat[len(lat)/2].Round(time.Microsecond), lat[len(lat)*99/100].Round(time.Microsecond))
			lat, window = lat[:0], time.Now()
		}
	}
}
