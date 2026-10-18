// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run whole Apps with humustest.Start, on a
// humustest Host, with failure injection turned on through
// humustest.WithApp(humus.WithFaults(…)), and record each App's telemetry
// with humustest.Telemetry.

// faultsMember is the baggage member the tests' Decoder reads: the ID of
// one of the scenarios a faultsRegistry holds.
const faultsMember = "test.scenario"

// faultsRegistry is the tests' fit.Decoder.
type faultsRegistry map[string]fit.Scenario

func (r faultsRegistry) Decode(b baggage.Baggage) (fit.Scenario, bool, error) {
	m := b.Member(faultsMember)
	if m.Key() == "" {
		return fit.Scenario{}, false, nil
	}
	s, ok := r[m.Value()]
	if !ok {
		return fit.Scenario{}, false, errors.New("unknown scenario " + m.Value())
	}
	return s, true, nil
}

func (faultsRegistry) Strip(b baggage.Baggage) baggage.Baggage {
	return b.DeleteMember(faultsMember)
}

// abortPayments is the registry of the scenario abort-payments, which
// aborts http:payments:8443.
var abortPayments = faultsRegistry{
	"abort-payments": {Rules: []fit.Rule{{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}}}},
}

// underTest returns a context whose baggage carries the scenario
// abort-payments and the member tenant=acme.
func underTest(t *testing.T) context.Context {
	t.Helper()
	id, err := baggage.NewMemberRaw(faultsMember, "abort-payments")
	require.NoError(t, err)
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(id, tenant)
	require.NoError(t, err)
	return baggage.ContextWithBaggage(context.Background(), b)
}

// faultsSettings is a Faults function that sources MaxDelay from
// FAULT_MAX_DELAY, as an end user's does, with abortPayments as its Decoder.
func faultsSettings(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
	d := fault.DefaultSettings()
	return config.Struct(func(r *config.Resolver) fault.Settings {
		c := d
		c.Decoder = abortPayments
		c.MaxDelay = r.Get(config.Env(s.Process.Environment, "FAULT_MAX_DELAY").
			Parse(time.ParseDuration).Default(d.MaxDelay).Validate(fault.CheckMaxDelay))
		return c
	}).Read(ctx)
}

// recordedBuilder returns a Builder that appends "telemetry" to order and
// then builds tel's SDK.
func recordedBuilder(tel *humustest.Telemetry, order *[]string) telemetry.Builder {
	build := tel.Builder()
	return func(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
		*order = append(*order, "telemetry")
		return build(ctx, p, dial)
	}
}

func TestApp_Run_faults_runs_after_telemetry_and_before_Init_with_the_same_Setup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		var order []string
		var faultsSetup *humus.Setup
		faults := func(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
			order = append(order, "faults")
			require.NotNil(t, s.Telemetry.TracerProvider, "telemetry is built before Faults runs")
			require.Zero(t, s.Faults, "the Injector is built from what Faults returns")
			faultsSetup = s
			return faultsSettings(ctx, s)
		}
		var initSetup *humus.Setup
		var f fit.Fault
		var stripped bool
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			order = append(order, "init")
			initSetup = s
			ctx := underTest(t)
			f = s.Faults.Eval(ctx, "http:payments:8443")
			_, stripped = s.Faults.Strip(ctx)
			return nil, nil
		}

		_, status := humustest.Start(t, init, humustest.WithApp(
			humus.WithTelemetry(recordedBuilder(tel, &order)),
			humus.WithFaults(faults),
		))
		st := <-status

		require.Equal(t, 0, st.Code(), "cause: %v", st.Cause())
		require.Equal(t, []string{"telemetry", "faults", "init"}, order)
		require.NotNil(t, initSetup)
		require.True(t, faultsSetup == initSetup, "Faults is given the Setup that Init is then given")
		require.Equal(t, fit.Fault{Abort: true}, f, "Setup.Faults is the Injector of the Settings Faults returned")
		require.True(t, stripped)
	})
}

