// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/internal/defaults"
	"github.com/z5labs/humus/telemetry"
)

// Main and MainOn exit the process, so these tests check mainApp, the
// function both build their bedrock.App with, and Defaults and Apply, which
// it runs. A test that runs that App does so on a bedrocktest Process inside
// a synctest bubble, with Services that never touch the host, so Main's
// OSHost default is safe to keep.

// funcPointer returns the code pointer of f, a func value, so that two
// references to the same top-level function compare equal.
func funcPointer(f any) uintptr {
	return reflect.ValueOf(f).Pointer()
}

// noFaults is a Faults function that returns the default Settings.
func noFaults(context.Context, *Setup, Host) (fault.Settings, error) {
	return fault.DefaultSettings(), nil
}

// noInit is an Init that returns no Services.
func noInit(context.Context, *Setup, Host) ([]Service, error) { return nil, nil }

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
func runUntilSIGTERM(t *testing.T, app bedrock.App) (*bedrocktest.Process, bedrock.ExitStatus) {
	t.Helper()
	p, status := bedrocktest.Start(t, app)
	<-p.Notified(syscall.SIGTERM)
	synctest.Wait()
	p.Signal(syscall.SIGTERM)
	return p, <-status
}

// ownHost is a host of an App's own, beside humus's Host: it embeds its
// network, so it is a Network, and has a field humus doesn't know.
type ownHost struct {
	IPNetwork
	name string
}

// countingAcquire returns an acquire for h that counts its calls in n.
func countingAcquire[H any](h H, n *atomic.Int32) func() (H, error) {
	return func() (H, error) {
		n.Add(1)
		return h, nil
	}
}

func TestDefaults_are_Mains_defaults(t *testing.T) {
	h := OSHost()
	app := Defaults(h, noInit)

	require.Equal(t, h, app.Host, "the host is h")
	require.Equal(t, 20*time.Second, app.ShutdownGracePeriod)
	require.Nil(t, app.Drain, "Main has no drain")
	require.Nil(t, app.Faults, "Main has no failure injection")
	require.Equal(t, funcPointer(defaults.Telemetry), funcPointer(app.Telemetry), "the telemetry default")
	require.Equal(t, funcPointer(noInit), funcPointer(app.Init))
}

func TestApply_applies_opts_in_order(t *testing.T) {
	app := Defaults(OSHost(), noInit)

	err := app.Apply(
		WithShutdownGracePeriod(time.Second),
		WithShutdownGracePeriod(2*time.Second),
		WithTelemetry(telemetry.NoopBuilder),
		WithDrain(DrainDelay(time.Second)),
		WithFaults(noFaults),
	)

	require.NoError(t, err)
	require.Equal(t, 2*time.Second, app.ShutdownGracePeriod, "the later Option wins")
	require.Equal(t, funcPointer(telemetry.NoopBuilder), funcPointer(app.Telemetry))
	require.NotNil(t, app.Drain)
	require.Equal(t, funcPointer(noFaults), funcPointer(app.Faults))
	require.Equal(t, OSHost(), app.Host, "no Option sets the host")
}

func TestApply_rejects_an_option_for_another_host_type(t *testing.T) {
	ownFaults := func(context.Context, *Setup, ownHost) (fault.Settings, error) {
		return fault.DefaultSettings(), nil
	}
	app := Defaults(OSHost(), noInit)
	before := app

	err := app.Apply(WithShutdownGracePeriod(time.Second), WithFaults(ownFaults))

	require.EqualError(t, err, "humus: WithFaults was made for an App on humus.ownHost, not on humus.Host")
	require.Equal(t, before.ShutdownGracePeriod, app.ShutdownGracePeriod, "Apply changed nothing")
	require.Nil(t, app.Faults, "Apply changed nothing")
}

func TestApply_rejects_the_zero_Option(t *testing.T) {
	app := Defaults(OSHost(), noInit)

	err := app.Apply(WithShutdownGracePeriod(time.Second), Option{})

	require.EqualError(t, err, "humus: option 1 is the zero Option: only humus's With functions make an Option")
	require.Equal(t, 20*time.Second, app.ShutdownGracePeriod, "Apply changed nothing")
}

func TestWithFaults_nil_removes_the_faults_an_earlier_Option_set(t *testing.T) {
	app := Defaults(OSHost(), noInit)

	require.NoError(t, app.Apply(WithFaults(noFaults), WithFaults[Host](nil)))
	require.Nil(t, app.Faults)
}

// noScenarios is a fit.Decoder that finds no scenario in any baggage.
type noScenarios struct{}

func (noScenarios) Decode(baggage.Baggage) (fit.Scenario, bool, error) {
	return fit.Scenario{}, false, nil
}

func (noScenarios) Strip(b baggage.Baggage) baggage.Baggage { return b }

// dialRecorder is an App's own host that records, by caller, every address
// dialled through it, and refuses the dial.
type dialRecorder struct {
	IPNetwork
	dialled chan string
}

