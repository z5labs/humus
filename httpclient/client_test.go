// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts.
// The end user's App is a job whose run calls the dependency, and the fake
// dependency is served by httpserver at payments:8443, over TLS with a
// certificate from a CA generated for each test, which the client trusts
// through RootCAs.

// startPayments runs a fake dependency on host, at payments:8443, over TLS
// with a certificate for payments from ca. It returns a function that
// stops the dependency and requires it to exit 0.
func startPayments(t *testing.T, h *humustest.Host, host humus.Host, ca *humustest.CA, handler http.Handler) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "payments")
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("payments:8443")
	return func() {
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}

// paymentsSettings are the client's defaults, for https://payments:8443,
// trusting only ca.
func paymentsSettings(t *testing.T, ca *humustest.CA) httpclient.Settings {
	t.Helper()
	settings := httpclient.DefaultSettings()
	settings.BaseURL = mustParse(t, "https://payments:8443")
	settings.TLS = &tls.Config{RootCAs: ca.Pool()}
	return settings
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// runJob runs the end user's App on host: a job whose run calls the
// dependency with the client NewClient builds from settings. It returns
// once the App has exited, with its exit status.
func runJob(t *testing.T, host humus.Host, settings httpclient.Settings, run func(ctx context.Context, c *http.Client) error) bedrock.ExitStatus {
	t.Helper()
	_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		c, err := httpclient.NewClient(s, settings)
		if err != nil {
			return nil, err
		}
		return []humus.Service{job.NewService(s, func(ctx context.Context) error { return run(ctx, c) })}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
	return <-status
}

// requireExitZero fails t unless st is a zero exit.
func requireExitZero(t *testing.T, st bedrock.ExitStatus) {
	t.Helper()
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

// response is what service code got back from a call.
type response struct {
	proto    int
	code     int
	body     string
	location string
	path     string
}

// get sends a GET to rawURL with c and ctx, and reads the whole response.
func get(ctx context.Context, c *http.Client, rawURL string) (response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return response{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return response{
		proto:    resp.ProtoMajor,
		code:     resp.StatusCode,
		body:     string(b),
		location: resp.Header.Get("Location"),
		path:     resp.Request.URL.Path,
	}, err
}

// dialled records the address of every Dial through a Host.
type dialled struct {
	mu    sync.Mutex
	addrs []string
}

// recordDials returns h's Host, with a Dial that records each address in
// the returned dialled before dialling it.
func recordDials(h *humustest.Host) (humus.Host, *dialled) {
	d := &dialled{}
	host := h.Host()
	dial := host.Dial
	host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		d.mu.Lock()
		d.addrs = append(d.addrs, address)
		d.mu.Unlock()
		return dial(ctx, network, address)
	}
	return host, d
}

func (d *dialled) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addrs)
}

// peerCloses counts the connections a listener accepted, and those that
// ended: the server read io.EOF, or closed the connection. A server closes
// a connection on its own only when it shuts down, or after its IdleTimeout,
// so before either, a connection that ended is one the client closed: over
// HTTP/2 the client sends a GOAWAY, and the server closes its end on reading
// it.
type peerCloses struct {
	accepted atomic.Int32
	closed   atomic.Int32
}

// watchConns returns h's Host, with a Listen whose connections report to
// the returned peerCloses.
func watchConns(h *humustest.Host) (humus.Host, *peerCloses) {
	pc := &peerCloses{}
	host := h.Host()
	listen := host.Listen
	host.Listen = func(network, address string) (net.Listener, error) {
		l, err := listen(network, address)
		if err != nil {
			return nil, err
		}
		return &watchedListener{Listener: l, pc: pc}, nil
	}
	return host, pc
}

type watchedListener struct {
	net.Listener
	pc *peerCloses
}

func (l *watchedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.pc.accepted.Add(1)
	return &watchedConn{Conn: c, pc: l.pc}, nil
}

type watchedConn struct {
	net.Conn
	pc   *peerCloses
	once sync.Once
}

