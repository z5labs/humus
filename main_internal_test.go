// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/internal/defaults"
	"github.com/z5labs/humus/telemetry"
)

// Main itself exits the process, so these tests check mainApp, the function
// Main builds its App with. A test that runs that App does so on a
// bedrocktest Process inside a synctest bubble, with Services that never
// touch the Host, so Main's OSHost default is safe to keep.

// funcPointer returns the code pointer of f, a func value, so that two
// references to the same top-level function compare equal.
func funcPointer(f any) uintptr {
	return reflect.ValueOf(f).Pointer()
}

// noFaults is a Faults function that returns the default Settings.
func noFaults(context.Context, *Setup) (fault.Settings, error) { return fault.DefaultSettings(), nil }

// noInit is an Init that returns no Services.
func noInit(context.Context, *Setup) ([]Service, error) { return nil, nil }

// stopsOnContext is a Service that returns once Serve's context is done, as
// a job or a queue consumer does, and counts its Serve and Shutdown calls.
type stopsOnContext struct {
	served, shutdowns atomic.Int32
}

func (s *stopsOnContext) Serve(ctx context.Context) error {
	s.served.Add(1)
	<-ctx.Done()
	return nil
}

func (s *stopsOnContext) Shutdown(context.Context) error {
	s.shutdowns.Add(1)
	return nil
}

// runUntilSIGTERM runs app on a new bedrocktest Process, sends it SIGTERM
// once it is listening for signals, and returns the Process and the App's
// status.
func runUntilSIGTERM(t *testing.T, app App) (*bedrocktest.Process, bedrock.ExitStatus) {
	t.Helper()
	p, status := bedrocktest.Start(t, app)
	<-p.Notified(syscall.SIGTERM)
	synctest.Wait()
	p.Signal(syscall.SIGTERM)
	return p, <-status
}

func TestMainApp_has_Mains_defaults(t *testing.T) {
	app := mainApp(noInit)

	require.Equal(t, funcPointer(net.Listen), funcPointer(app.Host.Listen), "Host.Listen is OSHost's")
	require.NotNil(t, app.Host.Dial, "Host.Dial is OSHost's")
	require.Equal(t, osFS{}, app.Host.FS, "Host.FS is OSHost's")
	require.Equal(t, 20*time.Second, app.ShutdownGracePeriod)
	require.Nil(t, app.Drain, "Main has no drain")
	require.Nil(t, app.Faults, "Main has no failure injection")
	require.Equal(t, funcPointer(defaults.Telemetry), funcPointer(app.Telemetry), "the telemetry default")
	require.Equal(t, funcPointer(noInit), funcPointer(app.Init))
}

func TestMainApp_sets_Init_before_it_applies_opts(t *testing.T) {
	var seen func(context.Context, *Setup) ([]Service, error)
	mainApp(noInit, func(a *App) { seen = a.Init })

	require.NotNil(t, seen, "an Option sees Init")
	require.Equal(t, funcPointer(noInit), funcPointer(seen))
}

func TestMainApp_applies_opts_in_order_after_the_defaults(t *testing.T) {
	var order []string
	record := func(name string) Option {
		return func(a *App) {
			// Each Option sees what the ones before it set.
			order = append(order, name+"@"+a.ShutdownGracePeriod.String())
		}
	}
	h := Host{FS: osFS{}}

	app := mainApp(noInit,
		record("first"),
		WithShutdownGracePeriod(time.Second),
		record("second"),
		WithShutdownGracePeriod(2*time.Second),
		record("third"),
		WithHost(h),
		WithTelemetry(telemetry.NoopBuilder),
		WithDrain(DrainDelay(time.Second)),
		WithFaults(noFaults),
	)

	require.Equal(t, []string{"first@20s", "second@1s", "third@2s"}, order)
	require.Equal(t, 2*time.Second, app.ShutdownGracePeriod, "the later Option wins")
	require.Nil(t, app.Host.Listen, "WithHost replaced OSHost")
	require.Equal(t, funcPointer(telemetry.NoopBuilder), funcPointer(app.Telemetry))
	require.NotNil(t, app.Drain)
	require.Equal(t, funcPointer(noFaults), funcPointer(app.Faults))
}

// runtimeDefaults is the sketch of a runtime package's defaults from
// Proposal: humus.Main: a 5s drain and a 10s grace period over Main's.
func runtimeDefaults() Option {
	return func(a *App) {
		a.Drain = DrainDelay(5 * time.Second)
		a.ShutdownGracePeriod = 10 * time.Second
	}
}

// runtimeMain is the sketch of a runtime package's Main: humus.Main with
// the package's defaults before the end user's options. It returns the App
// in place of running it.
func runtimeMain(init func(ctx context.Context, s *Setup) ([]Service, error), opts ...Option) App {
	return mainApp(init, append([]Option{runtimeDefaults()}, opts...)...)
}

