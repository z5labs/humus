// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// lines splits out into its lines, each of which must be valid JSON, and
// decodes them.
func lines(t *testing.T, out string) []map[string]any {
	t.Helper()
	if out == "" {
		return nil
	}
	require.True(t, strings.HasSuffix(out, "\n"), "output ends with a newline: %q", out)
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "line: %s", line)
		recs = append(recs, rec)
	}
	return recs
}

func TestNew_logs_carry_the_trace_context_of_the_SDKs_own_spans(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	sdk, err := telemetry.New(t.Context(), telemetry.DefaultSettings(), failingDialer(t), &out)
	require.NoError(t, err)

	ctx, span := sdk.TracerProvider.Tracer("carts").Start(t.Context(), "create cart")
	logger := otelslog.NewLogger("carts", otelslog.WithLoggerProvider(sdk.LoggerProvider))
	logger.InfoContext(ctx, "cart created")
	span.End()
	require.NoError(t, sdk.Shutdown(t.Context()))

	sc := span.SpanContext()
	require.True(t, sc.IsValid(), "the SDK's TracerProvider gives spans real IDs")
	recs := lines(t, out.String())
	require.Len(t, recs, 1, "the log line is the only output: %s", out.String())
	require.Equal(t, "cart created", recs[0]["body"])
	require.Equal(t, sc.TraceID().String(), recs[0]["trace_id"])
	require.Equal(t, sc.SpanID().String(), recs[0]["span_id"])
	require.Equal(t, "carts", recs[0]["scope"])
}

func TestNew_traces_at_none_get_IDs_and_the_samplers_decision_and_export_nothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		sampler sdktrace.Sampler
		sampled bool
	}{
		{"sampled", sdktrace.AlwaysSample(), true},
		{"not sampled", sdktrace.NeverSample(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := telemetry.DefaultSettings()
			s.Sampler = tc.sampler
			var out bytes.Buffer

			sdk, err := telemetry.New(t.Context(), s, failingDialer(t), &out)
			require.NoError(t, err)
			_, span := sdk.TracerProvider.Tracer("carts").Start(t.Context(), "create cart")
			span.End()
			require.NoError(t, sdk.Shutdown(t.Context()))

			require.True(t, span.SpanContext().IsValid())
			require.Equal(t, tc.sampled, span.SpanContext().IsSampled())
			require.Empty(t, out.String(), "nothing is exported")
		})
	}
}

func TestNew_LogLevel(t *testing.T) {
	t.Parallel()

	s := telemetry.DefaultSettings()
	s.LogLevel = log.SeverityWarn
	var out bytes.Buffer
	sdk, err := telemetry.New(t.Context(), s, failingDialer(t), &out)
	require.NoError(t, err)

	logger := sdk.LoggerProvider.Logger("carts")
	enabled := func(sev log.Severity) bool {
		return logger.Enabled(t.Context(), log.EnabledParameters{Severity: sev})
	}
	require.False(t, enabled(log.SeverityTrace1))
	require.False(t, enabled(log.SeverityInfo4))
	require.True(t, enabled(log.SeverityWarn))
	require.True(t, enabled(log.SeverityFatal4))
	require.True(t, enabled(log.SeverityUndefined), "a severity that is not set is not below the level")

	slogger := otelslog.NewLogger("carts", otelslog.WithLoggerProvider(sdk.LoggerProvider))
	require.False(t, slogger.Handler().Enabled(t.Context(), -4), "slog's DEBUG")
	slogger.InfoContext(t.Context(), "filtered")

	emit := func(sev log.Severity, body string) {
		var r log.Record
		r.SetSeverity(sev)
		r.SetBody(attribute.StringValue(body))
		logger.Emit(t.Context(), r)
	}
	emit(log.SeverityInfo, "filtered too")
	emit(log.SeverityWarn, "kept")
	emit(log.SeverityUndefined, "unset")
	require.NoError(t, sdk.Shutdown(t.Context()))

	var bodies []any
	for _, rec := range lines(t, out.String()) {
		bodies = append(bodies, rec["body"])
	}
	require.Equal(t, []any{"kept", "unset"}, bodies)
}

