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

	"github.com/z5labs/humus"
)

// Every test in this file runs the App lifecycle whole, inside a synctest
// bubble, on a bedrocktest Process and fake Services. None opens a socket or
// touches a file: the Host is OSHost, which the fake Services never use.

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

// appWith returns an App on OSHost with a 10s ShutdownGracePeriod whose
// Init is init.
func appWith(init func(ctx context.Context, s *humus.Setup) ([]humus.Service, error)) humus.App {
	return humus.App{
		Host:                humus.OSHost(),
		ShutdownGracePeriod: 10 * time.Second,
		Init:                init,
	}
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
			DrainDelay:          5 * time.Second,
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
			t.Errorf("Shutdown called %s after SIGTERM, want after the 5s drain delay", got)
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

func TestApp_Run_DrainDelay_pauses_before_Shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serveCtxDone := make(chan struct{})
		var (
			shutdownAt        time.Time
			serveDoneAtCall   bool
			shutdownRemaining time.Duration
		)
		svc := &fakeService{
			serve: func(ctx context.Context) error {
				<-ctx.Done()
				close(serveCtxDone)
				return nil
			},
			shutdown: func(ctx context.Context) error {
				shutdownAt = time.Now()
				select {
				case <-serveCtxDone:
					serveDoneAtCall = true
				default:
				}
				deadline, _ := ctx.Deadline()
				shutdownRemaining = time.Until(deadline)
				return nil
			},
		}
		app := appWith(servicesOf(svc))
		app.DrainDelay = 3 * time.Second

		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s",
		}))
		status := start(t, app, p)

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if n := svc.calls(); n != 1 {
			t.Fatalf("Shutdown called %d times, want 1", n)
		}
		if got := shutdownAt.Sub(sigterm); got != 3*time.Second {
			t.Errorf("Shutdown called %s after SIGTERM, want exactly 3s", got)
		}
		if !serveDoneAtCall {
			t.Error("Serve's context was not done when Shutdown was called")
		}
		// The grace period starts after the drain delay, so Shutdown is
		// given all of it.
		if shutdownRemaining != 2*time.Second {
			t.Errorf("Shutdown had %s of the grace period left, want 2s", shutdownRemaining)
		}
	})
}

func TestApp_Run_no_DrainDelay_calls_Shutdown_without_a_pause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var shutdownAt time.Time
		svc := &fakeService{shutdown: func(context.Context) error {
			shutdownAt = time.Now()
			return nil
		}}

		p := bedrocktest.NewProcess(t)
		status := start(t, appWith(servicesOf(svc)), p)

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if n := svc.calls(); n != 1 {
			t.Fatalf("Shutdown called %d times, want 1", n)
		}
		if got := shutdownAt.Sub(sigterm); got != 0 {
			t.Errorf("Shutdown called %s after SIGTERM, want no pause", got)
		}
	})
}

func TestApp_Run_DrainDelay_follows_a_Service_returning_on_its_own(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var shutdownAt time.Time
		job := &fakeService{
			serve: func(context.Context) error { return nil },
			shutdown: func(context.Context) error {
				shutdownAt = time.Now()
				return nil
			},
		}
		app := appWith(servicesOf(job))
		app.DrainDelay = time.Second
		p := bedrocktest.NewProcess(t)

		begin := time.Now()
		st := app.Run(t.Context(), p.Process())

		if st.Code() != 0 || st.Cause() != nil {
			t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
		}
		if got := shutdownAt.Sub(begin); got != time.Second {
			t.Errorf("Shutdown called %s after the Service returned, want exactly 1s", got)
		}
	})
}

func TestApp_Run_no_Services_skips_the_DrainDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := appWith(servicesOf())
		app.DrainDelay = time.Minute
		p := bedrocktest.NewProcess(t)

		begin := time.Now()
		st := app.Run(t.Context(), p.Process())

		if st.Code() != 0 || st.Cause() != nil {
			t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
		}
		if elapsed := time.Since(begin); elapsed != 0 {
			t.Errorf("Run took %s, want no pause", elapsed)
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
		status := start(t, appWith(servicesOf(stuck)), p)

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
		if got := p.Stderr(); !strings.Contains(got, "stuck shutdown: context deadline exceeded") {
			t.Errorf("stderr = %q, want the shutdown error", got)
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
		status := start(t, appWith(init), p)

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
		if want := "shutdown ran past the grace period of 1s: context deadline exceeded\n"; p.Stderr() != want {
			t.Errorf("stderr = %q, want %q", p.Stderr(), want)
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
		if want := "init: config DATABASE_URL: not found\n"; p.Stderr() != want {
			t.Errorf("stderr = %q, want %q", p.Stderr(), want)
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
		want := "init: dial database: connection refused\non stop: close pool\n"
		if p.Stderr() != want {
			t.Errorf("stderr = %q, want %q", p.Stderr(), want)
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

		st := appWith(servicesOf(failing, other)).Run(t.Context(), p.Process())

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if !errors.Is(st.Cause(), errServe) {
			t.Errorf("errors.Is(%v, errServe) = false", st.Cause())
		}
		if want := "serve: consume orders: broker went away\n"; p.Stderr() != want {
			t.Errorf("stderr = %q, want %q", p.Stderr(), want)
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

		st := appWith(init).Run(t.Context(), p.Process())

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		for _, want := range []error{errServe, errShutdown, errLateServe, errStop} {
			if !errors.Is(st.Cause(), want) {
				t.Errorf("errors.Is(cause, %v) = false", want)
			}
		}
		lines := strings.Split(strings.TrimSuffix(p.Stderr(), "\n"), "\n")
		if len(lines) != 4 {
			t.Errorf("stderr has %d lines, want 4: %q", len(lines), p.Stderr())
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
