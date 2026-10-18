// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles. Most serve the handler
// on its own, over plain HTTP/1.1, with a net/http server of the test's on a
// humustest Host, so a request's bytes on the wire can be read. The test of
// the attached handler serves it on an httpserver.Service.

// scope is the instrumentation scope of the handler's logger.
const scope = "github.com/z5labs/humus/httphealth"

// errSecret is what a failing monitor returns. It names a host and a user,
// which a probe's body must never hold.
var errSecret = errors.New("dial tcp db.internal:5432: password authentication failed for user orders")

// result returns a monitor that always returns healthy and err.
func result(healthy bool, err error) health.Monitor {
	return health.MonitorFunc(func(context.Context) (bool, error) {
		return healthy, err
	})
}

// healthyMonitor is a monitor that is always healthy.
var healthyMonitor = result(true, nil)

// newSetup returns a Setup whose telemetry is sdk.
func newSetup(sdk telemetry.SDK) *humus.Setup {
	return &humus.Setup{Telemetry: sdk}
}

// sdkOf returns the SDK tel records.
func sdkOf(t *testing.T, tel *humustest.Telemetry) telemetry.SDK {
	t.Helper()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	return sdk
}

// serve serves h over plain HTTP/1.1 on localhost:8080 of a new humustest
// Host, until the test ends, and returns the Host.
func serve(t *testing.T, h http.Handler) *humustest.Host {
	t.Helper()
	host := humustest.NewHost(t)
	l, err := host.Listen("tcp", "localhost:8080")
	require.NoError(t, err)
	srv := &http.Server{Handler: h}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		require.ErrorIs(t, <-served, http.ErrServerClosed)
	})
	return host
}

// wire is a response as it was sent: its status line and headers parsed,
// and every byte after the headers.
type wire struct {
	code   int
	header http.Header
	body   string
}

// send sends method and path to host's localhost:8080 on a connection of its
// own, asking the server to close it, and returns every byte the server
// sent, parsed into a wire.
func send(t *testing.T, host *humustest.Host, method, path string) wire {
	t.Helper()
	conn, err := host.Dial(t.Context(), "tcp", "localhost:8080")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, err = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: localhost:8080\r\nConnection: close\r\n\r\n", method, path)
	require.NoError(t, err)
	b, err := io.ReadAll(conn)
	require.NoError(t, err)

	head, body, ok := strings.Cut(string(b), "\r\n\r\n")
	require.True(t, ok, "no end of the headers in %q", b)
	// Parsed as the answer to a HEAD, so ReadResponse reads no body: the
	// body is whatever followed the headers on the wire.
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(head+"\r\n\r\n")), &http.Request{Method: http.MethodHead})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return wire{code: resp.StatusCode, header: resp.Header, body: body}
}

// errorRecords returns the ERROR records of tel emitted by the handler's
// logger.
func errorRecords(tel *humustest.Telemetry) []sdklog.Record {
	var rs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == scope && r.Severity() == log.SeverityError {
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

func TestNewHandler_answers_each_probe(t *testing.T) {
	monitors := []struct {
		name    string
		monitor health.Monitor
		code    int
		body    string
	}{
		{"a healthy monitor", healthyMonitor, http.StatusOK, "OK\n"},
		{"an unhealthy monitor", result(false, nil), http.StatusServiceUnavailable, "Service Unavailable\n"},
		{"a monitor that returns an error", result(false, errSecret), http.StatusServiceUnavailable, "Service Unavailable\n"},
		{"a healthy monitor that returns an error", result(true, errSecret), http.StatusServiceUnavailable, "Service Unavailable\n"},
	}
	probes := []struct {
		name  string
		path  string
		build func(m health.Monitor) health.Probes
	}{
		{"liveness", "/health/liveness", func(m health.Monitor) health.Probes {
			return health.Probes{Liveness: m, Readiness: healthyMonitor}
		}},
		{"readiness", "/health/readiness", func(m health.Monitor) health.Probes {
			return health.Probes{Liveness: healthyMonitor, Readiness: m}
		}},
	}

	for _, p := range probes {
		for _, m := range monitors {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(fmt.Sprintf("%s %s, %s", method, p.path, m.name), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						host := serve(t, httphealth.NewHandler(newSetup(telemetry.Noop()), p.build(m.monitor)))

						got := send(t, host, method, p.path)
						require.Equal(t, m.code, got.code)
						require.Equal(t, "text/plain; charset=utf-8", got.header.Get("Content-Type"))
						require.Equal(t, "no-store", got.header.Get("Cache-Control"))
						require.Equal(t, fmt.Sprint(len(m.body)), got.header.Get("Content-Length"))
						if method == http.MethodHead {
							require.Empty(t, got.body, "a HEAD got a body")
						} else {
							require.Equal(t, m.body, got.body)
						}
						require.NotContains(t, got.body, "db.internal")
						require.NotContains(t, got.body, "orders")
					})
				})
			}
		}
	}
}

func TestNewHandler_answers_only_its_two_paths(t *testing.T) {
	ok := health.Probes{Liveness: healthyMonitor, Readiness: healthyMonitor}

	for _, tc := range []struct {
		method, path string
		code         int
		allow        string
	}{
		{http.MethodPost, "/health/readiness", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodDelete, "/health/liveness", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, "/health/startup", http.StatusNotFound, ""},
		{http.MethodGet, "/health/", http.StatusNotFound, ""},
		{http.MethodGet, "/health/liveness/", http.StatusNotFound, ""},
		{http.MethodGet, "/livez", http.StatusNotFound, ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				host := serve(t, httphealth.NewHandler(newSetup(telemetry.Noop()), ok))

				got := send(t, host, tc.method, tc.path)
				require.Equal(t, tc.code, got.code)
				require.Equal(t, tc.allow, got.header.Get("Allow"))
				require.Equal(t, "no-store", got.header.Get("Cache-Control"))
			})
		})
	}
}