// consoleResource returns the resource attributes of the spans New's
// console exporter writes to out, as key=value strings.
func consoleResource(t *testing.T, out string) [][]string {
	t.Helper()
	var resources [][]string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var span struct {
			Resource []struct {
				Key   string
				Value struct{ Value any }
			}
		}
		require.NoError(t, json.Unmarshal([]byte(line), &span), "line: %s", line)
		var attrs []string
		for _, kv := range span.Resource {
			attrs = append(attrs, kv.Key+"="+jsonString(kv.Value.Value))
		}
		resources = append(resources, attrs)
	}
	return resources
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestNew_resource(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts []resource.Option
		want []string
	}{
		{
			name: "no options run no detector",
			want: []string{"service.name=carts"},
		},
		{
			name: "ServiceName wins over a resource option",
			opts: []resource.Option{resource.WithAttributes(
				attribute.String("service.name", "detected"),
				attribute.String("deployment.environment.name", "staging"),
			)},
			want: []string{"deployment.environment.name=staging", "service.name=carts"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := telemetry.DefaultSettings()
			s.ServiceName = "carts"
			s.ResourceOptions = tc.opts
			s.Traces = telemetry.ExporterConsole
			s.Logs = telemetry.ExporterNone
			var out bytes.Buffer

			sdk, err := telemetry.New(t.Context(), s, failingDialer(t), &out)
			require.NoError(t, err)
			_, span := sdk.TracerProvider.Tracer("carts").Start(t.Context(), "create cart")
			span.End()
			require.NoError(t, sdk.Shutdown(t.Context()))

			require.Equal(t, [][]string{tc.want}, consoleResource(t, out.String()))
		})
	}
}

type failingDetector struct{}

func (failingDetector) Detect(context.Context) (*resource.Resource, error) {
	return nil, errors.New("metadata server unreachable")
}

func TestNew_fails_when_a_resource_option_fails(t *testing.T) {
	t.Parallel()

	s := telemetry.DefaultSettings()
	s.ResourceOptions = []resource.Option{resource.WithDetectors(failingDetector{})}

	_, err := telemetry.New(t.Context(), s, failingDialer(t), &bytes.Buffer{})
	require.ErrorContains(t, err, "metadata server unreachable")
}

func TestNew_console_metrics(t *testing.T) {
	t.Parallel()

	s := telemetry.DefaultSettings()
	s.ServiceName = "carts"
	s.Metrics = telemetry.ExporterConsole
	s.Logs = telemetry.ExporterNone
	var out bytes.Buffer

	sdk, err := telemetry.New(t.Context(), s, failingDialer(t), &out)
	require.NoError(t, err)
	counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
	require.NoError(t, err)
	counter.Add(t.Context(), 3)
	require.NoError(t, sdk.Shutdown(t.Context()))

	var rm struct {
		Resource     []struct{ Key string }
		ScopeMetrics []struct {
			Metrics []struct{ Name string }
		}
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &rm), "output: %s", out.String())
	require.Len(t, rm.Resource, 1)
	require.Equal(t, "service.name", rm.Resource[0].Key)
	require.Len(t, rm.ScopeMetrics, 1)
	require.Equal(t, "carts.created", rm.ScopeMetrics[0].Metrics[0].Name)
}

// exclusiveWriter fails the test if two Writes overlap.
type exclusiveWriter struct {
	t    *testing.T
	busy atomic.Bool
	buf  bytes.Buffer
}

func (w *exclusiveWriter) Write(p []byte) (int, error) {
	if !w.busy.CompareAndSwap(false, true) {
		w.t.Error("two writes to stdout overlap")
		return len(p), nil
	}
	defer w.busy.Store(false)
	runtime.Gosched()
	return w.buf.Write(p)
}

