// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/http3/http3client"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts.
// The end user's App is a job whose run calls the dependency, and the fake
// dependency is a bare quic-go HTTP/3 server at payments:8443, on the
// Host's UDP, over TLS with a certificate from a CA generated for each
// test, which the client trusts through RootCAs.

// paymentsAddr is the fake dependency's address.
const paymentsAddr = "payments:8443"

// serveH3 serves handler over HTTP/3 on h's UDP at :8443, with cert, until t
// ends. The server logs nothing.
func serveH3(t *testing.T, h *humustest.Host, cert *humustest.Certificate, handler http.Handler) *http3.Server {
	t.Helper()
	sock, err := h.ListenUDP(t.Context(), "udp", ":8443")
	require.NoError(t, err)
	srv := &http3.Server{
		Handler:   handler,
		TLSConfig: http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert.TLS()}}),
		Logger:    slog.New(slog.DiscardHandler),
	}
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.Serve(sock) })
	t.Cleanup(func() {
		_ = srv.Close()
		_ = sock.Close()
		wg.Wait()
	})
	synctest.Wait()
	return srv
}

// paymentsSettings are the client's defaults, for https://payments:8443,
// trusting only ca.
func paymentsSettings(t *testing.T, ca *humustest.CA) http3client.Settings {
	t.Helper()
	settings := http3client.DefaultSettings()
	settings.BaseURL = mustParse(t, "https://"+paymentsAddr)
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
// once the App has exited, and requires it to have written nothing to
// stderr.
func runJob(t *testing.T, host humus.Host, tel telemetry.Builder, settings http3client.Settings, run func(ctx context.Context, c *http.Client) error) bedrock.ExitStatus {
	t.Helper()
	p, status := humustest.Start(t, host, func(_ context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
		c, err := http3client.NewClient(s, h, settings)
		if err != nil {
			return nil, err
		}
		return []humus.Service{job.NewService(s, func(ctx context.Context) error { return run(ctx, c) })}, nil
	}, humustest.WithApp(humus.WithTelemetry(tel)))
	st := <-status
	require.Empty(t, p.Stderr())
	return st
}

// requireExitZero fails t unless st is a zero exit.
func requireExitZero(t *testing.T, st bedrock.ExitStatus) {
	t.Helper()
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

// response is what service code got back from a call.
type response struct {
	proto string
	code  int
	body  string
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
	return response{proto: resp.Proto, code: resp.StatusCode, body: string(b)}, err
}

// sockets records every socket a Host dials over UDP, and fails every dial
// over TCP.
type sockets struct {
	mu    sync.Mutex
	dials []string
	conns []*socket
	tcp   atomic.Int32
}

// recordSockets returns h's Host, with a DialUDP that records each socket
// in the returned sockets, and a DialTCP that fails.
func recordSockets(h *humustest.Host) (humus.Host, *sockets) {
	s := &sockets{}
	host := h.Host()
	dial := host.DialUDP
	host.IPNetwork = hosttest.Seams{
		Network: host.IPNetwork,
		OnDialUDP: func(ctx context.Context, network, address string) (net.Conn, error) {
			c, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			sc := &socket{Conn: c}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.dials = append(s.dials, network+" "+address)
			s.conns = append(s.conns, sc)
			return sc, nil
		},
		OnDialTCP: func(context.Context, string, string) (net.Conn, error) {
			s.tcp.Add(1)
			return nil, errors.New("the test's Host dials nothing over TCP")
		},
	}
	return host, s
}

// all returns the network and address of every socket dialled.
func (s *sockets) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.dials)
}

// open reports, for each socket dialled, whether it is still open.
func (s *sockets) open() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	open := make([]bool, len(s.conns))
	for i, c := range s.conns {
		open[i] = c.closes.Load() == 0
	}
	return open
}

// socket is a dialled socket that counts its closes, and records the buffer
// sizes it is set to.
type socket struct {
	net.Conn
	closes      atomic.Int32
	read, write atomic.Int64

	mu      sync.Mutex
	bufErrs []error
}

func (c *socket) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func (c *socket) SetReadBuffer(n int) error {
	err := c.Conn.(interface{ SetReadBuffer(int) error }).SetReadBuffer(n)
	c.recordBuffer(&c.read, n, err)
	return err
}

