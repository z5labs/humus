// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run whole Apps, started with bedrocktest.Start on a
// humustest Host, and assert on how the lifecycle builds, uses and shuts down
// the App's telemetry. Each is recorded by humustest.Telemetry unless the
// test is about a Builder of its own.
//
// Run sets OpenTelemetry's process-wide error handler, so no test in this
// package runs in parallel: one App would see another's export errors.

// telemetryApp returns an App on h with a 10s ShutdownGracePeriod whose
// telemetry is built by build and whose Init is init.
func telemetryApp(h *humustest.Host, build telemetry.Builder, init func(context.Context, *humus.Setup) ([]humus.Service, error)) humus.App {
	return humus.App{
		Host:                h.Host(),
		ShutdownGracePeriod: 10 * time.Second,
		Telemetry:           build,
		Init:                init,
	}
}

// infoLogs returns the bodies of the records tel recorded at INFO, in the
// order they were emitted.
func infoLogs(tel *humustest.Telemetry) []string {
	var bodies []string
	for _, r := range tel.Logs() {
		if r.Severity() == log.SeverityInfo {
			bodies = append(bodies, r.Body().AsString())
		}
	}
	return bodies
}

// parseExporter is a parser for config.Value.Parse that takes a variable's
// value as a telemetry.Exporter, for telemetry.CheckExporter to check.
func parseExporter(v string) (telemetry.Exporter, error) {
	return telemetry.Exporter(v), nil
}

// envBuilder is a Builder that sources Settings.Traces from
// OTEL_TRACES_EXPORTER, as an end user's Builder does.
func envBuilder(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	traces, err := config.Env(p.Environment, "OTEL_TRACES_EXPORTER").
		Parse(parseExporter).
		Default(s.Traces).
		Validate(telemetry.CheckExporter).
		Read(ctx)
	if err != nil {
		return telemetry.SDK{}, err
	}
	s.Traces = traces
	s.Logs = telemetry.ExporterNone
	return telemetry.New(ctx, s, dial, p.Stdout)
}

func TestApp_Run_telemetry_a_malformed_variable_read_by_the_Builder_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		initCalled := false
		app := telemetryApp(h, envBuilder, func(context.Context, *humus.Setup) ([]humus.Service, error) {
			initCalled = true
			return nil, nil
		})

		p, status := bedrocktest.Start(t, app, bedrocktest.WithEnv(map[string]string{
			"OTEL_TRACES_EXPORTER": "zipkin",
		}))
		st := <-status

		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.Equal(t, "OTEL_TRACES_EXPORTER", cerr.Name)
		require.False(t, initCalled, "Init was called")
		require.Equal(t, st.Cause().Error()+"\n", p.Stderr())
		require.Equal(t, `build telemetry: config OTEL_TRACES_EXPORTER: "zipkin" is not one of none, console or otlp`, st.Cause().Error())
	})
}

func TestApp_Run_telemetry_a_signal_during_a_slow_Builder_cancels_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		started := make(chan struct{})
		var builderCause error
		slow := func(ctx context.Context, _ bedrock.Process, _ telemetry.Dialer) (telemetry.SDK, error) {
			close(started)
			<-ctx.Done() // a cloud resource detector calling its metadata server
			builderCause = context.Cause(ctx)
			return telemetry.SDK{}, context.Cause(ctx)
		}
		initCalled := false
		app := telemetryApp(h, slow, func(context.Context, *humus.Setup) ([]humus.Service, error) {
			initCalled = true
			return nil, nil
		})

		p, status := bedrocktest.Start(t, app)
		<-started
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status

		require.EqualError(t, builderCause, "terminated signal received")
		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), context.Canceled)
		require.False(t, initCalled, "Init was called")
		require.Equal(t, "build telemetry: terminated signal received\n", p.Stderr())
	})
}

func TestApp_Run_telemetry_a_failed_Builder_exits_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		errBuild := errors.New("detect resource: metadata server unreachable")
		failing := func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
			return telemetry.SDK{}, errBuild
		}

		p, status := bedrocktest.Start(t, telemetryApp(h, failing, servicesOf()))
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), errBuild)
		require.Equal(t, "build telemetry: detect resource: metadata server unreachable\n", p.Stderr())
	})
}