func TestApp_Run_faults_a_config_Error_exits_78_and_runs_OnStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		var stopped []string
		faults := func(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
			s.OnStop(func(context.Context) error {
				stopped = append(stopped, "registry")
				return nil
			})
			return faultsSettings(ctx, s)
		}
		initCalled := false
		init := func(context.Context, *humus.Setup) ([]humus.Service, error) {
			initCalled = true
			return nil, nil
		}

		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithTelemetry(tel.Builder()), humus.WithFaults(faults)),
			humustest.WithEnv(map[string]string{"FAULT_MAX_DELAY": "0s"}),
		)
		st := <-status

		require.Equal(t, humus.ExitConfig, st.Code())
		_, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.False(t, initCalled, "Init isn't called when failure injection fails")
		require.Equal(t, []string{"registry"}, stopped)
		require.Equal(t, []string{"failure injection: config FAULT_MAX_DELAY: must be positive"}, errorLogs(tel))
		require.Empty(t, p.Stderr())
	})
}

func TestApp_Run_faults_any_other_error_exits_1_and_runs_OnStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		errRegistry := errors.New("load scenario registry: connection refused")
		var stopped []int
		faults := func(_ context.Context, s *humus.Setup) (fault.Settings, error) {
			for i := range 2 {
				s.OnStop(func(context.Context) error {
					stopped = append(stopped, i)
					return nil
				})
			}
			return fault.Settings{}, errRegistry
		}
		initCalled := false
		init := func(context.Context, *humus.Setup) ([]humus.Service, error) {
			initCalled = true
			return nil, nil
		}

		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithTelemetry(tel.Builder()), humus.WithFaults(faults)),
		)
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.ErrorIs(t, st.Cause(), errRegistry)
		require.False(t, initCalled)
		require.Equal(t, []int{1, 0}, stopped, "the OnStop functions run in reverse")
		require.Equal(t, []string{"failure injection: load scenario registry: connection refused"}, errorLogs(tel))
		require.Empty(t, p.Stderr())
	})
}

func TestApp_Run_faults_bad_Settings_exit_1(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		faults := func(context.Context, *humus.Setup) (fault.Settings, error) {
			return fault.DefaultSettings(), nil // no Decoder
		}
		init := func(context.Context, *humus.Setup) ([]humus.Service, error) {
			t.Error("Init is called")
			return nil, nil
		}

		_, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithTelemetry(tel.Builder()), humus.WithFaults(faults)),
		)
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.Equal(t, []string{"failure injection: fault: New: Settings.Decoder must not be nil"}, errorLogs(tel))
	})
}

func TestApp_Run_faults_nil_leaves_the_zero_Injector(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		var faults fault.Injector
		var f fit.Fault
		var stripped bool
		init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			faults = s.Faults
			ctx := underTest(t)
			f = s.Faults.Eval(ctx, "http:payments:8443")
			_, stripped = s.Faults.Strip(ctx)
			return nil, nil
		}

		_, status := humustest.Start(t, init, humustest.WithApp(humus.WithTelemetry(tel.Builder())))
		st := <-status

		require.Equal(t, 0, st.Code(), "cause: %v", st.Cause())
		require.Zero(t, faults)
		require.Zero(t, f, "the zero Injector evaluates nothing")
		require.False(t, stripped, "the zero Injector strips nothing")
	})
}

func TestApp_Run_faults_a_signal_during_a_slow_Faults_cancels_its_context(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		faults := func(ctx context.Context, _ *humus.Setup) (fault.Settings, error) {
			<-ctx.Done()
			return fault.Settings{}, context.Cause(ctx)
		}
		init := func(context.Context, *humus.Setup) ([]humus.Service, error) {
			t.Error("Init is called")
			return nil, nil
		}

		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithTelemetry(tel.Builder()), humus.WithFaults(faults)),
		)
		synctest.Wait()
		p.Signal(syscall.SIGTERM)
		st := <-status

		require.Equal(t, humus.ExitFailure, st.Code())
		require.Len(t, errorLogs(tel), 1)
	})
}
