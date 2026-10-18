// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// runs returns an Init whose only Service is a job that runs run.
func runs(run func(context.Context) error) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		return []humus.Service{job.NewService(s, run)}, nil
	}
}

// setup returns a Setup whose telemetry is tel's, for a test that drives a
// Service directly rather than through an App.
func setup(t *testing.T, tel *humustest.Telemetry) *humus.Setup {
	t.Helper()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	return &humus.Setup{Telemetry: sdk}
}

func TestService_a_run_that_returns_nil_exits_0(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, runs(func(context.Context) error {
			time.Sleep(time.Minute) // fake time: returns at once
			return nil
		}), humustest.WithApp(humus.WithTelemetry(tel.Builder())))

		st := <-status
		require.Zero(t, st.Code())
		require.NoError(t, st.Cause())
		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, "job", spans[0].Name())
		require.Equal(t, trace.SpanKindInternal, spans[0].SpanKind())
		require.False(t, spans[0].Parent().IsValid(), "the span is the root of a new trace")
		require.Equal(t, codes.Unset, spans[0].Status().Code)
		require.Equal(t, "github.com/z5labs/humus/job", spans[0].InstrumentationScope().Name)
		require.Equal(t, time.Minute, spans[0].EndTime().Sub(spans[0].StartTime()))
	})
}

func TestService_SIGTERM_interrupts_the_run_and_exits_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, runs(func(ctx context.Context) error {
			<-ctx.Done()
			return context.Cause(ctx)
		}), humustest.WithApp(humus.WithTelemetry(tel.Builder())))
		synctest.Wait() // the run is blocked on its context
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorContains(t, st.Cause(), "serve: terminated signal received")
		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, codes.Error, spans[0].Status().Code)
		require.Equal(t, "terminated signal received", spans[0].Status().Description)
	})
}

func TestService_SIGTERM_and_a_run_that_returns_ctx_Err_exits_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, status := humustest.Start(t, runs(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}))
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), context.Canceled)
		require.ErrorContains(t, st.Cause(), "serve: context canceled")
	})
}

func TestService_meets_the_Service_contract(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		return job.NewService(s, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
	})
}

func TestService_a_run_that_fails_exits_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		errExport := errors.New("export: the bucket is gone")
		_, status := humustest.Start(t, runs(func(context.Context) error {
			return errExport
		}), humustest.WithApp(humus.WithTelemetry(tel.Builder())))

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), errExport)
		require.EqualError(t, st.Cause(), "serve: export: the bucket is gone")
		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, codes.Error, spans[0].Status().Code)
		require.Equal(t, "export: the bucket is gone", spans[0].Status().Description)
	})
}

func TestService_an_interrupted_run_that_returns_nil_exits_0(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, runs(func(ctx context.Context) error {
			<-ctx.Done()
			return nil // it chose to treat stopping early as done
		}), humustest.WithApp(humus.WithTelemetry(tel.Builder())))
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.EqualError(t, st.Cause(), "terminated signal received")
		require.Equal(t, codes.Unset, tel.Spans()[0].Status().Code)
	})
}

func TestService_a_run_that_ignores_its_context_ends_after_the_grace_period(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, status := humustest.Start(t, runs(func(context.Context) error {
			time.Sleep(time.Hour) // ignores its context
			return nil
		}))
		synctest.Wait()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
		require.Equal(t, 20*time.Second, gerr.GracePeriod)
	})
}

// recordShutdown is a Service that records when its Shutdown was called,
// and then calls the Shutdown of the Service it wraps.
type recordShutdown struct {
	humus.Service
	at atomic.Pointer[time.Time]
}

func (r *recordShutdown) Shutdown(ctx context.Context) error {
	now := time.Now()
	r.at.Store(&now)
	return r.Service.Shutdown(ctx)
}

