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
// Process.Process: humus.Host{IPNetwork: h, Filesystem: h.Filesystem()}.
func (h *Host) Host() humus.Host {
	return humus.Host{IPNetwork: h, Filesystem: h.Filesystem()}
}

// HTTPClient returns an http.Client that dials the Host, with keep-alives
// off, so no idle connection outlives a request. Each call returns a new
// client whose Transport is a new *http.Transport, so a test can set its
// TLSClientConfig without changing any other client.
func (h *Host) HTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:       h.DialTCP,
			DisableKeepAlives: true,
		},
	}
}

// ListenTCP is hosttest.Host's ListenTCP.
func (h *Host) ListenTCP(ctx context.Context, network, address string) (net.Listener, error) {
	return h.h.ListenTCP(ctx, network, address)
}

// DialTCP is hosttest.Host's DialTCP.
func (h *Host) DialTCP(ctx context.Context, network, address string) (net.Conn, error) {
	return h.h.DialTCP(ctx, network, address)
}

// ListenUDP is hosttest.Host's ListenUDP.
func (h *Host) ListenUDP(ctx context.Context, network, address string) (net.PacketConn, error) {
	return h.h.ListenUDP(ctx, network, address)
}

// DialUDP is hosttest.Host's DialUDP.
func (h *Host) DialUDP(ctx context.Context, network, address string) (net.Conn, error) {
	return h.h.DialUDP(ctx, network, address)
}

// Listening is hosttest.Host's Listening.
func (h *Host) Listening(address string) <-chan struct{} {
	return h.h.Listening(address)
}

// Filesystem is hosttest.Host's Filesystem, which is a humus.Filesystem.
func (h *Host) Filesystem() *hosttest.Filesystem {
	return h.h.Filesystem()
}

// Path is hosttest.Host's Path.
func (h *Host) Path(name string) string {
	return h.h.Path(name)
}