func TestApp_Run_telemetry_an_SDK_with_a_nil_field_exits_1(t *testing.T) {
	cases := []struct {
		field string
		clear func(*telemetry.SDK)
	}{
		{"TracerProvider", func(s *telemetry.SDK) { s.TracerProvider = nil }},
		{"MeterProvider", func(s *telemetry.SDK) { s.MeterProvider = nil }},
		{"LoggerProvider", func(s *telemetry.SDK) { s.LoggerProvider = nil }},
		{"Propagator", func(s *telemetry.SDK) { s.Propagator = nil }},
		{"Shutdown", func(s *telemetry.SDK) { s.Shutdown = nil }},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				shutdowns := 0
				build := func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
					sdk := telemetry.Noop()
					sdk.Shutdown = func(context.Context) error {
						shutdowns++
						return nil
					}
					c.clear(&sdk)
					return sdk, nil
				}
				initCalled := false
				app := telemetryApp(h, build, func(context.Context, *humus.Setup) ([]humus.Service, error) {
					initCalled = true
					return nil, nil
				})

				p, status := bedrocktest.Start(t, app)
				st := <-status

				require.Equal(t, humus.ExitFailure, st.Code())
				want := "build telemetry: the SDK's " + c.field + " is nil"
				require.EqualError(t, st.Cause(), want)
				require.Equal(t, want+"\n", p.Stderr())
				require.False(t, initCalled, "Init was called")
				if c.field != "Shutdown" {
					require.Equal(t, 1, shutdowns, "what the Builder built was not shut down")
				}
			})
		})
	}
}

func TestApp_Run_telemetry_an_SDK_with_a_nil_field_joins_its_failed_shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		errShutdown := errors.New("close exporter")
		build := func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
			sdk := telemetry.Noop()
			sdk.MeterProvider = nil
			sdk.Shutdown = func(context.Context) error { return errShutdown }
			return sdk, nil
		}

		p, status := bedrocktest.Start(t, telemetryApp(h, build, servicesOf()))
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), errShutdown)
		require.Equal(t, "build telemetry: the SDK's MeterProvider is nil\nshut down telemetry: close exporter\n", p.Stderr())
	})
}

func TestApp_Run_telemetry_a_nil_Builder_exits_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)

		p, status := bedrocktest.Start(t, telemetryApp(h, nil, servicesOf()))
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.EqualError(t, st.Cause(), "build telemetry: App.Telemetry is nil")
		require.Equal(t, "build telemetry: App.Telemetry is nil\n", p.Stderr())
	})
}

func TestApp_Run_telemetry_the_Builder_gets_the_Process_and_the_Hosts_Dial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		var gotArgs []string
		var dialErr error
		build := func(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
			gotArgs = p.Args
			// Nothing listens on the humustest Host, so its Dial refuses
			// the connection; OSHost's would have dialled the real network.
			_, dialErr = dial(ctx, "tcp", "collector:4317")
			return telemetry.Noop(), nil
		}

		_, status := bedrocktest.Start(t, telemetryApp(h, build, servicesOf()), bedrocktest.WithArgs("-v"))
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, []string{bedrocktest.ProgramName, "-v"}, gotArgs)
		var opErr *net.OpError
		require.ErrorAs(t, dialErr, &opErr)
		require.ErrorIs(t, dialErr, syscall.ECONNREFUSED)
	})
}

func TestApp_Run_telemetry_Init_and_the_Services_get_the_SDK(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		svc := &fakeService{}
		init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			_, span := s.Telemetry.TracerProvider.Tracer("carts").Start(ctx, "init")
			span.End()
			svc.serve = func(ctx context.Context) error {
				_, span := s.Telemetry.TracerProvider.Tracer("carts").Start(ctx, "serve")
				span.End()
				return nil
			}
			return []humus.Service{svc}, nil
		}

		_, status := bedrocktest.Start(t, telemetryApp(h, tel.Builder(), init))
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		var names []string
		for _, s := range tel.Spans() {
			names = append(names, s.Name())
		}
		require.Equal(t, []string{"init", "serve"}, names)
	})
}

func TestApp_Run_telemetry_a_failed_Init_is_logged_at_ERROR(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		errInit := errors.New("dial database: connection refused")
		app := telemetryApp(h, tel.Builder(), func(context.Context, *humus.Setup) ([]humus.Service, error) {
			return nil, errInit
		})

		p, status := bedrocktest.Start(t, app)
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), errInit)
		require.Empty(t, p.Stderr())

		logs := tel.Logs()
		require.Len(t, logs, 1, "a failed Init logs its error and nothing else")
		require.Equal(t, log.SeverityError, logs[0].Severity())
		require.Equal(t, "ERROR", logs[0].SeverityText())
		require.Equal(t, "init: dial database: connection refused", logs[0].Body().AsString())
		require.Equal(t, "github.com/z5labs/humus", logs[0].InstrumentationScope().Name)
	})
}

