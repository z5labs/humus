// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/config"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	humusjob "github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// Every test in this file runs the App lifecycle whole, inside a synctest
// bubble, on a bedrocktest Process and fake Services. None opens a socket or
// touches a file: the Host is OSHost, which the fake Services never use.
// A test that asserts on a failure the App logs records the App's telemetry
// with humustest.Telemetry and reads the failure from its Logs.

// fakeService is a Service built from two functions. A nil serve blocks
// until its context is done and returns nil; a nil shutdown returns nil.
type fakeService struct {
	serve    func(ctx context.Context) error
	shutdown func(ctx context.Context) error

	mu            sync.Mutex
	shutdownCalls int
}

func (s *fakeService) Serve(ctx context.Context) error {
	if s.serve == nil {
		<-ctx.Done()
		return nil
	}
	return s.serve(ctx)
}

func (s *fakeService) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shutdownCalls++
	s.mu.Unlock()
	if s.shutdown == nil {
		return nil
	}
	return s.shutdown(ctx)
}

func (s *fakeService) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdownCalls
}

// appWith returns an App on OSHost with a 10s ShutdownGracePeriod and no-op
// telemetry whose Init is init.
func appWith(init func(ctx context.Context, s *humus.Setup) ([]humus.Service, error)) humus.App {
	return humus.App{
		Host:                humus.OSHost(),
		ShutdownGracePeriod: 10 * time.Second,
		Telemetry:           telemetry.NoopBuilder,
		Init:                init,
	}
}

// recorded returns app with its telemetry recorded by a new
// humustest.Telemetry, and the Telemetry.
func recorded(app humus.App) (humus.App, *humustest.Telemetry) {
	tel := humustest.NewTelemetry()
	app.Telemetry = tel.Builder()
	return app, tel
}

// errorLogs returns the bodies of the records tel recorded at ERROR, in the
// order they were emitted.
func errorLogs(tel *humustest.Telemetry) []string {
	var bodies []string
	for _, r := range tel.Logs() {
		if r.Severity() == log.SeverityError {
			bodies = append(bodies, r.Body().AsString())
		}
	}
	return bodies
}

// servicesOf returns an Init that returns svcs.
func servicesOf(svcs ...humus.Service) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(context.Context, *humus.Setup) ([]humus.Service, error) {
		return svcs, nil
	}
}

// start runs app on p in a goroutine and returns the channel its status
// arrives on.
func start(t *testing.T, app humus.App, p *bedrocktest.Process) <-chan bedrock.ExitStatus {
	t.Helper()
	status := make(chan bedrock.ExitStatus, 1)
	go func() { status <- app.Run(t.Context(), p.Process()) }()
	return status
}

// TestApp_Run_a_whole_App runs an App with two kinds of Service through a
// whole lifecycle in fake time. The docs site quotes it.
func TestApp_Run_a_whole_App(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// api keeps serving until Shutdown, as an HTTP server does.
		stopped := make(chan struct{})
		var shutdownAt time.Time
		api := &fakeService{
			serve: func(context.Context) error {
				<-stopped
				return nil
			},
			shutdown: func(context.Context) error {
				shutdownAt = time.Now()
				close(stopped)
				return nil
			},
		}

		// consumer stops as soon as Serve's context is done, as a queue
		// consumer does.
		var consumerStoppedAt time.Time
		consumer := &fakeService{serve: func(ctx context.Context) error {
			<-ctx.Done()
			consumerStoppedAt = time.Now()
			return nil
		}}

		var poolClosedAt time.Time
		app := humus.App{
			Host:                humus.OSHost(), // the fake Services never use it
			ShutdownGracePeriod: 10 * time.Second,
			Drain:               humus.DrainDelay(5 * time.Second),
			Telemetry:           telemetry.NoopBuilder,
			Init: func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				s.OnStop(func(context.Context) error {
					poolClosedAt = time.Now()
					return nil
				})
				return []humus.Service{api, consumer}, nil
			},
		}

		p := bedrocktest.NewProcess(t)
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(t.Context(), p.Process()) }()

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if !errors.Is(st.Cause(), context.Canceled) {
			t.Errorf("cause = %v, want the signal", st.Cause())
		}
		if got := consumerStoppedAt.Sub(sigterm); got != 0 {
			t.Errorf("consumer stopped %s after SIGTERM, want at once", got)
		}
		if got := shutdownAt.Sub(sigterm); got != 5*time.Second {
			t.Errorf("Shutdown called %s after SIGTERM, want after the 5s drain", got)
		}
		if api.calls() != 1 || consumer.calls() != 1 {
			t.Errorf("Shutdown called %d and %d times, want once each", api.calls(), consumer.calls())
		}
		if poolClosedAt.IsZero() || poolClosedAt.Before(shutdownAt) {
			t.Errorf("OnStop ran at %v, want it after Shutdown at %v", poolClosedAt, shutdownAt)
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}