func (c *socket) SetWriteBuffer(n int) error {
	err := c.Conn.(interface{ SetWriteBuffer(int) error }).SetWriteBuffer(n)
	c.recordBuffer(&c.write, n, err)
	return err
}

// recordBuffer records a buffer size the socket was set to, and the error
// setting it returned.
func (c *socket) recordBuffer(size *atomic.Int64, n int, err error) {
	size.Store(int64(n))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bufErrs = append(c.bufErrs, err)
}

// hello answers every request with hello and the request's path.
var hello = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "hello "+r.URL.Path)
})

func TestNewClient_speaks_HTTP3_through_the_network_it_is_given(t *testing.T) {
	// quic-go writes its UDP-buffer warning with the log package, to the
	// process's stderr, which the App's Process doesn't see. The test
	// catches it in log's output instead, so it can't run in parallel.
	var logged bytes.Buffer
	defer log.SetOutput(log.Writer())
	log.SetOutput(&logged)

	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		serveH3(t, h, ca.Issue(t, "payments"), hello)
		host, socks := recordSockets(h)

		var got []response
		st := runJob(t, host, telemetry.NoopBuilder, paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			for _, path := range []string{"/a", "/b"} {
				r, err := get(ctx, c, "https://payments:8443"+path)
				if err != nil {
					return err
				}
				got = append(got, r)
			}
			return nil
		})
		requireExitZero(t, st)

		require.Equal(t, []response{
			{proto: "HTTP/3.0", code: http.StatusOK, body: "hello /a"},
			{proto: "HTTP/3.0", code: http.StatusOK, body: "hello /b"},
		}, got)
		require.Equal(t, []string{"udp " + paymentsAddr}, socks.all(), "one connection, dialled through the Network over udp")
		require.Zero(t, socks.tcp.Load(), "nothing is dialled over TCP")

		socks.mu.Lock()
		sock := socks.conns[0]
		socks.mu.Unlock()
		require.Positive(t, sock.read.Load(), "quic-go set the socket's receive buffer through the Network's socket")
		require.Positive(t, sock.write.Load(), "quic-go set the socket's send buffer through the Network's socket")
		sock.mu.Lock()
		for _, err := range sock.bufErrs {
			require.NoError(t, err, "a buffer size the socket couldn't take makes quic-go warn")
		}
		sock.mu.Unlock()
	})
	require.Empty(t, logged.String(), "quic-go wrote nothing through the log package")
}

func TestNewClient_never_falls_back_to_TCP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		cert := ca.Issue(t, "payments")

		// The dependency serves HTTP/2 and HTTP/1.1 at payments:8443, over
		// TCP, and nothing over UDP.
		var served atomic.Int32
		p, status := humustest.Start(t, h.Host(), func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
			settings := httpserver.DefaultSettings()
			settings.Addr = ":8443"
			settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
			svc, err := httpserver.NewService(ctx, s, h, settings, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served.Add(1) }))
			if err != nil {
				return nil, err
			}
			return []humus.Service{svc}, nil
		}, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
		<-h.Listening(paymentsAddr)
		host, socks := recordSockets(h)

		tel := humustest.NewTelemetry()
		var callErr error
		st := runJob(t, host, tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			_, callErr = get(ctx, c, "https://payments:8443/")
			return nil
		})
		requireExitZero(t, st)

		require.ErrorIs(t, callErr, syscall.ECONNREFUSED, "a dependency that doesn't answer over UDP fails")
		require.Equal(t, slices.Repeat([]string{"udp " + paymentsAddr}, 3), socks.all(), "each of the 3 attempts dialled over udp")
		require.Zero(t, socks.tcp.Load(), "nothing is dialled over TCP")
		require.Zero(t, served.Load())
		require.Equal(t, map[string]int64{"failure": 3}, counts(t, tel, "humus.circuit.calls", "humus.circuit.outcome"), "the breaker counts each attempt")

		p.Signal(syscall.SIGTERM)
		requireExitZero(t, <-status)
	})
}

// streamErrors answers its first request by resetting the request stream
// with H3_INTERNAL_ERROR before any header, and every later one with ok.
type streamErrors struct {
	calls atomic.Int32
}

