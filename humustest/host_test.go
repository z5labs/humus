// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humustest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// Every test in this file runs on hosttest's in-memory network and opens no
// real socket. The rules of that network, and its conformance to a real
// one, are hosttest's, and are tested in bedrock.

// serve accepts on ln until it is closed, writing msg to every connection
// and closing it. The listener is closed, and the accept loop waited for,
// when the test ends.
func serve(t *testing.T, ln net.Listener, msg string) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, msg)
			_ = c.Close()
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
}

// dialRead dials address on dial and returns everything the listener
// writes.
func dialRead(t *testing.T, dial func(context.Context, string, string) (net.Conn, error), address string) string {
	t.Helper()
	c, err := dial(t.Context(), "tcp", address)
	require.NoError(t, err)
	b, err := io.ReadAll(c)
	require.NoError(t, err)
	require.NoError(t, c.Close())
	return string(b)
}

func listen(t *testing.T, h *humustest.Host, address string) net.Listener {
	t.Helper()
	ln, err := h.ListenTCP(t.Context(), "tcp", address)
	require.NoError(t, err)
	return ln
}

// readFile reads the named file through fsys.
func readFile(t *testing.T, fsys humus.Filesystem, name string) string {
	t.Helper()
	f, err := fsys.Open(name)
	require.NoError(t, err)
	b, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return string(b)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestHost_Host_sets_every_field shows that both fields of Host() are one
// hosttest host's: a listener made through one is dialled through the
// Host's own methods, and a file it was built with is read through the
// Filesystem.
func TestHost_Host_sets_every_field(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t, hosttest.WithFiles(map[string][]byte{"etc/app.conf": []byte("conf")}))
		hh := h.Host()
		require.Same(t, h, hh.IPNetwork, "the IPNetwork is the Host")
		require.Equal(t, h.Filesystem(), hh.Filesystem, "the Filesystem is the Host's")

		require.Equal(t, "conf", readFile(t, hh.Filesystem, "etc/app.conf"), "the Filesystem works inside a bubble")
		require.Equal(t, "conf", readFile(t, hh.Filesystem, h.Path("etc/app.conf")), "Path names the Filesystem's file")

		ln, err := hh.ListenTCP(t.Context(), "tcp", "api:80")
		require.NoError(t, err)
		require.True(t, isClosed(h.Listening("api:80")), "Host()'s ListenTCP is the Host's")
		serve(t, ln, "api")
		require.Equal(t, "api", dialRead(t, h.DialTCP, "api:80"), "Host()'s ListenTCP is the Host's")

		ln, err = h.ListenTCP(t.Context(), "tcp", "admin:80")
		require.NoError(t, err)
		serve(t, ln, "admin")
		require.Equal(t, "admin", dialRead(t, hh.DialTCP, "admin:80"), "Host()'s DialTCP is the Host's")

		pc, err := hh.ListenUDP(t.Context(), "udp", "dns:53")
		require.NoError(t, err)
		_, err = h.ListenUDP(t.Context(), "udp", "dns:53")
		require.ErrorIs(t, err, syscall.EADDRINUSE, "Host()'s ListenUDP is the Host's")
		require.NoError(t, pc.Close())
	})
}

// TestNewHost_applies_hosttests_options shows that NewHost hands its
// options to hosttest.New, in order, and builds its Host on the
// hosttest.Host they configure.
func TestNewHost_applies_hosttests_options(t *testing.T) {
	var got []*hosttest.Host
	record := func(h *hosttest.Host) { got = append(got, h) }

	h := humustest.NewHost(t,
		record,
		hosttest.WithFiles(map[string][]byte{"a.txt": []byte("a")}),
		hosttest.WithFiles(map[string][]byte{"b.txt": []byte("b")}),
		record,
	)

	require.Len(t, got, 2)
	require.Same(t, got[0], got[1])
	require.Equal(t, got[0].Path("a.txt"), h.Path("a.txt"), "the Host is built on the hosttest.Host the options configured")
	require.Equal(t, "a", readFile(t, h.Filesystem(), "a.txt"))
	require.Equal(t, "b", readFile(t, h.Filesystem(), "b.txt"))
}

// TestHost_starts_no_goroutines builds a Host inside a bubble and leaves a
// listener and a socket open. synctest.Test fails with a deadlock if
// anything the Host did left a goroutine blocked behind.
func TestHost_starts_no_goroutines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		_, err := h.ListenTCP(t.Context(), "tcp", "api:80")
		require.NoError(t, err)
		_, err = h.ListenUDP(t.Context(), "udp", "api:80")
		require.NoError(t, err)
		_, err = h.DialTCP(t.Context(), "tcp", "api:81")
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
		require.True(t, isClosed(h.Listening("api:80")))
		require.False(t, isClosed(h.Listening("api:81")))
	})
}

