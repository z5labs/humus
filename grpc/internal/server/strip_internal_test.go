// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"

	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
)

var unaryInfo = &grpc.UnaryServerInfo{FullMethod: "/grpc.testing.TestService/UnaryCall"}

// stripCase is an RPC's context, carrying tenant=acme and, when asked, a
// scenario, inside a span the recorder records once end is called.
type stripCase struct {
	codec    jsonscenario.Codec
	injector fault.Injector
	recorder *tracetest.SpanRecorder
	ctx      context.Context
	end      func()
}

// newStripCase returns a stripCase whose injector is on when faults is set.
func newStripCase(t *testing.T, faults, scenario bool) *stripCase {
	t.Helper()
	c, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)

	var in fault.Injector
	if faults {
		settings := fault.DefaultSettings()
		settings.Decoder = c
		in, err = fault.New(settings)
		require.NoError(t, err)
	}

	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)
	if scenario {
		ctx, err = fit.ContextWithScenario(ctx, c, fit.Scenario{Rules: []fit.Rule{{Point: "grpc:payments:9090", Fault: fit.Fault{Abort: true}}}})
		require.NoError(t, err)
	}

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, span := tp.Tracer("test").Start(ctx, "rpc")
	return &stripCase{codec: c, injector: in, recorder: rec, ctx: ctx, end: func() { span.End() }}
}

// carries reports whether ctx carries a scenario the case's codec reads.
func (sc *stripCase) carries(ctx context.Context) bool {
	_, ok, err := fit.ScenarioFromContext(ctx, sc.codec)
	return ok && err == nil
}

// strips returns the number of humus.fit.strip events on the case's span,
// once it has ended.
func (sc *stripCase) strips(t *testing.T) int {
	t.Helper()
	sc.end()
	spans := sc.recorder.Ended()
	require.Len(t, spans, 1)
	n := 0
	for _, ev := range spans[0].Events() {
		if ev.Name == stripEvent {
			require.Empty(t, ev.Attributes)
			n++
		}
	}
	return n
}

func TestStripUnary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		faults   bool
		scenario bool
		trust    func(ctx context.Context, method string) bool
		asked    int
		kept     bool
		strips   int
	}{
		{"nil trusts no one", true, true, nil, 0, false, 1},
		{"a caller trust refuses", true, true, func(context.Context, string) bool { return false }, 1, false, 1},
		{"a trusted caller", true, true, func(context.Context, string) bool { return true }, 1, true, 0},
		{"an RPC without a scenario", true, false, func(context.Context, string) bool { return true }, 0, false, 0},
		{"no failure injection", false, true, func(context.Context, string) bool { return false }, 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := newStripCase(t, tc.faults, tc.scenario)
			asked := 0
			var trust Trust
			if tc.trust != nil {
				trust = func(ctx context.Context, method string) bool {
					asked++
					require.True(t, sc.carries(ctx), "trust was given a context without the scenario")
					require.Equal(t, unaryInfo.FullMethod, method)
					return tc.trust(ctx, method)
				}
			}

			var got context.Context
			resp, err := StripUnary(sc.injector, trust)(sc.ctx, "req", unaryInfo, func(ctx context.Context, req any) (any, error) {
				got = ctx
				require.Equal(t, "req", req)
				return "resp", nil
			})
			require.NoError(t, err)
			require.Equal(t, "resp", resp)

			require.Equal(t, tc.asked, asked)
			require.Equal(t, tc.kept, sc.carries(got))
			require.Equal(t, "acme", baggage.FromContext(got).Member("tenant").Value())
			require.Equal(t, tc.strips, sc.strips(t))
		})
	}
}

func TestStripStream(t *testing.T) {
	t.Run("a stripped stream's Context is the stripped one", func(t *testing.T) {
		sc := newStripCase(t, true, true)
		ss := &fakeStream{ctx: sc.ctx}

		var got grpc.ServerStream
		err := StripStream(sc.injector, nil)("srv", ss, streamInfo, func(srv any, stream grpc.ServerStream) error {
			require.Equal(t, "srv", srv)
			got = stream
			return nil
		})
		require.NoError(t, err)

		require.NotSame(t, ss, got)
		require.False(t, sc.carries(got.Context()), "the handler's stream carries the scenario of a caller nothing trusts")
		require.Equal(t, "acme", baggage.FromContext(got.Context()).Member("tenant").Value())
		require.Equal(t, 1, sc.strips(t))
	})

	t.Run("a stream nothing is stripped from is the stream itself", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			faults   bool
			scenario bool
		}{
			{"a trusted caller", true, true},
			{"a stream without a scenario", true, false},
			{"no failure injection", false, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sc := newStripCase(t, tc.faults, tc.scenario)
				ss := &fakeStream{ctx: sc.ctx}
				trusted := func(context.Context, string) bool { return true }

				var got grpc.ServerStream
				err := StripStream(sc.injector, trusted)(nil, ss, streamInfo, func(_ any, stream grpc.ServerStream) error {
					got = stream
					return nil
				})
				require.NoError(t, err)
				require.Same(t, ss, got)
				require.Zero(t, sc.strips(t))
			})
		}
	})
}