func TestService_HUMUS_SHUTDOWN_GRACE_PERIOD_overrides_the_default_and_there_is_no_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var svc *recordShutdown
		ignoresCtx := func(context.Context) error {
			time.Sleep(time.Hour)
			return nil
		}
		p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			svc = &recordShutdown{Service: job.NewService(s, ignoresCtx)}
			return []humus.Service{svc}, nil
		}, humustest.WithEnv(map[string]string{"HUMUS_SHUTDOWN_GRACE_PERIOD": "5s"}))
		synctest.Wait()
		signalled := time.Now()
		p.Signal(syscall.SIGTERM)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code())
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		require.True(t, ok, "cause = %v, want a *humus.GracePeriodError", st.Cause())
		require.Equal(t, 5*time.Second, gerr.GracePeriod)
		at := svc.at.Load()
		require.NotNil(t, at, "Shutdown was never called")
		require.Equal(t, signalled, *at, "Shutdown was called after a drain")
	})
}

func TestService_a_failed_Init_exits_78_and_never_runs_the_job(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ran atomic.Bool
		_, status := humustest.Start(t, func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			if _, err := config.Env(s.Process.Environment, "EXPORT_BUCKET").Read(ctx); err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(context.Context) error {
				ran.Store(true)
				return nil
			})}, nil
		})

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.Equal(t, "EXPORT_BUCKET", cerr.Name)
		require.False(t, ran.Load(), "the job ran")
	})
}

func TestService_the_runs_context_carries_its_span(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			tracer := s.Telemetry.TracerProvider.Tracer("exporter")
			logger := otelslog.NewLogger("exporter", otelslog.WithLoggerProvider(s.Telemetry.LoggerProvider))
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				_, span := tracer.Start(ctx, "export")
				defer span.End()
				logger.InfoContext(ctx, "exported")
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithTelemetry(tel.Builder())))

		require.Zero(t, (<-status).Code())
		spans := tel.Spans()
		require.Len(t, spans, 2)
		export, run := spans[0], spans[1]
		require.Equal(t, "export", export.Name())
		require.Equal(t, "job", run.Name())
		require.Equal(t, run.SpanContext().SpanID(), export.Parent().SpanID())
		require.Equal(t, run.SpanContext().TraceID(), export.SpanContext().TraceID())

		var found bool
		for _, rec := range tel.Logs() {
			if rec.Body().AsString() != "exported" {
				continue
			}
			found = true
			require.Equal(t, run.SpanContext().TraceID(), rec.TraceID())
		}
		require.True(t, found, "the run's record was not logged")
	})
}

func TestService_a_bare_Shutdown(t *testing.T) {
	errFlush := errors.New("flush: the buffer is full")
	cases := []struct {
		name string
		ret  func(ctx context.Context) error
		want error
	}{
		{"returns nil from Serve when the run returns ctx.Err()", func(ctx context.Context) error { return ctx.Err() }, nil},
		{"returns nil from Serve when the run returns context.Cause(ctx)", context.Cause, nil},
		{"returns nil from Serve when the run returns a wrapped cancellation", func(ctx context.Context) error {
			return fmt.Errorf("flush: %w", ctx.Err())
		}, nil},
		{"returns the run's error from Serve when it is not a cancellation", func(context.Context) error { return errFlush }, errFlush},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := humustest.NewTelemetry()
				svc := job.NewService(setup(t, tel), func(ctx context.Context) error {
					<-ctx.Done()
					return tc.ret(ctx)
				})
				served := make(chan error, 1)
				go func() { served <- svc.Serve(t.Context()) }()
				synctest.Wait() // the run is blocked on its context

				require.NoError(t, svc.Shutdown(t.Context()))
				require.Equal(t, tc.want, <-served)
				spans := tel.Spans()
				require.Len(t, spans, 1)
				require.Equal(t, codes.Error, spans[0].Status().Code, "the span records what the run returned")
			})
		})
	}
}

