// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpservertest_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this package run inside synctest bubbles, on humustest Hosts,
// with certificates from a CA generated for each test.

// serves returns an Init whose only Service serves h on DefaultSettings on
// :8443, over TLS with cert.
func serves(cert *humustest.Certificate, h http.Handler) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return servesWith(cert, h, func(*httpserver.Settings) {})
}

// servesWith is serves, with the Settings changed by edit.
func servesWith(cert *humustest.Certificate, h http.Handler, edit func(*httpserver.Settings)) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		edit(&settings)
		svc, err := httpserver.NewService(s, settings, h)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
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

func TestMain_serves_HTTP2_and_records_the_span(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /hello/{name}", greet)

		p, status := httpservertest.Start(t, serves(ca.Issue(t, "localhost"), mux),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
		)
		<-h.Listening("localhost:8443")

		resp, err := httpservertest.NewClient(h, ca.Pool()).Get("https://localhost:8443/hello/humus")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, 2, resp.ProtoMajor)
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

func TestInit_without_an_address_or_a_certificate_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := httpservertest.Start(t, Init)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config ADDR: not found")
		require.ErrorContains(t, st.Cause(), "config TLS_PEM_FILE: not found")
	})
}

func TestInit_serves_with_a_certificate_from_a_PKCS12_file(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		bundle, password := ca.Issue(t, "localhost").PKCS12(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{"tls.p12": bundle}))

		p, status := httpservertest.Start(t, InitPKCS12,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{
				"ADDR":                ":8443",
				"TLS_PKCS12_FILE":     "tls.p12",
				"TLS_PKCS12_PASSWORD": password,
			}),
		)
		<-h.Listening("localhost:8443")

		r := <-get(httpservertest.NewClient(h, ca.Pool()), "https://localhost:8443/hello/humus")
		require.NoError(t, r.err)
		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, 2, r.proto)
		require.Equal(t, "hello, humus", r.body)

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})

	t.Run("fails with exit 78 when the password is wrong", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			bundle, password := ca.Issue(t, "localhost").PKCS12(t)
			h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{"tls.p12": bundle}))

			_, status := httpservertest.Start(t, InitPKCS12,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				humustest.WithEnv(map[string]string{
					"ADDR":                ":8443",
					"TLS_PKCS12_FILE":     "tls.p12",
					"TLS_PKCS12_PASSWORD": password + "x",
				}),
			)
			st := <-status
			require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "config TLS_PKCS12_PASSWORD:")
		})
	})
}

func TestStart_applies_the_defaults_before_the_test_options(t *testing.T) {
	cases := []struct {
		name  string
		opts  []humustest.Option
		want  time.Duration
		grace time.Duration
	}{
		{
			name:  "cancel a stuck request after the 5s drain and the 10s grace period",
			want:  15 * time.Second,
			grace: 10 * time.Second,
		},
		{
			name:  "keep the drain under a grace period given with WithApp",
			opts:  []humustest.Option{humustest.WithApp(humus.WithShutdownGracePeriod(2 * time.Second))},
			want:  7 * time.Second,
			grace: 2 * time.Second,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
				// No WriteTimeout, which would otherwise cancel the request
				// 10s after it arrived: only the shutdown cancels it.
				init := servesWith(ca.Issue(t, "localhost"), handler, func(s *httpserver.Settings) { s.WriteTimeout = 0 })
				opts := append([]humustest.Option{
					humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				}, c.opts...)
				p, status := httpservertest.Start(t, init, opts...)
				<-h.Listening("localhost:8443")

				got := get(httpservertest.NewClient(h, ca.Pool()), "https://localhost:8443/")
				<-started
				<-p.Notified(syscall.SIGTERM)
				sigterm := time.Now()
				p.Signal(syscall.SIGTERM)

				require.Equal(t, c.want, (<-cancelled).Sub(sigterm), "the request's context is cancelled after the drain and the grace period")
				require.Error(t, (<-got).err, "the request's connection is closed")

				st := <-status
				require.Equal(t, humus.ExitFailure, st.Code())
				gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
				require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
				require.Equal(t, c.grace, gerr.GracePeriod)
			})
		})
	}
}
