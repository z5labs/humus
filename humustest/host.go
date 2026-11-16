// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
)

// Host is a hosttest.Host that hands out a humus.Host: a network of
// in-memory connections over TCP and UDP, and a filesystem in a directory
// from t.TempDir(). hosttest's package documentation gives the rules of
// both. The network starts no goroutines, so the Host works inside a
// testing/synctest bubble; build it inside the bubble that uses it.
type Host struct {
	h *hosttest.Host
}

// NewHost returns a Host built with hosttest.New(t, opts...).
func NewHost(t testing.TB, opts ...hosttest.Option) *Host {
	t.Helper()
	return &Host{h: hosttest.New(t, opts...)}
}

// Host returns the humus.Host to run an App on, like bedrocktest's
// Process.Process: the Host's Listen, ListenPacket, Dial and FS.
func (h *Host) Host() humus.Host {
	return humus.Host{
		Listen:       h.Listen,
		ListenPacket: h.ListenPacket,
		Dial:         h.Dial,
		FS:           h.FS(),
	}
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

// Listen is hosttest.Host's Listen.
func (h *Host) Listen(network, address string) (net.Listener, error) {
	return h.h.Listen(network, address)
}

// ListenPacket is hosttest.Host's ListenPacket.
func (h *Host) ListenPacket(network, address string) (net.PacketConn, error) {
	return h.h.ListenPacket(network, address)
}

// Dial is hosttest.Host's Dial.
func (h *Host) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return h.h.Dial(ctx, network, address)
}

// Listening is hosttest.Host's Listening.
func (h *Host) Listening(address string) <-chan struct{} {
	return h.h.Listening(address)
}

// FS is hosttest.Host's FS, as a humus.FS.
func (h *Host) FS() humus.FS {
	return h.h.FS()
}

// Path is hosttest.Host's Path.
func (h *Host) Path(name string) string {
	return h.h.Path(name)
}