func TestService_runs_once(t *testing.T) {
	t.Run("Serve called twice runs the job once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var runs atomic.Int32
			svc := job.NewService(setup(t, humustest.NewTelemetry()), func(context.Context) error {
				runs.Add(1)
				return nil
			})
			require.NoError(t, svc.Serve(t.Context()))
			require.NoError(t, svc.Serve(t.Context()))
			require.Equal(t, int32(1), runs.Load())
			require.NoError(t, svc.Shutdown(t.Context()), "Shutdown after Serve has returned")
		})
	})

	t.Run("Serve called after Shutdown never runs the job", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var ran atomic.Bool
			svc := job.NewService(setup(t, humustest.NewTelemetry()), func(context.Context) error {
				ran.Store(true)
				return nil
			})
			require.NoError(t, svc.Shutdown(t.Context()))
			require.NoError(t, svc.Serve(t.Context()))
			require.False(t, ran.Load(), "the job ran")
		})
	})

	t.Run("Shutdown with a done context returns, and Serve still waits for the run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			finished := make(chan struct{})
			errLate := errors.New("finished late")
			svc := job.NewService(setup(t, humustest.NewTelemetry()), func(context.Context) error {
				time.Sleep(time.Minute) // ignores its context
				close(finished)
				return errLate
			})
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()
			synctest.Wait()

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err := svc.Shutdown(ctx)
			require.ErrorIs(t, err, context.Canceled)

			select {
			case err := <-served:
				t.Fatalf("Serve returned %v before the run did", err)
			default:
			}
			require.ErrorIs(t, <-served, errLate)
			select {
			case <-finished:
			default:
				t.Fatal("Serve returned before the run did")
			}
		})
	})

	t.Run("Shutdown whose context runs out returns an error that wraps it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc := job.NewService(setup(t, humustest.NewTelemetry()), func(context.Context) error {
				time.Sleep(time.Minute) // ignores its context
				return nil
			})
			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()
			synctest.Wait()

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			require.ErrorIs(t, svc.Shutdown(ctx), context.DeadlineExceeded)
			require.NoError(t, <-served)
		})
	})
}

func TestNewService_panics_on_a_nil_run(t *testing.T) {
	s := setup(t, humustest.NewTelemetry())
	require.PanicsWithValue(t, "job: NewService called with a nil run", func() {
		job.NewService(s, nil)
	})
}

// traceparent is a valid W3C traceparent: its trace ID is
// 4bf92f3577b34da6a3ce929d0e0e4736 and its span ID 00f067aa0ba902b7.
const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestService_joins_the_trace_in_TRACEPARENT(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		var schedule string
		_, status := humustest.Start(t, runs(func(ctx context.Context) error {
			schedule = baggage.FromContext(ctx).Member("schedule").Value()
			return nil
		}),
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"TRACEPARENT": traceparent,
				"BAGGAGE":     "schedule=nightly",
			}),
		)

		require.Zero(t, (<-status).Code())
		spans := tel.Spans()
		require.Len(t, spans, 1)
		span := spans[0]
		require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", span.SpanContext().TraceID().String())
		require.Equal(t, "00f067aa0ba902b7", span.Parent().SpanID().String())
		require.True(t, span.Parent().IsRemote())
		require.Equal(t, trace.SpanKindInternal, span.SpanKind())
		require.Equal(t, "nightly", schedule)
	})
}

func TestService_a_malformed_TRACEPARENT_starts_a_new_trace(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"malformed", map[string]string{"TRACEPARENT": "00-not-a-trace-context-01"}},
		{"an all-zero trace ID", map[string]string{"TRACEPARENT": "00-00000000000000000000000000000000-00f067aa0ba902b7-01"}},
		{"unset", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tel := humustest.NewTelemetry()
				p, status := humustest.Start(t, runs(func(context.Context) error { return nil }),
					humustest.WithApp(humus.WithTelemetry(tel.Builder())),
					humustest.WithEnv(tc.env),
				)

				st := <-status
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
				require.NoError(t, st.Cause())
				spans := tel.Spans()
				require.Len(t, spans, 1)
				require.False(t, spans[0].Parent().IsValid(), "the span is the root of a new trace")
				require.True(t, spans[0].SpanContext().IsValid())
				require.NotEqual(t, "00000000000000000000000000000000", spans[0].SpanContext().TraceID().String())

				require.Empty(t, p.Stderr())
				for _, rec := range tel.Logs() {
					body := strings.ToLower(rec.Body().AsString())
					require.NotContains(t, body, "traceparent")
					require.NotContains(t, body, "trace context")
					require.Less(t, rec.Severity(), otellog.SeverityWarn, "record %q", rec.Body().AsString())
				}
			})
		})
	}
}