func TestNewHandler_logs_a_monitors_error(t *testing.T) {
	for _, tc := range []struct {
		probe, path string
		build       func(m health.Monitor) health.Probes
	}{
		{"liveness", "/health/liveness", func(m health.Monitor) health.Probes {
			return health.Probes{Liveness: m, Readiness: healthyMonitor}
		}},
		{"readiness", "/health/readiness", func(m health.Monitor) health.Probes {
			return health.Probes{Liveness: healthyMonitor, Readiness: m}
		}},
	} {
		t.Run("as one ERROR record, on "+tc.probe, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := humustest.NewTelemetry()
				host := serve(t, httphealth.NewHandler(newSetup(sdkOf(t, tel)), tc.build(result(false, errSecret))))

				got := send(t, host, http.MethodGet, tc.path)
				require.Equal(t, http.StatusServiceUnavailable, got.code)

				rs := errorRecords(tel)
				require.Len(t, rs, 1)
				require.Equal(t, errSecret.Error(), rs[0].Body().AsString())
				require.Equal(t, "ERROR", rs[0].SeverityText())
				probe, ok := attr(rs[0], "humus.health.probe")
				require.True(t, ok, "the record has no humus.health.probe")
				require.Equal(t, tc.probe, probe.AsString())
				_, ok = attr(rs[0], "health.probe")
				require.False(t, ok, "the record has an unprefixed health.probe")
				require.Len(t, tel.Logs(), 1, "the handler logged more than the error")
			})
		})

		t.Run("and nothing for an unhealthy monitor without one, on "+tc.probe, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := humustest.NewTelemetry()
				host := serve(t, httphealth.NewHandler(newSetup(sdkOf(t, tel)), tc.build(result(false, nil))))

				require.Equal(t, http.StatusServiceUnavailable, send(t, host, http.MethodGet, tc.path).code)
				require.Equal(t, http.StatusOK, send(t, host, http.MethodGet, "/health/"+otherProbe(tc.probe)).code)
				require.Empty(t, tel.Logs())
			})
		})
	}
}

// otherProbe returns the name of the probe that is not name.
func otherProbe(name string) string {
	if name == "liveness" {
		return "readiness"
	}
	return "liveness"
}

func TestNewHandler_checks_once_per_request(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var liveness, readiness int
		p := health.Probes{
			Liveness:  health.MonitorFunc(func(context.Context) (bool, error) { liveness++; return true, nil }),
			Readiness: health.MonitorFunc(func(context.Context) (bool, error) { readiness++; return false, nil }),
		}
		host := serve(t, httphealth.NewHandler(newSetup(telemetry.Noop()), p))

		for i := range 3 {
			require.Equal(t, http.StatusOK, send(t, host, http.MethodGet, "/health/liveness").code)
			require.Equal(t, http.StatusServiceUnavailable, send(t, host, http.MethodHead, "/health/readiness").code)
			require.Equal(t, i+1, liveness, "liveness checks")
			require.Equal(t, i+1, readiness, "readiness checks")
		}
		require.Equal(t, http.StatusNotFound, send(t, host, http.MethodGet, "/health/").code)
		require.Equal(t, http.StatusMethodNotAllowed, send(t, host, http.MethodPost, "/health/liveness").code)
		require.Equal(t, 3, liveness, "a 404 or a 405 checked liveness")
		require.Equal(t, 3, readiness, "a 404 or a 405 checked readiness")
	})
}

func TestNewHandler_checks_with_the_requests_context(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		returned := make(chan error, 1)
		blocked := health.MonitorFunc(func(ctx context.Context) (bool, error) {
			close(entered)
			<-ctx.Done()
			returned <- ctx.Err()
			return false, ctx.Err()
		})
		host := serve(t, httphealth.NewHandler(newSetup(telemetry.Noop()), health.Probes{Liveness: healthyMonitor, Readiness: blocked}))

		ctx, cancel := context.WithCancel(t.Context())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:8080/health/readiness", nil)
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() {
			resp, err := host.HTTPClient().Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()

		<-entered
		synctest.Wait()
		select {
		case err := <-returned:
			t.Fatalf("the monitor returned %v before the client gave up", err)
		default:
		}

		cancel() // the prober's timeout
		require.ErrorIs(t, <-done, context.Canceled)
		require.ErrorIs(t, <-returned, context.Canceled)
	})
}

func TestNewHandler_recovers_no_panic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		panicking := health.MonitorFunc(func(context.Context) (bool, error) { panic(errSecret) })
		h := httphealth.NewHandler(newSetup(telemetry.Noop()), health.Probes{Liveness: panicking, Readiness: healthyMonitor})

		require.PanicsWithValue(t, errSecret, func() {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health/liveness", nil))
		})
	})
}

func TestNewHandler_panics_on_a_nil_monitor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSetup(telemetry.Noop())

		require.PanicsWithValue(t, "httphealth: NewHandler called with a nil Probes.Liveness", func() {
			httphealth.NewHandler(s, health.Probes{Readiness: healthyMonitor})
		})
		require.PanicsWithValue(t, "httphealth: NewHandler called with a nil Probes.Readiness", func() {
			httphealth.NewHandler(s, health.Probes{Liveness: healthyMonitor})
		})
		require.PanicsWithValue(t, "httphealth: NewHandler called with a nil Probes.Liveness", func() {
			httphealth.NewHandler(s, health.Probes{})
		})
	})
}
