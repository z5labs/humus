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
	"strconv"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
)

// Every test in this file runs on the in-memory network and opens no real
// socket. The loopback half of the conformance test is in
// conformance_loopback_test.go, behind the conformance build tag.

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
	ln, err := h.Listen("tcp", address)
	require.NoError(t, err)
	return ln
}

func portOf(t *testing.T, a net.Addr) int {
	t.Helper()
	_, port, err := net.SplitHostPort(a.String())
	require.NoError(t, err)
	n, err := strconv.Atoi(port)
	require.NoError(t, err)
	return n
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestHost_Host_sets_Listen_and_Dial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		hh := h.Host()
		require.NotNil(t, hh.Listen)
		require.NotNil(t, hh.Dial)
		require.Nil(t, hh.FS, "the filesystem is a later story")

		ln, err := hh.Listen("tcp", "api:80")
		require.NoError(t, err)
		serve(t, ln, "api")
		require.Equal(t, "api", dialRead(t, hh.Dial, "api:80"))
	})
}

func TestNewHost_applies_its_options(t *testing.T) {
	var got *humustest.Host
	h := humustest.NewHost(t, func(h *humustest.Host) { got = h })
	require.Same(t, h, got)
}

// TestHost_starts_no_goroutines builds a Host inside a bubble and leaves a
// listener open. synctest.Test fails with a deadlock if anything the Host
// did left a goroutine blocked behind.
func TestHost_starts_no_goroutines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		_, err := h.Listen("tcp", "api:80")
		require.NoError(t, err)
		_, err = h.Dial(t.Context(), "tcp", "api:81")
		require.ErrorIs(t, err, syscall.ECONNREFUSED)
		require.True(t, isClosed(h.Listening("api:80")))
		require.False(t, isClosed(h.Listening("api:81")))
	})
}

// TestHost_channels_belong_to_the_bubble shows that Accept, Dial and
// Listening block durably inside a bubble: synctest.Wait returns only once
// every goroutine in the bubble is blocked on a channel of the bubble.
func TestHost_channels_belong_to_the_bubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)

		listening := make(chan struct{})
		go func() {
			<-h.Listening("api:80")
			close(listening)
		}()
		synctest.Wait()
		require.False(t, isClosed(listening))

		ln := listen(t, h, "api:80")
		synctest.Wait()
		require.True(t, isClosed(listening))

		dialed := make(chan error, 1)
		go func() {
			c, err := h.Dial(t.Context(), "tcp", "api:80")
			if err == nil {
				err = c.Close()
			}
			dialed <- err
		}()
		synctest.Wait()
		require.Empty(t, dialed, "Dial returns only once Accept takes the connection")

		c, err := ln.Accept()
		require.NoError(t, err)
		require.NoError(t, <-dialed)
		require.NoError(t, c.Close())

		accepted := make(chan error, 1)
		go func() {
			_, err := ln.Accept()
			accepted <- err
		}()
		synctest.Wait()
		require.Empty(t, accepted)
		require.NoError(t, ln.Close())
		require.ErrorIs(t, <-accepted, net.ErrClosed)
	})
}

func TestHost_two_names_on_one_port_both_serve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		serve(t, listen(t, h, "api.internal:8080"), "api")
		serve(t, listen(t, h, "admin.internal:8080"), "admin")

		require.Equal(t, "api", dialRead(t, h.Dial, "api.internal:8080"))
		require.Equal(t, "admin", dialRead(t, h.Dial, "admin.internal:8080"))
	})
}

func TestHost_Listen_EADDRINUSE(t *testing.T) {
	testCases := []struct {
		name          string
		first, second string
	}{
		{name: "the same name and port twice", first: "api:80", second: "api:80"},
		{name: "every name after a name", first: "api:80", second: ":80"},
		{name: "0.0.0.0 after a name", first: "api:80", second: "0.0.0.0:80"},
		{name: ":: after a name", first: "api:80", second: "[::]:80"},
		{name: "a name after every name", first: ":80", second: "api:80"},
		{name: "a name after 0.0.0.0", first: "0.0.0.0:80", second: "api:80"},
		{name: "every name after every name", first: "0.0.0.0:80", second: ":80"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := humustest.NewHost(t)
			ln := listen(t, h, tc.first)
			defer func() { require.NoError(t, ln.Close()) }()

			_, err := h.Listen("tcp", tc.second)
			require.ErrorIs(t, err, syscall.EADDRINUSE)
			opErr, ok := errors.AsType[*net.OpError](err)
			require.True(t, ok, "%T is not a *net.OpError", err)
			require.Equal(t, "listen", opErr.Op)
			require.Equal(t, tc.second, opErr.Addr.String())
		})
	}
}

