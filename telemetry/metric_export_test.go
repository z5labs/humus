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
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// export is what a metricsReceiver records of one metrics export: when it
// arrived, and when the receiver saw it end.
type export struct {
	start time.Time
	// end is when the receiver answered or saw the export cancelled.
	end time.Time
	// cancelled reports whether the exporter gave up before the receiver
	// answered.
	cancelled bool
}

// metricsReceiver is an in-process OTLP metrics receiver that answers each
// export after answerAfter, or never when answerAfter is negative, and
// records when each export starts and ends.
type metricsReceiver struct {
	answerAfter time.Duration

	mu      sync.Mutex
	exports []export
}

// startMetricsReceiver serves a metricsReceiver over protocol on ln until
// the test ends.
func startMetricsReceiver(t *testing.T, ln net.Listener, protocol telemetry.Protocol, answerAfter time.Duration) *metricsReceiver {
	t.Helper()
	r := &metricsReceiver{answerAfter: answerAfter}
	switch protocol {
	case telemetry.ProtocolGRPC:
		srv := grpc.NewServer()
		colmetricspb.RegisterMetricsServiceServer(srv, delayedMetricsService{r: r})
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(srv.Stop)
	case telemetry.ProtocolHTTPProtobuf:
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, req *http.Request) {
			if _, err := io.Copy(io.Discard, req.Body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if !r.wait(req.Context()) {
				return
			}
			b, err := proto.Marshal(&colmetricspb.ExportMetricsServiceResponse{})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-protobuf")
			_, _ = w.Write(b)
		})
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
	default:
		t.Fatalf("unknown protocol %q", protocol)
	}
	return r
}

// wait records an export, holds it for answerAfter, and reports whether
// the receiver should answer it: false when the exporter cancelled first.
func (r *metricsReceiver) wait(ctx context.Context) bool {
	e := export{start: time.Now()}
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

// recorded returns the exports the receiver has seen end, in the order
// they ended.
func (r *metricsReceiver) recorded() []export {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]export(nil), r.exports...)
}

type delayedMetricsService struct {
	colmetricspb.UnimplementedMetricsServiceServer
	r *metricsReceiver
}

func (s delayedMetricsService) Export(ctx context.Context, _ *colmetricspb.ExportMetricsServiceRequest) (*colmetricspb.ExportMetricsServiceResponse, error) {
	if !s.r.wait(ctx) {
		return nil, ctx.Err()
	}
	return &colmetricspb.ExportMetricsServiceResponse{}, nil
}

// since returns when each export started, relative to origin.
func since(origin time.Time, exports []export) []time.Duration {
	var starts []time.Duration
	for _, e := range exports {
		starts = append(starts, e.start.Sub(origin))
	}
	return starts
}

// metricSettings exports metrics alone, over OTLP and protocol, to
// otlp:4317, every interval.
func metricSettings(protocol telemetry.Protocol, interval time.Duration) telemetry.Settings {
	s := telemetry.DefaultSettings()
	s.Metrics = telemetry.ExporterOTLP
	s.Logs = telemetry.ExporterNone
	s.OTLP.Endpoint = "otlp:4317"
	s.OTLP.Protocol = protocol
	s.MetricExport.Interval = interval
	return s
}

var protocols = []telemetry.Protocol{telemetry.ProtocolGRPC, telemetry.ProtocolHTTPProtobuf}

func TestNew_MetricExport_Interval_sets_when_the_first_export_starts(t *testing.T) {
	t.Parallel()

	for _, protocol := range protocols {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				host := humustest.NewHost(t)
				rcv := startMetricsReceiver(t, listen(t, host, "otlp:4317"), protocol, 0)

				origin := time.Now()
				sdk, err := telemetry.New(t.Context(), metricSettings(protocol, 2*time.Second), host, nil)
				require.NoError(t, err)
				counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
				require.NoError(t, err)
				counter.Add(t.Context(), 1)

				time.Sleep(2*time.Second - time.Millisecond)
				synctest.Wait()
				require.Empty(t, rcv.recorded(), "an export started before the interval")

				time.Sleep(time.Second)
				synctest.Wait()
				require.Equal(t, []time.Duration{2 * time.Second}, since(origin, rcv.recorded()))

				require.NoError(t, sdk.Shutdown(context.Background()))
			})
		})
	}
}

// timedWriter records when each Write happens.
type timedWriter struct {
	mu     sync.Mutex
	writes []time.Time
}

func (w *timedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, time.Now())
	return len(p), nil
}

func (w *timedWriter) recorded() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Time(nil), w.writes...)
}