func TestService_TRACESTATE_reaches_the_spans_trace_state(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, runs(func(context.Context) error { return nil }),
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"TRACEPARENT": traceparent,
				"TRACESTATE":  "congo=t61rcWkgMzE,rojo=00f067aa0ba902b7",
			}),
		)

		require.Zero(t, (<-status).Code())
		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, "congo=t61rcWkgMzE,rojo=00f067aa0ba902b7", spans[0].SpanContext().TraceState().String())
	})
}

func TestService_only_the_run_joins_the_trace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		_, status := humustest.Start(t, func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			_, span := s.Telemetry.TracerProvider.Tracer("init").Start(ctx, "init")
			span.End()
			return []humus.Service{job.NewService(s, func(context.Context) error { return nil })}, nil
		},
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{"TRACEPARENT": traceparent}),
		)

		require.Zero(t, (<-status).Code())
		spans := tel.Spans()
		require.Len(t, spans, 2)
		initSpan, run := spans[0], spans[1]
		require.Equal(t, "init", initSpan.Name())
		require.Equal(t, "job", run.Name())
		require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", run.SpanContext().TraceID().String())
		require.False(t, initSpan.Parent().IsValid(), "Init's span is the root of a trace of its own")
		require.NotEqual(t, run.SpanContext().TraceID(), initSpan.SpanContext().TraceID())
	})
}

// testPropagator is a propagator of the test's own. Its only key is
// x-test.parent, whose value is a W3C traceparent.
type testPropagator struct{}

func (testPropagator) Inject(context.Context, propagation.TextMapCarrier) {}

func (testPropagator) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": c.Get("x-test.parent")})
}

func (testPropagator) Fields() []string { return []string{"x-test.parent"} }

func TestService_reads_the_variables_of_the_Builders_propagator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		build := func(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
			sdk, err := tel.Builder()(ctx, p, dial)
			sdk.Propagator = testPropagator{}
			return sdk, err
		}
		_, status := humustest.Start(t, runs(func(context.Context) error { return nil }),
			humustest.WithApp(humus.WithTelemetry(build)),
			humustest.WithEnv(map[string]string{
				"X_TEST_PARENT": traceparent,
				// W3C trace context's variable, which this propagator
				// doesn't read.
				"TRACEPARENT": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
			}),
		)

		require.Zero(t, (<-status).Code())
		spans := tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", spans[0].SpanContext().TraceID().String())
		require.Equal(t, "00f067aa0ba902b7", spans[0].Parent().SpanID().String())
		require.True(t, spans[0].Parent().IsRemote())
	})
}

func TestInit_runs_the_job_and_releases_its_dependencies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{"EXPORT_BUCKET": "nightly"}),
		)

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, "exported to nightly\n", p.Stdout(), "OnStop flushed the run's output")
		require.Equal(t, "job", tel.Spans()[0].Name())
	})
}

func TestInit_without_its_bucket_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.Equal(t, "EXPORT_BUCKET", cerr.Name)
	})
}

func TestService_a_job_next_to_the_health_Service_ends_the_App(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			run := func(context.Context) error { return nil }
			return []humus.Service{
				health.NewService(),
				job.NewService(s, run),
			}, nil
		})

		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.NoError(t, st.Cause())
	})
}

// TestService_a_job_next_to_a_probes_server_ends_the_App runs a
// long-running job next to httphealth's probes server, on humus.Main's
// defaults: the probes are answered while the job runs, and the job
// finishing ends the App, and the probes server with it.
func TestService_a_job_next_to_a_probes_server_ends_the_App(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		probeSettings := httphealth.Settings{
			Addr: ":8081",
			TLS:  &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}},
		}
		finish := make(chan struct{})
		run := func(ctx context.Context) error {
			select {
			case <-finish:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			probes, err := httphealth.NewService(s, probeSettings)
			if err != nil {
				return nil, err
			}
			return []humus.Service{
				probes,
				job.NewService(s, run),
			}, nil
		}, humustest.WithApp(humus.WithHost(h.Host())))
		<-h.Listening("localhost:8081")
		synctest.Wait()

		c := h.HTTPClient()
		c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
		resp, err := c.Get("https://localhost:8081/health/liveness")
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, "the probes are answered while the job runs")

		close(finish)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		_, err = h.Dial(t.Context(), "tcp", "localhost:8081")
		require.Error(t, err, "the probes server outlived the job")
	})
}
