// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts,
// with certificates from a CA generated for each test. Whole Apps run with
// humustest.Start. Probes are requested with the Host's HTTPClient, trusting
// the CA, over HTTP/1.1, as the kubelet speaks.

// The URLs of the probes server's two probes, on :8081.
const (
	livenessURL  = "https://localhost:8081/health/liveness"
	readinessURL = "https://localhost:8081/health/readiness"
)

// probeSettings returns Settings on :8081, over TLS with a certificate for
// localhost from ca.
func probeSettings(t *testing.T, ca *humustest.CA) httphealth.Settings {
	t.Helper()
	return httphealth.Settings{
		Addr: ":8081",
		TLS:  &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}},
	}
}

// probes returns an Init whose only Service is a probes server on settings,
// with opts.
func probes(settings httphealth.Settings, opts ...health.Option) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := httphealth.NewService(s, settings, opts...)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}

// kubelet returns h's HTTP client, trusting only ca, and speaking HTTP/1.1
// alone, as the kubelet does.
func kubelet(h *humustest.Host, ca *humustest.CA) *http.Client {
	c := h.HTTPClient()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{RootCAs: ca.Pool(), NextProtos: []string{"http/1.1"}}
	var protos http.Protocols
	protos.SetHTTP1(true)
	tr.Protocols = &protos
	return c
}

// probe GETs url with c, and returns the status code. It fails t if the
// request fails.
func probe(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}

// hostSetup returns a Setup on h with telemetry off, for a test that drives
// a Service directly rather than through an App.
func hostSetup(h *humustest.Host) *humus.Setup {
	return &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}
}

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

// records returns the records of tel emitted by the package's logger.
func records(tel *humustest.Telemetry) []sdklog.Record {
	var rs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == scope {
			rs = append(rs, r)
		}
	}
	return rs
}

func TestService(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		cert := humustest.NewCA(t).Issue(t, "localhost")
		svc, err := httphealth.NewService(s, httphealth.Settings{
			Addr: ":8081",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		})
		require.NoError(t, err)
		return svc
	})
}

func TestInit_is_not_ready_during_the_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)

		// apiInit returns an API on :8443 and a probes server on :8081, and
		// no health Service: the probes server owns readiness.
		apiInit := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			settings := httpserver.DefaultSettings()
			settings.Addr = ":8443"
			settings.TLS = &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
			api, err := httpserver.NewService(s, settings, http.NotFoundHandler())
			if err != nil {
				return nil, err
			}
			probes, err := httphealth.NewService(s, probeSettings(t, ca))
			if err != nil {
				return nil, err
			}
			return []humus.Service{api, probes}, nil
		}

		p, status := humustest.Start(t, apiInit,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder), httpserver.Defaults()),
		)
		<-h.Listening("localhost:8081")
		synctest.Wait()

		c := kubelet(h, ca)
		require.Equal(t, http.StatusOK, probe(t, c, readinessURL))

		p.Signal(syscall.SIGTERM)
		time.Sleep(time.Second) // inside the 5s drain
		require.Equal(t, http.StatusServiceUnavailable, probe(t, c, readinessURL))
		require.Equal(t, http.StatusOK, probe(t, c, livenessURL))

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
	})
}

func TestService_readiness(t *testing.T) {
	t.Run("with no Options, ready while it serves and not from the moment Serve's context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			p, status := humustest.Start(t, probes(probeSettings(t, ca)),
				humustest.WithApp(
					humus.WithHost(h.Host()),
					humus.WithTelemetry(telemetry.NoopBuilder),
					humus.WithDrain(humus.DrainDelay(5*time.Second)),
				),
			)
			<-h.Listening("localhost:8081")
			synctest.Wait()

			c := kubelet(h, ca)
			require.Equal(t, http.StatusOK, probe(t, c, readinessURL))
			require.Equal(t, http.StatusOK, probe(t, c, livenessURL))

			<-p.Notified(syscall.SIGTERM)
			sigterm := time.Now()
			p.Signal(syscall.SIGTERM)
			synctest.Wait()
			require.Equal(t, http.StatusServiceUnavailable, probe(t, c, readinessURL))
			require.Zero(t, time.Since(sigterm), "readiness turned false the moment shutdown started")
			require.Equal(t, http.StatusOK, probe(t, c, livenessURL))

			time.Sleep(4 * time.Second) // still inside the drain
			require.Equal(t, http.StatusServiceUnavailable, probe(t, c, readinessURL))
			require.Equal(t, http.StatusOK, probe(t, c, livenessURL))

			st := <-status
			require.Zero(t, st.Code(), "cause: %v", st.Cause())
			requireRefused(t, h, "localhost:8081")
		})
	})

	t.Run("an unhealthy monitor added with health.Readiness answers readiness 503", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			p, status := humustest.Start(t, probes(probeSettings(t, ca), health.Readiness(result(false, nil))),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening("localhost:8081")
			synctest.Wait()

			c := kubelet(h, ca)
			require.Equal(t, http.StatusServiceUnavailable, probe(t, c, readinessURL))
			require.Equal(t, http.StatusOK, probe(t, c, livenessURL))

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
		})
	})

	t.Run("an unhealthy monitor added with health.Liveness answers liveness 503", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			p, status := humustest.Start(t, probes(probeSettings(t, ca), health.Liveness(result(false, nil))),
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			<-h.Listening("localhost:8081")
			synctest.Wait()

			c := kubelet(h, ca)
			require.Equal(t, http.StatusServiceUnavailable, probe(t, c, livenessURL))
			require.Equal(t, http.StatusOK, probe(t, c, readinessURL))

			p.Signal(syscall.SIGTERM)
			require.Zero(t, (<-status).Code())
		})
	})
}