func TestHost_Listen_port_0(t *testing.T) {
	t.Run("picks distinct free ports and Addr reports them", func(t *testing.T) {
		h := humustest.NewHost(t)
		a := listen(t, h, "api:0")
		b := listen(t, h, "api:0")

		pa, pb := portOf(t, a.Addr()), portOf(t, b.Addr())
		require.NotZero(t, pa)
		require.NotEqual(t, pa, pb)
		require.Equal(t, "api:"+strconv.Itoa(pa), a.Addr().String())
		require.Equal(t, "tcp", a.Addr().Network())

		serve(t, a, "a")
		serve(t, b, "b")
		require.Equal(t, "a", dialRead(t, h.Dial, a.Addr().String()))
		require.Equal(t, "b", dialRead(t, h.Dial, b.Addr().String()))
	})

	t.Run("reports every name as written", func(t *testing.T) {
		h := humustest.NewHost(t)
		for _, host := range []string{"", "0.0.0.0", "::"} {
			ln := listen(t, h, net.JoinHostPort(host, "0"))
			require.Equal(t, net.JoinHostPort(host, strconv.Itoa(portOf(t, ln.Addr()))), ln.Addr().String())
			require.NoError(t, ln.Close())
		}
	})

	t.Run("skips a port held on every name", func(t *testing.T) {
		h := humustest.NewHost(t)
		every := listen(t, h, ":0")
		require.NoError(t, every.Close())

		// The next pick is the port after every's; hold that one on every
		// name, and a name's port 0 must pass it by.
		next := portOf(t, every.Addr()) + 1
		held := listen(t, h, ":"+strconv.Itoa(next))
		defer func() { require.NoError(t, held.Close()) }()

		ln := listen(t, h, "api:0")
		defer func() { require.NoError(t, ln.Close()) }()
		require.NotEqual(t, next, portOf(t, ln.Addr()))
	})

	t.Run("a name can take a port another name holds", func(t *testing.T) {
		h := humustest.NewHost(t)
		a := listen(t, h, "a:0")
		defer func() { require.NoError(t, a.Close()) }()
		b := listen(t, h, "b:"+strconv.Itoa(portOf(t, a.Addr())))
		defer func() { require.NoError(t, b.Close()) }()
	})

	t.Run("fails with EADDRINUSE when every port is held", func(t *testing.T) {
		h := humustest.NewHost(t)
		var lns []net.Listener
		for {
			ln, err := h.Listen("tcp", ":0")
			if err != nil {
				require.ErrorIs(t, err, syscall.EADDRINUSE)
				break
			}
			lns = append(lns, ln)
		}
		require.Len(t, lns, 65535-49152+1)
		for _, ln := range lns {
			require.NoError(t, ln.Close())
		}
	})
}