func TestNew_console_exporters_serialize_their_writes(t *testing.T) {
	t.Parallel()

	s := telemetry.DefaultSettings()
	s.Traces = telemetry.ExporterConsole
	s.Metrics = telemetry.ExporterConsole
	s.Logs = telemetry.ExporterConsole
	out := &exclusiveWriter{t: t}

	sdk, err := telemetry.New(t.Context(), s, failingDialer(t), out)
	require.NoError(t, err)
	logger := otelslog.NewLogger("carts", otelslog.WithLoggerProvider(sdk.LoggerProvider))
	tracer := sdk.TracerProvider.Tracer("carts")

	const goroutines, each = 8, 50
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range each {
				ctx, span := tracer.Start(t.Context(), "op")
				logger.InfoContext(ctx, "in a span")
				span.End()
			}
		})
	}
	wg.Wait()
	require.NoError(t, sdk.Shutdown(t.Context()))

	// Every span and every record is a line of its own, and the metrics
	// export at shutdown is one more.
	require.Len(t, strings.Split(strings.TrimSuffix(out.buf.String(), "\n"), "\n"), 2*goroutines*each+1)
}

func TestNew_OTLP(t *testing.T) {
	t.Parallel()

	for _, protocol := range []telemetry.Protocol{telemetry.ProtocolGRPC, telemetry.ProtocolHTTPProtobuf} {
		for _, secure := range []bool{false, true} {
			name := string(protocol) + "/plaintext"
			if secure {
				name = string(protocol) + "/tls"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				testOTLP(t, protocol, secure)
			})
		}
	}
}

func testOTLP(t *testing.T, protocol telemetry.Protocol, secure bool) {
	host := humustest.NewHost(t)
	ln := listen(t, host, "otlp:4317")
	s := telemetry.DefaultSettings()
	s.ServiceName = "carts"
	s.Traces = telemetry.ExporterOTLP
	s.Metrics = telemetry.ExporterOTLP
	s.Logs = telemetry.ExporterOTLP
	s.OTLP.Endpoint = "otlp:4317"
	s.OTLP.Protocol = protocol
	tenant := rand.Text()
	s.OTLP.Headers = map[string]string{"x-tenant": tenant}
	if secure {
		ca := humustest.NewCA(t)
		alpn := "http/1.1"
		if protocol == telemetry.ProtocolGRPC {
			alpn = "h2"
		}
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{ca.Issue(t, "otlp").TLS()},
			NextProtos:   []string{alpn},
		})
		s.OTLP.TLS = &tls.Config{RootCAs: ca.Pool()}
	}
	rcv := startReceiver(t, ln, protocol)
	dialer := newCountingDialer(host)

	sdk, err := telemetry.New(t.Context(), s, dialer.Dial, nil)
	require.NoError(t, err)
	_, span := sdk.TracerProvider.Tracer("carts").Start(t.Context(), "create cart")
	span.End()
	counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
	require.NoError(t, err)
	counter.Add(t.Context(), 1)
	var r log.Record
	r.SetSeverity(log.SeverityInfo)
	r.SetBody(attribute.StringValue("cart created"))
	sdk.LoggerProvider.Logger("carts").Emit(t.Context(), r)
	require.NoError(t, sdk.Shutdown(t.Context()))

	spans, metrics, logs, requests := rcv.snapshot()
	require.Len(t, spans, 1)
	require.Equal(t, "create cart", spans[0].GetScopeSpans()[0].GetSpans()[0].GetName())
	require.Equal(t, map[string]string{"service.name": "carts"}, resourceAttributes(spans[0].GetResource()))
	require.Len(t, metrics, 1)
	require.Equal(t, "carts.created", metrics[0].GetScopeMetrics()[0].GetMetrics()[0].GetName())
	require.Equal(t, map[string]string{"service.name": "carts"}, resourceAttributes(metrics[0].GetResource()))
	require.Len(t, logs, 1)
	require.Equal(t, "cart created", logs[0].GetScopeLogs()[0].GetLogRecords()[0].GetBody().GetStringValue())
	require.Equal(t, map[string]string{"service.name": "carts"}, resourceAttributes(logs[0].GetResource()))

	var signals []string
	for _, req := range requests {
		signals = append(signals, req.signal)
		require.Equal(t, []string{tenant}, req.headers["x-tenant"], "the %s request carries the configured header", req.signal)
		require.Empty(t, req.encoding, "the %s request is not compressed", req.signal)
	}
	require.ElementsMatch(t, []string{"traces", "metrics", "logs"}, signals)

	calls := dialer.Calls()
	require.NotEmpty(t, calls, "the exporters dial through dial")
	for _, addr := range calls {
		require.Equal(t, "otlp:4317", addr)
	}
}