func TestService_answers_no_probe_before_Init_returns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		settings := probeSettings(t, ca)
		start := time.Now()
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := httphealth.NewService(s, settings)
			if err != nil {
				return nil, err
			}
			time.Sleep(30 * time.Second) // a slow Init, such as one that runs migrations
			return []humus.Service{svc}, nil
		}
		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("localhost:8081")

		c := kubelet(h, ca)
		c.Timeout = time.Second // the probe's timeoutSeconds
		for time.Since(start) < 29*time.Second {
			resp, err := c.Get(livenessURL)
			if err == nil {
				_ = resp.Body.Close()
				t.Fatalf("a probe %v after the App started was answered %d, before Init returned", time.Since(start), resp.StatusCode)
			}
			require.ErrorIs(t, err, context.DeadlineExceeded)
		}

		time.Sleep(30*time.Second - time.Since(start))
		synctest.Wait() // until Init has returned and the Service serves
		require.Equal(t, http.StatusOK, probe(t, c, livenessURL))
		require.Equal(t, http.StatusOK, probe(t, c, readinessURL))

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-status).Code())
	})
}

func TestService_refuses_plain_text(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, probes(probeSettings(t, ca)),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("localhost:8081")

		// net/http answers a plain-HTTP request to a TLS port with a 400, so
		// the probe fails, and logs the failed handshake.
		code := probe(t, h.HTTPClient(), "http://localhost:8081/health/liveness")
		require.Equal(t, http.StatusBadRequest, code)
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
		require.Empty(t, p.Stderr())
	})
}

func TestService_logs_listening(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, probes(probeSettings(t, ca)),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("localhost:8081")
		synctest.Wait()

		rs := records(tel)
		require.Len(t, rs, 1)
		require.Equal(t, "listening", rs[0].Body().AsString())
		require.Equal(t, log.SeverityInfo, rs[0].Severity())
		require.Equal(t, "INFO", rs[0].SeverityText())
		addr, ok := attr(rs[0], "server.address")
		require.True(t, ok, "the record has no server.address")
		require.Equal(t, ":8081", addr.AsString())

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-status).Code())
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

func TestService_speaks_HTTP1_only(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		settings := probeSettings(t, ca)
		settings.TLS.NextProtos = []string{"h2", "acme-tls/1"}
		cfg := settings.TLS
		p, status := humustest.Start(t, probes(settings),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)
		<-h.Listening("localhost:8081")
		synctest.Wait()

		// A client that offers h2 and http/1.1 gets HTTP/1.1.
		c := h.HTTPClient()
		tr := c.Transport.(*http.Transport)
		tr.TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
		var protos http.Protocols
		protos.SetHTTP1(true)
		protos.SetHTTP2(true)
		tr.Protocols = &protos
		resp, err := c.Get(livenessURL)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, 1, resp.ProtoMajor)

		// ALPN offers http/1.1 and nothing else, whatever the config lists.
		require.Equal(t, "http/1.1", negotiate(t, h, ca, "localhost:8081", "h2", "http/1.1"))
		conn, err := h.Dial(t.Context(), "tcp", "localhost:8081")
		require.NoError(t, err)
		tc := tls.Client(conn, &tls.Config{RootCAs: ca.Pool(), ServerName: "localhost", NextProtos: []string{"h2", "acme-tls/1"}})
		require.ErrorContains(t, tc.HandshakeContext(t.Context()), "no application protocol",
			"a client that offers ALPN protocols, none of them http/1.1, completes no handshake")
		_ = tc.Close()
		synctest.Wait()

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-status).Code())
		require.Equal(t, []string{"h2", "acme-tls/1"}, cfg.NextProtos, "the end user's config is unchanged")
	})
}