func TestApp_Run_SIGTERM_exits_0_with_the_signal_as_its_cause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var serveCtxDone bool
		svc := &fakeService{}
		svc.serve = func(ctx context.Context) error {
			<-ctx.Done()
			serveCtxDone = true
			return nil
		}

		p := bedrocktest.NewProcess(t)
		status := start(t, appWith(servicesOf(svc)), p)

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0", st.Code())
		}
		cause := st.Cause()
		if cause == nil || cause.Error() != "terminated signal received" {
			t.Errorf("cause = %v, want terminated signal received", cause)
		}
		if !errors.Is(cause, context.Canceled) {
			t.Errorf("errors.Is(%v, context.Canceled) = false", cause)
		}
		if !serveCtxDone {
			t.Error("Serve's context was not done")
		}
		if n := svc.calls(); n != 1 {
			t.Errorf("Shutdown called %d times, want 1", n)
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}

func TestApp_Run_interrupt_exits_0(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := bedrocktest.NewProcess(t)
		status := start(t, appWith(servicesOf(&fakeService{})), p)

		<-p.Notified(os.Interrupt)
		p.Signal(os.Interrupt)
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0", st.Code())
		}
		if cause := st.Cause(); cause == nil || cause.Error() != "interrupt signal received" {
			t.Errorf("cause = %v, want interrupt signal received", cause)
		}
	})
}

func TestApp_Run_cancelled_root_context_shuts_down_gracefully(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var shutdownCtxErr error
		svc := &fakeService{shutdown: func(ctx context.Context) error {
			shutdownCtxErr = ctx.Err()
			return nil
		}}
		app := appWith(servicesOf(svc))
		p := bedrocktest.NewProcess(t)

		ctx, cancel := context.WithCancel(t.Context())
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(ctx, p.Process()) }()

		<-p.Notified(syscall.SIGTERM)
		cancel()
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0", st.Code())
		}
		if !errors.Is(st.Cause(), context.Canceled) {
			t.Errorf("cause = %v, want context.Canceled", st.Cause())
		}
		if shutdownCtxErr != nil {
			t.Errorf("Shutdown's context was already done: %v", shutdownCtxErr)
		}
	})
}

func TestApp_Run_shutdown_is_bounded_by_the_grace_period(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// stuck serves until shutdown starts, then never finishes shutting
		// down until its context is done.
		stuck := &fakeService{shutdown: func(ctx context.Context) error {
			<-ctx.Done()
			return fmt.Errorf("stuck shutdown: %w", ctx.Err())
		}}

		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s",
		}))
		app, tel := recorded(appWith(servicesOf(stuck)))
		status := start(t, app, p)

		<-p.Notified(syscall.SIGTERM)
		begin := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-status

		if elapsed := time.Since(begin); elapsed != 2*time.Second {
			t.Errorf("shutdown took %s, want exactly 2s", elapsed)
		}
		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		if !ok {
			t.Fatalf("cause = %v, want a *humus.GracePeriodError", st.Cause())
		}
		if gerr.GracePeriod != 2*time.Second {
			t.Errorf("GracePeriod = %s, want 2s", gerr.GracePeriod)
		}
		if !errors.Is(st.Cause(), context.DeadlineExceeded) {
			t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false", st.Cause())
		}
		if got := strings.Join(errorLogs(tel), "\n"); !strings.Contains(got, "stuck shutdown: context deadline exceeded") {
			t.Errorf("logged errors = %q, want the shutdown error", got)
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}