func (s *streamErrors) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if s.calls.Add(1) == 1 {
		reset(w, http3.ErrCodeInternalError)
		return
	}
	_, _ = io.WriteString(w, "ok")
}

// reset resets the request stream w writes to with code, in both
// directions, before any header is sent.
func reset(w http.ResponseWriter, code http3.ErrCode) {
	str := w.(http3.HTTPStreamer).HTTPStream()
	str.CancelRead(quic.StreamErrorCode(code))
	str.CancelWrite(quic.StreamErrorCode(code))
}

func TestClient_closes_every_connection_it_dialled_at_OnStop(t *testing.T) {
	t.Run("a connection quic-go stopped using after a stream error included", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			serveH3(t, h, ca.Issue(t, "payments"), &streamErrors{})
			host, socks := recordSockets(h)

			settings := paymentsSettings(t, ca)
			settings.MaxAttempts = 1
			var (
				first, second    error
				openWhileRunning []bool
			)
			st := runJob(t, host, telemetry.NoopBuilder, settings, func(ctx context.Context, c *http.Client) error {
				_, first = get(ctx, c, "https://payments:8443/")
				_, second = get(ctx, c, "https://payments:8443/")
				synctest.Wait()
				openWhileRunning = socks.open()
				return nil
			})
			requireExitZero(t, st)
			synctest.Wait()

			var h3err *http3.Error
			require.ErrorAs(t, first, &h3err)
			require.Equal(t, http3.ErrCodeInternalError, h3err.ErrorCode)
			require.NoError(t, second)
			require.Equal(t, []bool{true, true}, openWhileRunning,
				"quic-go dialled a second connection after the stream error, and left the first open")
			require.Equal(t, []bool{false, false}, socks.open(), "every socket is closed once the App stops")
		})
	})

	t.Run("when Init fails after NewClient", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			serveH3(t, h, ca.Issue(t, "payments"), hello)
			host, socks := recordSockets(h)

			errInit := errors.New("init failed")
			settings := paymentsSettings(t, ca)
			_, status := humustest.Start(t, host, func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
				c, err := http3client.NewClient(s, h, settings)
				if err != nil {
					return nil, err
				}
				if _, err := get(ctx, c, "https://payments:8443/"); err != nil {
					return nil, err
				}
				return nil, errInit
			}, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
			st := <-status
			synctest.Wait()

			require.ErrorIs(t, st.Cause(), errInit)
			require.Equal(t, []bool{false}, socks.open(), "the socket is closed once the App stops")
		})
	})
}

func TestClient_queues_calls_past_the_servers_streams_on_one_connection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		var inFlight, peak atomic.Int32
		serveH3(t, h, ca.Issue(t, "payments"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Second)
			inFlight.Add(-1)
			_, _ = io.WriteString(w, "ok")
		}))
		host, socks := recordSockets(h)

		const calls = 150
		var (
			mu    sync.Mutex
			codes []int
		)
		var took time.Duration
		st := runJob(t, host, telemetry.NoopBuilder, paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			start := time.Now()
			var wg sync.WaitGroup
			for range calls {
				wg.Go(func() {
					r, err := get(ctx, c, "https://payments:8443/")
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						codes = append(codes, 0)
						return
					}
					codes = append(codes, r.code)
				})
			}
			wg.Wait()
			took = time.Since(start)
			return nil
		})
		requireExitZero(t, st)

		require.Equal(t, slices.Repeat([]int{http.StatusOK}, calls), codes)
		require.Equal(t, []string{"udp " + paymentsAddr}, socks.all(), "every call went over one connection")
		require.EqualValues(t, 100, peak.Load(), "the server allows 100 streams at once")
		require.Equal(t, 2*time.Second, took.Round(time.Second), "the 50 past them waited for the first 100")
	})
}

// rejectsOnce resets its first request stream with H3_REQUEST_REJECTED, so
// the client may send the request again, and answers every later one with
// ok.
type rejectsOnce struct {
	calls atomic.Int32
}

func (s *rejectsOnce) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if s.calls.Add(1) == 1 {
		reset(w, http3.ErrCodeRequestRejected)
		return
	}
	_, _ = io.WriteString(w, "ok")
}

