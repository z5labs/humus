// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// batchExport is what a batchReceiver records of one span or log export.
type batchExport struct {
	signal string
	// items is how many spans or log records the export carried.
	items int
	start time.Time
	// end is when the receiver answered or saw the export cancelled.
	end time.Time
	// cancelled reports whether the exporter gave up before the receiver
	// answered.
	cancelled bool
}

// batchReceiver is an in-process OTLP traces and logs receiver that answers
// each export after answerAfter, or never when answerAfter is negative, and
// records each one when it ends.
type batchReceiver struct {
	answerAfter time.Duration

	mu      sync.Mutex
	exports []batchExport
}

// startBatchReceiver serves a batchReceiver over protocol on ln until the
// test ends.
func startBatchReceiver(t *testing.T, ln net.Listener, protocol telemetry.Protocol, answerAfter time.Duration) *batchReceiver {
	t.Helper()
	r := &batchReceiver{answerAfter: answerAfter}
	switch protocol {
	case telemetry.ProtocolGRPC:
		srv := grpc.NewServer()
		coltracepb.RegisterTraceServiceServer(srv, batchTraceService{r: r})
		collogspb.RegisterLogsServiceServer(srv, batchLogsService{r: r})
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(srv.Stop)
	case telemetry.ProtocolHTTPProtobuf:
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/traces", r.handle("traces", &coltracepb.ExportTraceServiceRequest{}, &coltracepb.ExportTraceServiceResponse{}))
		mux.HandleFunc("POST /v1/logs", r.handle("logs", &collogspb.ExportLogsServiceRequest{}, &collogspb.ExportLogsServiceResponse{}))
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
	default:
		t.Fatalf("unknown protocol %q", protocol)
	}
	return r
}

func (r *batchReceiver) handle(signal string, msg, resp proto.Message) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m := proto.Clone(msg)
		proto.Reset(m)
		if err := proto.Unmarshal(body, m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !r.wait(req.Context(), signal, countItems(m)) {
			return
		}
		b, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(b)
	}
}

// countItems counts the spans or log records of an export request.
func countItems(msg proto.Message) int {
	var n int
	switch m := msg.(type) {
	case *coltracepb.ExportTraceServiceRequest:
		for _, rs := range m.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				n += len(ss.GetSpans())
			}
		}
	case *collogspb.ExportLogsServiceRequest:
		for _, rl := range m.GetResourceLogs() {
			for _, sl := range rl.GetScopeLogs() {
				n += len(sl.GetLogRecords())
			}
		}
	}
	return n
}

// wait holds an export for answerAfter, records it, and reports whether the
// receiver should answer it: false when the exporter cancelled first.
func (r *batchReceiver) wait(ctx context.Context, signal string, items int) bool {
	e := batchExport{signal: signal, items: items, start: time.Now()}
	var answer <-chan time.Time
	if r.answerAfter >= 0 {
		answer = time.After(r.answerAfter)
	}
	select {
	case <-answer:
	case <-ctx.Done():
		e.cancelled = true
	}
	e.end = time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exports = append(r.exports, e)
	return !e.cancelled
}

// recorded returns the exports of signal the receiver has seen end, in the
// order they ended.
func (r *batchReceiver) recorded(signal string) []batchExport {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []batchExport
	for _, e := range r.exports {
		if e.signal == signal {
			out = append(out, e)
		}
	}
	return out
}

type batchTraceService struct {
	coltracepb.UnimplementedTraceServiceServer
	r *batchReceiver
}

