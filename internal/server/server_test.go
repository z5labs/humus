// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/internal/server"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts,
// with certificates from a CA generated for each test. The Server knows
// nothing of HTTP/2 or of httpserver's settings, so each test configures a
// bare *http.Server with a TLS config and a handler, as a package building
// on it does.

// scope is the name of the logger each test passes to New, which is not the
// name of any package's, so a record under it can only have come through
// the logger the caller passed.
const scope = "example.com/probes"

// tlsConfig returns a TLS config with a certificate for localhost from ca.
func tlsConfig(t *testing.T, ca *humustest.CA) *tls.Config {
	t.Helper()
	return &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
}

// newServer returns a Server on addr, serving h over TLS with a certificate
// from ca, and logging through s's logger named scope.
func newServer(s *humus.Setup, addr string, cfg *tls.Config, h http.Handler) (*server.Server, error) {
	return server.New(s, addr, &http.Server{Handler: h, TLSConfig: cfg}, s.Telemetry.LoggerProvider.Logger(scope))
}

// serves returns an Init whose only Service is a Server serving h on :8443.
func serves(cfg *tls.Config, h http.Handler) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		srv, err := newServer(s, ":8443", cfg, h)
		if err != nil {
			return nil, err
		}
		return []humus.Service{srv}, nil
	}
}

// newSetup returns a Setup on h with telemetry off, for a test that drives a
// Server directly rather than through an App.
func newSetup(h *humustest.Host) *humus.Setup {
	return &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}
}

// client returns h's HTTP client, trusting only ca.
func client(h *humustest.Host, ca *humustest.CA) *http.Client {
	c := h.HTTPClient()
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	return c
}

// response is what a client got back from a request.
type response struct {
	code int
	body string
	err  error
}

// get sends a GET to url with c in a new goroutine, and returns the channel
// its response arrives on.
func get(c *http.Client, url string) <-chan response {
	got := make(chan response, 1)
	go func() {
		resp, err := c.Get(url)
		if err != nil {
			got <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- response{code: resp.StatusCode, body: string(b), err: err}
	}()
	return got
}

// hello answers every request with "hello".
var hello = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "hello")
})

// requireRefused fails t unless a Dial to address is refused.
func requireRefused(t *testing.T, h *humustest.Host, address string) {
	t.Helper()
	conn, err := h.Dial(t.Context(), "tcp", address)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("Dial(%q) connected, want ECONNREFUSED", address)
	}
	require.ErrorIs(t, err, syscall.ECONNREFUSED)
}

// records returns the records of tel emitted by the logger named scope.
func records(tel *humustest.Telemetry) []sdklog.Record {
	var rs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == scope {
			rs = append(rs, r)
		}
	}
	return rs
}

// attr returns the value of r's attribute key, and whether it has one.
func attr(r sdklog.Record, key string) (attribute.Value, bool) {
	var v attribute.Value
	found := false
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == key {
			v, found = kv.Value, true
			return false
		}
		return true
	})
	return v, found
}

func TestServer_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		srv, err := newServer(s, ":8443", tlsConfig(t, humustest.NewCA(t)), http.NotFoundHandler())
		require.NoError(t, err)
		return srv
	})
}

func TestServer_serves_a_request_over_TLS(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, status := humustest.Start(t, serves(tlsConfig(t, ca), hello),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("localhost:8443")

		r := <-get(client(h, ca), "https://localhost:8443/")
		require.NoError(t, r.err)
		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, "hello", r.body)

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
		requireRefused(t, h, "localhost:8443")
	})
}

func TestServer_serves_the_callers_http_Server(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		s := newSetup(h)
		// A handler that outlives the WriteTimeout gets its connection
		// closed with no response, so the timeout the caller set is the
		// one served.
		slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(2 * time.Second)
			_, _ = io.WriteString(w, "too late")
		})
		srv, err := server.New(s, ":8443", &http.Server{
			Handler:      slow,
			TLSConfig:    tlsConfig(t, ca),
			WriteTimeout: time.Second,
		}, s.Telemetry.LoggerProvider.Logger(scope))
		require.NoError(t, err)
		served := make(chan error, 1)
		go func() { served <- srv.Serve(t.Context()) }()

		r := <-get(client(h, ca), "https://localhost:8443/")
		require.Error(t, r.err, "want the connection closed after the WriteTimeout")

		require.NoError(t, srv.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}

func TestServer_an_in_flight_request_finishes_during_Shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		started := make(chan time.Time, 1)
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			started <- time.Now()
			time.Sleep(3 * time.Second)
			_, _ = io.WriteString(w, "finished")
		})
		srv, err := newServer(newSetup(h), ":8443", tlsConfig(t, ca), handler)
		require.NoError(t, err)
		served := make(chan error, 1)
		go func() { served <- srv.Serve(t.Context()) }()

		got := get(client(h, ca), "https://localhost:8443/")
		startedAt := <-started
		require.NoError(t, srv.Shutdown(t.Context()))
		require.GreaterOrEqual(t, time.Since(startedAt), 3*time.Second, "Shutdown waits for the request")

		r := <-got
		require.NoError(t, r.err)
		require.Equal(t, "finished", r.body)
		require.NoError(t, <-served)
		requireRefused(t, h, "localhost:8443")
	})
}

