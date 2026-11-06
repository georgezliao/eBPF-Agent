//go:build linux

// Command store is the backend of the demo: an HTTP service that commits every request to
// a write-ahead log with write + fdatasync. POST /delay?d=40ms makes each request sleep
// that much longer, standing in for a slow dependency. It exists to give TraceLens
// something to find.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:9201", "address to serve on")
	path := flag.String("wal", "/var/tmp/tracelens-demo.wal", "log file to append to")
	flag.Parse()

	f, err := os.OpenFile(*path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	var (
		mu    sync.Mutex // one commit at a time, like a single log writer
		delay atomic.Int64
	)
	http.HandleFunc("POST /commit", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		_, err = f.Write(body)
		if err == nil {
			err = syscall.Fdatasync(int(f.Fd()))
		}
		mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		time.Sleep(time.Duration(delay.Load()))
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("POST /delay", func(w http.ResponseWriter, r *http.Request) {
		d, err := time.ParseDuration(r.URL.Query().Get("d"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		delay.Store(int64(d))
	})
	fmt.Println(http.ListenAndServe(*addr, nil))
}
