// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// Every test in this file runs an App whole with a Drain, inside a synctest
// bubble, on a bedrocktest Process and a humustest Host.

// drainApp returns an App on a new humustest Host, with a 10s grace period,
// no-op telemetry and drain, whose Init returns svcs.
func drainApp(t *testing.T, drain humus.Drain, svcs ...humus.Service) humus.App {
	return humus.App{
		Host:                humustest.NewHost(t).Host(),
		ShutdownGracePeriod: 10 * time.Second,
		Drain:               drain,
		Telemetry:           telemetry.NoopBuilder,
		Init:                servicesOf(svcs...),
	}
}

// serving returns a fakeService that keeps serving until Shutdown, as an
// HTTP or gRPC server does. Its Shutdown calls shutdown, if it is not nil.
func serving(shutdown func(ctx context.Context) error) *fakeService {
	stopped := make(chan struct{})
	var once sync.Once
	return &fakeService{
		serve: func(context.Context) error {
			<-stopped
			return nil
		},
		shutdown: func(ctx context.Context) error {
			once.Do(func() { close(stopped) })
			if shutdown == nil {
				return nil
			}
			return shutdown(ctx)
		},
	}
}

// countedDrain is a Drain that counts its calls and then calls drain, if
// it is not nil.
type countedDrain struct {
	drain humus.Drain

	mu    sync.Mutex
	calls int
}

func (d *countedDrain) Drain(ctx context.Context, s *humus.Setup) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	if d.drain == nil {
		return nil
	}
	return d.drain(ctx, s)
}

func (d *countedDrain) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// signalled starts app on p, sends SIGTERM once the App listens for it, and
// returns the time it was sent and the channel the status arrives on.
func signalled(t *testing.T, app humus.App, p *bedrocktest.Process) (time.Time, <-chan bedrock.ExitStatus) {
	t.Helper()
	status := start(t, app, p)
	<-p.Notified(syscall.SIGTERM)
	synctest.Wait() // every Serve has started
	sigterm := time.Now()
	p.Signal(syscall.SIGTERM)
	return sigterm, status
}

func TestApp_the_drain_ends_once_every_Serve_has_returned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var shutdownAt time.Time
		svc := &fakeService{
			serve: func(ctx context.Context) error {
				<-ctx.Done()
				time.Sleep(2 * time.Second) // finishing the work in flight
				return nil
			},
			shutdown: func(context.Context) error {
				shutdownAt = time.Now()
				return nil
			},
		}
		app := humus.App{
			Host:                humustest.NewHost(t).Host(),
			ShutdownGracePeriod: 10 * time.Second,
			Drain:               humus.DrainDelay(5 * time.Second),
			Telemetry:           telemetry.NoopBuilder,
			Init:                servicesOf(svc),
		}

		sigterm, status := signalled(t, app, bedrocktest.NewProcess(t))
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if got := shutdownAt.Sub(sigterm); got != 2*time.Second {
			t.Errorf("Shutdown called %s after SIGTERM, want 2s, not the drain's 5s", got)
		}
	})
}

func TestApp_a_Service_that_serves_until_Shutdown_gets_the_whole_drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var shutdownAt time.Time
		svc := serving(func(context.Context) error {
			shutdownAt = time.Now()
			return nil
		})
		app := humus.App{
			Host:                humustest.NewHost(t).Host(),
			ShutdownGracePeriod: 10 * time.Second,
			Drain:               humus.DrainDelay(5 * time.Second),
			Telemetry:           telemetry.NoopBuilder,
			Init:                servicesOf(svc),
		}

		sigterm, status := signalled(t, app, bedrocktest.NewProcess(t))
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if got := shutdownAt.Sub(sigterm); got != 5*time.Second {
			t.Errorf("Shutdown called %s after SIGTERM, want the drain's 5s", got)
		}
	})
}

func TestApp_Run_a_lone_Service_returning_on_its_own_skips_the_Drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &countedDrain{drain: humus.DrainDelay(time.Minute)}
		job := &fakeService{serve: func(context.Context) error { return nil }}
		app := drainApp(t, d.Drain, job)
		p := bedrocktest.NewProcess(t)

		begin := time.Now()
		st := app.Run(t.Context(), p.Process())

		if st.Code() != 0 || st.Cause() != nil {
			t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
		}
		if elapsed := time.Since(begin); elapsed != 0 {
			t.Errorf("Run took %s, want no pause", elapsed)
		}
		if n := d.count(); n != 0 {
			t.Errorf("the Drain was called %d times, want never", n)
		}
		if n := job.calls(); n != 1 {
			t.Errorf("Shutdown called %d times, want 1", n)
		}
	})
}

