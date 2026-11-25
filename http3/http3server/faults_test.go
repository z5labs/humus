// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/http3/internal/quicdial"
	"github.com/z5labs/humus/humustest"
)

// The tests in this file are httpserver's faults_test.go over HTTP/3: whole
// Apps inside synctest bubbles, on humustest Hosts, where the test is the
// edge. It sends a request whose baggage carries a scenario, written with
// jsonscenario, and a tenant member.

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// stripEvent is the span event of a strip.
const stripEvent = "humus.fit.strip"

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
}

// seen is what the handler saw of the request it served.
type seen struct {
	mu       sync.Mutex
	served   int
	scenario bool
	rules    []fit.Rule
	tenant   string
}

// runFaults runs an App configured by run, sends it one GET /orders from
// the edge, carrying abortPayments, then stops the App and returns the
// request's server span and what the handler saw.
func runFaults(t *testing.T, run faultsRun) (sdktrace.ReadOnlySpan, *seen) {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)

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

	opts := []humus.Option{humus.WithTelemetry(tel.Builder())}
	if run.faults {
		opts = append(opts, humus.WithFaults(func(context.Context, *humus.Setup, humus.Host) (fault.Settings, error) {
			d := fault.DefaultSettings()
			d.Decoder = c
			return d, nil
		}))
	}
	p, status := humustest.Start(t, h.Host(), serves(settings, mux), humustest.WithApp(opts...))
	synctest.Wait()

	clientTLS := &tls.Config{RootCAs: ca.Pool()}
	if run.clientCerts {
		clientTLS.Certificates = []tls.Certificate{ca.Issue(t, "orders-caller").TLS()}
	}
	d := quicdial.New(h.DialUDP)
	tr := &http3.Transport{TLSClientConfig: clientTLS, Dial: d.Dial}
	defer d.Close()
	defer func() { _ = tr.Close() }()

	r := <-do(&http.Client{Transport: tr}, edgeRequest(t, c))
	require.NoError(t, r.err)
	require.Equal(t, http.StatusOK, r.code)

	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	require.Empty(t, p.Stderr())

	spans := tel.Spans()
	require.Len(t, spans, 1)
	got.mu.Lock()
	defer got.mu.Unlock()
	require.Equal(t, 1, got.served)
	require.Equal(t, "acme", got.tenant, "a baggage member other than the scenario was lost")
	require.Equal(t, "GET /orders", spans[0].Name())
	return spans[0], got
}

// edgeRequest returns GET https://localhost:8443/orders, whose baggage
// carries tenant=acme and abortPayments written with c.
func edgeRequest(t *testing.T, c jsonscenario.Codec) *http.Request {
	t.Helper()
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	ctx, err := fit.ContextWithScenario(baggage.ContextWithBaggage(t.Context(), b), c, abortPayments)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/orders", nil)
	require.NoError(t, err)
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	return req
}

// strips returns how many strip events span has.
func strips(span sdktrace.ReadOnlySpan) int {
	n := 0
	for _, ev := range span.Events() {
		if ev.Name == stripEvent {
			n++
		}
	}
	return n
}

func TestService_strips_a_failure_scenario(t *testing.T) {
	t.Run("nil trusts no one", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			span, got := runFaults(t, faultsRun{faults: true})
			require.False(t, got.scenario, "the handler saw the scenario of a caller nothing trusts")
			require.Equal(t, 1, strips(span))
		})
	})

	t.Run("a caller TrustScenario doesn't trust", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			span, got := runFaults(t, faultsRun{faults: true, trust: func(*http.Request) bool { return false }})
			require.False(t, got.scenario, "the handler saw the scenario of a caller TrustScenario refused")
			require.Equal(t, 1, strips(span))
		})
	})

	t.Run("a trusted caller keeps its scenario", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			span, got := runFaults(t, faultsRun{faults: true, trust: func(*http.Request) bool { return true }})
			require.True(t, got.scenario, "the handler didn't see the scenario of a trusted caller")
			require.Equal(t, abortPayments.Rules, got.rules)
			require.Zero(t, strips(span))
		})
	})

	t.Run("a caller with a client certificate the CA verified", func(t *testing.T) {
		verified := func(r *http.Request) bool { return r.TLS != nil && len(r.TLS.VerifiedChains) > 0 }

		synctest.Test(t, func(t *testing.T) {
			span, got := runFaults(t, faultsRun{faults: true, clientCerts: true, trust: verified})
			require.True(t, got.scenario, "the handler didn't see the scenario of a verified caller")
			require.Zero(t, strips(span))
		})

		synctest.Test(t, func(t *testing.T) {
			span, got := runFaults(t, faultsRun{faults: true, trust: verified})
			require.False(t, got.scenario, "the handler saw the scenario of a caller with no certificate")
			require.Equal(t, 1, strips(span))
		})
	})

	t.Run("off: with no humus.WithFaults, the scenario reaches the handler", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			span, got := runFaults(t, faultsRun{})
			require.True(t, got.scenario, "the handler didn't see the scenario, with failure injection off")
			require.Zero(t, strips(span))
		})
	})
}
