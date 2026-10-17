// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"errors"
	"maps"
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

// The lifecycle and testing pages of the docs site quote the code in this
// file, and TestDocs_code_samples checks that every quote still matches it.
//
// httpDefaults, grpcDefaults and httpMain stand in for what a runtime
// package whose Services need a drain exports, as Proposal: humus.Main
// sketches it: its defaults as one humus.Option, and a Main built on
// humus.Main. The durations are the HTTP server foundation's and gRPC
// services' proposed ones, which those proposals decide.

// httpDefaults stands in for a runtime package's defaults, such as
// httpserver.Defaults: a 5s drain and a 10s grace period over humus.Main's.
func httpDefaults() humus.Option {
	return func(a *humus.App) {
		a.Drain = humus.DrainDelay(5 * time.Second)
		a.ShutdownGracePeriod = 10 * time.Second
	}
}

// httpMain stands in for a runtime package's Main, such as httpserver.Main:
// humus.Main with the package's defaults applied before the end user's
// options, so that the end user's options win.
func httpMain(init func(ctx context.Context, s *humus.Setup) ([]humus.Service, error), opts ...humus.Option) {
	humus.Main(init, append([]humus.Option{httpDefaults()}, opts...)...)
}

// grpcDefaults stands in for a second runtime package's defaults: a 5s
// drain and a 15s grace period over humus.Main's.
func grpcDefaults() humus.Option {
	return func(a *humus.App) {
		a.Drain = humus.DrainDelay(5 * time.Second)
		a.ShutdownGracePeriod = 15 * time.Second
	}
}

// A runtime package whose Services need a drain wraps humus.Main with its
// defaults, and the end user's main calls the package's Main. Like
// Example_main, this example is compiled but never run.
func Example_runtimeMain() {
	httpMain(Init, humus.WithTelemetry(buildTelemetry))
}

// mainOptions returns the options main passes to humus.Main, so that a
// test passes them too and runs with production's grace period.
func mainOptions() []humus.Option {
	return []humus.Option{
		humus.WithTelemetry(buildTelemetry),
		humus.WithShutdownGracePeriod(30 * time.Second),
	}
}

// A main whose options a test should keep takes them from one function,
// which the test calls too. Like Example_main, this example is compiled but
// never run.
func Example_mainOptions() {
	humus.Main(Init, mainOptions()...)
}

// An end user's own Drain is passed with humus.WithDrain. This one waits
// for a load balancer to deregister the pod, but only after a signal. Like
// Example_main, this example is compiled but never run.
func Example_drain() {
	humus.Main(Init, humus.WithDrain(deregisteringDrain))
}

// withEnv sets the environment Init needs, and adds more to it. Given more
// than once, humustest.WithEnv keeps only the last one, so a helper that
// sets a base environment merges the maps itself.
func withEnv(more map[string]string) humustest.Option {
	env := map[string]string{"DATABASE_URL": "postgres://db.internal:5432/greeter"}
	maps.Copy(env, more)
	return humustest.WithEnv(env)
}

// stopApp sends SIGTERM once the App listens for it and every goroutine in
// the bubble is blocked, and returns the App's exit status. It names
// bedrocktest's Process and bedrock's ExitStatus, so it imports both.
func stopApp(p *bedrocktest.Process, status <-chan bedrock.ExitStatus) bedrock.ExitStatus {
	<-p.Notified(syscall.SIGTERM)
	synctest.Wait()
	p.Signal(syscall.SIGTERM)
	return <-status
}

func TestInit_runs_with_mains_options(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		p, status := humustest.Start(t, Init,
			humustest.WithApp(mainOptions()...),
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
			withEnv(nil),
		)
		st := stopApp(p, status)

		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		// buildTelemetry, which writes JSON lines to stdout, was replaced
		// by the recorder the test passed after it.
		want := "serve: serving\nshutdown: stopped serving\non stop: close the queue\non stop: close the cache\n"
		if got := p.Stdout(); got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}
	})
}

func TestStart_a_runtime_packages_defaults(t *testing.T) {
	cases := []struct {
		name  string
		opts  []humus.Option
		drain time.Duration
		grace time.Duration
	}{
		{"set the drain and the grace period", []humus.Option{httpDefaults()}, 5 * time.Second, 10 * time.Second},
		{"come before the end user's options", []humus.Option{httpDefaults(), humus.WithShutdownGracePeriod(15 * time.Second)}, 5 * time.Second, 15 * time.Second},
		{"are replaced by another package's", []humus.Option{httpDefaults(), grpcDefaults()}, 5 * time.Second, 15 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var shutdownAt time.Time
				var grace time.Duration
				api := serving(func(ctx context.Context) error {
					shutdownAt = time.Now()
					deadline, _ := ctx.Deadline()
					grace = time.Until(deadline)
					return nil
				})

				p, status := humustest.Start(t, servicesOf(api),
					humustest.WithApp(c.opts...),
					humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
				)
				<-p.Notified(syscall.SIGTERM)
				synctest.Wait()
				sigterm := time.Now()
				p.Signal(syscall.SIGTERM)
				st := <-status

				if st.Code() != 0 {
					t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
				}
				if got := shutdownAt.Sub(sigterm); got != c.drain {
					t.Errorf("Shutdown called %s after SIGTERM, want after the %s drain", got, c.drain)
				}
				if grace != c.grace {
					t.Errorf("grace period = %s, want %s", grace, c.grace)
				}
			})
		})
	}
}

func TestStart_a_whole_App(t *testing.T) {
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
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			s.OnStop(func(context.Context) error {
				poolClosedAt = time.Now()
				return nil
			})
			return []humus.Service{api, consumer}, nil
		}

		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithDrain(humus.DrainDelay(5*time.Second))),
		)
		<-p.Notified(syscall.SIGTERM)
		synctest.Wait() // every Serve has started
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
