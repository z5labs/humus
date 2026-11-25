// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus/http3/http3client"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file fail a call over HTTP/3 in each way the proposal's
// prototypes did, and check how the default Classify counts the error the
// attempt failed with.

// classified is one attempt's error, and how the default Classify counted it.
type classified struct {
	err   error
	class http3client.Classification
}

// callFailing runs one call to payments:8443 with one attempt, through the
// default Classify, and returns the error the attempt failed with and its
// Classification. between, if it is set, runs after a first call that must
// succeed, and before the one that fails.
func callFailing(t *testing.T, h *humustest.Host, ca *humustest.CA, settings http3client.Settings, between func()) classified {
	t.Helper()
	classify := settings.Classify
	var (
		mu  sync.Mutex
		got []classified
	)
	settings.Classify = func(resp *http.Response, err error) http3client.Classification {
		c := classify(resp, err)
		mu.Lock()
		defer mu.Unlock()
		got = append(got, classified{err: err, class: c})
		return c
	}
	settings.MaxAttempts = 1
	var callErr error
	st := runJob(t, h.Host(), telemetry.NoopBuilder, settings, func(ctx context.Context, c *http.Client) error {
		if between != nil {
			if _, err := get(ctx, c, "https://payments:8443/"); err != nil {
				return err
			}
			between()
		}
		_, callErr = get(ctx, c, "https://payments:8443/")
		return nil
	})
	requireExitZero(t, st)
	require.Error(t, callErr)

	mu.Lock()
	defer mu.Unlock()
	last := got[len(got)-1]
	require.Error(t, last.err, "the attempt failed")
	return last
}

// retryFailure is how the default Classify counts an error the dependency
// may not meet again.
var retryFailure = http3client.Classification{Retry: retry.Retry, Breaker: circuit.Failure}

func TestDefaultSettings_Classify_counts_each_QUIC_failure(t *testing.T) {
	t.Run("an untrusted server certificate is Stop, Failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca, other := humustest.NewCA(t), humustest.NewCA(t)
			h := humustest.NewHost(t)
			serveH3(t, h, other.Issue(t, "payments"), hello)

			got := callFailing(t, h, ca, paymentsSettings(t, ca), nil)
			var verify *tls.CertificateVerificationError
			require.ErrorAs(t, got.err, &verify)
			require.Equal(t, http3client.Classification{Retry: retry.Stop, Breaker: circuit.Failure}, got.class)
		})
	})

	t.Run("a refused port is Retry, Failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)

			got := callFailing(t, h, ca, paymentsSettings(t, ca), nil)
			require.ErrorIs(t, got.err, syscall.ECONNREFUSED)
			require.Equal(t, retryFailure, got.class)
		})
	})

	t.Run("an idle timeout is Retry, Failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			sock := silenceable(t, h)
			serveH3On(t, sock, ca.Issue(t, "payments"), hello)

			settings := paymentsSettings(t, ca)
			// Long enough for QUIC's 30s idle timeout to end the attempt.
			settings.AttemptTimeout = time.Minute
			settings.Timeout = 2 * time.Minute
			got := callFailing(t, h, ca, settings, func() { sock.silent.Store(true) })
			var idle *quic.IdleTimeoutError
			require.ErrorAs(t, got.err, &idle)
			require.Equal(t, retryFailure, got.class)
		})
	})

	t.Run("a stateless reset is Retry, Failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			cert := ca.Issue(t, "payments")
			restart := restartable(t, h, cert)

			got := callFailing(t, h, ca, paymentsSettings(t, ca), restart)
			var reset *quic.StatelessResetError
			require.ErrorAs(t, got.err, &reset)
			require.Equal(t, retryFailure, got.class)
		})
	})

	t.Run("no h3 in ALPN is Retry, Failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			sock, err := h.ListenUDP(t.Context(), "udp", ":8443")
			require.NoError(t, err)
			ln, err := quic.Listen(sock, &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "payments").TLS()}, NextProtos: []string{"echo"}}, nil)
			require.NoError(t, err)
			defer func() {
				_ = ln.Close()
				_ = sock.Close()
			}()

			got := callFailing(t, h, ca, paymentsSettings(t, ca), nil)
			var te *quic.TransportError
			require.ErrorAs(t, got.err, &te)
			require.Equal(t, quic.TransportErrorCode(0x100+120), te.ErrorCode, "no_application_protocol")
			require.Equal(t, retryFailure, got.class)
		})
	})

	t.Run("H3_INTERNAL_ERROR is Retry, Failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			serveH3(t, h, ca.Issue(t, "payments"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reset(w, http3.ErrCodeInternalError)
			}))

			got := callFailing(t, h, ca, paymentsSettings(t, ca), nil)
			var h3err *http3.Error
			require.ErrorAs(t, got.err, &h3err)
			require.Equal(t, http3.ErrCodeInternalError, h3err.ErrorCode)
			require.Equal(t, retryFailure, got.class)
		})
	})
}