func TestApp_Run_telemetry_logs_serving_shutting_down_and_stopped_in_order(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		var atShutdown []string
		svc := &fakeService{shutdown: func(context.Context) error {
			atShutdown = infoLogs(tel)
			return nil
		}}

		p, status := bedrocktest.Start(t, telemetryApp(h, tel.Builder(), servicesOf(svc)))
		<-p.Notified(syscall.SIGTERM)
		synctest.Wait() // until the App waits for the signal
		require.Equal(t, []string{"serving"}, infoLogs(tel), "logged before the signal")
		p.Signal(syscall.SIGTERM)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, []string{"serving", "shutting down"}, atShutdown, "logged when Shutdown was called")
		require.Equal(t, []string{"serving", "shutting down", "stopped"}, infoLogs(tel))
		for _, r := range tel.Logs() {
			require.Equal(t, "INFO", r.SeverityText())
			require.Equal(t, "github.com/z5labs/humus", r.InstrumentationScope().Name)
		}
		require.Empty(t, p.Stderr())
	})
}

func TestApp_Run_telemetry_failures_are_logged_before_stopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		failing := &fakeService{serve: func(context.Context) error {
			return errors.New("consume orders: broker went away")
		}}

		p, status := bedrocktest.Start(t, telemetryApp(h, tel.Builder(), servicesOf(failing)))
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		var got []string
		for _, r := range tel.Logs() {
			got = append(got, r.SeverityText()+" "+r.Body().AsString())
		}
		require.Equal(t, []string{
			"INFO serving",
			"INFO shutting down",
			"ERROR serve: consume orders: broker went away",
			"INFO stopped",
		}, got)
		require.Empty(t, p.Stderr())
	})
}

func TestApp_Run_telemetry_a_span_recorded_by_an_OnStop_function_is_exported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(ctx context.Context) error {
				_, span := s.Telemetry.TracerProvider.Tracer("carts").Start(ctx, "close pool")
				span.End()
				return nil
			})
			return []humus.Service{&fakeService{}}, nil
		}

		p, status := bedrocktest.Start(t, telemetryApp(h, tel.Builder(), init))
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		spans := tel.Spans()
		require.Len(t, spans, 1, "the span was recorded after telemetry shut down")
		require.Equal(t, "close pool", spans[0].Name())
	})
}

func TestApp_Run_telemetry_shuts_down_last(t *testing.T) {
	cases := []struct {
		name    string
		failing bool
	}{
		{"after the OnStop functions", false},
		{"after a failed Init and its OnStop functions", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				var order []string
				build := func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
					sdk := telemetry.Noop()
					sdk.Shutdown = func(context.Context) error {
						order = append(order, "telemetry")
						return nil
					}
					return sdk, nil
				}
				svc := &fakeService{
					serve: func(context.Context) error { return nil },
					shutdown: func(context.Context) error {
						order = append(order, "service")
						return nil
					},
				}
				init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
					s.OnStop(func(context.Context) error {
						order = append(order, "on stop")
						return nil
					})
					if c.failing {
						return nil, errors.New("boom")
					}
					return []humus.Service{svc}, nil
				}

				_, status := bedrocktest.Start(t, telemetryApp(h, build, init))
				<-status

				want := []string{"service", "on stop", "telemetry"}
				if c.failing {
					want = want[1:]
				}
				require.Equal(t, want, order)
			})
		})
	}
}

// ctxKey is the type of a context value the root context carries.
type ctxKey struct{}

func TestApp_Run_telemetry_shutdown_keeps_the_values_but_not_the_cancellation_of_ctx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		var (
			shutdownErr   error
			shutdownValue any
			hasDeadline   bool
		)
		build := func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
			sdk := telemetry.Noop()
			sdk.Shutdown = func(ctx context.Context) error {
				shutdownErr = ctx.Err()
				shutdownValue = ctx.Value(ctxKey{})
				_, hasDeadline = ctx.Deadline()
				return nil
			}
			return sdk, nil
		}
		app := telemetryApp(h, build, servicesOf(&fakeService{}))
		p := bedrocktest.NewProcess(t)

		ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "request-scoped"))
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(ctx, p.Process()) }()
		<-p.Notified(syscall.SIGTERM)
		cancel()
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.NoError(t, shutdownErr, "the shutdown's context was cancelled")
		require.Equal(t, "request-scoped", shutdownValue)
		require.False(t, hasDeadline, "the shutdown's context has the grace period's deadline")
	})
}

func TestApp_Run_telemetry_a_failed_shutdown_turns_exit_0_into_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		errShutdown := errors.New("flush logs: collector unavailable")
		build := func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
			sdk := telemetry.Noop()
			sdk.Shutdown = func(context.Context) error { return errShutdown }
			return sdk, nil
		}
		job := &fakeService{serve: func(context.Context) error { return nil }}

		p, status := bedrocktest.Start(t, telemetryApp(h, build, servicesOf(job)))
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), errShutdown)
		require.Equal(t, "shut down telemetry: flush logs: collector unavailable\n", p.Stderr())
	})
}