func TestApp_Run_OnStop_shares_the_grace_period(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := &fakeService{shutdown: func(ctx context.Context) error {
			time.Sleep(time.Second)
			return nil
		}}
		var remaining time.Duration
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(ctx context.Context) error {
				deadline, _ := ctx.Deadline()
				remaining = time.Until(deadline)
				return nil
			})
			return []humus.Service{slow}, nil
		}

		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "3s",
		}))
		status := start(t, appWith(init), p)

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if remaining != 2*time.Second {
			t.Errorf("OnStop had %s of the grace period left, want 2s", remaining)
		}
	})
}

func TestApp_Run_OnStop_running_past_the_grace_period(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(ctx context.Context) error {
				<-ctx.Done()
				return nil
			})
			return []humus.Service{&fakeService{}}, nil
		}

		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "1s",
		}))
		app, tel := recorded(appWith(init))
		status := start(t, app, p)

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if _, ok := errors.AsType[*humus.GracePeriodError](st.Cause()); !ok {
			t.Errorf("cause = %v, want a *humus.GracePeriodError", st.Cause())
		}
		// No function returned an error, so the grace period's own error
		// is the cause's.
		if !errors.Is(st.Cause(), context.DeadlineExceeded) {
			t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false", st.Cause())
		}
		want := []string{"shutdown ran past the grace period of 1s: context deadline exceeded"}
		if got := errorLogs(tel); !slices.Equal(got, want) {
			t.Errorf("logged errors = %q, want %q", got, want)
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}

func TestApp_Run_malformed_grace_period_exits_78_without_calling_Init(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		initCalled := false
		app := appWith(func(context.Context, *humus.Setup) ([]humus.Service, error) {
			initCalled = true
			return nil, nil
		})
		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "30",
		}))

		st := app.Run(t.Context(), p.Process())

		if st.Code() != humus.ExitConfig {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitConfig)
		}
		cerr, ok := errors.AsType[*config.Error](st.Cause())
		if !ok {
			t.Fatalf("cause = %v, want a *config.Error", st.Cause())
		}
		if cerr.Name != "HUMUS_SHUTDOWN_GRACE_PERIOD" {
			t.Errorf("config.Error.Name = %q, want HUMUS_SHUTDOWN_GRACE_PERIOD", cerr.Name)
		}
		if initCalled {
			t.Error("Init was called")
		}
		if want := st.Cause().Error() + "\n"; p.Stderr() != want {
			t.Errorf("stderr = %q, want %q", p.Stderr(), want)
		}
	})
}

func TestApp_Run_missing_config_in_Init_exits_78_and_runs_OnStop_in_reverse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stopped []int
		app := appWith(func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			for i := range 3 {
				s.OnStop(func(context.Context) error {
					stopped = append(stopped, i)
					return nil
				})
			}
			url, err := config.Env(s.Process.Environment, "DATABASE_URL").Read(ctx)
			if err != nil {
				return nil, err
			}
			t.Errorf("DATABASE_URL = %q, want it unset", url)
			return nil, nil
		})
		app, tel := recorded(app)
		p := bedrocktest.NewProcess(t)

		st := app.Run(t.Context(), p.Process())

		if st.Code() != humus.ExitConfig {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitConfig)
		}
		if !errors.Is(st.Cause(), config.ErrNotFound) {
			t.Errorf("errors.Is(%v, config.ErrNotFound) = false", st.Cause())
		}
		if want := []int{2, 1, 0}; !slices.Equal(stopped, want) {
			t.Errorf("OnStop functions ran in order %v, want %v", stopped, want)
		}
		if want := []string{"init: config DATABASE_URL: not found"}; !slices.Equal(errorLogs(tel), want) {
			t.Errorf("logged errors = %q, want %q", errorLogs(tel), want)
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}

