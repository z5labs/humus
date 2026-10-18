// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fault_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/z5labs/humus/fault"
)

// scenarioKey is the baggage member the tests' Decoder reads.
const scenarioKey = "test.scenario"

// unknownScenarioError is the error of a scenario ID the registry doesn't
// hold.
type unknownScenarioError struct{ id string }

func (e unknownScenarioError) Error() string { return "unknown scenario " + e.id }

// registry is the tests' fit.Decoder: the member scenarioKey carries the ID
// of one of its scenarios, and an ID it doesn't hold is rejected.
type registry map[string]fit.Scenario

func (r registry) Decode(b baggage.Baggage) (fit.Scenario, bool, error) {
	m := b.Member(scenarioKey)
	if m.Key() == "" {
		return fit.Scenario{}, false, nil
	}
	s, ok := r[m.Value()]
	if !ok {
		return fit.Scenario{}, false, unknownScenarioError{id: m.Value()}
	}
	return s, true, nil
}

func (registry) Strip(b baggage.Baggage) baggage.Baggage {
	return b.DeleteMember(scenarioKey)
}

// scenarios is the registry every test's Injector reads.
var scenarios = registry{
	"abort-payments": {Rules: []fit.Rule{{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}}}},
	"slow-postgres":  {Rules: []fit.Rule{{Point: "postgresql:*", Fault: fit.Fault{Delay: time.Minute}}}},
}

// newInjector returns an Injector over scenarios with a MaxDelay of 10s.
func newInjector(t *testing.T) fault.Injector {
	t.Helper()
	s := fault.DefaultSettings()
	s.Decoder = scenarios
	in, err := fault.New(s)
	require.NoError(t, err)
	return in
}

// withBaggage returns ctx with baggage of the members kv names, in pairs of
// key and value.
func withBaggage(t *testing.T, ctx context.Context, kv ...string) context.Context {
	t.Helper()
	var members []baggage.Member
	for i := 0; i < len(kv); i += 2 {
		m, err := baggage.NewMemberRaw(kv[i], kv[i+1])
		require.NoError(t, err)
		members = append(members, m)
	}
	b, err := baggage.New(members...)
	require.NoError(t, err)
	return baggage.ContextWithBaggage(ctx, b)
}

// inSpan runs f with a context holding a recorded span, ends the span, and
// returns it.
func inSpan(t *testing.T, ctx context.Context, f func(ctx context.Context)) sdktrace.ReadOnlySpan {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, span := tp.Tracer("test").Start(ctx, "request")
	f(ctx)
	span.End()
	spans := rec.Ended()
	require.Len(t, spans, 1)
	return spans[0]
}

// evaluations returns the attributes of every humus.fit.evaluation event
// of span, in order.
func evaluations(span sdktrace.ReadOnlySpan) [][]attribute.KeyValue {
	var evs [][]attribute.KeyValue
	for _, ev := range span.Events() {
		if ev.Name == "humus.fit.evaluation" {
			evs = append(evs, ev.Attributes)
		}
	}
	return evs
}

func TestDefaultSettings(t *testing.T) {
	s := fault.DefaultSettings()

	require.Nil(t, s.Decoder, "Decoder has no default")
	require.Equal(t, 10*time.Second, s.MaxDelay)
}

func TestCheckMaxDelay(t *testing.T) {
	require.NoError(t, fault.CheckMaxDelay(time.Nanosecond))
	require.NoError(t, fault.CheckMaxDelay(10*time.Second))
	for _, d := range []time.Duration{0, -time.Second} {
		err := fault.CheckMaxDelay(d)
		require.EqualError(t, err, "must be positive", "MaxDelay %s", d)
		require.NotContains(t, err.Error(), "MaxDelay", "a check doesn't name the field")
	}
}

func TestNew_reports_a_nil_Decoder_and_a_bad_MaxDelay_together(t *testing.T) {
	_, err := fault.New(fault.Settings{MaxDelay: -time.Second})

	require.EqualError(t, err, "fault: New: Settings.Decoder must not be nil\nSettings.MaxDelay must be positive")
}