func (d dialRecorder) DialTCP(_ context.Context, _, address string) (net.Conn, error) {
	d.dialled <- address
	return nil, errors.New("refused")
}

// TestApp_runs_every_option_against_the_apps_host puts every Option in one
// list with no type argument, on an App on a host of its own: telemetry and
// the Drain dial through the App's host, and Faults gets that host before
// Init.
func TestApp_runs_every_option_against_the_apps_host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := dialRecorder{dialled: make(chan string, 2)}
		var order []string
		var faultsHost, initHost dialRecorder
		build := func(ctx context.Context, _ bedrock.Process, n telemetry.Network) (telemetry.SDK, error) {
			_, _ = n.DialTCP(ctx, "tcp", "collector:4317")
			return telemetry.Noop(), nil
		}
		drain := func(ctx context.Context, _ *Setup, n Network) error {
			_, _ = n.DialTCP(ctx, "tcp", "balancer:80")
			return nil
		}
		faults := func(_ context.Context, _ *Setup, h dialRecorder) (fault.Settings, error) {
			order = append(order, "faults")
			faultsHost = h
			s := fault.DefaultSettings()
			s.Decoder = noScenarios{}
			return s, nil
		}
		svc := &untilShutdown{stopped: make(chan struct{})}
		init := func(_ context.Context, _ *Setup, h dialRecorder) ([]Service, error) {
			order = append(order, "init")
			initHost = h
			return []Service{svc}, nil
		}
		var acquired atomic.Int32

		app := mainApp("MainOn", countingAcquire(h, &acquired), init,
			WithShutdownGracePeriod(time.Second),
			WithTelemetry(build),
			WithDrain(drain),
			WithFaults(faults),
		)
		_, st := runUntilSIGTERM(t, app)

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.EqualValues(t, 1, acquired.Load(), "acquire is called once")
		require.Equal(t, "collector:4317", <-h.dialled, "telemetry dials through the App's host")
		require.Equal(t, "balancer:80", <-h.dialled, "the Drain dials through the App's host")
		require.Equal(t, []string{"faults", "init"}, order, "Faults runs before Init")
		require.Equal(t, h.dialled, faultsHost.dialled, "Faults gets the App's host")
		require.Equal(t, h.dialled, initHost.dialled, "Init gets the App's host")
	})
}

// TestApp_on_its_own_host_gives_Setup_the_zero_Host checks the stopgap: a
// runtime that still reads Setup.Host gets the App's host only when it is
// the default host.
func TestApp_on_its_own_host_gives_Setup_the_zero_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got Host
		init := func(_ context.Context, s *Setup, _ ownHost) ([]Service, error) {
			got = s.Host
			return nil, nil
		}
		app := Defaults(ownHost{IPNetwork: OSHost().IPNetwork, name: "own"}, init)
		app.Telemetry = telemetry.NoopBuilder

		_, status := bedrocktest.Start(t, app)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, Host{}, got)
	})
}

func TestApp_on_the_default_host_gives_Setup_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got Host
		init := func(_ context.Context, s *Setup, _ Host) ([]Service, error) {
			got = s.Host
			return nil, nil
		}
		app := Defaults(OSHost(), init)
		app.Telemetry = telemetry.NoopBuilder

		_, status := bedrocktest.Start(t, app)
		st := <-status

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, OSHost(), got)
	})
}

// TestMainApp_a_failed_Apply_exits_ExitConfig_without_acquiring runs the
// App of Main and of MainOn with an Option made for another host type and
// with the zero Option: each exits ExitConfig, with Apply's error on
// stderr, and never calls acquire.
func TestMainApp_a_failed_Apply_exits_ExitConfig_without_acquiring(t *testing.T) {
	ownFaults := func(context.Context, *Setup, ownHost) (fault.Settings, error) {
		return fault.DefaultSettings(), nil
	}
	cases := []struct {
		name string
		opt  Option
		want string
	}{
		{"an Option for another host type", WithFaults(ownFaults), "humus: WithFaults was made for an App on humus.ownHost, not on humus.Host"},
		{"the zero Option", Option{}, "humus: option 1 is the zero Option: only humus's With functions make an Option"},
	}
	for _, caller := range []string{"Main", "MainOn"} {
		for _, c := range cases {
			t.Run(caller+"/"+c.name, func(t *testing.T) {
				var acquired atomic.Int32
				app := mainApp(caller, countingAcquire(OSHost(), &acquired), noInit, WithTelemetry(telemetry.NoopBuilder), c.opt)

				p, status := bedrocktest.Start(t, app)
				st := <-status

				require.Equal(t, ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.EqualError(t, st.Cause(), c.want)
				require.Equal(t, c.want+"\n", p.Stderr())
				require.Zero(t, acquired.Load(), "acquire was called")
			})
		}
	}
}

// TestMainApp_an_error_from_acquire_exits_as_an_error_from_Init_does
// checks both codes: ExitConfig for a *config.Error in the chain, and
// ExitFailure otherwise. Init is never called.
func TestMainApp_an_error_from_acquire_exits_as_an_error_from_Init_does(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"a config error", &config.Error{Name: "HOST_ADDR", Err: errors.New("is not set")}, ExitConfig},
		{"any other error", errors.New("no network"), ExitFailure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			initCalled := false
			init := func(context.Context, *Setup, ownHost) ([]Service, error) {
				initCalled = true
				return nil, nil
			}
			acquire := func() (ownHost, error) { return ownHost{}, c.err }
			app := mainApp("MainOn", acquire, init, WithTelemetry(telemetry.NoopBuilder))

			p, status := bedrocktest.Start(t, app)
			st := <-status

			require.Equal(t, c.code, st.Code(), "cause: %v", st.Cause())
			require.ErrorIs(t, st.Cause(), c.err)
			require.Equal(t, "acquire the host: "+c.err.Error()+"\n", p.Stderr())
			require.False(t, initCalled, "Init was called")
		})
	}
}

