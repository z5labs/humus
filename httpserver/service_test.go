// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

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
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts,
// with certificates from a CA generated for each test. Whole Apps run with
// humustest.Start, and clients come from the Host's HTTPClient.

// scope is the instrumentation scope of the Service's logger.
const scope = "github.com/z5labs/humus/httpserver"

// settingsFor returns DefaultSettings on :8443, over TLS with a certificate
// for localhost from ca.
func settingsFor(t *testing.T, ca *humustest.CA) httpserver.Settings {
	t.Helper()
	settings := httpserver.DefaultSettings()
	settings.Addr = ":8443"
	settings.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
	return settings
}

// serves returns an Init whose only Service serves h on settings.
func serves(settings httpserver.Settings, h http.Handler) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := httpserver.NewService(s, settings, h)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}

// protocols is what a client offers in ALPN.
type protocols struct{ http1, http2 bool }

var (
	offersH2    = protocols{http2: true}
	offersHTTP1 = protocols{http1: true}
	offersBoth  = protocols{http1: true, http2: true}
)

// client returns h's HTTP client, trusting only ca, and offering p.
func client(h *humustest.Host, ca *humustest.CA, p protocols) *http.Client {
	c := h.HTTPClient()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	if !p.http2 {
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	var protos http.Protocols
	protos.SetHTTP1(p.http1)
	protos.SetHTTP2(p.http2)
	tr.Protocols = &protos
	return c
}

// response is what a client got back from a request.
type response struct {
	proto int
	code  int
	body  string
	err   error
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
		got <- response{proto: resp.ProtoMajor, code: resp.StatusCode, body: string(b), err: err}
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

// records returns the records of tel emitted by the Service's logger.
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

func TestService_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		cert := humustest.NewCA(t).Issue(t, "localhost")
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, http.NotFoundHandler())
		require.NoError(t, err)
		return svc
	})
}

func TestService_serves_HTTP2_to_a_client_that_offers_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, status := humustest.Start(t, serves(settingsFor(t, ca), hello),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("localhost:8443")

		r := <-get(client(h, ca, offersH2), "https://localhost:8443/")
		require.NoError(t, r.err)
		require.Equal(t, 2, r.proto)
		require.Equal(t, "hello", r.body)

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
	})
}

func TestService_an_in_flight_request_finishes_during_shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		started := make(chan time.Time, 1)
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			started <- time.Now()
			time.Sleep(3 * time.Second)
			_, _ = io.WriteString(w, "finished")
		})
		p, status := humustest.Start(t, serves(settingsFor(t, ca), handler),
			humustest.WithApp(
				humus.WithHost(h.Host()),
				humus.WithTelemetry(telemetry.NoopBuilder),
				humus.WithDrain(humus.DrainDelay(5*time.Second)),
			),
		)
		<-h.Listening("localhost:8443")

		got := get(client(h, ca, offersH2), "https://localhost:8443/")
		startedAt := <-started
		<-p.Notified(syscall.SIGTERM)
		require.Zero(t, time.Since(startedAt), "SIGTERM is sent while the handler sleeps")
		p.Signal(syscall.SIGTERM)

		r := <-got
		require.NoError(t, r.err)
		require.Equal(t, "finished", r.body)
		require.Equal(t, 3*time.Second, time.Since(startedAt))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
	})
}

func TestService_serves_a_request_during_the_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		p, status := humustest.Start(t, serves(settingsFor(t, ca), hello),
			humustest.WithApp(
				humus.WithHost(h.Host()),
				humus.WithTelemetry(telemetry.NoopBuilder),
				humus.WithDrain(humus.DrainDelay(5*time.Second)),
			),
		)
		<-h.Listening("localhost:8443")
		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)

		time.Sleep(time.Second)
		r := <-get(client(h, ca, offersH2), "https://localhost:8443/")
		require.NoError(t, r.err)
		require.Equal(t, "hello", r.body)

		st := <-status
		require.Equal(t, 5*time.Second, time.Since(sigterm), "the App exits after the drain")
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		requireRefused(t, h, "localhost:8443")
	})
}

func TestService_the_grace_period_closes_what_it_could_not_drain(t *testing.T) {
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
		p, status := humustest.Start(t, serves(settingsFor(t, ca), handler),
			humustest.WithApp(
				humus.WithHost(h.Host()),
				humus.WithTelemetry(telemetry.NoopBuilder),
				humus.WithDrain(humus.DrainDelay(5*time.Second)),
			),
			humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s"}),
		)
		<-h.Listening("localhost:8443")

		got := get(client(h, ca, offersH2), "https://localhost:8443/")
		<-started
		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)

		require.Equal(t, 7*time.Second, (<-cancelled).Sub(sigterm), "the drain's 5s, then the grace period's 2s")
		require.Error(t, (<-got).err, "the request's connection is closed")

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
		require.Equal(t, 2*time.Second, gerr.GracePeriod)
		require.ErrorIs(t, st.Cause(), context.DeadlineExceeded)
	})
}