func TestNew_names_each_bad_field_alone(t *testing.T) {
	_, err := fault.New(fault.DefaultSettings())
	require.EqualError(t, err, "fault: New: Settings.Decoder must not be nil")

	_, err = fault.New(fault.Settings{Decoder: scenarios})
	require.EqualError(t, err, "fault: New: Settings.MaxDelay must be positive")
}

func TestInjector_zero_is_off(t *testing.T) {
	var in fault.Injector
	ctx := withBaggage(t, context.Background(), scenarioKey, "abort-payments", "tenant", "acme")

	var f fit.Fault
	span := inSpan(t, ctx, func(ctx context.Context) {
		f = in.Eval(ctx, "http:payments:8443")
	})
	require.Zero(t, f)
	require.Empty(t, span.Events())

	got, stripped := in.Strip(ctx)
	require.False(t, stripped)
	require.True(t, got == ctx, "Strip returns its context unchanged")
	require.Equal(t, "abort-payments", baggage.FromContext(got).Member(scenarioKey).Value())
}

func TestInjector_Eval_a_request_with_no_scenario_gets_the_zero_Fault_and_no_event(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), "tenant", "acme")

	var f fit.Fault
	span := inSpan(t, ctx, func(ctx context.Context) {
		f = in.Eval(ctx, "http:payments:8443")
	})

	require.Zero(t, f)
	require.Empty(t, span.Events())
}

func TestInjector_Eval_a_matched_abort(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), scenarioKey, "abort-payments")

	var f fit.Fault
	span := inSpan(t, ctx, func(ctx context.Context) {
		f = in.Eval(ctx, "http:payments:8443")
	})

	require.Equal(t, fit.Fault{Abort: true}, f)
	require.ErrorIs(t, f.Inject(ctx), fit.ErrInjected)
	require.Equal(t, [][]attribute.KeyValue{{
		attribute.String("humus.fit.point", "http:payments:8443"),
		attribute.String("humus.fit.rule", "http:payments:8443"),
		attribute.Float64("humus.fit.delay", 0),
		attribute.Bool("humus.fit.abort", true),
	}}, evaluations(span))
}

func TestInjector_Eval_a_Delay_over_MaxDelay_is_clamped(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), scenarioKey, "slow-postgres")

	var f fit.Fault
	span := inSpan(t, ctx, func(ctx context.Context) {
		f = in.Eval(ctx, "postgresql:orders")
	})

	require.Equal(t, fit.Fault{Delay: 10 * time.Second}, f, "the scenario's 1m is clamped to MaxDelay")
	require.Equal(t, [][]attribute.KeyValue{{
		attribute.String("humus.fit.point", "postgresql:orders"),
		attribute.String("humus.fit.rule", "postgresql:*"),
		attribute.Float64("humus.fit.delay", 10),
		attribute.Bool("humus.fit.abort", false),
	}}, evaluations(span))
}

func TestInjector_Eval_an_unmatched_point_has_only_its_point(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), scenarioKey, "abort-payments")

	var f fit.Fault
	span := inSpan(t, ctx, func(ctx context.Context) {
		f = in.Eval(ctx, "kafka:orders")
	})

	require.Zero(t, f)
	require.Equal(t, [][]attribute.KeyValue{{
		attribute.String("humus.fit.point", "kafka:orders"),
	}}, evaluations(span))
}

func TestInjector_Eval_a_rejected_scenario_is_an_event_and_the_request_goes_on(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), scenarioKey, "no-such-scenario")

	var f fit.Fault
	var injected error
	span := inSpan(t, ctx, func(ctx context.Context) {
		f = in.Eval(ctx, "http:payments:8443")
		injected = f.Inject(ctx)
	})

	require.Zero(t, f)
	require.NoError(t, injected, "the request goes on")
	require.Equal(t, [][]attribute.KeyValue{{
		attribute.String("humus.fit.point", "http:payments:8443"),
		attribute.String("error.type", "github.com/z5labs/humus/fault_test.unknownScenarioError"),
		attribute.String("error.message", "unknown scenario no-such-scenario"),
	}}, evaluations(span))
}