func TestNewService_settings(t *testing.T) {
	t.Run("the zero Settings fails, naming Addr and TLS in one error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			svc, err := httphealth.NewService(hostSetup(h), httphealth.Settings{})
			require.Nil(t, svc)
			require.ErrorContains(t, err, "httphealth: NewService")
			require.ErrorContains(t, err, "Settings.Addr")
			require.ErrorContains(t, err, "Settings.TLS")
		})
	})

	t.Run("a nil Option is rejected, with its index", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			svc, err := httphealth.NewService(hostSetup(h), probeSettings(t, humustest.NewCA(t)),
				health.Readiness(healthyMonitor), nil,
			)
			require.Nil(t, svc)
			require.ErrorContains(t, err, "opts[1] must not be nil")
			require.NotContains(t, err.Error(), "opts[0]")
			requireRefused(t, h, "localhost:8081")
		})
	})

	t.Run("every problem is in one error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)

			_, err := httphealth.NewService(hostSetup(h), httphealth.Settings{Addr: ":8081"}, nil)
			require.ErrorContains(t, err, "Settings.TLS must not be nil")
			require.ErrorContains(t, err, "opts[0] must not be nil")
			require.NotContains(t, err.Error(), "Settings.Addr")
			requireRefused(t, h, "localhost:8081")
		})
	})

	t.Run("a config that requires client certificates is rejected, naming TLS", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			settings := probeSettings(t, humustest.NewCA(t))
			settings.TLS.ClientAuth = tls.RequireAndVerifyClientCert

			svc, err := httphealth.NewService(hostSetup(h), settings)
			require.Nil(t, svc)
			require.ErrorContains(t, err, "Settings.TLS")
			require.ErrorContains(t, err, "client certificate")
			require.NotContains(t, err.Error(), "Settings.Addr")
			requireRefused(t, h, "localhost:8081")
		})
	})
}

func TestNewService_the_listener(t *testing.T) {
	t.Run("an Init that fails after NewService exits 1 and closes the listener", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			errInit := errors.New("the brokers are misconfigured")
			settings := probeSettings(t, ca)
			init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				if _, err := httphealth.NewService(s, settings); err != nil {
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
			requireRefused(t, h, "localhost:8081")
		})
	})

	t.Run("a taken address fails NewService, with the Host's error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			l, err := h.Listen("tcp", ":8081")
			require.NoError(t, err)
			defer func() { _ = l.Close() }()

			svc, err := httphealth.NewService(hostSetup(h), probeSettings(t, humustest.NewCA(t)))
			require.Nil(t, svc)
			require.ErrorIs(t, err, syscall.EADDRINUSE)
			require.ErrorContains(t, err, "httphealth: NewService")
		})
	})
}

func TestService_Addr_reports_the_port_the_Host_picked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		settings := probeSettings(t, ca)
		settings.Addr = ":0"
		addrs := make(chan net.Addr, 1)
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc, err := httphealth.NewService(s, settings)
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
		synctest.Wait()

		require.Equal(t, http.StatusOK, probe(t, kubelet(h, ca), fmt.Sprintf("https://localhost:%s/health/readiness", port)))

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-status).Code())
	})
}

