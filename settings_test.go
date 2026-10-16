// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
)

// Every test in this file reads the lifecycle's settings through Run: the
// root package exports no API that reads the environment.

// shutdownTiming runs app on p with one Service, sends SIGTERM once the App
// listens for it, and returns how long after SIGTERM Shutdown was called and
// how much of the grace period its context had left.
func shutdownTiming(t *testing.T, app humus.App, p *bedrocktest.Process) (drain, grace time.Duration) {
	t.Helper()
	var shutdownAt time.Time
	svc := &fakeService{shutdown: func(ctx context.Context) error {
		shutdownAt = time.Now()
		deadline, _ := ctx.Deadline()
		grace = time.Until(deadline)
		return nil
	}}
	app.Init = servicesOf(svc)
	status := start(t, app, p)

	<-p.Notified(syscall.SIGTERM)
	sigterm := time.Now()
	p.Signal(syscall.SIGTERM)
	st := <-status

	if st.Code() != 0 {
		t.Fatalf("code = %d, want 0: %v", st.Code(), st.Cause())
	}
	if n := svc.calls(); n != 1 {
		t.Fatalf("Shutdown called %d times, want 1", n)
	}
	return shutdownAt.Sub(sigterm), grace
}

func TestApp_Run_settings_default_to_the_App_fields(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := humus.App{
			Host:                humus.OSHost(),
			ShutdownGracePeriod: 4 * time.Second,
			DrainDelay:          2 * time.Second,
		}
		p := bedrocktest.NewProcess(t)

		drain, grace := shutdownTiming(t, app, p)

		if drain != 2*time.Second {
			t.Errorf("Shutdown called %s after SIGTERM, want the 2s DrainDelay", drain)
		}
		if grace != 4*time.Second {
			t.Errorf("Shutdown had %s of the grace period, want the 4s ShutdownGracePeriod", grace)
		}
	})
}

func TestApp_Run_HUMUS_SHUTDOWN_DRAIN_DELAY_overrides_DrainDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := humus.App{
			Host:                humus.OSHost(),
			ShutdownGracePeriod: 4 * time.Second,
		}
		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_DRAIN_DELAY": "3s",
		}))

		drain, grace := shutdownTiming(t, app, p)

		if drain != 3*time.Second {
			t.Errorf("Shutdown called %s after SIGTERM, want 3s", drain)
		}
		if grace != 4*time.Second {
			t.Errorf("Shutdown had %s of the grace period, want 4s", grace)
		}
	})
}

func TestApp_Run_HUMUS_SHUTDOWN_GRACE_PERIOD_overrides_ShutdownGracePeriod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := humus.App{
			Host:                humus.OSHost(),
			ShutdownGracePeriod: 4 * time.Second,
		}
		p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "1m30s",
		}))

		drain, grace := shutdownTiming(t, app, p)

		if drain != 0 {
			t.Errorf("Shutdown called %s after SIGTERM, want no pause", drain)
		}
		if grace != 90*time.Second {
			t.Errorf("Shutdown had %s of the grace period, want 1m30s", grace)
		}
	})
}

// runConfigFailure runs app on a Process with env, and checks that it exits
// ExitConfig without calling Init and writes its cause to stderr. It returns
// the cause.
func runConfigFailure(t *testing.T, app humus.App, env map[string]string) error {
	t.Helper()
	initCalled := false
	app.Init = func(context.Context, *humus.Setup) ([]humus.Service, error) {
		initCalled = true
		return nil, nil
	}
	p := bedrocktest.NewProcess(t, bedrocktest.WithEnv(env))

	st := app.Run(t.Context(), p.Process())

	if st.Code() != humus.ExitConfig {
		t.Errorf("code = %d, want %d", st.Code(), humus.ExitConfig)
	}
	if initCalled {
		t.Error("Init was called")
	}
	if st.Cause() == nil {
		t.Fatal("cause = nil, want the config error")
	}
	if want := st.Cause().Error() + "\n"; p.Stderr() != want {
		t.Errorf("stderr = %q, want %q", p.Stderr(), want)
	}
	return st.Cause()
}

func TestApp_Run_zero_ShutdownGracePeriod_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cause := runConfigFailure(t, humus.App{Host: humus.OSHost()}, nil)

		if want := "config HUMUS_SHUTDOWN_GRACE_PERIOD: must be positive"; cause.Error() != want {
			t.Errorf("cause = %q, want %q", cause, want)
		}
		cerr, ok := errors.AsType[*config.Error](cause)
		if !ok {
			t.Fatalf("cause = %v, want a *config.Error", cause)
		}
		if cerr.Name != "HUMUS_SHUTDOWN_GRACE_PERIOD" {
			t.Errorf("config.Error.Name = %q, want HUMUS_SHUTDOWN_GRACE_PERIOD", cerr.Name)
		}
	})
}

func TestApp_Run_a_grace_period_that_is_not_positive_exits_78(t *testing.T) {
	for _, value := range []string{"0s", "-5s"} {
		t.Run(value, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app := humus.App{Host: humus.OSHost(), ShutdownGracePeriod: 10 * time.Second}

				cause := runConfigFailure(t, app, map[string]string{
					"HUMUS_SHUTDOWN_GRACE_PERIOD": value,
				})

				if want := "config HUMUS_SHUTDOWN_GRACE_PERIOD: must be positive"; cause.Error() != want {
					t.Errorf("cause = %q, want %q", cause, want)
				}
			})
		})
	}
}

func TestApp_Run_negative_drain_delay_exits_78(t *testing.T) {
	cases := []struct {
		name  string
		delay time.Duration
		env   map[string]string
	}{
		{name: "DrainDelay", delay: -time.Second},
		{name: "HUMUS_SHUTDOWN_DRAIN_DELAY", env: map[string]string{"HUMUS_SHUTDOWN_DRAIN_DELAY": "-1s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				app := humus.App{
					Host:                humus.OSHost(),
					ShutdownGracePeriod: 10 * time.Second,
					DrainDelay:          c.delay,
				}

				cause := runConfigFailure(t, app, c.env)

				if want := "config HUMUS_SHUTDOWN_DRAIN_DELAY: must not be negative"; cause.Error() != want {
					t.Errorf("cause = %q, want %q", cause, want)
				}
				cerr, ok := errors.AsType[*config.Error](cause)
				if !ok {
					t.Fatalf("cause = %v, want a *config.Error", cause)
				}
				if cerr.Name != "HUMUS_SHUTDOWN_DRAIN_DELAY" {
					t.Errorf("config.Error.Name = %q, want HUMUS_SHUTDOWN_DRAIN_DELAY", cerr.Name)
				}
			})
		})
	}
}

func TestApp_Run_malformed_settings_are_reported_together(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := humus.App{Host: humus.OSHost(), ShutdownGracePeriod: 10 * time.Second}

		cause := runConfigFailure(t, app, map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "30",
			"HUMUS_SHUTDOWN_DRAIN_DELAY":  "soon",
		})

		lines := strings.Split(cause.Error(), "\n")
		if len(lines) != 2 {
			t.Fatalf("cause has %d lines, want one per variable: %q", len(lines), cause)
		}
		for i, name := range []string{"HUMUS_SHUTDOWN_GRACE_PERIOD", "HUMUS_SHUTDOWN_DRAIN_DELAY"} {
			if prefix := "config " + name + ": "; !strings.HasPrefix(lines[i], prefix) {
				t.Errorf("cause line %d = %q, want it to start %q", i, lines[i], prefix)
			}
		}
	})
}