func TestHost_Dial(t *testing.T) {
	t.Run("falls back to the every-name listener", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			serve(t, listen(t, h, ":80"), "every")

			require.Equal(t, "every", dialRead(t, h.Dial, "api:80"))
			require.Equal(t, "every", dialRead(t, h.Dial, "localhost:80"))
			require.Equal(t, "every", dialRead(t, h.Dial, ":80"))
		})
	})

	testCases := []struct {
		name     string
		listen   string
		dial     string
		wantAddr string
	}{
		{name: "nothing listens", dial: "api:80"},
		{name: "another port", listen: "api:81", dial: "api:80"},
		{name: "another name", listen: "api:80", dial: "admin:80"},
		{name: "names are not resolved", listen: "127.0.0.1:80", dial: "localhost:80"},
		{name: "every name on another port", listen: ":81", dial: "api:80"},
	}
	for _, tc := range testCases {
		t.Run("ECONNREFUSED when "+tc.name, func(t *testing.T) {
			h := humustest.NewHost(t)
			if tc.listen != "" {
				ln := listen(t, h, tc.listen)
				defer func() { require.NoError(t, ln.Close()) }()
			}

			c, err := h.Dial(t.Context(), "tcp", tc.dial)
			require.Nil(t, c)
			require.ErrorIs(t, err, syscall.ECONNREFUSED)
			opErr, ok := errors.AsType[*net.OpError](err)
			require.True(t, ok, "%T is not a *net.OpError", err)
			require.Equal(t, "dial", opErr.Op)
			require.Equal(t, tc.dial, opErr.Addr.String())
		})
	}

	t.Run("ECONNREFUSED after the listener closes, and the port can be listened on again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ln := listen(t, h, "api:80")
			require.NoError(t, ln.Close())

			_, err := h.Dial(t.Context(), "tcp", "api:80")
			require.ErrorIs(t, err, syscall.ECONNREFUSED)

			serve(t, listen(t, h, "api:80"), "again")
			require.Equal(t, "again", dialRead(t, h.Dial, "api:80"))
		})
	})

	t.Run("an every-name port can be listened on again after it closes", func(t *testing.T) {
		h := humustest.NewHost(t)
		require.NoError(t, listen(t, h, ":80").Close())
		require.NoError(t, listen(t, h, "api:80").Close())
		require.NoError(t, listen(t, h, ":80").Close())
	})

	t.Run("ECONNREFUSED when the listener closes before Accept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ln := listen(t, h, "api:80")

			dialed := make(chan error, 1)
			go func() {
				_, err := h.Dial(t.Context(), "tcp", "api:80")
				dialed <- err
			}()
			synctest.Wait()
			require.NoError(t, ln.Close())
			require.ErrorIs(t, <-dialed, syscall.ECONNREFUSED)
		})
	})

	t.Run("returns ctx's error when ctx is done before Accept", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ln := listen(t, h, "api:80")
			defer func() { require.NoError(t, ln.Close()) }()

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			start := time.Now()
			c, err := h.Dial(ctx, "tcp", "api:80")
			require.Nil(t, c)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			_, ok := errors.AsType[*net.OpError](err)
			require.True(t, ok, "%T is not a *net.OpError", err)
			require.Equal(t, time.Second, time.Since(start))
		})
	})

	t.Run("returns ctx's error when ctx is already done", func(t *testing.T) {
		h := humustest.NewHost(t)
		ln := listen(t, h, "api:80")
		defer func() { require.NoError(t, ln.Close()) }()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := h.Dial(ctx, "tcp", "api:80")
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("reports the listener's and the dialled address", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ln := listen(t, h, ":80")
			defer func() { require.NoError(t, ln.Close()) }()

			accepted := make(chan net.Conn, 1)
			go func() {
				c, err := ln.Accept()
				if err == nil {
					accepted <- c
				}
				close(accepted)
			}()

			client, err := h.Dial(t.Context(), "tcp", "api:80")
			require.NoError(t, err)
			server := <-accepted
			require.NotNil(t, server)

			require.Equal(t, "api:80", client.RemoteAddr().String())
			require.Equal(t, ln.Addr(), server.LocalAddr())
			require.NotNil(t, client.LocalAddr())
			require.NotNil(t, server.RemoteAddr())

			go func() { _, _ = io.WriteString(server, "ping"); _ = server.Close() }()
			b, err := io.ReadAll(client)
			require.NoError(t, err)
			require.Equal(t, "ping", string(b))
			require.NoError(t, client.Close())
		})
	})
}

func TestHost_malformed_addresses(t *testing.T) {
	testCases := []struct {
		name    string
		network string
		address string
	}{
		{name: "no port", network: "tcp", address: "api"},
		{name: "a service name for a port", network: "tcp", address: "api:http"},
		{name: "a port out of range", network: "tcp", address: "api:65536"},
		{name: "a negative port", network: "tcp", address: "api:-1"},
		{name: "too many colons", network: "tcp", address: "::1:80"},
		{name: "a network that is not TCP", network: "udp", address: "api:80"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := humustest.NewHost(t)

			ln, err := h.Listen(tc.network, tc.address)
			require.Nil(t, ln)
			opErr, ok := errors.AsType[*net.OpError](err)
			require.True(t, ok, "Listen: %T is not a *net.OpError", err)
			require.Equal(t, "listen", opErr.Op)
			require.Equal(t, tc.network, opErr.Net)

			c, err := h.Dial(t.Context(), tc.network, tc.address)
			require.Nil(t, c)
			opErr, ok = errors.AsType[*net.OpError](err)
			require.True(t, ok, "Dial: %T is not a *net.OpError", err)
			require.Equal(t, "dial", opErr.Op)
		})
	}

	t.Run("an empty port is port 0", func(t *testing.T) {
		h := humustest.NewHost(t)
		ln := listen(t, h, "api:")
		require.NotZero(t, portOf(t, ln.Addr()))
		require.NoError(t, ln.Close())
	})

	t.Run("tcp4 and tcp6 share tcp's ports", func(t *testing.T) {
		h := humustest.NewHost(t)
		ln, err := h.Listen("tcp4", "api:80")
		require.NoError(t, err)
		defer func() { require.NoError(t, ln.Close()) }()
		_, err = h.Listen("tcp6", "api:80")
		require.ErrorIs(t, err, syscall.EADDRINUSE)
	})
}