func TestApp_Run_failed_Init_exits_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errInit := errors.New("dial database: connection refused")
		errStop := errors.New("close pool")
		app := appWith(func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(context.Context) error { return errStop })
			return nil, errInit
		})
		app, tel := recorded(app)
		p := bedrocktest.NewProcess(t)

		st := app.Run(t.Context(), p.Process())

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if !errors.Is(st.Cause(), errInit) {
			t.Errorf("errors.Is(%v, errInit) = false", st.Cause())
		}
		if !errors.Is(st.Cause(), errStop) {
			t.Errorf("errors.Is(%v, errStop) = false", st.Cause())
		}
		want := []string{"init: dial database: connection refused", "on stop: close pool"}
		if got := errorLogs(tel); !slices.Equal(got, want) {
			t.Errorf("logged errors = %q, want %q", got, want)
		}
	})
}

func TestApp_Run_failed_Init_OnStop_is_bounded_by_the_grace_period(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errInit := errors.New("boom")
		app := appWith(func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			})
			return nil, errInit
		})
		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "5s",
		}))

		begin := time.Now()
		st := app.Run(t.Context(), p.Process())

		if elapsed := time.Since(begin); elapsed != 5*time.Second {
			t.Errorf("Run took %s, want exactly 5s", elapsed)
		}
		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		if !ok {
			t.Fatalf("cause = %v, want a *humus.GracePeriodError", st.Cause())
		}
		if !errors.Is(gerr, errInit) {
			t.Errorf("errors.Is(%v, errInit) = false", gerr)
		}
	})
}

func TestApp_Run_service_returning_nil_exits_0_and_shuts_every_Service_down(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job := &fakeService{serve: func(context.Context) error { return nil }}
		others := []*fakeService{{}, {}}
		var serveCtxErrs []error
		var mu sync.Mutex
		for _, o := range others {
			o.serve = func(ctx context.Context) error {
				<-ctx.Done()
				mu.Lock()
				serveCtxErrs = append(serveCtxErrs, ctx.Err())
				mu.Unlock()
				return nil
			}
		}
		p := bedrocktest.NewProcess(t)

		st := appWith(servicesOf(job, others[0], others[1])).Run(t.Context(), p.Process())

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0", st.Code())
		}
		if st.Cause() != nil {
			t.Errorf("cause = %v, want none", st.Cause())
		}
		for i, svc := range append([]*fakeService{job}, others...) {
			if n := svc.calls(); n != 1 {
				t.Errorf("Service %d: Shutdown called %d times, want 1", i, n)
			}
		}
		if len(serveCtxErrs) != len(others) {
			t.Errorf("%d Serve contexts were done, want %d", len(serveCtxErrs), len(others))
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}

func TestApp_Run_stops_listening_for_signals_when_shutdown_starts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := bedrocktest.NewProcess(t)
		listening := true
		svc := &fakeService{
			serve: func(context.Context) error { return nil },
			shutdown: func(context.Context) error {
				select {
				case <-p.Notified(syscall.SIGTERM):
				default:
					listening = false
				}
				return nil
			},
		}

		st := appWith(servicesOf(svc)).Run(t.Context(), p.Process())

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if listening {
			t.Error("the App was still listening for SIGTERM during Shutdown")
		}
	})
}

func TestApp_Run_failing_service_exits_1_with_its_error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errServe := errors.New("consume orders: broker went away")
		failing := &fakeService{serve: func(context.Context) error { return errServe }}
		other := &fakeService{}
		p := bedrocktest.NewProcess(t)

		app, tel := recorded(appWith(servicesOf(failing, other)))
		st := app.Run(t.Context(), p.Process())

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if !errors.Is(st.Cause(), errServe) {
			t.Errorf("errors.Is(%v, errServe) = false", st.Cause())
		}
		if want := []string{"serve: consume orders: broker went away"}; !slices.Equal(errorLogs(tel), want) {
			t.Errorf("logged errors = %q, want %q", errorLogs(tel), want)
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
		if n := other.calls(); n != 1 {
			t.Errorf("Shutdown called %d times on the other Service, want 1", n)
		}
	})
}