func TestMainApp_a_runtime_packages_defaults_and_the_end_users_options_compose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The end user's grace period wins over the package's 10s, and the
		// package's drain stays, since the end user doesn't set one.
		app := runtimeMain(noInit, WithShutdownGracePeriod(12*time.Second))
		require.Equal(t, 12*time.Second, app.ShutdownGracePeriod)
		require.NotNil(t, app.Drain)

		// Run, the package's drain is 5s, and the grace period after it is
		// the end user's.
		var shutdownAt time.Time
		var grace time.Duration
		stopped := make(chan struct{})
		app.Init = func(context.Context, *Setup) ([]Service, error) {
			return []Service{&untilShutdown{
				stopped: stopped,
				shutdown: func(ctx context.Context) {
					shutdownAt = time.Now()
					deadline, _ := ctx.Deadline()
					grace = time.Until(deadline)
				},
			}}, nil
		}
		app.Telemetry = telemetry.NoopBuilder

		p, status := bedrocktest.Start(t, app)
		<-p.Notified(syscall.SIGTERM)
		synctest.Wait()
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, 5*time.Second, shutdownAt.Sub(sigterm), "the package's drain")
		require.Equal(t, 12*time.Second, grace, "the end user's grace period")
	})
}

// untilShutdown is a Service that keeps serving until Shutdown, as an HTTP
// server does. shutdown, if not nil, is called with Shutdown's context.
type untilShutdown struct {
	stopped  chan struct{}
	shutdown func(ctx context.Context)
}

func (s *untilShutdown) Serve(context.Context) error {
	<-s.stopped
	return nil
}

func (s *untilShutdown) Shutdown(ctx context.Context) error {
	if s.shutdown != nil {
		s.shutdown(ctx)
	}
	close(s.stopped)
	return nil
}

func TestMainApp_an_Option_that_wraps_Init_runs_its_Service_next_to_the_end_users(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		own := &stopsOnContext{}
		added := &stopsOnContext{}
		// withProbes is the shape a package's Option could take: it runs
		// the end user's Init and adds a Service of its own beside theirs.
		withProbes := func(a *App) {
			init := a.Init
			a.Init = func(ctx context.Context, s *Setup) ([]Service, error) {
				svcs, err := init(ctx, s)
				if err != nil {
					return nil, err
				}
				return append(svcs, added), nil
			}
		}

		app := mainApp(func(context.Context, *Setup) ([]Service, error) {
			return []Service{own}, nil
		}, WithTelemetry(telemetry.NoopBuilder), withProbes)
		_, st := runUntilSIGTERM(t, app)

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.EqualValues(t, 1, own.served.Load(), "Serve calls on the end user's Service")
		require.EqualValues(t, 1, added.served.Load(), "Serve calls on the added Service")
		require.EqualValues(t, 1, own.shutdowns.Load(), "Shutdown calls on the end user's Service")
		require.EqualValues(t, 1, added.shutdowns.Load(), "Shutdown calls on the added Service")
	})
}

func TestWithDrain_nil_removes_a_drain_an_earlier_Option_set(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := mainApp(noInit, runtimeDefaults(), WithDrain(nil))
		require.Nil(t, app.Drain)

		// Run, a Service that serves until Shutdown is shut down at the
		// same fake instant as the signal: there is no drain.
		var shutdownAt time.Time
		stopped := make(chan struct{})
		app.Init = func(context.Context, *Setup) ([]Service, error) {
			return []Service{&untilShutdown{
				stopped:  stopped,
				shutdown: func(context.Context) { shutdownAt = time.Now() },
			}}, nil
		}
		app.Telemetry = telemetry.NoopBuilder

		p, status := bedrocktest.Start(t, app)
		<-p.Notified(syscall.SIGTERM)
		synctest.Wait()
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Zero(t, shutdownAt.Sub(sigterm))
	})
}

// TestMainApp_the_telemetry_default_writes_JSON_lines_to_stdout runs the
// App mainApp builds, telemetry default and all, and reads the lifecycle's
// log records off the Process's stdout.
func TestMainApp_the_telemetry_default_writes_JSON_lines_to_stdout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc := &stopsOnContext{}
		app := mainApp(func(context.Context, *Setup) ([]Service, error) {
			return []Service{svc}, nil
		})

		p, st := runUntilSIGTERM(t, app)

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
		var bodies []string
		for line := range strings.Lines(p.Stdout()) {
			var rec struct {
				Severity string `json:"severity_text"`
				Body     string `json:"body"`
			}
			require.NoError(t, json.Unmarshal([]byte(line), &rec), "stdout line %q", line)
			require.Equal(t, "INFO", rec.Severity, "stdout line %q", line)
			bodies = append(bodies, rec.Body)
		}
		require.Equal(t, []string{"serving", "shutting down", "stopped"}, bodies)
	})
}

func TestMainApp_panics(t *testing.T) {
	require.PanicsWithValue(t, "humus: Main called with a nil init", func() { mainApp(nil) })
	require.PanicsWithValue(t, "humus: Main called with a nil Option", func() {
		mainApp(noInit, WithDrain(nil), nil)
	})
}