func TestApp_Run_telemetry_a_failed_shutdown_is_joined_into_the_cause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		errShutdown := errors.New("flush logs: collector unavailable")
		build := func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
			sdk := telemetry.Noop()
			sdk.Shutdown = func(context.Context) error { return errShutdown }
			return sdk, nil
		}
		app := telemetryApp(h, build, servicesOf(&fakeService{}))

		p, status := bedrocktest.Start(t, app)
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), errShutdown)
		require.ErrorIs(t, st.Cause(), context.Canceled, "the signal is still in the cause")
	})
}

func TestApp_Run_telemetry_a_failed_shutdown_keeps_a_config_failure_at_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		errShutdown := errors.New("flush logs: collector unavailable")
		build := func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
			sdk := telemetry.Noop()
			sdk.Shutdown = func(context.Context) error { return errShutdown }
			return sdk, nil
		}
		init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			_, err := config.Env(s.Process.Environment, "DATABASE_URL").Read(ctx)
			return nil, err
		}

		_, status := bedrocktest.Start(t, telemetryApp(h, build, init))
		st := <-status

		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorIs(t, st.Cause(), config.ErrNotFound)
		require.ErrorIs(t, st.Cause(), errShutdown)
	})
}

func TestApp_Run_telemetry_sets_an_error_handler_that_writes_one_line_to_stderr(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		init := func(context.Context, *humus.Setup) ([]humus.Service, error) {
			otel.Handle(errors.Join(errors.New("export spans"), errors.New("connection refused")))
			return nil, nil
		}

		p, status := bedrocktest.Start(t, telemetryApp(h, telemetry.NoopBuilder, init))
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, "telemetry: export spans; connection refused\n", p.Stderr())
	})
}

// TestApp_Run_telemetry_an_export_error_is_written_to_stderr runs in real
// time, outside synctest: the OTLP exporter's gRPC client starts goroutines
// and timers of its own.
func TestApp_Run_telemetry_an_export_error_is_written_to_stderr(t *testing.T) {
	h := humustest.NewHost(t)
	build := func(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
		s := telemetry.DefaultSettings()
		s.Traces = telemetry.ExporterOTLP
		s.Logs = telemetry.ExporterNone
		s.OTLP.Endpoint = "collector:4317" // nothing listens on it
		s.OTLP.Timeout = 100 * time.Millisecond
		return telemetry.New(ctx, s, dial, p.Stdout)
	}
	init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
		_, span := s.Telemetry.TracerProvider.Tracer("carts").Start(ctx, "init")
		span.End()
		return nil, nil
	}

	p, status := bedrocktest.Start(t, telemetryApp(h, build, init))
	st := <-status

	var lines []string
	for line := range strings.Lines(p.Stderr()) {
		lines = append(lines, line)
	}
	require.NotEmpty(t, lines, "nothing was written to stderr; exit %d, cause %v", st.Code(), st.Cause())
	require.True(t, slices.ContainsFunc(lines, func(l string) bool {
		return strings.HasPrefix(l, "telemetry: ")
	}), "stderr = %q, want a line starting with \"telemetry: \"", p.Stderr())
}

func TestApp_Run_telemetry_a_failed_shutdown_after_a_SIGTERM_as_the_App_starts_serving_is_joined_into_the_cause(t *testing.T) {
	errShutdown := errors.New("flush logs: collector unavailable")
	lost := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		synctest.Test(t, func(t *testing.T) {
			p := bedrocktest.NewProcess(t)
			logs := sigtermAsServing(p, nil)
			app := appWith(servicesOf(&fakeService{}))
			app.Telemetry = hookedTelemetry(logs, errShutdown)

			st = app.Run(t.Context(), p.Process())
		})

		t.Logf("App %d: code %d, cause %q", i+1, st.Code(), fmt.Sprint(st.Cause()))
		if st.Code() != humus.ExitFailure {
			t.Errorf("App %d: code = %d, want %d", i+1, st.Code(), humus.ExitFailure)
		}
		if !errors.Is(st.Cause(), errShutdown) {
			t.Errorf("App %d: cause = %v, want the telemetry's shutdown error in it", i+1, st.Cause())
		}
		if !errors.Is(st.Cause(), context.Canceled) {
			lost.fail(i, "cause = %v, want the signal still in it", st.Cause())
		}
	}
	lost.report("the signal was not in the cause")
}