func TestApp_Run_no_Services_skips_the_Drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &countedDrain{drain: humus.DrainDelay(time.Minute)}
		app := drainApp(t, d.Drain)
		p := bedrocktest.NewProcess(t)

		begin := time.Now()
		st := app.Run(t.Context(), p.Process())

		if st.Code() != 0 || st.Cause() != nil {
			t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
		}
		if elapsed := time.Since(begin); elapsed != 0 {
			t.Errorf("Run took %s, want no pause", elapsed)
		}
		if n := d.count(); n != 0 {
			t.Errorf("the Drain was called %d times, want never", n)
		}
	})
}

func TestApp_Run_a_nil_Drain_does_not_wait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var shutdownAt time.Time
		svc := serving(func(context.Context) error {
			shutdownAt = time.Now()
			return nil
		})

		sigterm, status := signalled(t, drainApp(t, nil, svc), bedrocktest.NewProcess(t))
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if got := shutdownAt.Sub(sigterm); got != 0 {
			t.Errorf("Shutdown called %s after SIGTERM, want at once", got)
		}
	})
}

func TestApp_Run_the_Drain_context_may_be_done_at_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var doneAt time.Time
		d := &countedDrain{drain: func(ctx context.Context, _ *humus.Setup) error {
			<-ctx.Done()
			doneAt = time.Now()
			return nil
		}}
		job := &fakeService{serve: func(context.Context) error { return nil }}
		consumer := &fakeService{} // stops on Serve's context
		app := drainApp(t, d.Drain, job, consumer)
		p := bedrocktest.NewProcess(t)

		begin := time.Now()
		st := app.Run(t.Context(), p.Process())

		if st.Code() != 0 || st.Cause() != nil {
			t.Errorf("status = (%d, %v), want (0, nil)", st.Code(), st.Cause())
		}
		if n := d.count(); n != 1 {
			t.Errorf("the Drain was called %d times, want once", n)
		}
		if got := doneAt.Sub(begin); got != 0 {
			t.Errorf("the Drain's context was done %s after the job returned, want at once", got)
		}
	})
}

func TestApp_Run_a_Drain_returning_ctx_Err_fails_the_App(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		drain := func(ctx context.Context, _ *humus.Setup) error {
			<-ctx.Done()
			return ctx.Err()
		}
		app, tel := recorded(drainApp(t, drain, &fakeService{}))

		_, status := signalled(t, app, bedrocktest.NewProcess(t))
		st := <-status

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if st.Cause() == nil || !strings.Contains(st.Cause().Error(), "drain: context canceled") {
			t.Errorf("cause = %v, want it to hold drain: context canceled", st.Cause())
		}
		if want := []string{"drain: context canceled"}; !slices.Equal(errorLogs(tel), want) {
			t.Errorf("logged errors = %q, want %q", errorLogs(tel), want)
		}
	})
}

func TestApp_Run_a_SIGTERM_as_the_App_starts_serving_calls_the_Drain(t *testing.T) {
	skipped := &tally{t: t}
	for i := range tries {
		var st bedrock.ExitStatus
		var calls int
		synctest.Test(t, func(t *testing.T) {
			d := &countedDrain{}
			p := bedrocktest.NewProcess(t)
			logs := sigtermAsServing(p, nil)
			// The lone Service stops on Serve's context, as a job does, so it
			// has returned on the signal by the time Run waits.
			app := drainApp(t, d.Drain, &fakeService{})
			app.Telemetry = hookedTelemetry(logs, nil)

			st = app.Run(t.Context(), p.Process())
			calls = d.count()
		})

		if calls != 1 {
			skipped.fail(i, "the Drain was called %d times, want once: the Service was serving as the shutdown started", calls)
		}
		if st.Code() != 0 {
			t.Errorf("App %d: code = %d, want 0: %v", i+1, st.Code(), st.Cause())
		}
	}
	skipped.report("the Drain was not called")
}

func TestApp_Run_the_grace_period_starts_after_the_Drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var deadline time.Time
		svc := serving(func(ctx context.Context) error {
			deadline, _ = ctx.Deadline()
			return nil
		})
		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s",
		}))

		sigterm, status := signalled(t, drainApp(t, humus.DrainDelay(3*time.Second), svc), p)
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if got := deadline.Sub(sigterm); got != 5*time.Second {
			t.Errorf("Shutdown's deadline is %s after SIGTERM, want 5s: the 3s Drain, then the 2s grace period", got)
		}
	})
}

func TestApp_Run_a_Serve_error_during_the_drain_fails_the_App(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errServe := errors.New("commit offsets: broker went away")
		consumer := &fakeService{serve: func(ctx context.Context) error {
			<-ctx.Done()
			time.Sleep(time.Second)
			return errServe
		}}
		app, tel := recorded(drainApp(t, humus.DrainDelay(5*time.Second), serving(nil), consumer))

		_, status := signalled(t, app, bedrocktest.NewProcess(t))
		st := <-status

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if !errors.Is(st.Cause(), errServe) {
			t.Errorf("errors.Is(%v, errServe) = false", st.Cause())
		}
		if want := []string{"serve: commit offsets: broker went away"}; !slices.Equal(errorLogs(tel), want) {
			t.Errorf("logged errors = %q, want %q", errorLogs(tel), want)
		}
	})
}

