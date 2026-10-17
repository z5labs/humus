// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
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
			case <-served:
				t.Fatal("Serve returned before the run did")
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
