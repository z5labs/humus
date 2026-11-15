// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package sse_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/sse"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file serve a Stream on httpserver, inside synctest
// bubbles, on humustest Hosts, with DefaultSettings' WriteTimeout of 10s.

// response is what a client got back from a request.
type response struct {
	proto int
	body  string
	err   error
}

// fetch serves handler on an App on h, over TLS from ca, and GETs it with
// a client that offers only HTTP/2, or only HTTP/1.1. It returns the
// response once the App has exited 0.
func fetch(t *testing.T, h *humustest.Host, http2 bool, handler http.Handler) response {
	t.Helper()
	ca := humustest.NewCA(t)
	settings := httpserver.DefaultSettings()
	settings.Addr = ":8443"
	settings.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
	require.Equal(t, 10*time.Second, settings.WriteTimeout)
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("localhost:8443")

	c := h.HTTPClient()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	var protos http.Protocols
	if http2 {
		protos.SetHTTP2(true)
	} else {
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
		protos.SetHTTP1(true)
	}
	tr.Protocols = &protos

	var r response
	resp, err := c.Get("https://localhost:8443/feed")
	if err != nil {
		r.err = err
	} else {
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		r = response{proto: resp.ProtoMajor, body: string(b), err: err}
	}

	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Empty(t, p.Stderr())
	return r
}

// overBoth is each protocol a client speaks to the Service.
var overBoth = []struct {
	name  string
	http2 bool
	proto int
}{
	{"over HTTP/2", true, 2},
	{"over HTTP/1.1", false, 1},
}

func TestStream_an_idle_stream_outlives_WriteTimeout(t *testing.T) {
	for _, tc := range overBoth {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				var sendErr error
				r := fetch(t, h, tc.http2, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					s, err := sse.NewStream(w, 10*time.Second)
					if err != nil {
						sendErr = err
						return
					}
					if sendErr = s.Send(sse.Event{Data: "first"}); sendErr != nil {
						return
					}
					time.Sleep(25 * time.Second)
					if sendErr = s.Comment("keep-alive"); sendErr != nil {
						return
					}
					time.Sleep(25 * time.Second)
					sendErr = s.Send(sse.Event{Data: "last"})
				}))
				require.NoError(t, sendErr)
				require.NoError(t, r.err)
				require.Equal(t, tc.proto, r.proto)
				require.Equal(t, "data: first\n\n: keep-alive\ndata: last\n\n", r.body)
			})
		})
	}
}

// TestStream_WriteTimeout_resets_an_idle_HTTP2_stream_that_moves_its_deadline_only_forward
// shows what clearing the deadline between writes is for: the same feed,
// moving the deadline forward before each write and never clearing it, is
// reset over HTTP/2 while it is idle.
func TestStream_WriteTimeout_resets_an_idle_HTTP2_stream_that_moves_its_deadline_only_forward(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		returned := make(chan struct{})
		r := fetch(t, h, true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			defer close(returned)
			rc := http.NewResponseController(w)
			send := func(data string) {
				_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_, _ = io.WriteString(w, "data: "+data+"\n\n")
				_ = rc.Flush()
			}
			w.Header().Set("Content-Type", "text/event-stream")
			send("first")
			time.Sleep(25 * time.Second)
			send("last")
		}))
		require.Error(t, r.err, "the stream is reset before its last event")
		require.Equal(t, 2, r.proto)
		require.Equal(t, "data: first\n\n", r.body)
		<-returned
	})
}
