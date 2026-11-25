// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpchain_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/internal/httpchain"
)

// The tests in this file call the chain's ServeHTTP directly, with
// httptest's recorder: the chain knows nothing of the protocol, so no
// server is needed. httpserver's and http3server's tests run it inside
// whole Apps.

// scope is the name of the logger each test passes to Wrap, which is not
// the name of any package's, so a record under it can only have come
// through the logger the caller passed.
const scope = "example.com/server"

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// setup returns a Setup recording into tel, with failure injection on
// through c when c is not nil.
func setup(t *testing.T, tel *humustest.Telemetry, c fit.Decoder) *humus.Setup {
	t.Helper()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	s := &humus.Setup{Telemetry: sdk}
	if c != nil {
		settings := fault.DefaultSettings()
		settings.Decoder = c
		s.Faults, err = fault.New(settings)
		require.NoError(t, err)
	}
	return s
}

// serve serves req through h, recovering the panic the chain aborts with,
// and returns it.
func serve(h http.Handler, req *http.Request) (recovered any) {
	defer func() { recovered = recover() }()
	h.ServeHTTP(httptest.NewRecorder(), req)
	return nil
}

func TestWrap_records_a_panic_and_aborts_the_response(t *testing.T) {
	tel := humustest.NewTelemetry()
	s := setup(t, tel, nil)
	h := httpchain.Wrap(s, s.Telemetry.LoggerProvider.Logger(scope), nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	recovered := serve(h, httptest.NewRequest(http.MethodGet, "/orders", nil))
	require.Equal(t, http.ErrAbortHandler, recovered, "the response is aborted without net/http logging it")

	spans := tel.Spans()
	require.Len(t, spans, 1)
	require.Equal(t, codes.Error, spans[0].Status().Code)
	var exceptions []sdktrace.Event
	for _, ev := range spans[0].Events() {
		if ev.Name == "exception" {
			exceptions = append(exceptions, ev)
		}
	}
	require.Len(t, exceptions, 1, "the panic is recorded once, and the abort not at all")

	var bodies []string
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == scope {
			bodies = append(bodies, r.Body().AsString())
		}
	}
	require.Equal(t, []string{"handler panicked"}, bodies)
}

func TestWrap_passes_an_abort_through_untouched(t *testing.T) {
	tel := humustest.NewTelemetry()
	s := setup(t, tel, nil)
	h := httpchain.Wrap(s, s.Telemetry.LoggerProvider.Logger(scope), nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	require.Equal(t, http.ErrAbortHandler, serve(h, httptest.NewRequest(http.MethodGet, "/orders", nil)))

	spans := tel.Spans()
	require.Len(t, spans, 1)
	require.Empty(t, spans[0].Events())
	require.Empty(t, tel.Logs())
}

func TestWrap_names_the_span_after_the_route(t *testing.T) {
	tel := humustest.NewTelemetry()
	s := setup(t, tel, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	h := httpchain.Wrap(s, s.Telemetry.LoggerProvider.Logger(scope), nil, mux)

	require.Nil(t, serve(h, httptest.NewRequest(http.MethodGet, "/orders/7", nil)))

	spans := tel.Spans()
	require.Len(t, spans, 1)
	require.Equal(t, "GET /orders/{id}", spans[0].Name())
}

func TestWrap_strips_a_scenario_unless_trust_trusts_the_caller(t *testing.T) {
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		trust  func(*http.Request) bool
		strips bool
	}{
		{"nil trusts no one", nil, true},
		{"an untrusted caller", func(*http.Request) bool { return false }, true},
		{"a trusted caller", func(*http.Request) bool { return true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := humustest.NewTelemetry()
			s := setup(t, tel, c)

			var carried bool
			var tenant string
			mux := http.NewServeMux()
			mux.HandleFunc("GET /orders", func(w http.ResponseWriter, r *http.Request) {
				_, carried, _ = fit.ScenarioFromContext(r.Context(), c)
				tenant = baggage.FromContext(r.Context()).Member("tenant").Value()
				_, _ = io.WriteString(w, "ok")
			})
			h := httpchain.Wrap(s, s.Telemetry.LoggerProvider.Logger(scope), tc.trust, mux)

			require.Nil(t, serve(h, edgeRequest(t, c)))

			require.Equal(t, !tc.strips, carried)
			require.Equal(t, "acme", tenant, "a baggage member other than the scenario was lost")
			spans := tel.Spans()
			require.Len(t, spans, 1)
			require.Equal(t, "GET /orders", spans[0].Name(), "the span is named after the route the stripped copy matched")
			var strips int
			for _, ev := range spans[0].Events() {
				if ev.Name == "humus.fit.strip" {
					strips++
				}
			}
			if tc.strips {
				require.Equal(t, 1, strips)
			} else {
				require.Zero(t, strips)
			}
		})
	}
}

// edgeRequest returns GET /orders, whose baggage carries tenant=acme and a
// scenario written with c.
func edgeRequest(t *testing.T, c jsonscenario.Codec) *http.Request {
	t.Helper()
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(context.Background(), b)
	ctx, err = fit.ContextWithScenario(ctx, c, fit.Scenario{Rules: []fit.Rule{{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}}}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	return req
}
