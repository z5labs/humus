// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3servertest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/http3/http3server/http3servertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run whole Apps inside synctest bubbles, on
// humustest Hosts, with certificates from a CA generated for each test.

// serves returns an Init whose only Service serves h over HTTP/3 on :8443,
// with cert and no WriteTimeout.
func serves(cert *humustest.Certificate, h http.Handler) func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error) {
	return func(ctx context.Context, s *humus.Setup, n humus.Host) ([]humus.Service, error) {
		settings := http3server.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		// No WriteTimeout, which would otherwise cancel the request of a
		// handler that blocks through the grace period first.
		settings.WriteTimeout = 0
		svc, err := http3server.NewService(ctx, s, n, settings, h)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}

// greet answers GET /hello/{name} with "hello, " and the name.
func greet(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "hello, "+r.PathValue("name"))
}

func TestStart_serves_HTTP3_and_records_the_span(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /hello/{name}", greet)

		p, status := http3servertest.Start(t, h.Host(), serves(ca.Issue(t, "localhost"), mux),
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
		)
		synctest.Wait()

		resp, err := http3servertest.NewClient(t, h, ca.Pool()).Get("https://localhost:8443/hello/humus")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, 3, resp.ProtoMajor)
		require.Equal(t, "hello, humus", string(body))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, "GET /hello/{name}", spans[0].Name())
		require.Empty(t, p.Stderr())
	})
}

func TestStart_applies_the_Defaults_before_the_tests_options(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []humustest.Option
		want time.Duration
	}{
		// The 5s drain, then the 10s grace period.
		{"the Defaults alone", nil, 15 * time.Second},
		// The test's grace period wins, and the drain is still the 5s.
		{"a grace period of the test's", []humustest.Option{humustest.WithApp(humus.WithShutdownGracePeriod(2 * time.Second))}, 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				var signalled time.Time
				cancelled := make(chan time.Duration, 1)
				handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					<-r.Context().Done()
					cancelled <- time.Since(signalled)
				})
				opts := append([]humustest.Option{humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder))}, tc.opts...)
				p, status := http3servertest.Start(t, h.Host(), serves(ca.Issue(t, "localhost"), handler), opts...)
				synctest.Wait()

				go func() {
					resp, err := http3servertest.NewClient(t, h, ca.Pool()).Get("https://localhost:8443/")
					if err == nil {
						_ = resp.Body.Close()
					}
				}()
				synctest.Wait()
				signalled = time.Now()
				p.Signal(syscall.SIGTERM)

				st := <-status
				require.Equal(t, humus.ExitFailure, st.Code())
				require.Equal(t, tc.want, <-cancelled)
			})
		})
	}
}

func TestNewClient(t *testing.T) {
	t.Run("trusts only the roots it is given", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			p, status := http3servertest.Start(t, h.Host(), serves(ca.Issue(t, "localhost"), http.HandlerFunc(greet)),
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			synctest.Wait()

			_, err := http3servertest.NewClient(t, h, humustest.NewCA(t).Pool()).Get("https://localhost:8443/")
			var uerr x509.UnknownAuthorityError
			require.ErrorAs(t, err, &uerr)

			p.Signal(syscall.SIGTERM)
			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
		})
	})

	t.Run("speaks only HTTP/3", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			c := http3servertest.NewClient(t, h, x509.NewCertPool())
			_, ok := c.Transport.(*http3.Transport)
			require.True(t, ok, "Transport = %T, want *http3.Transport", c.Transport)

			// Nothing listens on UDP: a TCP listener is no fallback.
			l, err := h.ListenTCP(t.Context(), "tcp", ":8443")
			require.NoError(t, err)
			defer func() { _ = l.Close() }()
			_, err = c.Get("https://localhost:8443/")
			require.Error(t, err)
		})
	})

	t.Run("closes every connection it opened when the test ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			sock, err := h.ListenUDP(t.Context(), "udp", ":8443")
			require.NoError(t, err)
			defer func() { _ = sock.Close() }()
			ln, err := quic.ListenEarly(sock, http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}), nil)
			require.NoError(t, err)
			defer func() { _ = ln.Close() }()
			srv := &http3.Server{Handler: http.HandlerFunc(greet)}
			accepted := make(chan *quic.Conn, 1)
			go func() {
				conn, err := ln.Accept(context.Background())
				if err != nil {
					return
				}
				accepted <- conn
				_ = srv.ServeQUICConn(conn)
			}()

			tb := &cleanups{TB: t}
			resp, err := http3servertest.NewClient(tb, h, ca.Pool()).Get("https://localhost:8443/hello/humus")
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())
			conn := <-accepted

			synctest.Wait()
			require.NoError(t, conn.Context().Err(), "the connection is kept between requests")
			tb.run()
			<-conn.Context().Done()
		})
	})

	t.Run("panics on a nil Host or a nil pool", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			require.PanicsWithValue(t, "http3servertest: NewClient called with a nil *humustest.Host", func() {
				http3servertest.NewClient(t, nil, x509.NewCertPool())
			})
			require.PanicsWithValue(t, "http3servertest: NewClient called with a nil *x509.CertPool", func() {
				http3servertest.NewClient(t, humustest.NewHost(t), nil)
			})
		})
	})
}

// cleanups is a testing.TB whose Cleanup functions run when run is called,
// in reverse order, rather than when the test ends.
type cleanups struct {
	testing.TB
	fns []func()
}

func (c *cleanups) Cleanup(f func()) { c.fns = append(c.fns, f) }

func (c *cleanups) run() {
	for i := len(c.fns) - 1; i >= 0; i-- {
		c.fns[i]()
	}
}