func (c *watchedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if errors.Is(err, io.EOF) {
		c.ended()
	}
	return n, err
}

func (c *watchedConn) Close() error {
	c.ended()
	return c.Conn.Close()
}

func (c *watchedConn) ended() {
	c.once.Do(func() { c.pc.closed.Add(1) })
}

// ok answers every request with "ok", and counts them.
type ok struct{ calls atomic.Int32 }

func (h *ok) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls.Add(1)
	_, _ = io.WriteString(w, "ok")
}

func TestClient_a_call_reaches_the_dependency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var got *http.Request
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r
			_, _ = io.WriteString(w, "charge 1: paid")
		}))
		defer stop()

		var r response
		st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) (err error) {
			r, err = get(ctx, c, "https://payments:8443/charges/1")
			return err
		})
		requireExitZero(t, st)

		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, "charge 1: paid", r.body)
		require.Equal(t, http.MethodGet, got.Method)
		require.Equal(t, "/charges/1", got.URL.Path)
	})
}

func TestClient_calls_over_HTTP2(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var protos []int
		var mu sync.Mutex
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			protos = append(protos, r.ProtoMajor)
			mu.Unlock()
		}))
		defer stop()

		var r response
		st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			for range 3 {
				var err error
				if r, err = get(ctx, c, "https://payments:8443/"); err != nil {
					return err
				}
			}
			return nil
		})
		requireExitZero(t, st)

		require.Equal(t, 2, r.proto)
		require.Equal(t, []int{2, 2, 2}, protos)
	})
}

func TestClient_calls_in_plain_text_over_HTTP1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		l, err := h.Listen("tcp", "payments:8080")
		require.NoError(t, err)
		var proto string
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proto = r.Proto
				_, _ = io.WriteString(w, "plain")
			}),
			ReadHeaderTimeout: time.Second,
		}
		served := make(chan error, 1)
		go func() { served <- srv.Serve(l) }()

		settings := httpclient.DefaultSettings()
		settings.BaseURL = mustParse(t, "http://payments:8080")
		var r response
		st := runJob(t, h.Host(), settings, func(ctx context.Context, c *http.Client) (err error) {
			r, err = get(ctx, c, "http://payments:8080/status")
			return err
		})
		requireExitZero(t, st)

		require.Equal(t, "plain", r.body)
		require.Equal(t, 1, r.proto)
		require.Equal(t, "HTTP/1.1", proto)

		require.NoError(t, srv.Close())
		require.ErrorIs(t, <-served, http.ErrServerClosed)
	})
}

func TestClient_refuses_another_origin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		handler := &ok{}
		stop := startPayments(t, h, h.Host(), ca, handler) // :8443 is reachable as ledger:8443 too
		defer stop()

		host, dials := recordDials(h)
		var callErr error
		st := runJob(t, host, paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			_, callErr = get(ctx, c, "https://ledger:8443/")
			return nil
		})
		requireExitZero(t, st)

		var ue *url.Error
		require.ErrorAs(t, callErr, &ue)
		require.EqualError(t, ue.Err, "httpclient: request to https://ledger:8443 on a client for https://payments:8443")
		require.Empty(t, dials.all(), "nothing is dialled")
		require.Zero(t, handler.calls.Load())
	})
}

