// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
)

// The tests in this file run whole Apps inside synctest bubbles, on
// humustest Hosts, over TLS from humustest.NewCA. The test is the edge: it
// sends requests whose baggage carries a scenario, written with
// jsonscenario, and a tenant member.

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// stripEvent is the span event of a strip.
const stripEvent = "humus.fit.strip"

// codec returns the tests' jsonscenario Codec.
func codec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	return c
}

// abortPayments is the scenario the edge sends: it aborts
// http:payments:8443.
var abortPayments = fit.Scenario{Rules: []fit.Rule{{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}}}}

// faultsRun configures a run of an App serving one Service.
type faultsRun struct {
	// faults turns failure injection on, with the tests' codec.
	faults bool
	// trust is the Service's TrustScenario.
	trust func(r *http.Request) bool
	// clientCerts makes the Service ask for a client certificate, verified
	// against the CA, and the client present one.
	clientCerts bool
	// scenario makes the edge's request carry abortPayments.
	scenario bool
}

// seen is what the handler saw of the request it served.
type seen struct {
	mu       sync.Mutex
	served   int
	scenario bool
	rules    []fit.Rule
	tenant   string
}

// faultsResult is what a run recorded.
type faultsResult struct {
	resp   response
	seen   *seen
	spans  []sdktrace.ReadOnlySpan
	tel    *humustest.Telemetry
	stderr string
}

// runFaults runs an App configured by run, sends it one GET /orders from
// the edge, then stops the App and returns what happened.
func runFaults(t *testing.T, run faultsRun) faultsResult {
	t.Helper()
	ca := humustest.NewCA(t)
	host := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	c := codec(t)

	got := &seen{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders", func(w http.ResponseWriter, r *http.Request) {
		s, ok, err := fit.ScenarioFromContext(r.Context(), c)
		got.mu.Lock()
		got.served++
		got.scenario = ok && err == nil
		got.rules = s.Rules
		got.tenant = baggage.FromContext(r.Context()).Member("tenant").Value()
		got.mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	})

	settings := settingsFor(t, ca)
	settings.TrustScenario = run.trust
	if run.clientCerts {
		settings.TLS.ClientAuth = tls.VerifyClientCertIfGiven
		settings.TLS.ClientCAs = ca.Pool()
	}

	opts := []humus.Option{humus.WithHost(host.Host()), humus.WithTelemetry(tel.Builder())}
	if run.faults {
		opts = append(opts, humus.WithFaults(func(context.Context, *humus.Setup) (fault.Settings, error) {
			d := fault.DefaultSettings()
			d.Decoder = c
			return d, nil
		}))
	}
	p, status := humustest.Start(t, serves(settings, mux), humustest.WithApp(opts...))
	<-host.Listening("localhost:8443")

	cl := client(host, ca, offersBoth)
	if run.clientCerts {
		cl.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{ca.Issue(t, "orders-caller").TLS()}
	}
	r := <-do(cl, edgeRequest(t, c, run.scenario))
	synctest.Wait()

	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	return faultsResult{resp: r, seen: got, spans: serverSpans(tel), tel: tel, stderr: p.Stderr()}
}

// edgeRequest returns GET https://localhost:8443/orders, whose baggage
// carries tenant=acme and, when scenario is set, abortPayments written with
// c.
func edgeRequest(t *testing.T, c jsonscenario.Codec, scenario bool) *http.Request {
	t.Helper()
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)
	if scenario {
		ctx, err = fit.ContextWithScenario(ctx, c, abortPayments)
		require.NoError(t, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/orders", nil)
	require.NoError(t, err)
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	return req
}

// eventsNamed returns the events of span named name.
func eventsNamed(span sdktrace.ReadOnlySpan, name string) []sdktrace.Event {
	var evs []sdktrace.Event
	for _, ev := range span.Events() {
		if ev.Name == name {
			evs = append(evs, ev)
		}
	}
	return evs
}

// requireServed fails t unless the request was answered with 200, the
// handler served it once with tenant=acme, and the App wrote nothing to
// stderr. It returns the request's server span.
func requireServed(t *testing.T, res faultsResult) sdktrace.ReadOnlySpan {
	t.Helper()
	require.NoError(t, res.resp.err)
	require.Equal(t, http.StatusOK, res.resp.code)
	res.seen.mu.Lock()
	defer res.seen.mu.Unlock()
	require.Equal(t, 1, res.seen.served)
	require.Equal(t, "acme", res.seen.tenant, "a baggage member other than the scenario was lost")
	require.Empty(t, res.stderr)
	require.Len(t, res.spans, 1)
	return res.spans[0]
}

func TestService_strips_a_failure_scenario(t *testing.T) {
	t.Run("nil trusts no one", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true})
			span := requireServed(t, res)

			require.False(t, res.seen.scenario, "the handler saw the scenario of a caller nothing trusts")
			strips := eventsNamed(span, stripEvent)
			require.Len(t, strips, 1)
			require.Empty(t, strips[0].Attributes)
		})
	})

	t.Run("a caller TrustScenario doesn't trust", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, trust: func(*http.Request) bool { return false }})
			span := requireServed(t, res)

			require.False(t, res.seen.scenario, "the handler saw the scenario of a caller TrustScenario refused")
			require.Len(t, eventsNamed(span, stripEvent), 1)
		})
	})

	t.Run("a trusted caller keeps its scenario", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, trust: func(*http.Request) bool { return true }})
			span := requireServed(t, res)

			require.True(t, res.seen.scenario, "the handler didn't see the scenario of a trusted caller")
			require.Equal(t, abortPayments.Rules, res.seen.rules)
			require.Empty(t, eventsNamed(span, stripEvent))
		})
	})

	t.Run("a caller with a client certificate the CA verified", func(t *testing.T) {
		verified := func(r *http.Request) bool { return r.TLS != nil && len(r.TLS.VerifiedChains) > 0 }

		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, clientCerts: true, trust: verified})
			span := requireServed(t, res)
			require.True(t, res.seen.scenario, "the handler didn't see the scenario of a verified caller")
			require.Empty(t, eventsNamed(span, stripEvent))
		})

		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, trust: verified})
			span := requireServed(t, res)
			require.False(t, res.seen.scenario, "the handler saw the scenario of a caller with no certificate")
			require.Len(t, eventsNamed(span, stripEvent), 1)
		})
	})

	t.Run("off: with no humus.WithFaults, the scenario reaches the handler", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{scenario: true})
			span := requireServed(t, res)

			require.True(t, res.seen.scenario, "the handler didn't see the scenario, with failure injection off")
			require.Equal(t, abortPayments.Rules, res.seen.rules)
			require.Empty(t, eventsNamed(span, stripEvent))
		})
	})
}