func TestApp_Run_the_Drain_context_keeps_values_but_not_cancellation(t *testing.T) {
	type key struct{}
	synctest.Test(t, func(t *testing.T) {
		var (
			value    any
			errAtEnd error
		)
		drain := func(ctx context.Context, s *humus.Setup) error {
			value = ctx.Value(key{})
			err := humus.DrainDelay(3*time.Second)(ctx, s)
			errAtEnd = ctx.Err()
			return err
		}
		var shutdownAt time.Time
		svc := serving(func(context.Context) error {
			shutdownAt = time.Now()
			return nil
		})
		app := drainApp(t, drain, svc)
		p := bedrocktest.NewProcess(t)

		ctx, cancel := context.WithCancel(context.WithValue(t.Context(), key{}, "request-scoped"))
		status := make(chan bedrock.ExitStatus, 1)
		go func() { status <- app.Run(ctx, p.Process()) }()
		<-p.Notified(syscall.SIGTERM)
		synctest.Wait()
		cancelled := time.Now()
		cancel()
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if value != "request-scoped" {
			t.Errorf("the Drain's context has value %v, want request-scoped", value)
		}
		if errAtEnd != nil {
			t.Errorf("the Drain's context was done with Run's: %v", errAtEnd)
		}
		if got := shutdownAt.Sub(cancelled); got != 3*time.Second {
			t.Errorf("Shutdown called %s after Run's context was cancelled, want after the whole 3s drain", got)
		}
	})
}

func TestApp_Run_a_failing_Drain_fails_the_App_and_still_shuts_down(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errDrain := errors.New("deregister target: load balancer unreachable")
		drain := func(context.Context, *humus.Setup) error { return errDrain }
		api, consumer := serving(nil), &fakeService{}
		app, tel := recorded(drainApp(t, drain, api, consumer))

		_, status := signalled(t, app, bedrocktest.NewProcess(t))
		st := <-status

		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if !errors.Is(st.Cause(), errDrain) {
			t.Errorf("errors.Is(%v, errDrain) = false", st.Cause())
		}
		want := []string{"drain: deregister target: load balancer unreachable"}
		if got := errorLogs(tel); !slices.Equal(got, want) {
			t.Errorf("logged errors = %q, want %q", got, want)
		}
		if api.calls() != 1 || consumer.calls() != 1 {
			t.Errorf("Shutdown called %d and %d times, want once each", api.calls(), consumer.calls())
		}
	})
}

func TestApp_Run_gives_the_Drain_the_Setup_Init_was_given(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var initSetup, drainSetup *humus.Setup
		app := drainApp(t, func(_ context.Context, s *humus.Setup) error {
			drainSetup = s
			return nil
		})
		app.Init = func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			initSetup = s
			return []humus.Service{serving(nil)}, nil
		}

		_, status := signalled(t, app, bedrocktest.NewProcess(t))
		st := <-status

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if initSetup == nil || drainSetup != initSetup {
			t.Errorf("the Drain was given Setup %p, want Init's %p", drainSetup, initSetup)
		}
	})
}

func TestApp_Run_does_not_recover_a_panic_in_the_Drain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		drain := func(context.Context, *humus.Setup) error { panic("drain exploded") }
		job := &fakeService{serve: func(context.Context) error { return nil }}
		app := drainApp(t, drain, job, &fakeService{})
		p := bedrocktest.NewProcess(t)

		got := func() (v any) {
			defer func() { v = recover() }()
			app.Run(t.Context(), p.Process())
			return nil
		}()

		if got != "drain exploded" {
			t.Errorf("recovered %v, want the panic from the Drain", got)
		}
	})
}

func TestDrainDelay(t *testing.T) {
	t.Run("returns nil when its delay ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			begin := time.Now()
			err := humus.DrainDelay(5*time.Second)(t.Context(), nil)

			if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if elapsed := time.Since(begin); elapsed != 5*time.Second {
				t.Errorf("waited %s, want 5s", elapsed)
			}
		})
	})

	t.Run("returns nil when its context is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			begin := time.Now()
			err := humus.DrainDelay(5*time.Second)(ctx, nil)

			if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if elapsed := time.Since(begin); elapsed != time.Second {
				t.Errorf("waited %s, want 1s, when its context was done", elapsed)
			}
		})
	})

	t.Run("of zero returns at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			begin := time.Now()
			err := humus.DrainDelay(0)(t.Context(), nil)

			if err != nil {
				t.Errorf("err = %v, want nil", err)
			}
			if elapsed := time.Since(begin); elapsed != 0 {
				t.Errorf("waited %s, want no pause", elapsed)
			}
		})
	})

	t.Run("panics on a negative duration", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("DrainDelay(-1s) did not panic")
			}
		}()
		humus.DrainDelay(-time.Second)
	})
}