func TestClient_redirects(t *testing.T) {
	newDependency := func() http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /old", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/new", http.StatusFound)
		})
		mux.HandleFunc("GET /new", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "new")
		})
		mux.HandleFunc("GET /away", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://ledger:8443/entries", http.StatusFound)
		})
		mux.HandleFunc("GET /loop", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/loop", http.StatusFound)
		})
		return mux
	}

	t.Run("a redirect within the origin is followed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, newDependency())
			defer stop()

			var r response
			st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) (err error) {
				r, err = get(ctx, c, "https://payments:8443/old")
				return err
			})
			requireExitZero(t, st)

			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, "new", r.body)
			require.Equal(t, "/new", r.path)
		})
	})

	t.Run("a redirect to another origin returns the 302 to service code", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, newDependency())
			defer stop()

			host, dials := recordDials(h)
			var r response
			st := runJob(t, host, paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) (err error) {
				r, err = get(ctx, c, "https://payments:8443/away")
				return err
			})
			requireExitZero(t, st)

			require.Equal(t, http.StatusFound, r.code)
			require.Equal(t, "https://ledger:8443/entries", r.location)
			require.Equal(t, "/away", r.path)
			require.Equal(t, []string{"payments:8443"}, dials.all())
		})
	})

	t.Run("it stops after 10 redirects", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, newDependency())
			defer stop()

			var callErr error
			st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
				_, callErr = get(ctx, c, "https://payments:8443/loop")
				return nil
			})
			requireExitZero(t, st)

			require.ErrorContains(t, callErr, "stopped after 10 redirects")
		})
	})
}

func TestClient_a_call_fails_after_Timeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // never answers
		}))
		defer stop()

		settings := paymentsSettings(t, ca)
		settings.Timeout = 3 * time.Second
		var callErr error
		var took time.Duration
		st := runJob(t, h.Host(), settings, func(ctx context.Context, c *http.Client) error {
			start := time.Now()
			_, callErr = get(ctx, c, "https://payments:8443/")
			took = time.Since(start)
			return nil
		})
		requireExitZero(t, st)

		require.Error(t, callErr)
		var ue *url.Error
		require.ErrorAs(t, callErr, &ue)
		require.True(t, ue.Timeout(), "error: %v", callErr)
		require.Equal(t, 3*time.Second, took)
	})
}

func TestClient_uses_no_proxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("HTTP_PROXY", "http://proxy:3128")

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		handler := &ok{}
		stop := startPayments(t, h, h.Host(), ca, handler)
		defer stop()

		host, dials := recordDials(h)
		var r response
		st := runJob(t, host, paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) (err error) {
			r, err = get(ctx, c, "https://payments:8443/")
			return err
		})
		requireExitZero(t, st)

		require.Equal(t, "ok", r.body)
		require.EqualValues(t, 1, handler.calls.Load())
		require.Equal(t, []string{"payments:8443"}, dials.all(), "the client dials the dependency directly")
	})
}

func TestClient_closes_its_idle_connections_at_OnStop(t *testing.T) {
	t.Run("after the App exits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			host, conns := watchConns(h)
			stop := startPayments(t, h, host, ca, &ok{})
			defer stop()

			var openWhileRunning bool
			st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
				if _, err := get(ctx, c, "https://payments:8443/"); err != nil {
					return err
				}
				synctest.Wait()
				// The connection is idle in the pool while the App runs.
				openWhileRunning = conns.accepted.Load() == 1 && conns.closed.Load() == 0
				return nil
			})
			requireExitZero(t, st)
			synctest.Wait()

			require.True(t, openWhileRunning, "the connection was closed before the App stopped")
			require.EqualValues(t, 1, conns.accepted.Load())
			require.EqualValues(t, 1, conns.closed.Load(), "the dependency sees its connection from the client closed")
		})
	})

	t.Run("when Init fails after NewClient", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			host, conns := watchConns(h)
			stop := startPayments(t, h, host, ca, &ok{})
			defer stop()

			errInit := errors.New("init failed")
			settings := paymentsSettings(t, ca)
			_, status := humustest.Start(t, func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
				c, err := httpclient.NewClient(s, settings)
				if err != nil {
					return nil, err
				}
				if _, err := get(ctx, c, "https://payments:8443/"); err != nil {
					return nil, err
				}
				return nil, errInit
			}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
			st := <-status
			synctest.Wait()

			require.NotZero(t, st.Code())
			require.ErrorIs(t, st.Cause(), errInit)
			require.EqualValues(t, 1, conns.accepted.Load())
			require.EqualValues(t, 1, conns.closed.Load(), "the dependency sees its connection from the client closed")
		})
	})
}