func (s batchTraceService) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if !s.r.wait(ctx, "traces", countItems(req)) {
		return nil, ctx.Err()
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type batchLogsService struct {
	collogspb.UnimplementedLogsServiceServer
	r *batchReceiver
}

func (s batchLogsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if !s.r.wait(ctx, "logs", countItems(req)) {
		return nil, ctx.Err()
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// batchSignal is one of the two signals a batch processor exports: otlp
// exports it alone over OTLP, set changes its BatchSettings, and emit ends
// one span or emits one record.
type batchSignal struct {
	name string
	set  func(*telemetry.Settings, func(*telemetry.BatchSettings))
	emit func(context.Context, telemetry.SDK)
	otlp func(*telemetry.Settings)
}

var batchSignals = []batchSignal{
	{
		name: "traces",
		otlp: func(s *telemetry.Settings) {
			s.Traces = telemetry.ExporterOTLP
			s.Logs = telemetry.ExporterNone
		},
		set: func(s *telemetry.Settings, f func(*telemetry.BatchSettings)) { f(&s.SpanBatch) },
		emit: func(ctx context.Context, sdk telemetry.SDK) {
			_, span := sdk.TracerProvider.Tracer("carts").Start(ctx, "carts.create")
			span.End()
		},
	},
	{
		name: "logs",
		otlp: func(s *telemetry.Settings) {
			s.Traces = telemetry.ExporterNone
			s.Logs = telemetry.ExporterOTLP
		},
		set: func(s *telemetry.Settings, f func(*telemetry.BatchSettings)) { f(&s.LogBatch) },
		emit: func(ctx context.Context, sdk telemetry.SDK) {
			var rec log.Record
			rec.SetSeverity(log.SeverityInfo)
			rec.SetBody(attribute.StringValue("cart created"))
			sdk.LoggerProvider.Logger("carts").Emit(ctx, rec)
		},
	},
}

// batchSettings exports sig alone, over OTLP and protocol, to otlp:4317.
func batchSettings(sig batchSignal, protocol telemetry.Protocol) telemetry.Settings {
	s := telemetry.DefaultSettings()
	sig.otlp(&s)
	s.OTLP.Endpoint = "otlp:4317"
	s.OTLP.Protocol = protocol
	return s
}

func TestNew_ScheduleDelay_sets_when_the_first_export_starts(t *testing.T) {
	t.Parallel()

	delays := map[string]time.Duration{"traces": 2 * time.Second, "logs": 3 * time.Second}
	for _, sig := range batchSignals {
		for _, protocol := range protocols {
			t.Run(sig.name+"/"+string(protocol), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					host := humustest.NewHost(t)
					rcv := startBatchReceiver(t, listen(t, host, "otlp:4317"), protocol, 0)

					delay := delays[sig.name]
					s := batchSettings(sig, protocol)
					sig.set(&s, func(b *telemetry.BatchSettings) { b.ScheduleDelay = delay })

					origin := time.Now()
					sdk, err := telemetry.New(t.Context(), s, host, nil)
					require.NoError(t, err)
					sig.emit(t.Context(), sdk)

					time.Sleep(delay - time.Millisecond)
					synctest.Wait()
					require.Empty(t, rcv.recorded(sig.name), "an export started before the schedule delay")

					time.Sleep(time.Second / 2)
					synctest.Wait()
					exports := rcv.recorded(sig.name)
					require.Len(t, exports, 1)
					require.Equal(t, delay, exports[0].start.Sub(origin))
					require.Equal(t, 1, exports[0].items)

					require.NoError(t, sdk.Shutdown(context.Background()))
				})
			})
		}
	}
}

func TestNew_MaxExportBatchSize_bounds_each_export(t *testing.T) {
	t.Parallel()

	for _, sig := range batchSignals {
		for _, protocol := range protocols {
			t.Run(sig.name+"/"+string(protocol), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					host := humustest.NewHost(t)
					rcv := startBatchReceiver(t, listen(t, host, "otlp:4317"), protocol, 0)

					s := batchSettings(sig, protocol)
					s.SpanBatch.MaxExportBatchSize = 2
					s.LogBatch.MaxExportBatchSize = 2
					sdk, err := telemetry.New(t.Context(), s, host, nil)
					require.NoError(t, err)
					for range 5 {
						sig.emit(t.Context(), sdk)
					}

					// Past either group's default schedule delay, so that
					// what did not fill a batch is exported too.
					time.Sleep(6 * time.Second)
					synctest.Wait()
					exports := rcv.recorded(sig.name)
					var total int
					for i, e := range exports {
						require.LessOrEqual(t, e.items, 2, "export %d", i)
						total += e.items
					}
					require.Equal(t, 5, total)

					require.NoError(t, sdk.Shutdown(context.Background()))
				})
			})
		}
	}
}

func TestNew_ExportTimeout_and_OTLP_Timeout_bound_the_same_export(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		exportTimeout time.Duration
		otlpTimeout   time.Duration
		want          time.Duration
	}{
		{"ExportTimeout shorter", time.Second, 3 * time.Second, time.Second},
		{"OTLP.Timeout shorter", 3 * time.Second, 2 * time.Second, 2 * time.Second},
	} {
		for _, sig := range batchSignals {
			for _, protocol := range protocols {
				t.Run(tc.name+"/"+sig.name+"/"+string(protocol), func(t *testing.T) {
					t.Parallel()
					synctest.Test(t, func(t *testing.T) {
						host := humustest.NewHost(t)
						rcv := startBatchReceiver(t, listen(t, host, "otlp:4317"), protocol, -1)

						s := batchSettings(sig, protocol)
						s.OTLP.Timeout = tc.otlpTimeout
						sig.set(&s, func(b *telemetry.BatchSettings) {
							b.ScheduleDelay = time.Second
							b.ExportTimeout = tc.exportTimeout
						})
						origin := time.Now()
						sdk, err := telemetry.New(t.Context(), s, host, nil)
						require.NoError(t, err)
						sig.emit(t.Context(), sdk)

						// The export starts at 1s and is cancelled 3s after
						// it at the latest.
						time.Sleep(time.Second + 3*time.Second + time.Second/2)
						synctest.Wait()
						exports := rcv.recorded(sig.name)
						require.Len(t, exports, 1)
						require.Equal(t, time.Second, exports[0].start.Sub(origin))
						require.True(t, exports[0].cancelled, "the export was answered")
						require.Equal(t, tc.want, exports[0].end.Sub(exports[0].start))

						_ = sdk.Shutdown(context.Background())
					})
				})
			}
		}
	}
}