func TestApp_Run_joins_every_error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errServe := errors.New("serve failed")
		errShutdown := errors.New("shutdown failed")
		errLateServe := errors.New("late serve failed")
		errStop := errors.New("stop failed")

		failing := &fakeService{serve: func(context.Context) error { return errServe }}
		late := &fakeService{
			serve: func(ctx context.Context) error {
				<-ctx.Done()
				return errLateServe
			},
			shutdown: func(context.Context) error { return errShutdown },
		}
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(context.Context) error { return errStop })
			return []humus.Service{failing, late}, nil
		}
		p := bedrocktest.NewProcess(t)

		app, tel := recorded(appWith(init))
		st := app.Run(t.Context(), p.Process())

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		for _, want := range []error{errServe, errShutdown, errLateServe, errStop} {
			if !errors.Is(st.Cause(), want) {
				t.Errorf("errors.Is(cause, %v) = false", want)
			}
		}
		if got := errorLogs(tel); len(got) != 4 {
			t.Errorf("%d errors were logged, want 4, one per error: %q", len(got), got)
		}
	})
}

func TestApp_Run_SIGTERM_during_a_slow_Init_cancels_its_context(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		var initCtxErr error
		app := appWith(func(ctx context.Context, _ *humus.Setup) ([]humus.Service, error) {
			close(started)
			<-ctx.Done()
			initCtxErr = context.Cause(ctx)
			return nil, fmt.Errorf("connect to database: %w", context.Cause(ctx))
		})
		p := bedrocktest.NewProcess(t)
		status := start(t, app, p)

		<-started
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status

		if initCtxErr == nil || initCtxErr.Error() != "terminated signal received" {
			t.Errorf("Init's context cause = %v, want terminated signal received", initCtxErr)
		}
		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if !errors.Is(st.Cause(), context.Canceled) {
			t.Errorf("errors.Is(%v, context.Canceled) = false", st.Cause())
		}
	})
}

func TestApp_Run_no_Services_exits_0_and_runs_OnStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopped := false
		app := appWith(func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(context.Context) error {
				stopped = true
				return nil
			})
			return nil, nil
		})
		p := bedrocktest.NewProcess(t)

		st := app.Run(t.Context(), p.Process())

		if st.Code() != 0 || st.Cause() != nil {
			t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
		}
		if !stopped {
			t.Error("the OnStop function did not run")
		}
	})
}

func TestApp_Run_gives_Init_the_Process_and_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := bedrocktest.NewProcess(t, bedrocktest.WithArgs("-v"))
		var gotArgs []string
		var gotHost humus.Host
		app := appWith(func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			gotArgs = s.Process.Args
			gotHost = s.Host
			return nil, nil
		})

		st := app.Run(t.Context(), p.Process())

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0", st.Code())
		}
		if want := []string{bedrocktest.ProgramName, "-v"}; !slices.Equal(gotArgs, want) {
			t.Errorf("Setup.Process.Args = %q, want %q", gotArgs, want)
		}
		if gotHost.Listen == nil || gotHost.Dial == nil || gotHost.FS == nil {
			t.Error("Setup.Host is not the App's Host")
		}
	})
}

func TestApp_Run_does_not_recover_a_panic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := appWith(func(context.Context, *humus.Setup) ([]humus.Service, error) {
			panic("init exploded")
		})
		p := bedrocktest.NewProcess(t)

		got := func() (v any) {
			defer func() { v = recover() }()
			app.Run(t.Context(), p.Process())
			return nil
		}()

		if got != "init exploded" {
			t.Errorf("recovered %v, want the panic from Init", got)
		}
	})
}

func TestApp_is_a_bedrock_App(t *testing.T) {
	var _ bedrock.App = humus.App{}
}

func TestSetup_OnStop_panics_on_nil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("OnStop(nil) did not panic")
		}
	}()
	var s humus.Setup
	s.OnStop(nil)
}

func TestExitCodes(t *testing.T) {
	if humus.ExitFailure != 1 {
		t.Errorf("ExitFailure = %d, want 1", humus.ExitFailure)
	}
	if humus.ExitConfig != 78 {
		t.Errorf("ExitConfig = %d, want 78", humus.ExitConfig)
	}
}