func TestClient_replays_a_request_quic_go_may_send_again_inside_one_attempt(t *testing.T) {
	t.Run("after H3_REQUEST_REJECTED", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			handler := &rejectsOnce{}
			serveH3(t, h, ca.Issue(t, "payments"), handler)
			host, socks := recordSockets(h)

			tel := humustest.NewTelemetry()
			var got response
			st := runJob(t, host, tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) (err error) {
				got, err = get(ctx, c, "https://payments:8443/")
				return err
			})
			requireExitZero(t, st)

			require.Equal(t, response{proto: "HTTP/3.0", code: http.StatusOK, body: "ok"}, got)
			require.EqualValues(t, 2, handler.calls.Load(), "quic-go sent the request again")
			require.Len(t, socks.all(), 2, "on a connection of its own")
			requireOneAttempt(t, tel)
		})
	})

	t.Run("after a GOAWAY", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			cert := ca.Issue(t, "payments")

			// One QUIC listener, whose first connection the old server
			// serves, and every later one the new server: a rollout behind
			// one address.
			sock, err := h.ListenUDP(t.Context(), "udp", ":8443")
			require.NoError(t, err)
			ln, err := quic.ListenEarly(sock, http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert.TLS()}}), nil)
			require.NoError(t, err)
			discard := slog.New(slog.DiscardHandler)
			answer := func(body string) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			}
			old := &http3.Server{Handler: answer("old"), Logger: discard}
			next := &http3.Server{Handler: answer("new"), Logger: discard}
			var wg sync.WaitGroup
			wg.Go(func() {
				for i := 0; ; i++ {
					conn, err := ln.Accept(context.Background())
					if err != nil {
						return
					}
					srv := next
					if i == 0 {
						srv = old
					}
					wg.Go(func() { _ = srv.ServeQUICConn(conn) })
				}
			})
			defer func() {
				_ = ln.Close()
				_ = old.Close()
				_ = next.Close()
				_ = sock.Close()
				wg.Wait()
			}()
			host, socks := recordSockets(h)

			tel := humustest.NewTelemetry()
			var (
				got     []string
				replays int32
			)
			st := runJob(t, host, tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
				r, err := get(ctx, c, "https://payments:8443/")
				if err != nil {
					return err
				}
				got = append(got, r.body)

				// The old server sends its GOAWAY, while the connection is
				// idle in the client's pool. quic-go keeps the connection
				// pooled, so the next request is sent on it, can't open a
				// stream there, and quic-go sends it again on a new
				// connection, inside the one RoundTrip: httptrace sees it
				// ask for a connection twice.
				shutdown, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				wg.Go(func() { _ = old.Shutdown(shutdown) })
				synctest.Wait()

				var getConns atomic.Int32
				traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GetConn: func(string) { getConns.Add(1) }})
				r, err = get(traced, c, "https://payments:8443/")
				if err != nil {
					return err
				}
				got = append(got, r.body)
				replays = getConns.Load() - 1
				return nil
			})
			requireExitZero(t, st)

			require.Equal(t, []string{"old", "new"}, got)
			require.EqualValues(t, 1, replays, "quic-go sent the second call again after the GOAWAY")
			require.Len(t, socks.all(), 2, "the second call went over a connection of its own")
			require.Len(t, clientSpans(tel), 2, "one attempt for each call")
			requireCounts(t, tel, "humus.retry.decisions", "humus.retry.decision", map[string]int64{"succeeded": 2})
			requireCounts(t, tel, "humus.circuit.calls", "humus.circuit.outcome", map[string]int64{"success": 2})
		})
	})
}

// requireOneAttempt requires tel to have recorded one call of one attempt,
// which succeeded, and which the breaker counted once: the call touched
// neither the breaker nor the budget again.
func requireOneAttempt(t *testing.T, tel *humustest.Telemetry) {
	t.Helper()
	requireCounts(t, tel, "humus.retry.decisions", "humus.retry.decision", map[string]int64{"succeeded": 1})
	requireCounts(t, tel, "humus.circuit.calls", "humus.circuit.outcome", map[string]int64{"success": 1})
	var spans int
	for _, s := range tel.Spans() {
		if s.SpanKind().String() == "client" {
			spans++
		}
	}
	require.Equal(t, 1, spans, "one attempt, so one client span")
}
