package main

import (
	"net"
	"syscall"
)

// workloads builds the operation a worker repeats. Each runs on the worker's locked thread
// and is chosen to reach a different part of the BPF programs. One "op" in the results is
// one call of the returned function, which can be several syscalls.
var workloads = map[string]func() (op func(), err error){
	"read":      readZero,
	"write":     writeNull,
	"eagain":    eagain,
	"openclose": openClose,
	"pingpong":  pingPong,
	"tcp":       tcpEcho,
}

// readZero is one traced syscall that's cheap in itself, the worst case for probe overhead.
func readZero() (func(), error) {
	fd, err := syscall.Open("/dev/zero", syscall.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1)
	return func() { syscall.Read(fd, buf) }, nil
}

// writeNull adds the read_wait lookup that every successful write pays.
func writeNull() (func(), error) {
	fd, err := syscall.Open("/dev/null", syscall.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1)
	return func() { syscall.Write(fd, buf) }, nil
}

// eagain is a non-blocking read that finds nothing, a write, and a read that gets it: the
// insert, stamp and delete paths of read wait and response.
func eagain() (func(), error) {
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(p[0], true); err != nil {
		return nil, err
	}
	buf := make([]byte, 1)
	return func() {
		syscall.Read(p[0], buf)
		syscall.Write(p[1], buf)
		syscall.Read(p[0], buf)
	}, nil
}

// openClose reaches the close hook, which deletes from read_wait.
func openClose() (func(), error) {
	return func() {
		if fd, err := syscall.Open("/dev/null", syscall.O_RDONLY, 0); err == nil {
			syscall.Close(fd)
		}
	}, nil
}

// pingPong bounces a byte through two blocking pipes to a partner thread, so each op is two
// sleeps and two wakeups through the scheduler hooks.
func pingPong() (func(), error) {
	var to, from [2]int
	if err := syscall.Pipe(to[:]); err != nil {
		return nil, err
	}
	if err := syscall.Pipe(from[:]); err != nil {
		return nil, err
	}
	go echo(func(b []byte) { syscall.Read(to[0], b) }, func(b []byte) { syscall.Write(from[1], b) })
	buf := make([]byte, 1)
	return func() {
		syscall.Write(to[1], buf)
		syscall.Read(from[0], buf)
	}, nil
}

// tcpEcho sends a byte over loopback and waits for it to come back, through tcp_sendmsg and
// the Go netpoller.
func tcpEcho() (func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return nil, err
	}
	s, err := ln.Accept()
	if err != nil {
		return nil, err
	}
	go echo(func(b []byte) { s.Read(b) }, func(b []byte) { s.Write(b) })
	buf := make([]byte, 1)
	return func() {
		c.Write(buf)
		c.Read(buf)
	}, nil
}

func echo(read, write func([]byte)) {
	buf := make([]byte, 1)
	for {
		read(buf)
		write(buf)
	}
}
