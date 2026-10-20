// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"

	"github.com/z5labs/humus"
)

// Host is a humus.Host for tests: a network of in-memory connections, and a
// filesystem in a directory from t.TempDir(). The network starts no
// goroutines, so the Host works inside a testing/synctest bubble; build it
// inside the bubble that uses it.
//
// A Host is safe for concurrent use.
type Host struct {
	tb   testing.TB
	dir  string
	root *os.Root

	mu        sync.Mutex
	listeners map[key]*listener
	held      map[int]int // the number of listeners on each port
	listening map[key]chan struct{}
	nextPort  int
}

// HostOption configures a [Host] built by [NewHost].
type HostOption func(*Host)

// NewHost returns a Host whose directory comes from t.TempDir(), so it is
// removed when the test ends. It is configured by opts, and reports misuse
// through t.
//
// NewHost starts no goroutines and makes no channels: Listen and Listening
// make theirs when they are called. To use a Host with testing/synctest,
// call NewHost inside synctest.Test with the bubble's t, so that every
// channel the Host makes belongs to the bubble that uses it.
func NewHost(t testing.TB, opts ...HostOption) *Host {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("humustest: NewHost: %v", err)
	}
	// Cleanups run last in, first out, so the root closes before
	// t.TempDir's cleanup removes the directory.
	t.Cleanup(func() { _ = root.Close() })

	h := &Host{
		tb:        t,
		dir:       dir,
		root:      root,
		listeners: make(map[key]*listener),
		held:      make(map[int]int),
		listening: make(map[key]chan struct{}),
		nextPort:  firstEphemeralPort,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Host returns the humus.Host to run an App on, like bedrocktest's
// Process.Process. Its Listen, Dial and FS are the Host's.
func (h *Host) Host() humus.Host {
	return humus.Host{
		Listen: h.Listen,
		Dial:   h.Dial,
		FS:     h.FS(),
	}
}

// Listen listens on the host and port of address. An empty host, 0.0.0.0
// or :: listens on every name. Port 0 picks a free port. Listen fails with
// EADDRINUSE when a listener already holds the same name and port, or
// when one of the two listens on every name for that port.
//
// The network is "tcp", "tcp4" or "tcp6", which share one set of ports.
// Every error is a *net.OpError. The listener's Addr reports the host as
// written in address and the port it holds.
func (h *Host) Listen(network, address string) (net.Listener, error) {
	k, err := parse(network, address)
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Err: err}
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if k.port == 0 {
		port, ok := h.freePortLocked(k.name)
		if !ok {
			return nil, &net.OpError{Op: "listen", Net: network, Addr: k.addr(), Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
		}
		k.port = port
	} else if h.inUseLocked(k) {
		return nil, &net.OpError{Op: "listen", Net: network, Addr: k.addr(), Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}

	l := &listener{
		host:  h,
		key:   k,
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
	h.listeners[k.bound()] = l
	h.held[k.port]++

	for wk, ch := range h.listening {
		if l.reaches(wk) && !isClosed(ch) {
			close(ch)
		}
	}
	return l, nil
}

// Dial connects to the listener for the host and port of address: one
// bound to that exact name, or else one that listens on every name for
// that port. It fails with ECONNREFUSED when there is none. It never
// waits for a listener.
//
// A Dial that finds its listener returns once the listener's Accept takes
// the connection. If ctx is done first, Dial closes both ends of the
// connection and returns ctx's error. Every error is a *net.OpError.
func (h *Host) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	k, err := parse(network, address)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Addr: k.addr(), Err: err}
	}

	h.mu.Lock()
	l := h.reachableLocked(k)
	h.mu.Unlock()
	if l == nil {
		return nil, refused(network, k)
	}

	client, server := newPipe()
	client.remote = k.addr()
	server.local = l.Addr()
	if err := l.connect(ctx, client, server); err != nil {
		if errors.Is(err, errListenerClosed) {
			return nil, refused(network, k)
		}
		return nil, &net.OpError{Op: "dial", Net: network, Addr: k.addr(), Err: err}
	}
	return client, nil
}

// Listening returns a channel that is closed once a listener exists that a
// Dial to address would reach. A test receives from it before dialling, so
// that it doesn't dial before the App listens. It works like bedrocktest's
// Process.Notified: once closed, the channel stays closed, and after that
// listener closes, Listening returns a new channel, closed when the next
// one is made. The channel is created on the first call, and no goroutine
// is started, so Listening works inside a testing/synctest bubble.
//
// If address is not a host and port, Listening fails the test through t
// and returns a closed channel, so that the Dial which follows reports the
// same mistake rather than the test hanging.
func (h *Host) Listening(address string) <-chan struct{} {
	k, err := parseAddress(address)
	if err != nil {
		h.tb.Helper()
		h.tb.Errorf("humustest: Listening(%q): %v", address, err)
		ch := make(chan struct{})
		close(ch)
		return ch
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if ch, ok := h.listening[k]; ok {
		return ch
	}
	ch := make(chan struct{})
	h.listening[k] = ch
	if h.reachableLocked(k) != nil {
		close(ch)
	}
	return ch
}

// HTTPClient returns an http.Client that dials the Host, with keep-alives
// off, so no idle connection outlives a request. Each call returns a new
// client whose Transport is a new *http.Transport, so a test can set its
// TLSClientConfig without changing any other client.
func (h *Host) HTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:       h.Dial,
			DisableKeepAlives: true,
		},
	}
}

// reachableLocked returns the listener a Dial to k reaches, or nil.
func (h *Host) reachableLocked(k key) *listener {
	if l, ok := h.listeners[k.bound()]; ok {
		return l
	}
	return h.listeners[key{port: k.port}]
}

// inUseLocked reports whether a Listen on k conflicts with a listener.
func (h *Host) inUseLocked(k key) bool {
	if k.everyName() {
		return h.held[k.port] > 0
	}
	_, named := h.listeners[k]
	_, every := h.listeners[key{port: k.port}]
	return named || every
}

// freePortLocked picks a port in the ephemeral range that name can listen
// on, starting after the last one picked, as a real network does.
func (h *Host) freePortLocked(name string) (int, bool) {
	for range lastEphemeralPort - firstEphemeralPort + 1 {
		port := h.nextPort
		h.nextPort++
		if h.nextPort > lastEphemeralPort {
			h.nextPort = firstEphemeralPort
		}
		if !h.inUseLocked(key{name: name, port: port}) {
			return port, true
		}
	}
	return 0, false
}

// remove forgets l, and every closed Listening channel that a Dial no
// longer reaches, so the next Listening call makes a new one.
func (h *Host) remove(l *listener) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.listeners, l.key.bound())
	if h.held[l.key.port]--; h.held[l.key.port] == 0 {
		delete(h.held, l.key.port)
	}
	for wk, ch := range h.listening {
		if isClosed(ch) && h.reachableLocked(wk) == nil {
			delete(h.listening, wk)
		}
	}
}

func refused(network string, k key) error {
	return &net.OpError{Op: "dial", Net: network, Addr: k.addr(), Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