func TestInjector_Eval_every_evaluation_is_an_event(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), scenarioKey, "abort-payments")

	span := inSpan(t, ctx, func(ctx context.Context) {
		in.Eval(ctx, "kafka:orders")
		in.Eval(ctx, "http:payments:8443")
	})

	evs := evaluations(span)
	require.Len(t, evs, 2)
	require.Equal(t, attribute.String("humus.fit.point", "kafka:orders"), evs[0][0])
	require.Equal(t, attribute.String("humus.fit.point", "http:payments:8443"), evs[1][0])
}

func TestInjector_Eval_without_a_span_injects_all_the_same(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), scenarioKey, "abort-payments")

	require.Equal(t, fit.Fault{Abort: true}, in.Eval(ctx, "http:payments:8443"))
}

func TestInjector_Strip_keeps_every_other_member(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), "tenant", "acme", scenarioKey, "abort-payments", "region", "eu")

	got, stripped := in.Strip(ctx)

	require.True(t, stripped)
	b := baggage.FromContext(got)
	require.Equal(t, 2, b.Len())
	require.Equal(t, "acme", b.Member("tenant").Value())
	require.Equal(t, "eu", b.Member("region").Value())
	require.Empty(t, b.Member(scenarioKey).Key())
	require.Zero(t, in.Eval(got, "http:payments:8443"), "a stripped request carries no scenario")
	require.Equal(t, 3, baggage.FromContext(ctx).Len(), "Strip leaves its argument alone")
}

func TestInjector_Strip_a_request_with_no_scenario(t *testing.T) {
	in := newInjector(t)
	for name, ctx := range map[string]context.Context{
		"other members": withBaggage(t, context.Background(), "tenant", "acme"),
		"no baggage":    context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			got, stripped := in.Strip(ctx)

			require.False(t, stripped)
			require.True(t, got == ctx, "Strip returns its context unchanged")
		})
	}
}

func TestInjector_Strip_a_rejected_scenario_is_stripped_too(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), scenarioKey, "no-such-scenario")

	got, stripped := in.Strip(ctx)

	require.True(t, stripped)
	require.Zero(t, baggage.FromContext(got).Len())
}

func TestInjector_is_safe_for_concurrent_use(t *testing.T) {
	in := newInjector(t)
	ctx := withBaggage(t, context.Background(), scenarioKey, "abort-payments", "tenant", "acme")

	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 100 {
				if !in.Eval(ctx, "http:payments:8443").Abort {
					t.Error("Eval spared an aborted point")
				}
				if _, ok := in.Strip(ctx); !ok {
					t.Error("Strip stripped nothing")
				}
			}
		}()
	}
	for range 8 {
		<-done
	}
}

func TestInjectedError_an_abort(t *testing.T) {
	var err error = &fault.InjectedError{Point: "http:payments:8443"}

	require.ErrorIs(t, err, fit.ErrInjected)
	require.EqualError(t, err, "fit: injected failure at http:payments:8443")
	require.ErrorIs(t, fmtWrap(err), fit.ErrInjected)
}

func TestInjectedError_a_Delay_cut_short(t *testing.T) {
	var err error = &fault.InjectedError{Point: "http:payments:8443", Err: context.DeadlineExceeded}

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, fit.ErrInjected)
	require.EqualError(t, err, "fault: the delay injected at http:payments:8443 was cut short: context deadline exceeded")
}

func TestInjectedError_errors_As_through_a_wrap(t *testing.T) {
	err := fmtWrap(&fault.InjectedError{Point: "kafka:orders"})

	ie, ok := errors.AsType[*fault.InjectedError](err)
	require.True(t, ok)
	require.Equal(t, "kafka:orders", ie.Point)
}

// fmtWrap wraps err as a client wraps the error of an attempt.
func fmtWrap(err error) error {
	return errors.Join(errors.New("calling payments"), err)
}
