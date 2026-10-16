// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build loopback

package humus_test

import (
	"io"
	"testing"

	"github.com/z5labs/humus"
)

// TestOSHost_Listen_and_Dial_loopback opens real sockets on the loopback
// interface, so it runs only with -tags loopback.
func TestOSHost_Listen_and_Dial_loopback(t *testing.T) {
	h := humus.OSHost()

	ln, err := h.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
	}()

	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- err
			return
		}
		_, err = io.WriteString(conn, "hello")
		if cerr := conn.Close(); err == nil {
			err = cerr
		}
		accepted <- err
	}()

	conn, err := h.Dial(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()

	b, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Errorf("read %q, want %q", b, "hello")
	}
	if err := <-accepted; err != nil {
		t.Error(err)
	}
}