// TestHost_udp_through_Host binds a socket with ListenUDP and dials it
// with DialUDP, both through Host(), so the datagram network is wired.
func TestHost_udp_through_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hh := humustest.NewHost(t).Host()

		pc, err := hh.ListenUDP(t.Context(), "udp", "dns:53")
		require.NoError(t, err)
		defer func() { require.NoError(t, pc.Close()) }()

		c, err := hh.DialUDP(t.Context(), "udp", "dns:53")
		require.NoError(t, err)
		defer func() { require.NoError(t, c.Close()) }()

		_, err = c.Write([]byte("ping"))
		require.NoError(t, err)

		b := make([]byte, 64)
		n, from, err := pc.ReadFrom(b)
		require.NoError(t, err)
		require.Equal(t, "ping", string(b[:n]))
		require.Equal(t, c.LocalAddr().String(), from.String(), "the datagram comes from the dialled socket")

		_, err = pc.WriteTo([]byte("pong"), from)
		require.NoError(t, err)
		n, err = c.Read(b)
		require.NoError(t, err)
		require.Equal(t, "pong", string(b[:n]))
	})
}

// httpService serves an http.Server on a listener, as the REST runtime
// will. It keeps serving after Serve's context is done, until Shutdown, and
// it passes TestService.
type httpService struct {
	ln  net.Listener
	srv *http.Server
}

func (s *httpService) Serve(context.Context) error {
	err := s.srv.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the server gracefully. http.Server.Shutdown returns nil
// for a done context when no connection is open, so Shutdown checks the
// context itself, and closes every connection at once when it is done.
func (s *httpService) Shutdown(ctx context.Context) error {
	err := s.srv.Shutdown(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(fmt.Errorf("http service: %w", ctxErr), s.srv.Close())
	}
	return err
}

// TestHost_Listening_an_App_in_real_time runs an App without synctest. The
// test waits for the App to listen with Listening, then dials it.
func TestHost_Listening_an_App_in_real_time(t *testing.T) {
	host := humustest.NewHost(t)
	app := humus.App[humus.Host]{
		Host:                host.Host(),
		ShutdownGracePeriod: 10 * time.Second,
		Telemetry:           telemetry.NoopBuilder,
		Init: func(_ context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
			ln, err := h.ListenTCP(t.Context(), "tcp", ":8080")
			if err != nil {
				return nil, err
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "hello")
			})}
			return []humus.Service{&httpService{ln: ln, srv: srv}}, nil
		},
	}

	p := bedrocktest.NewProcess(t)
	status := make(chan bedrock.ExitStatus, 1)
	go func() { status <- app.Run(t.Context(), p.Process()) }()

	<-host.Listening(":8080")
	resp, err := host.HTTPClient().Get("http://localhost:8080/")
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "hello", string(b))

	<-p.Notified(syscall.SIGTERM)
	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Equal(t, 0, st.Code(), "cause: %v, stderr: %s", st.Cause(), p.Stderr())

	_, err = host.DialTCP(t.Context(), "tcp", "localhost:8080")
	require.ErrorIs(t, err, syscall.ECONNREFUSED, "the server's listener is closed with the App")
}

// TestHost_HTTPClient sends a request inside a bubble. With keep-alives
// off, no connection outlives the response body, so the server sees every
// connection closed and the bubble ends without a deadlock.
func TestHost_HTTPClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ln := listen(t, h, "api.internal:80")

		var mu sync.Mutex
		open := 0
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, r.URL.Path)
			}),
			ConnState: func(_ net.Conn, state http.ConnState) {
				mu.Lock()
				defer mu.Unlock()
				switch state {
				case http.StateNew:
					open++
				case http.StateClosed, http.StateHijacked:
					open--
				}
			},
		}
		served := make(chan error, 1)
		go func() { served <- srv.Serve(ln) }()

		client := h.HTTPClient()
		_, ok := client.Transport.(*http.Transport)
		require.True(t, ok, "the Transport is an *http.Transport, so a test can set its TLS config")

		for _, path := range []string{"/one", "/two"} {
			resp, err := client.Get("http://api.internal" + path)
			require.NoError(t, err)
			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, path, string(b))
		}

		synctest.Wait()
		mu.Lock()
		require.Zero(t, open, "no connection is left open after the response body is closed")
		mu.Unlock()

		require.NoError(t, srv.Shutdown(t.Context()))
		require.ErrorIs(t, <-served, http.ErrServerClosed)
	})
}