func TestService_Shutdown(t *testing.T) {
	t.Run("before Serve, Serve then returns nil at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc, err := httphealth.NewService(hostSetup(h), probeSettings(t, humustest.NewCA(t)))
			require.NoError(t, err)

			require.NoError(t, svc.Shutdown(t.Context()))
			start := time.Now()
			require.NoError(t, svc.Serve(t.Context()))
			require.Zero(t, time.Since(start))
			requireRefused(t, h, "localhost:8081")
		})
	})

	t.Run("with a done context, closes every connection", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			started := make(chan struct{})
			cancelled := make(chan error, 1)
			slow := health.MonitorFunc(func(ctx context.Context) (bool, error) {
				close(started)
				<-ctx.Done()
				cancelled <- ctx.Err()
				return false, ctx.Err()
			})
			svc, err := httphealth.NewService(hostSetup(h), probeSettings(t, ca), health.Liveness(slow))
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()

			got := make(chan error, 1)
			go func() {
				resp, err := kubelet(h, ca).Get(livenessURL)
				if err == nil {
					_ = resp.Body.Close()
				}
				got <- err
			}()
			<-started
			done, cancel := context.WithCancel(t.Context())
			cancel()
			start := time.Now()
			err = svc.Shutdown(done)
			require.ErrorIs(t, err, context.Canceled)
			require.ErrorContains(t, err, "httphealth: shutting down")
			require.Zero(t, time.Since(start))

			require.ErrorIs(t, <-cancelled, context.Canceled)
			require.Error(t, <-got)
			require.NoError(t, <-served)
		})
	})

	t.Run("a second call returns nil", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			svc, err := httphealth.NewService(hostSetup(h), probeSettings(t, humustest.NewCA(t)))
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

func TestService_records_no_span_and_no_metric(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, probes(probeSettings(t, ca), health.Liveness(result(false, errSecret))),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("localhost:8081")
		synctest.Wait()

		c := kubelet(h, ca)
		require.Equal(t, http.StatusOK, probe(t, c, readinessURL))
		require.Equal(t, http.StatusServiceUnavailable, probe(t, c, livenessURL))

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-status).Code())

		require.Empty(t, tel.Spans(), "a probes server records no span")
		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		require.Empty(t, rm.ScopeMetrics, "a probes server records no metric")
		require.Len(t, errorRecords(tel), 1, "the monitor's error is still logged")
	})
}

// This test sets OpenTelemetry's globals, so it doesn't run in parallel,
// and it restores them when it ends.
func TestService_uses_no_OpenTelemetry_globals(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()), sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prop := &countingPropagator{}
	logs := &countingExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logs)))

	//nolint:forbidigo // The test sets OpenTelemetry's globals to recorders of its own, to show the probes server never uses them, as the proposal for exposing health probes, #761, requires.
	{
		prevTP, prevMP, prevProp, prevLP := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator(), otel.GetLoggerProvider()
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetMeterProvider(prevMP)
			otel.SetTextMapPropagator(prevProp)
			otel.SetLoggerProvider(prevLP)
		})
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
		otel.SetLoggerProvider(lp)
	}

	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, probes(probeSettings(t, ca), health.Liveness(result(false, errSecret))),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("localhost:8081")
		synctest.Wait()

		c := kubelet(h, ca)
		require.Equal(t, http.StatusServiceUnavailable, probe(t, c, livenessURL))
		require.Equal(t, http.StatusOK, probe(t, c, readinessURL))
		// A plain-HTTP request, so net/http's error log writes a line too.
		require.Equal(t, http.StatusBadRequest, probe(t, h.HTTPClient(), "http://localhost:8081/health/liveness"))
		synctest.Wait()

		p.Signal(syscall.SIGTERM)
		require.Zero(t, (<-status).Code())
		require.Len(t, errorRecords(tel), 2, "the Setup's LoggerProvider recorded the monitor's error and the handshake's")
	})

	require.Empty(t, spans.Ended(), "the global TracerProvider recorded spans")
	require.Empty(t, spans.Started(), "the global TracerProvider started spans")
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Empty(t, rm.ScopeMetrics, "the global MeterProvider recorded metrics")
	require.Zero(t, prop.calls.Load(), "the global propagator was called")
	require.Zero(t, logs.records.Load(), "the global LoggerProvider recorded records")
}

// pemFile returns c's certificate chain and private key, PEM-encoded in one
// file, as Init's serverTLS reads them.
func pemFile(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var b []byte
	for _, der := range cert.Certificate {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

func TestInit_serves_the_probes_next_to_a_worker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls.pem": pemFile(t, ca.Issue(t, "localhost")),
		}))
		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"PROBES_ADDR": ":8081", "PROBES_TLS_PEM_FILE": "tls.pem"}),
		)
		<-h.Listening("localhost:8081")
		synctest.Wait()

		c := kubelet(h, ca)
		require.Equal(t, http.StatusOK, probe(t, c, readinessURL))
		require.Equal(t, http.StatusOK, probe(t, c, livenessURL))

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})

	t.Run("fails with exit 78 when PROBES_TLS_PEM_FILE is not set", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			_, status := humustest.Start(t, Init,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				humustest.WithEnv(map[string]string{"PROBES_ADDR": ":8081"}),
			)
			st := <-status
			require.Equal(t, 78, int(st.Code()), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "PROBES_TLS_PEM_FILE")
			requireRefused(t, h, "localhost:8081")
		})
	})
}