// runtimeDefaults is the sketch of a runtime package's defaults: a 5s drain
// and a 10s grace period over Main's.
func runtimeDefaults() []Option {
	return []Option{
		WithDrain(DrainDelay(5 * time.Second)),
		WithShutdownGracePeriod(10 * time.Second),
	}
}

// runtimeMain is the sketch of a runtime package's Main: humus.Main with
// the package's defaults before the end user's options. It returns the App
// in place of running it.
func runtimeMain(init Init[Host], opts ...Option) bedrock.App {
	return mainApp("Main", acquireOSHost, init, append(runtimeDefaults(), opts...)...)
}

func TestMainApp_a_runtime_packages_defaults_and_the_end_users_options_compose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The end user's grace period wins over the package's 10s, and the
		// package's drain stays, since the end user doesn't set one. Run,
		// the package's drain is 5s, and the grace period after it is the
		// end user's.
		var shutdownAt time.Time
		var grace time.Duration
		stopped := make(chan struct{})
		init := func(context.Context, *Setup, Host) ([]Service, error) {
			return []Service{&untilShutdown{
				stopped: stopped,
				shutdown: func(ctx context.Context) {
					shutdownAt = time.Now()
					deadline, _ := ctx.Deadline()
					grace = time.Until(deadline)
				},
			}}, nil
		}
		app := runtimeMain(init, WithShutdownGracePeriod(12*time.Second), WithTelemetry(telemetry.NoopBuilder))

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

// TestMainApp_an_init_wrapped_by_a_package_runs_its_Service_next_to_the_end_users
// wraps init, as a package or a test that adds a Service does now that no
// Option can wrap it.
func TestMainApp_an_init_wrapped_by_a_package_runs_its_Service_next_to_the_end_users(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		own := &stopsOnContext{}
		added := &stopsOnContext{}
		// withProbes runs the end user's Init and adds a Service of its own
		// beside theirs.
		withProbes := func(init Init[Host]) Init[Host] {
			return func(ctx context.Context, s *Setup, h Host) ([]Service, error) {
				svcs, err := init(ctx, s, h)
				if err != nil {
					return nil, err
				}
				return append(svcs, added), nil
			}
		}

		app := mainApp("Main", acquireOSHost, withProbes(func(context.Context, *Setup, Host) ([]Service, error) {
			return []Service{own}, nil
		}), WithTelemetry(telemetry.NoopBuilder))
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
		// Run, a Service that serves until Shutdown is shut down at the
		// same fake instant as the signal: there is no drain.
		var shutdownAt time.Time
		stopped := make(chan struct{})
		init := func(context.Context, *Setup, Host) ([]Service, error) {
			return []Service{&untilShutdown{
				stopped:  stopped,
				shutdown: func(context.Context) { shutdownAt = time.Now() },
			}}, nil
		}
		app := Defaults(OSHost(), init)
		require.NoError(t, app.Apply(append(runtimeDefaults(), WithDrain(nil), WithTelemetry(telemetry.NoopBuilder))...))
		require.Nil(t, app.Drain)

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
		app := mainApp("Main", acquireOSHost, func(context.Context, *Setup, Host) ([]Service, error) {
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

func TestAcquireOSHost_returns_OSHost(t *testing.T) {
	h, err := acquireOSHost()
	require.NoError(t, err)
	require.Equal(t, OSHost(), h)
}

func TestMainApp_panics_on_a_nil_init(t *testing.T) {
	require.PanicsWithValue(t, "humus: Main called with a nil init", func() { mainApp[Host]("Main", acquireOSHost, nil) })
	require.PanicsWithValue(t, "humus: MainOn called with a nil init", func() { mainApp[Host]("MainOn", acquireOSHost, nil) })
}