func TestServer_Shutdown(t *testing.T) {
	t.Run("before Serve, Serve then returns nil at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			srv, err := newServer(newSetup(h), ":8443", tlsConfig(t, humustest.NewCA(t)), hello)
			require.NoError(t, err)

			require.NoError(t, srv.Shutdown(t.Context()))
			start := time.Now()
			require.NoError(t, srv.Serve(t.Context()))
			require.Zero(t, time.Since(start))
			requireRefused(t, h, "localhost:8443")
		})
	})

	t.Run("with a done context, closes every connection", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			started := make(chan struct{})
			cancelled := make(chan error, 1)
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				cancelled <- r.Context().Err()
			})
			srv, err := newServer(newSetup(h), ":8443", tlsConfig(t, ca), handler)
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- srv.Serve(t.Context()) }()

			got := get(client(h, ca), "https://localhost:8443/")
			<-started
			done, cancel := context.WithCancel(t.Context())
			cancel()
			start := time.Now()
			err = srv.Shutdown(done)
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "shutting down")
			require.Zero(t, time.Since(start))

			require.ErrorIs(t, <-cancelled, context.Canceled)
			require.Error(t, (<-got).err)
			require.NoError(t, <-served)
		})
	})

	t.Run("a context done during it closes what it could not drain", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			started := make(chan struct{})
			cancelled := make(chan time.Time, 1)
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				cancelled <- time.Now()
			})
			srv, err := newServer(newSetup(h), ":8443", tlsConfig(t, ca), handler)
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- srv.Serve(t.Context()) }()

			got := get(client(h, ca), "https://localhost:8443/")
			<-started
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			start := time.Now()
			err = srv.Shutdown(ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, 2*time.Second, (<-cancelled).Sub(start))
			require.Error(t, (<-got).err)
			require.NoError(t, <-served)
		})
	})

	t.Run("a second call returns nil", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			srv, err := newServer(newSetup(h), ":8443", tlsConfig(t, humustest.NewCA(t)), hello)
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- srv.Serve(t.Context()) }()
			synctest.Wait()

			done, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, srv.Shutdown(done), context.Canceled)
			require.NoError(t, srv.Shutdown(done))
			require.NoError(t, <-served)
		})
	})
}

func TestNew_the_listener(t *testing.T) {
	t.Run("an Init that fails after New closes it through OnStop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			errInit := errors.New("the database is unreachable")
			cfg := tlsConfig(t, ca)
			init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				if _, err := newServer(s, ":8443", cfg, hello); err != nil {
					return nil, err
				}
				return nil, errInit
			}
			_, status := humustest.Start(t, init,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)

			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), errInit)
			requireRefused(t, h, "localhost:8443")
		})
	})

	t.Run("a taken address is New's error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			s := newSetup(h)
			first, err := newServer(s, ":8443", tlsConfig(t, ca), hello)
			require.NoError(t, err)
			defer func() { _ = first.Shutdown(t.Context()) }()

			second, err := newServer(s, ":8443", tlsConfig(t, ca), hello)
			require.Nil(t, second)
			require.ErrorIs(t, err, syscall.EADDRINUSE)
		})
	})

	t.Run("Addr reports the port the Host picked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			srv, err := newServer(newSetup(h), ":0", tlsConfig(t, ca), hello)
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- srv.Serve(t.Context()) }()

			_, port, err := net.SplitHostPort(srv.Addr().String())
			require.NoError(t, err)
			require.NotEqual(t, "0", port)
			r := <-get(client(h, ca), fmt.Sprintf("https://localhost:%s/", port))
			require.NoError(t, r.err)
			require.Equal(t, "hello", r.body)

			require.NoError(t, srv.Shutdown(t.Context()))
			require.NoError(t, <-served)
		})
	})
}

func TestServer_Serve_without_a_certificate_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		srv, err := newServer(newSetup(h), ":8443", &tls.Config{}, hello)
		require.NoError(t, err)

		err = srv.Serve(t.Context())
		require.ErrorContains(t, err, "serving")
		require.NoError(t, srv.Shutdown(t.Context()))
	})
}

func TestServer_logs(t *testing.T) {
	t.Run("listening, with the address", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			tel := humustest.NewTelemetry()
			p, status := humustest.Start(t, serves(tlsConfig(t, ca), hello),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			)
			<-h.Listening("localhost:8443")
			synctest.Wait()

			rs := records(tel)
			require.Len(t, rs, 1)
			require.Equal(t, "listening", rs[0].Body().AsString())
			require.Equal(t, log.SeverityInfo, rs[0].Severity())
			require.Equal(t, "INFO", rs[0].SeverityText())
			addr, ok := attr(rs[0], "server.address")
			require.True(t, ok, "the record has no server.address")
			require.Equal(t, ":8443", addr.AsString())

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
		})
	})

	t.Run("a TLS handshake error, as one ERROR record under the caller's scope", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			tel := humustest.NewTelemetry()
			p, status := humustest.Start(t, serves(tlsConfig(t, ca), hello),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			)
			<-h.Listening("localhost:8443")

			r := <-get(h.HTTPClient(), "http://localhost:8443/")
			require.NoError(t, r.err)
			require.Equal(t, http.StatusBadRequest, r.code)
			synctest.Wait()

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())

			var errs []sdklog.Record
			for _, r := range records(tel) {
				if r.Severity() == log.SeverityError {
					errs = append(errs, r)
				}
			}
			require.Len(t, errs, 1)
			require.Equal(t, "ERROR", errs[0].SeverityText())
			require.Contains(t, errs[0].Body().AsString(), "TLS handshake error")
			require.Contains(t, errs[0].Body().AsString(), "client sent an HTTP request to an HTTPS server")
			require.NotContains(t, errs[0].Body().AsString(), "\n")
			require.Empty(t, p.Stderr())
		})
	})
}