func TestService_names_a_requests_span_after_its_route_whether_its_scenario_is_stripped_or_not(t *testing.T) {
	testCases := []struct {
		name  string
		run   faultsRun
		strip bool
	}{
		{name: "no scenario", run: faultsRun{faults: true}},
		{name: "a trusted caller keeps its scenario", run: faultsRun{faults: true, scenario: true, trust: func(*http.Request) bool { return true }}},
		{name: "a caller nothing trusts, its scenario stripped", run: faultsRun{faults: true, scenario: true}, strip: true},
		{name: "a caller TrustScenario doesn't trust, its scenario stripped", run: faultsRun{faults: true, scenario: true, trust: func(*http.Request) bool { return false }}, strip: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var name string
			var strips int
			var routes []string
			synctest.Test(t, func(t *testing.T) {
				res := runFaults(t, tc.run)
				span := requireServed(t, res)
				name = span.Name()
				strips = len(eventsNamed(span, stripEvent))
				routes = durationRoutes(t, res.tel)
			})
			t.Logf("span %q, %d strip events, http.server.request.duration routes %q", name, strips, routes)
			if tc.strip {
				require.Equal(t, 1, strips)
			} else {
				require.Zero(t, strips)
			}
			require.Equal(t, "GET /orders", name)
			require.Equal(t, []string{"/orders"}, routes)
		})
	}
}

func TestService_asks_TrustScenario_only_when_it_must(t *testing.T) {
	for _, tc := range []struct {
		name     string
		faults   bool
		scenario bool
		asked    int64
	}{
		{"a request with a scenario, with failure injection", true, true, 1},
		{"a request without a scenario", true, false, 0},
		{"an App without failure injection", false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var asked atomic.Int64
				trust := func(r *http.Request) bool {
					asked.Add(1)
					return true
				}
				res := runFaults(t, faultsRun{faults: tc.faults, scenario: tc.scenario, trust: trust})
				span := requireServed(t, res)

				require.Equal(t, tc.asked, asked.Load())
				require.Equal(t, tc.scenario, res.seen.scenario)
				require.Empty(t, eventsNamed(span, stripEvent))
			})
		})
	}
}

func TestService_a_TrustScenario_panic(t *testing.T) {
	t.Run("aborts the request and is recorded, as a handler's is", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runFaults(t, faultsRun{faults: true, scenario: true, trust: func(*http.Request) bool { panic(errBoom) }})

			requireAborted(t, res.resp, offersBoth)
			require.Zero(t, res.resp.code, "want no response")
			res.seen.mu.Lock()
			require.Zero(t, res.seen.served, "the handler ran after TrustScenario panicked")
			res.seen.mu.Unlock()

			require.Len(t, res.spans, 1)
			span := res.spans[0]
			require.Equal(t, codes.Error, span.Status().Code)
			evs := exceptions(span)
			require.Len(t, evs, 1)
			msg, ok := eventAttr(evs[0], "exception.message")
			require.True(t, ok, "the exception has no exception.message")
			require.Equal(t, "boom", msg)
			stack, ok := eventAttr(evs[0], "exception.stacktrace")
			require.True(t, ok, "the exception has no exception.stacktrace")
			require.Contains(t, stack, "faults_test.go", "the stack trace does not reach TrustScenario")
			require.Empty(t, eventsNamed(span, stripEvent))

			var errs []string
			for _, r := range records(res.tel) {
				if r.Severity() == log.SeverityError {
					errs = append(errs, r.Body().AsString())
				}
			}
			require.Equal(t, []string{"handler panicked"}, errs)
			require.Empty(t, res.stderr)
		})
	})
}