func TestService_logs(t *testing.T) {
	t.Run("listening, with the address", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			tel := humustest.NewTelemetry()
			p, status := humustest.Start(t, serves(settingsFor(t, ca), hello),
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

	t.Run("a plain-HTTP request, as a handshake error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			tel := humustest.NewTelemetry()
			p, status := humustest.Start(t, serves(settingsFor(t, ca), hello),
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

func TestService_protocols(t *testing.T) {
	t.Run("a client that offers only HTTP/1.1 gets HTTP/1.1 by default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			p, status := humustest.Start(t, serves(settingsFor(t, ca), hello),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening("localhost:8443")

			r := <-get(client(h, ca, offersHTTP1), "https://localhost:8443/")
			require.NoError(t, r.err)
			require.Equal(t, 1, r.proto)
			require.Equal(t, "hello", r.body)

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
		})
	})

	t.Run("HTTP2Only disconnects a client that offers only HTTP/1.1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			tel := humustest.NewTelemetry()
			settings := settingsFor(t, ca)
			settings.HTTP2Only = true
			p, status := humustest.Start(t, serves(settings, hello),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			)
			<-h.Listening("localhost:8443")

			r := <-get(client(h, ca, offersHTTP1), "https://localhost:8443/")
			require.Error(t, r.err, "want the connection closed with no response")
			// The client sees the close when it reads, as io.EOF, or when it
			// writes its request, as a write on a closed pipe.
			require.True(t, errors.Is(r.err, io.EOF) || errors.Is(r.err, io.ErrClosedPipe),
				"err = %v, want the connection closed", r.err)
			require.Zero(t, r.code)

			r = <-get(client(h, ca, offersH2), "https://localhost:8443/")
			require.NoError(t, r.err)
			require.Equal(t, 2, r.proto)
			synctest.Wait()

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
			for _, r := range records(tel) {
				require.NotEqual(t, log.SeverityError, r.Severity(), "logged %q", r.Body().AsString())
			}
			require.Empty(t, p.Stderr())
		})
	})

	t.Run("unencrypted HTTP/2 gets no response", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			tel := humustest.NewTelemetry()
			p, status := humustest.Start(t, serves(settingsFor(t, ca), hello),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			)
			<-h.Listening("localhost:8443")

			c := h.HTTPClient()
			var h2c http.Protocols
			h2c.SetUnencryptedHTTP2(true)
			c.Transport.(*http.Transport).Protocols = &h2c
			r := <-get(c, "http://localhost:8443/")
			require.Error(t, r.err, "want no response")
			require.Zero(t, r.code)
			synctest.Wait()

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
			var bodies []string
			for _, r := range records(tel) {
				if r.Severity() == log.SeverityError {
					bodies = append(bodies, r.Body().AsString())
				}
			}
			require.Len(t, bodies, 1)
			require.Contains(t, bodies[0], "TLS handshake error")
			require.Empty(t, p.Stderr())
		})
	})
}

// negotiate completes a TLS handshake with the Service on address, offering
// protos, and returns the protocol ALPN picked.
func negotiate(t *testing.T, h *humustest.Host, ca *humustest.CA, address string, protos ...string) string {
	t.Helper()
	conn, err := h.Dial(t.Context(), "tcp", address)
	require.NoError(t, err)
	tc := tls.Client(conn, &tls.Config{RootCAs: ca.Pool(), ServerName: "localhost", NextProtos: protos})
	defer func() { _ = tc.Close() }()
	require.NoError(t, tc.HandshakeContext(t.Context()))
	return tc.ConnectionState().NegotiatedProtocol
}

func TestService_ALPN_order(t *testing.T) {
	t.Run("a config that lists only http/1.1 still serves HTTP/2", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.TLS.NextProtos = []string{"http/1.1"}
			p, status := humustest.Start(t, serves(settings, hello),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening("localhost:8443")

			r := <-get(client(h, ca, offersBoth), "https://localhost:8443/")
			require.NoError(t, r.err)
			require.Equal(t, 2, r.proto)

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
		})
	})

	t.Run("another protocol the config lists is offered after h2 and http/1.1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.TLS.NextProtos = []string{"acme-tls/1"}
			p, status := humustest.Start(t, serves(settings, hello),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening("localhost:8443")

			require.Equal(t, "acme-tls/1", negotiate(t, h, ca, "localhost:8443", "acme-tls/1"))
			require.Equal(t, "h2", negotiate(t, h, ca, "localhost:8443", "acme-tls/1", "h2"))
			require.Equal(t, "http/1.1", negotiate(t, h, ca, "localhost:8443", "acme-tls/1", "http/1.1"))
			require.Equal(t, "h2", negotiate(t, h, ca, "localhost:8443", "http/1.1", "h2"))
			synctest.Wait()

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
		})
	})

	t.Run("the end user's config is unchanged", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			settings.TLS.NextProtos = []string{"acme-tls/1", "http/1.1"}
			settings.HTTP2Only = true
			cfg := settings.TLS
			p, status := humustest.Start(t, serves(settings, hello),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening("localhost:8443")

			r := <-get(client(h, ca, offersH2), "https://localhost:8443/")
			require.NoError(t, r.err)
			require.Equal(t, 2, r.proto)

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
			require.Equal(t, []string{"acme-tls/1", "http/1.1"}, cfg.NextProtos)
			require.Empty(t, cfg.CipherSuites)
			require.Zero(t, cfg.MinVersion)
			require.Zero(t, cfg.MaxVersion)
		})
	})
}