// serveH3On serves handler over HTTP/3 on sock, with cert, until t ends.
func serveH3On(t *testing.T, sock net.PacketConn, cert *humustest.Certificate, handler http.Handler) {
	t.Helper()
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
}

// silentSocket is a server's socket that drops every packet, in and out,
// once silent is set: a server that went away without a word.
type silentSocket struct {
	net.PacketConn
	silent atomic.Bool
}

// silenceable returns a silentSocket on h's UDP at :8443.
func silenceable(t *testing.T, h *humustest.Host) *silentSocket {
	t.Helper()
	sock, err := h.ListenUDP(t.Context(), "udp", ":8443")
	require.NoError(t, err)
	return &silentSocket{PacketConn: sock}
}

func (s *silentSocket) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := s.PacketConn.ReadFrom(p)
		if err != nil || !s.silent.Load() {
			return n, addr, err
		}
	}
}

func (s *silentSocket) WriteTo(p []byte, addr net.Addr) (int, error) {
	if s.silent.Load() {
		return len(p), nil
	}
	return s.PacketConn.WriteTo(p, addr)
}

// restartable serves hello over HTTP/3 on h's UDP at :8443, with cert, and
// returns a function that restarts the server: every packet after it goes
// to a fresh quic.Transport with the same stateless reset key, which knows
// none of the first one's connections, and so answers a packet of one with
// a stateless reset, as a restarted server does.
func restartable(t *testing.T, h *humustest.Host, cert *humustest.Certificate) (restart func()) {
	t.Helper()
	sock, err := h.ListenUDP(t.Context(), "udp", ":8443")
	require.NoError(t, err)
	var key quic.StatelessResetKey
	_, _ = rand.Read(key[:])
	tlsConf := http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert.TLS()}})

	first, second := newFeed(sock), newFeed(sock)
	var current atomic.Pointer[feed]
	current.Store(first)
	var wg sync.WaitGroup
	wg.Go(func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := sock.ReadFrom(buf)
			if err != nil {
				first.close()
				second.close()
				return
			}
			current.Load().deliver(buf[:n], addr)
		}
	})

	before := &quic.Transport{Conn: first, StatelessResetKey: &key}
	after := &quic.Transport{Conn: second, StatelessResetKey: &key}
	srv := &http3.Server{Handler: hello, Logger: slog.New(slog.DiscardHandler)}
	ln, err := before.ListenEarly(tlsConf, nil)
	require.NoError(t, err)
	wg.Go(func() { _ = srv.ServeListener(ln) })
	// The second transport reads only once it listens.
	unused, err := after.Listen(tlsConf, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = srv.Close()
		_ = unused.Close()
		// A Transport closes once its reads fail, and never closes the
		// net.PacketConn it was given.
		first.close()
		second.close()
		_ = before.Close()
		_ = after.Close()
		_ = sock.Close()
		wg.Wait()
	})
	synctest.Wait()
	return func() { current.Store(second) }
}

// feed is a net.PacketConn for a quic.Transport, which reads the packets
// delivered to it, and writes to the socket it shares.
type feed struct {
	sock    net.PacketConn
	packets chan packet
	done    chan struct{}
	once    sync.Once
}

type packet struct {
	b    []byte
	addr net.Addr
}

func newFeed(sock net.PacketConn) *feed {
	return &feed{sock: sock, packets: make(chan packet, 64), done: make(chan struct{})}
}

func (f *feed) deliver(b []byte, addr net.Addr) {
	select {
	case f.packets <- packet{b: append([]byte(nil), b...), addr: addr}:
	case <-f.done:
	}
}

func (f *feed) close() { f.once.Do(func() { close(f.done) }) }

func (f *feed) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case pk := <-f.packets:
		return copy(p, pk.b), pk.addr, nil
	case <-f.done:
		return 0, nil, net.ErrClosed
	}
}

func (f *feed) WriteTo(p []byte, addr net.Addr) (int, error) { return f.sock.WriteTo(p, addr) }

func (f *feed) Close() error {
	f.close()
	return nil
}

func (f *feed) LocalAddr() net.Addr              { return f.sock.LocalAddr() }
func (f *feed) SetDeadline(time.Time) error      { return nil }
func (f *feed) SetReadDeadline(time.Time) error  { return nil }
func (f *feed) SetWriteDeadline(time.Time) error { return nil }
func (f *feed) SetReadBuffer(int) error          { return nil }
func (f *feed) SetWriteBuffer(int) error         { return nil }