func TestNew_Disabled_returns_Noop_and_dials_nothing(t *testing.T) {
	t.Parallel()

	// Every other field is out of range, and is ignored.
	sdk, err := telemetry.New(t.Context(), telemetry.Settings{Disabled: true, Traces: telemetry.ExporterOTLP}, failingDialer(t), nil)
	require.NoError(t, err)
	requireNoop(t, sdk)
}

func TestNoop(t *testing.T) {
	t.Parallel()
	requireNoop(t, telemetry.Noop())
}

func TestNoopBuilder(t *testing.T) {
	t.Parallel()

	sdk, err := telemetry.NoopBuilder(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	requireNoop(t, sdk)
}

func requireNoop(t *testing.T, sdk telemetry.SDK) {
	t.Helper()
	require.IsType(t, tracenoop.TracerProvider{}, sdk.TracerProvider)
	require.IsType(t, metricnoop.MeterProvider{}, sdk.MeterProvider)
	require.IsType(t, lognoop.LoggerProvider{}, sdk.LoggerProvider)
	require.ElementsMatch(t, []string{"traceparent", "tracestate", "baggage"}, sdk.Propagator.Fields())
	require.NoError(t, sdk.Shutdown(t.Context()))
}

func TestNew_needs_dial_for_OTLP_and_stdout_for_the_console(t *testing.T) {
	t.Parallel()

	s := telemetry.DefaultSettings()
	s.Logs = telemetry.ExporterOTLP
	_, err := telemetry.New(t.Context(), s, nil, &bytes.Buffer{})
	require.ErrorContains(t, err, "dial is nil")

	_, err = telemetry.New(t.Context(), telemetry.DefaultSettings(), failingDialer(t), nil)
	require.ErrorContains(t, err, "stdout is nil")
}

func TestNew_Shutdown_returns_within_ShutdownTimeout_when_the_receiver_never_answers(t *testing.T) {
	t.Parallel()

	for _, protocol := range []telemetry.Protocol{telemetry.ProtocolGRPC, telemetry.ProtocolHTTPProtobuf} {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				host := humustest.NewHost(t)
				ln := listen(t, host, "otlp:4317")
				var mu sync.Mutex
				var held []net.Conn
				go func() {
					for {
						conn, err := ln.Accept()
						if err != nil {
							return
						}
						// Never read from, never written to.
						mu.Lock()
						held = append(held, conn)
						mu.Unlock()
					}
				}()

				s := telemetry.DefaultSettings()
				s.Traces = telemetry.ExporterOTLP
				s.Metrics = telemetry.ExporterOTLP
				s.Logs = telemetry.ExporterOTLP
				s.OTLP.Endpoint = "otlp:4317"
				s.OTLP.Protocol = protocol
				s.ShutdownTimeout = 2 * time.Second
				sdk, err := telemetry.New(t.Context(), s, host.Dial, nil)
				require.NoError(t, err)

				_, span := sdk.TracerProvider.Tracer("carts").Start(t.Context(), "create cart")
				span.End()
				counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
				require.NoError(t, err)
				counter.Add(t.Context(), 1)
				var r log.Record
				r.SetSeverity(log.SeverityInfo)
				r.SetBody(attribute.StringValue("cart created"))
				sdk.LoggerProvider.Logger("carts").Emit(t.Context(), r)

				start := time.Now()
				err = sdk.Shutdown(context.Background())
				elapsed := time.Since(start)
				require.Error(t, err)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.LessOrEqual(t, elapsed, s.ShutdownTimeout)

				// Let the exports still in flight fail, so the bubble's
				// goroutines can end.
				require.NoError(t, ln.Close())
				mu.Lock()
				for _, conn := range held {
					_ = conn.Close()
				}
				mu.Unlock()
				// Shutdown returns at its deadline, but an export already
				// in flight runs on until OTLP.Timeout, retrying. Every
				// goroutine of the bubble must end before it does.
				time.Sleep(s.OTLP.Timeout)
			})
		})
	}
}