func TestGracePeriodError(t *testing.T) {
	t.Run("with Err", func(t *testing.T) {
		inner := errors.New("stuck")
		err := &humus.GracePeriodError{GracePeriod: 2 * time.Second, Err: inner}

		if want := "shutdown ran past the grace period of 2s: stuck"; err.Error() != want {
			t.Errorf("Error() = %q, want %q", err.Error(), want)
		}
		if !errors.Is(err, inner) {
			t.Error("errors.Is(err, Err) = false")
		}
		if err.Unwrap() != inner {
			t.Error("Unwrap() is not Err")
		}
	})

	t.Run("without Err", func(t *testing.T) {
		err := &humus.GracePeriodError{GracePeriod: time.Minute}

		if want := "shutdown ran past the grace period of 1m0s"; err.Error() != want {
			t.Errorf("Error() = %q, want %q", err.Error(), want)
		}
		if err.Unwrap() != nil {
			t.Errorf("Unwrap() = %v, want nil", err.Unwrap())
		}
	})
}

// The tests below run whole Apps that a signal, or the cancellation of
// Run's context, stops before Run waits for a signal or for the first
// Service to return. Most deliver SIGTERM, or cancel Run's context, while
// the App logs "serving", after Run has started every Serve, and then wait
// with synctest.Wait until a Service that stops on Serve's context, as a
// job, a queue consumer or the health Service does, has returned. When Run
// starts to wait, the signal and that Serve result are both there. A select
// would pick one of them at random, so each test runs tries Apps.

// tries is how many Apps each test runs.
const tries = 20

// hookLogger is the logger of the lifecycle. It records every record's
// body, at its time, and calls onServing when the App logs "serving", on
// Run's goroutine.
type hookLogger struct {
	embedded.Logger
	onServing func()

	mu      sync.Mutex
	records []string
}

func (l *hookLogger) Emit(_ context.Context, r log.Record) {
	body := r.Body().AsString()
	l.mu.Lock()
	l.records = append(l.records, time.Now().UTC().Format("15:04:05.000")+" "+body)
	l.mu.Unlock()
	if body == "serving" && l.onServing != nil {
		l.onServing()
	}
}

func (l *hookLogger) Enabled(context.Context, log.EnabledParameters) bool {
	return true
}

// emitted returns the records l recorded, in order.
func (l *hookLogger) emitted() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.records...)
}

// hookProvider is a LoggerProvider whose every Logger is logger.
type hookProvider struct {
	embedded.LoggerProvider
	logger *hookLogger
}

func (p hookProvider) Logger(string, ...log.LoggerOption) log.Logger {
	return p.logger
}

// hookedTelemetry returns a Builder of no-op telemetry that logs through l
// and whose Shutdown returns shutdownErr.
func hookedTelemetry(l *hookLogger, shutdownErr error) telemetry.Builder {
	return func(context.Context, bedrock.Process, telemetry.Dialer) (telemetry.SDK, error) {
		sdk := telemetry.Noop()
		sdk.LoggerProvider = hookProvider{logger: l}
		sdk.Shutdown = func(context.Context) error { return shutdownErr }
		return sdk, nil
	}
}

// sigtermAsServing returns a hookLogger that delivers SIGTERM to p as the
// App logs "serving", calls also, if it is not nil, and waits until every
// other goroutine in the bubble is blocked, so a Serve that stops on its
// context has returned.
func sigtermAsServing(p *bedrocktest.Process, also func()) *hookLogger {
	return &hookLogger{onServing: func() {
		if also != nil {
			also()
		}
		p.Signal(syscall.SIGTERM)
		synctest.Wait() // a Serve that stops on its context has returned
	}}
}

// sigtermAfterReturned returns a hookLogger that, as the App logs
// "serving", waits until a Service that returns on its own has returned,
// then delivers SIGTERM to p and waits until the signal has come. Run then
// finds the Serve result and the signal both there, the Serve result first.
func sigtermAfterReturned(p *bedrocktest.Process) *hookLogger {
	return &hookLogger{onServing: func() {
		synctest.Wait() // a Serve that returns on its own has returned
		p.Signal(syscall.SIGTERM)
		synctest.Wait() // the signal has come
	}}
}

// Each test checks every App outside its bubble, because synctest.Test
// stops the test at the first App that fails inside it.

// tally counts how many of tries Apps failed a check, and logs it.
type tally struct {
	t      *testing.T
	failed int
}

