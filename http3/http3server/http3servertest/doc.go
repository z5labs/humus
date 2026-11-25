// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package http3servertest tests an HTTP/3 service on the defaults that
// http3server.Main gives it in production, with a client that speaks
// HTTP/3 to a humustest Host.
//
// It sits beside http3server as httpservertest sits beside httpserver. It
// has two functions:
//
//	p, status := http3servertest.Start(t, h.Host(), Init,
//		humustest.WithEnv(env),
//	)
//	synctest.Wait()
//
//	resp, err := http3servertest.NewClient(t, h, ca.Pool()).Get("https://localhost:8443/hello/humus")
//
// humustest's Host.Listening waits for TCP listeners only, so a test waits
// for a UDP socket to be bound with synctest.Wait, once every goroutine of
// the App is blocked.
//
// # Start and the defaults
//
// [Start] is humustest.Start with http3server.Defaults applied before the
// test's options, and does nothing else: a drain of
// humus.DrainDelay(5 * time.Second) and a 10s grace period. Options given
// with humustest.WithApp come after the defaults, so they win.
//
// Start makes no CA and no certificate, as httpservertest's doesn't: the
// test issues one from a humustest.CA and gives it to the App the way
// production does.
//
// # NewClient speaks only HTTP/3
//
// [NewClient] returns an http.Client whose Transport is quic-go's HTTP/3
// transport. It dials each QUIC connection through h.DialUDP, so it reaches
// a Service on the same Host, and it trusts only the given roots. Every
// request it makes is HTTP/3: there is no TCP to fall back to, so a Service
// that stopped serving HTTP/3 fails the test.
//
// Unlike httpservertest's client, it keeps its connections between
// requests, as an HTTP/3 client does, so it closes every connection it
// opened when t's test ends, through t.Cleanup. Nothing it dialled outlives
// a testing/synctest bubble, whatever quic-go does with a connection it
// stops using. It forwards each socket's SetReadBuffer and SetWriteBuffer,
// so quic-go writes no UDP-buffer warning to the real standard error.
//
// NewClient works without Start: it needs only the Host the App runs on
// and the roots its certificate chains to. A test of a process that serves
// HTTP/2 beside HTTP/3 asks once over each, with httpservertest's client
// and this one.
package http3servertest