// neverAnswer accepts every connection to ln and never reads from or
// writes to it, until the returned function closes them all.
func neverAnswer(ln net.Listener) (closeAll func()) {
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	return func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	}
}

func TestNew_OTLP_Timeout_bounds_each_export(t *testing.T) {
	t.Parallel()

	for _, protocol := range []telemetry.Protocol{telemetry.ProtocolGRPC, telemetry.ProtocolHTTPProtobuf} {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				host := humustest.NewHost(t)
				closeAll := neverAnswer(listen(t, host, "otlp:4317"))
				defer closeAll()

				s := telemetry.DefaultSettings()
				s.Traces = telemetry.ExporterOTLP
				s.Metrics = telemetry.ExporterOTLP
				s.Logs = telemetry.ExporterOTLP
				s.OTLP.Endpoint = "otlp:4317"
				s.OTLP.Protocol = protocol
				s.OTLP.Timeout = time.Second
				s.ShutdownTimeout = time.Minute
				sdk, err := telemetry.New(t.Context(), s, host.Dial, nil)
				require.NoError(t, err)

				_, span := sdk.TracerProvider.Tracer("carts").Start(t.Context(), "create cart")
				span.End()
				counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
				require.NoError(t, err)
				counter.Add(t.Context(), 1)
				var r log.Record
				r.SetBody(attribute.StringValue("cart created"))
				sdk.LoggerProvider.Logger("carts").Emit(t.Context(), r)

				// Shutdown exports what is pending; each export gives up
				// after OTLP.Timeout, long before ShutdownTimeout.
				start := time.Now()
				_ = sdk.Shutdown(context.Background())
				require.LessOrEqual(t, time.Since(start), s.OTLP.Timeout)
			})
		})
	}
}

// blockingWriter blocks every Write until release is closed.
type blockingWriter struct {
	writing chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	<-w.release
	return len(p), nil
}

func TestNew_Shutdown_returns_within_ShutdownTimeout_when_stdout_blocks(t *testing.T) {
	t.Parallel()

	// Not in a synctest bubble: the span processor's Shutdown waits on a
	// mutex the blocked export holds, and a goroutine waiting on a mutex
	// is not durably blocked, so the bubble's clock would never reach the
	// deadline.
	out := &blockingWriter{writing: make(chan struct{}), release: make(chan struct{})}
	defer close(out.release)

	s := telemetry.DefaultSettings()
	s.Traces = telemetry.ExporterConsole
	s.ShutdownTimeout = 100 * time.Millisecond
	sdk, err := telemetry.New(t.Context(), s, failingDialer(t), out)
	require.NoError(t, err)

	go func() {
		_, span := sdk.TracerProvider.Tracer("carts").Start(context.Background(), "create cart")
		span.End()
	}()
	<-out.writing

	start := time.Now()
	err = sdk.Shutdown(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 5*time.Second, "Shutdown returns about ShutdownTimeout after it was called")
}