// fail fails t with the message, for App i.
func (c *tally) fail(i int, format string, args ...any) {
	c.t.Helper()
	c.failed++
	c.t.Errorf("App %d: "+format, append([]any{i + 1}, args...)...)
}

// report logs how many Apps failed, with what.
func (c *tally) report(what string) {
	c.t.Helper()
	c.t.Logf("%s in %d of %d Apps", what, c.failed, tries)
}

func TestApp_Run_a_SIGTERM_as_the_App_starts_serving_is_the_cause(t *testing.T) {
	lost := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		var records []string
		synctest.Test(t, func(t *testing.T) {
			p := bedrocktest.NewProcess(t)
			logs := sigtermAsServing(p, nil)
			app := appWith(servicesOf(&fakeService{}))
			app.Telemetry = hookedTelemetry(logs, nil)

			st = app.Run(t.Context(), p.Process())
			records = logs.emitted()
		})

		t.Logf("App %d: code %d, cause %v, records %q", i+1, st.Code(), st.Cause(), records)
		if st.Code() != 0 {
			t.Errorf("App %d: code = %d, want 0", i+1, st.Code())
		}
		if cause := st.Cause(); cause == nil || cause.Error() != "terminated signal received" {
			lost.fail(i, "cause = %v, want terminated signal received", cause)
		}
	}
	lost.report("the signal was not the cause")
}

func TestApp_Run_a_job_that_returns_nil_after_a_SIGTERM_as_the_App_starts_serving_has_the_signal_as_its_cause(t *testing.T) {
	lost := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		synctest.Test(t, func(t *testing.T) {
			p := bedrocktest.NewProcess(t)
			logs := sigtermAsServing(p, nil)
			init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				run := func(ctx context.Context) error {
					<-ctx.Done()
					return nil // treats stopping early as done
				}
				return []humus.Service{humusjob.NewService(s, run)}, nil
			}
			app := appWith(init)
			app.Telemetry = hookedTelemetry(logs, nil)

			st = app.Run(t.Context(), p.Process())
		})

		t.Logf("App %d: code %d, cause %v", i+1, st.Code(), st.Cause())
		if st.Code() != 0 {
			t.Errorf("App %d: code = %d, want 0", i+1, st.Code())
		}
		if cause := st.Cause(); cause == nil || cause.Error() != "terminated signal received" {
			lost.fail(i, "cause = %v, want terminated signal received", cause)
		}
	}
	lost.report("the signal was not the cause")
}

// TestApp_Run_a_SIGTERM_while_the_App_waits_is_the_cause is the variation
// that passes: SIGTERM comes once Run waits for a signal or for the first
// Service to return, as it does in TestShutdownCause.
func TestApp_Run_a_SIGTERM_while_the_App_waits_is_the_cause(t *testing.T) {
	lost := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		synctest.Test(t, func(t *testing.T) {
			p := bedrocktest.NewProcess(t)
			status := start(t, appWith(servicesOf(&fakeService{})), p)
			<-p.Notified(syscall.SIGTERM)
			synctest.Wait() // Run waits for a signal or for the first Service to return
			p.Signal(syscall.SIGTERM)
			st = <-status
		})

		t.Logf("App %d: code %d, cause %v", i+1, st.Code(), st.Cause())
		if cause := st.Cause(); cause == nil || cause.Error() != "terminated signal received" {
			lost.fail(i, "cause = %v, want terminated signal received", cause)
		}
	}
	lost.report("the signal was not the cause")
}

func TestApp_Run_a_Service_that_returns_nil_on_its_own_before_a_SIGTERM_as_the_App_starts_serving_has_no_cause(t *testing.T) {
	signalled := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		synctest.Test(t, func(t *testing.T) {
			p := bedrocktest.NewProcess(t)
			logs := sigtermAfterReturned(p)
			job := &fakeService{serve: func(context.Context) error { return nil }}
			app := appWith(servicesOf(job))
			app.Telemetry = hookedTelemetry(logs, nil)

			st = app.Run(t.Context(), p.Process())
		})

		if st.Code() != 0 || st.Cause() != nil {
			signalled.fail(i, "status = (%d, %v), want (0, nil): the Service returned before the signal", st.Code(), st.Cause())
		}
	}
	signalled.report("the signal was the cause")
}