// newSetup returns a Setup on h with telemetry off, for a test that drives a
// Service directly rather than through an App.
func newSetup(h *humustest.Host) *humus.Setup {
	return &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}
}

func TestNewService_settings(t *testing.T) {
	t.Run("DefaultSettings alone fails, naming Addr and TLS", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			svc, err := httpserver.NewService(newSetup(h), httpserver.DefaultSettings(), hello)
			require.Nil(t, svc)
			require.ErrorContains(t, err, "Settings.Addr")
			require.ErrorContains(t, err, "Settings.TLS")
			requireRefused(t, h, "localhost:0")
		})
	})

	t.Run("every bad field is named in one error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			settings := settingsFor(t, humustest.NewCA(t))
			settings.ReadTimeout = -time.Second
			settings.MaxHeaderBytes = 0

			_, err := httpserver.NewService(newSetup(h), settings, hello)
			require.ErrorContains(t, err, "Settings.ReadTimeout must not be negative")
			require.ErrorContains(t, err, "Settings.MaxHeaderBytes must be positive")
			require.NotContains(t, err.Error(), "Settings.Addr")
			requireRefused(t, h, "localhost:8443")
		})
	})

	t.Run("DefaultSettings with an address and a TLS config passes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			svc, err := httpserver.NewService(newSetup(h), settingsFor(t, humustest.NewCA(t)), hello)
			require.NoError(t, err)
			require.NotNil(t, svc)
			require.NoError(t, svc.Shutdown(t.Context()))
		})
	})

	t.Run("a nil handler is rejected", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			svc, err := httpserver.NewService(newSetup(h), settingsFor(t, humustest.NewCA(t)), nil)
			require.Nil(t, svc)
			require.ErrorContains(t, err, "handler must not be nil")
			requireRefused(t, h, "localhost:8443")
		})
	})
}

func TestNewService_the_listener(t *testing.T) {
	t.Run("a taken address fails Init, and the first listener is closed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			settings := settingsFor(t, ca)
			init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				first, err := httpserver.NewService(s, settings, hello)
				if err != nil {
					return nil, err
				}
				second, err := httpserver.NewService(s, settings, hello)
				if err != nil {
					return nil, err
				}
				return []humus.Service{first, second}, nil
			}
			_, status := humustest.Start(t, init,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)

			st := <-status
			require.Equal(t, humus.ExitFailure, st.Code())
			require.ErrorIs(t, st.Cause(), syscall.EADDRINUSE)
			requireRefused(t, h, "localhost:8443")
		})
	})

	t.Run("an Init that fails after NewService closes the listener", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			errInit := errors.New("the database is unreachable")
			settings := settingsFor(t, ca)
			init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				if _, err := httpserver.NewService(s, settings, hello); err != nil {
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
}

func TestService_Addr_reports_the_port_the_Host_picked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		settings := settingsFor(t, ca)
		settings.Addr = ":0"
		addrs := make(chan net.Addr, 1)
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := httpserver.NewService(s, settings, hello)
			if err != nil {
				return nil, err
			}
			addrs <- svc.Addr()
			return []humus.Service{svc}, nil
		}
		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)

		_, port, err := net.SplitHostPort((<-addrs).String())
		require.NoError(t, err)
		require.NotEqual(t, "0", port)

		r := <-get(client(h, ca, offersH2), fmt.Sprintf("https://localhost:%s/", port))
		require.NoError(t, r.err)
		require.Equal(t, "hello", r.body)

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-status).Code())
	})
}

func TestService_Shutdown(t *testing.T) {
	t.Run("before Serve, Serve then returns nil at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc, err := httpserver.NewService(newSetup(h), settingsFor(t, humustest.NewCA(t)), hello)
			require.NoError(t, err)

			require.NoError(t, svc.Shutdown(t.Context()))
			start := time.Now()
			require.NoError(t, svc.Serve(t.Context()))
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
			svc, err := httpserver.NewService(newSetup(h), settingsFor(t, ca), handler)
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()

			got := get(client(h, ca, offersH2), "https://localhost:8443/")
			<-started
			done, cancel := context.WithCancel(t.Context())
			cancel()
			start := time.Now()
			err = svc.Shutdown(done)
			require.ErrorIs(t, err, context.Canceled)
			require.Zero(t, time.Since(start))

			require.ErrorIs(t, <-cancelled, context.Canceled)
			require.Error(t, (<-got).err)
			require.NoError(t, <-served)
		})
	})

	t.Run("a second call returns nil", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc, err := httpserver.NewService(newSetup(h), settingsFor(t, humustest.NewCA(t)), hello)
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()
			synctest.Wait()

			done, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, svc.Shutdown(done), context.Canceled)
			require.NoError(t, svc.Shutdown(done))
			require.NoError(t, <-served)
		})
	})
}
