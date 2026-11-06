// Command client is the caller in the demo: it sends fixed-size commits to store over
// keep-alive connections at a fixed rate and prints its own request latency every second,
// so there's a number to compare TraceLens's output against. Latency runs from when a
// request was due, so a stall counts against every request it delays.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:9201/commit", "where to send commits")
	rate := flag.Int("rate", 200, "requests per second, across all workers")
	workers := flag.Int("workers", 8, "concurrent connections")
	size := flag.Int("size", 32*1024, "request body bytes")
	flag.Parse()

	var (
		mu       sync.Mutex
		lat      []time.Duration
		failures int
	)
	body := make([]byte, *size)
	// The default keeps two idle connections per host, so more workers would reconnect.
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: *workers}}
	interval := time.Second * time.Duration(*workers) / time.Duration(*rate)
	for range *workers {
		go func() {
			for due := time.Now(); ; due = due.Add(interval) {
				time.Sleep(time.Until(due))
				ok := commit(client, *url, body)
				mu.Lock()
				if ok {
					lat = append(lat, time.Since(due))
				} else {
					failures++
				}
				mu.Unlock()
			}
		}()
	}

	for range time.Tick(time.Second) {
		mu.Lock()
		slices.Sort(lat)
		if n := len(lat); n > 0 {
			fmt.Printf("%s  requests %d  p50 %v  p99 %v\n", time.Now().Format("15:04:05"), n,
				lat[n/2].Round(time.Microsecond), lat[n*99/100].Round(time.Microsecond))
		}
		if failures > 0 {
			fmt.Printf("%s  failed %d\n", time.Now().Format("15:04:05"), failures)
		}
		lat, failures = lat[:0], 0
		mu.Unlock()
	}
}

func commit(client *http.Client, url string, body []byte) bool {
	resp, err := client.Post(url, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