func TestNew_MetricExport_Interval_sets_when_the_first_console_export_lands(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		s := telemetry.DefaultSettings()
		s.Metrics = telemetry.ExporterConsole
		s.Logs = telemetry.ExporterNone
		s.MetricExport.Interval = 2 * time.Second
		out := &timedWriter{}

		origin := time.Now()
		sdk, err := telemetry.New(t.Context(), s, failingDialer(t), out)
		require.NoError(t, err)
		counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
		require.NoError(t, err)
		counter.Add(t.Context(), 1)

		time.Sleep(2*time.Second - time.Millisecond)
		synctest.Wait()
		require.Empty(t, out.recorded(), "metrics were written before the interval")

		time.Sleep(time.Second)
		synctest.Wait()
		writes := out.recorded()
		require.NotEmpty(t, writes, "no metrics were written at the interval")
		for _, at := range writes {
			require.Equal(t, 2*time.Second, at.Sub(origin))
		}

		require.NoError(t, sdk.Shutdown(context.Background()))
	})
}

func TestNew_MetricExport_Interval_runs_from_the_start_of_one_export_to_the_start_of_the_next(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		answerAfter time.Duration
		want        []time.Duration
	}{
		{"an export shorter than the interval", time.Second, []time.Duration{2 * time.Second, 4 * time.Second, 6 * time.Second}},
		{"an export longer than the interval", 5 * time.Second, []time.Duration{2 * time.Second, 7 * time.Second, 12 * time.Second}},
	} {
		for _, protocol := range protocols {
			t.Run(tc.name+"/"+string(protocol), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					host := humustest.NewHost(t)
					rcv := startMetricsReceiver(t, listen(t, host, "otlp:4317"), protocol, tc.answerAfter)

					origin := time.Now()
					s := metricSettings(protocol, 2*time.Second)
					s.ShutdownTimeout = time.Minute
					sdk, err := telemetry.New(t.Context(), s, host, nil)
					require.NoError(t, err)
					counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
					require.NoError(t, err)
					counter.Add(t.Context(), 1)

					// Past the third export's start, and before the
					// fourth's; the third is still in flight, and the
					// receiver records an export when it ends.
					last := tc.want[len(tc.want)-1]
					time.Sleep(last + time.Second/2)
					_ = sdk.Shutdown(context.Background())

					starts := since(origin, rcv.recorded())
					var periodic []time.Duration
					for _, at := range starts {
						if at <= last {
							periodic = append(periodic, at)
						}
					}
					require.ElementsMatch(t, tc.want, periodic, "every export start: %v", starts)
				})
			})
		}
	}
}

func TestNew_MetricExport_Timeout_bounds_each_periodic_export_but_not_the_last(t *testing.T) {
	t.Parallel()

	for _, protocol := range protocols {
		t.Run(string(protocol), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				host := humustest.NewHost(t)
				rcv := startMetricsReceiver(t, listen(t, host, "otlp:4317"), protocol, -1)

				origin := time.Now()
				s := metricSettings(protocol, 2*time.Second)
				s.MetricExport.Timeout = time.Second
				s.OTLP.Timeout = 3 * time.Second
				s.ShutdownTimeout = 5 * time.Second
				sdk, err := telemetry.New(t.Context(), s, host, nil)
				require.NoError(t, err)
				counter, err := sdk.MeterProvider.Meter("carts").Int64Counter("carts.created")
				require.NoError(t, err)
				counter.Add(t.Context(), 1)

				// The periodic export starts at 2s and is cancelled at 3s;
				// the next would start at 4s.
				time.Sleep(3*time.Second + time.Second/2)
				synctest.Wait()
				exports := rcv.recorded()
				require.Len(t, exports, 1, "the periodic export")
				require.Equal(t, 2*time.Second, exports[0].start.Sub(origin))
				require.True(t, exports[0].cancelled, "the periodic export was answered")
				require.Equal(t, s.MetricExport.Timeout, exports[0].end.Sub(exports[0].start))

				// The last export, the one Shutdown runs, is bounded by
				// OTLP.Timeout within ShutdownTimeout, not by
				// MetricExport.Timeout.
				shutdownAt := time.Now()
				_ = sdk.Shutdown(context.Background())
				synctest.Wait()
				exports = rcv.recorded()
				require.Len(t, exports, 2, "the periodic export and the last one")
				require.Equal(t, shutdownAt, exports[1].start)
				require.True(t, exports[1].cancelled, "the last export was answered")
				require.Equal(t, s.OTLP.Timeout, exports[1].end.Sub(exports[1].start))
			})
		})
	}
}