func TestListener_Close(t *testing.T) {
	h := humustest.NewHost(t)
	ln := listen(t, h, "api:80")
	require.NoError(t, ln.Close())

	c, err := ln.Accept()
	require.Nil(t, c)
	require.ErrorIs(t, err, net.ErrClosed)

	require.NotPanics(t, func() {
		require.ErrorIs(t, ln.Close(), net.ErrClosed, "a second Close reports the listener closed, as a real one does")
	})
}

// fakeTB records the failures a Host reports through its testing.TB.
type fakeTB struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (*fakeTB) Helper() {}

func (tb *fakeTB) Errorf(format string, args ...any) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.errors = append(tb.errors, fmt.Sprintf(format, args...))
}

func TestHost_Listening(t *testing.T) {
	t.Run("closes when a listener a Dial would reach is made", func(t *testing.T) {
		testCases := []struct {
			name, wait, listen string
		}{
			{name: "the same name", wait: "api:80", listen: "api:80"},
			{name: "every name", wait: "api:80", listen: ":80"},
			{name: "0.0.0.0", wait: "api:80", listen: "0.0.0.0:80"},
			{name: "every name, waited for as written", wait: ":80", listen: "[::]:80"},
		}
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				h := humustest.NewHost(t)
				ch := h.Listening(tc.wait)
				require.False(t, isClosed(ch))
				ln := listen(t, h, tc.listen)
				defer func() { require.NoError(t, ln.Close()) }()
				require.True(t, isClosed(ch))
			})
		}
	})

	t.Run("stays open for a listener a Dial would not reach", func(t *testing.T) {
		testCases := []struct {
			name, wait, listen string
		}{
			{name: "another name", wait: "api:80", listen: "admin:80"},
			{name: "another port", wait: "api:80", listen: "api:81"},
			{name: "a name, waited for on every name", wait: ":80", listen: "api:80"},
		}
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				h := humustest.NewHost(t)
				ch := h.Listening(tc.wait)
				ln := listen(t, h, tc.listen)
				defer func() { require.NoError(t, ln.Close()) }()
				require.False(t, isClosed(ch))
			})
		}
	})

	t.Run("is already closed when such a listener exists", func(t *testing.T) {
		h := humustest.NewHost(t)
		ln := listen(t, h, ":80")
		defer func() { require.NoError(t, ln.Close()) }()
		require.True(t, isClosed(h.Listening("api:80")))
	})

	t.Run("stays closed, and after the listener closes the next call is a new open channel", func(t *testing.T) {
		h := humustest.NewHost(t)
		first := h.Listening("api:80")
		ln := listen(t, h, "api:80")
		require.True(t, isClosed(first))
		require.Equal(t, first, h.Listening("api:80"), "the same channel while the listener lives")

		require.NoError(t, ln.Close())
		require.True(t, isClosed(first), "a closed channel stays closed")

		second := h.Listening("api:80")
		require.NotEqual(t, first, second)
		require.False(t, isClosed(second))

		ln = listen(t, h, "api:80")
		defer func() { require.NoError(t, ln.Close()) }()
		require.True(t, isClosed(second))
	})

	t.Run("stays closed while another listener still reaches it", func(t *testing.T) {
		h := humustest.NewHost(t)
		named := listen(t, h, "api:80")
		ch := h.Listening("api:80")
		require.True(t, isClosed(ch))
		require.NoError(t, named.Close())

		every := listen(t, h, ":80")
		defer func() { require.NoError(t, every.Close()) }()
		require.True(t, isClosed(h.Listening("api:80")))
	})

	t.Run("fails the test for a malformed address", func(t *testing.T) {
		tb := &fakeTB{TB: t}
		h := humustest.NewHost(tb)
		ch := h.Listening("api")
		require.True(t, isClosed(ch), "a closed channel, so the test goes on to the Dial that reports it")
		require.Len(t, tb.errors, 1)
		require.Contains(t, tb.errors[0], `Listening("api")`)
	})
}

// httpService serves an http.Server on a listener, as the REST runtime
// will.
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

func (s *httpService) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// TestHost_Listening_an_App_in_real_time runs an App without synctest. The
// test waits for the App to listen with Listening, then dials it.
func TestHost_Listening_an_App_in_real_time(t *testing.T) {
	host := humustest.NewHost(t)
	app := humus.App{
		Host: host.Host(),
		Init: func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			ln, err := s.Host.Listen("tcp", ":8080")
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

	_, err = host.Dial(t.Context(), "tcp", "localhost:8080")
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
