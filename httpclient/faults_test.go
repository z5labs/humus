// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/fit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
)

// The tests in this file run the orders service as a whole App inside a
// synctest bubble: an httpserver Service whose handler calls the payments
// dependency through a Client. Both run on one humustest Host, over TLS
// from humustest.NewCA. The test is the edge: it sends orders requests
// whose baggage carries tenant=acme and, for most, a scenario written with
// jsonscenario. orders trusts the edge, so its scenarios reach the Client.

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// paymentsPoint is the injection point of the payments dependency.
const paymentsPoint = "http:payments:8443"

// injectedErrorType is the error.type of an injected failure.
const injectedErrorType = "github.com/z5labs/humus/fault.InjectedError"

// The span events of failure injection.
const (
	evaluationEvent = "humus.fit.evaluation"
	stripEvent      = "humus.fit.strip"
)

// abortPayments aborts every attempt to payments.
var abortPayments = &fit.Scenario{Rules: []fit.Rule{{Point: paymentsPoint, Fault: fit.Fault{Abort: true}}}}

// delayPayments delays every attempt to payments by d.
func delayPayments(d time.Duration) *fit.Scenario {
	return &fit.Scenario{Rules: []fit.Rule{{Point: paymentsPoint, Fault: fit.Fault{Delay: d}}}}
}

// sparesPayments aborts a dependency other than payments.
var sparesPayments = &fit.Scenario{Rules: []fit.Rule{{Point: "http:ledger:8443", Fault: fit.Fault{Abort: true}}}}

// faultsCodec returns the tests' jsonscenario Codec.
func faultsCodec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	return c
}

// ordersRun configures a run of orders.
type ordersRun struct {
	// faults turns failure injection on, with the tests' codec.
	faults bool
	// forward is the Client's ForwardScenario.
	forward bool
	// classify replaces the Client's Classify, when it is set.
	classify func(resp *http.Response, err error) httpclient.Classification
	// scenarios are the edge's requests, sent one after another: each
	// carries its scenario, or none when it is nil.
	scenarios []*fit.Scenario
}

// paymentsRequest is what payments saw of a request it served.
type paymentsRequest struct {
	scenario    bool
	tenant      string
	traceparent trace.SpanContext
}

// ordersCall is what orders' handler saw of one edge request.
type ordersCall struct {
	// scenario is whether the handler's context carried a scenario.
	scenario bool
	// err is the error the Client's call to payments returned.
	err error
	// code is the status the edge got.
	code int
}

// ordersResult is what a run recorded.
type ordersResult struct {
	calls    []ordersCall
	payments []paymentsRequest
	tel      *humustest.Telemetry
}

// runOrders runs payments and orders, sends orders the edge's requests,
// one after another, then stops both and returns what happened.
func runOrders(t *testing.T, run ordersRun) ordersResult {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	tel := humustest.NewTelemetry()
	codec := faultsCodec(t)

	var mu sync.Mutex
	var seen []paymentsRequest
	stopPayments := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := baggage.Parse(r.Header.Get("baggage"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(r.Header)))
		mu.Lock()
		seen = append(seen, paymentsRequest{
			scenario:    b.Member(scenarioKey).Key() != "",
			tenant:      b.Member("tenant").Value(),
			traceparent: sc,
		})
		mu.Unlock()
		_, _ = io.WriteString(w, "paid")
	}))

	var calls []ordersCall
	cert := ca.Issue(t, "orders")
	init := func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := paymentsSettings(t, ca)
		settings.ForwardScenario = run.forward
		if run.classify != nil {
			settings.Classify = run.classify
		}
		payments, err := httpclient.NewClient(s, settings)
		if err != nil {
			return nil, err
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /orders/{id}/payment", func(w http.ResponseWriter, r *http.Request) {
			_, carried, _ := fit.ScenarioFromContext(r.Context(), codec)
			call := ordersCall{scenario: carried}
			defer func() {
				mu.Lock()
				calls = append(calls, call)
				mu.Unlock()
			}()
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://payments:8443/charges/"+r.PathValue("id"), nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp, err := payments.Do(req)
			if err != nil {
				call.err = err
				http.Error(w, "payments is unavailable", http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(w, resp.Body)
		})
		server := httpserver.DefaultSettings()
		server.Addr = ":9443"
		server.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		server.TrustScenario = func(*http.Request) bool { return true } // the edge
		server.WriteTimeout = time.Minute                               // outlasts the Client's Timeout
		svc, err := httpserver.NewService(s, server, mux)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}

	opts := []humus.Option{humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())}
	if run.faults {
		opts = append(opts, humus.WithFaults(func(context.Context, *humus.Setup) (fault.Settings, error) {
			d := fault.DefaultSettings()
			d.Decoder = codec
			return d, nil
		}))
	}
	p, status := humustest.Start(t, init, humustest.WithApp(opts...))
	<-h.Listening("orders:9443")

	edge := h.HTTPClient()
	edge.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	codes := make([]int, 0, len(run.scenarios))
	for _, sc := range run.scenarios {
		resp, err := edge.Do(edgeRequest(t, codec, sc))
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		require.NoError(t, resp.Body.Close())
		codes = append(codes, resp.StatusCode)
	}

	p.Signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	stopPayments()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, len(codes))
	for i := range calls {
		calls[i].code = codes[i]
	}
	return ordersResult{calls: calls, payments: seen, tel: tel}
}

// edgeRequest returns GET https://orders:9443/orders/7/payment, whose
// baggage carries tenant=acme and, unless sc is nil, sc written with c.
func edgeRequest(t *testing.T, c jsonscenario.Codec, sc *fit.Scenario) *http.Request {
	t.Helper()
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)
	if sc != nil {
		ctx, err = fit.ContextWithScenario(ctx, c, *sc)
		require.NoError(t, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://orders:9443/orders/7/payment", nil)
	require.NoError(t, err)
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	return req
}

// repeat returns n of sc.
func repeat(sc *fit.Scenario, n int) []*fit.Scenario {
	out := make([]*fit.Scenario, n)
	for i := range out {
		out[i] = sc
	}
	return out
}

// spanEvents returns the events of span named name.
func spanEvents(span sdktrace.ReadOnlySpan, name string) []sdktrace.Event {
	var evs []sdktrace.Event
	for _, ev := range span.Events() {
		if ev.Name == name {
			evs = append(evs, ev)
		}
	}
	return evs
}

// eventAttrs returns ev's attributes by key.
func eventAttrs(ev sdktrace.Event) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value)
	for _, kv := range ev.Attributes {
		m[kv.Key] = kv.Value
	}
	return m
}

// requireAborted fails t unless err is an abort injected at payments.
func requireAborted(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, fit.ErrInjected)
	var ie *fault.InjectedError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, paymentsPoint, ie.Point)
	require.NoError(t, ie.Err, "an abort's Err is nil")
	var ue *url.Error
	require.ErrorAs(t, err, &ue, "an abort is a transport error")
}

func TestClient_an_aborted_attempt(t *testing.T) {
	t.Run("is retried, and opens the breaker, which sheds a request with no scenario", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runOrders(t, ordersRun{faults: true, scenarios: []*fit.Scenario{abortPayments, abortPayments, nil}})

			require.Empty(t, res.payments, "an aborted attempt, or a shed one, reached payments")
			for _, c := range res.calls {
				require.Equal(t, http.StatusBadGateway, c.code)
			}

			first := res.calls[0]
			require.True(t, first.scenario)
			requireAborted(t, first.err)

			// The second call's attempts are the fourth and the fifth in a
			// row. The fifth drains the retry budget to half, so the call
			// ends there, and opens the breaker, which rejects the third
			// call at once.
			requireAborted(t, res.calls[1].err)
			require.ErrorIs(t, res.calls[2].err, circuit.ErrRejected)
			var ie *fault.InjectedError
			require.False(t, errors.As(res.calls[2].err, &ie), "a request with no scenario failed with an injected error")

			spans := clientSpans(res.tel)
			require.Len(t, spans, 6, "three aborted attempts, two, and one rejected")
			rm := collect(t, t.Context(), res.tel)
			require.Equal(t, map[string]int64{
				"humus.circuit.outcome=failure":  5,
				"humus.circuit.outcome=rejected": 1,
			}, points(t, rm, "humus.circuit.calls"))
			require.Equal(t, breakerIs(0, 1, 0), state(t, rm))
		})
	})

	t.Run("is a client span with an evaluation and its error.type, and is measured", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runOrders(t, ordersRun{faults: true, scenarios: []*fit.Scenario{abortPayments}})
			require.Empty(t, res.payments)
			requireAborted(t, res.calls[0].err)

			spans := clientSpans(res.tel)
			require.Len(t, spans, 3, "an attempt per MaxAttempts")
			for _, span := range spans {
				require.Equal(t, attribute.StringValue(injectedErrorType), attrs(span)["error.type"])
				evs := spanEvents(span, evaluationEvent)
				require.Len(t, evs, 1)
				ev := eventAttrs(evs[0])
				require.Equal(t, attribute.StringValue(paymentsPoint), ev["humus.fit.point"])
				require.Equal(t, attribute.StringValue(paymentsPoint), ev["humus.fit.rule"])
				require.Equal(t, attribute.BoolValue(true), ev["humus.fit.abort"])
				require.Empty(t, spanEvents(span, stripEvent), "an aborted attempt sends nothing to strip")
			}

			rm := collect(t, t.Context(), res.tel)
			duration := histogram[float64](t, rm, "http.client.request.duration")
			var counted uint64
			for _, dp := range duration.DataPoints {
				et, ok := dp.Attributes.Value("error.type")
				require.True(t, ok, "a data point without error.type: %v", dp.Attributes.ToSlice())
				require.Equal(t, injectedErrorType, et.AsString())
				counted += dp.Count
			}
			require.EqualValues(t, 3, counted)
		})
	})
}

func TestClient_a_delayed_attempt(t *testing.T) {
	t.Run("waits the delay in the attempt's span, and succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runOrders(t, ordersRun{faults: true, scenarios: []*fit.Scenario{delayPayments(time.Second)}})

			require.Equal(t, http.StatusOK, res.calls[0].code)
			require.NoError(t, res.calls[0].err)
			require.Len(t, res.payments, 1)

			spans := clientSpans(res.tel)
			require.Len(t, spans, 1)
			require.GreaterOrEqual(t, spans[0].EndTime().Sub(spans[0].StartTime()), time.Second)
			evs := spanEvents(spans[0], evaluationEvent)
			require.Len(t, evs, 1)
			require.Equal(t, attribute.Float64Value(1), eventAttrs(evs[0])["humus.fit.delay"])

			rm := collect(t, t.Context(), res.tel)
			duration := histogram[float64](t, rm, "http.client.request.duration")
			require.Len(t, duration.DataPoints, 1)
			require.GreaterOrEqual(t, duration.DataPoints[0].Sum, 1.0, "the delay isn't in http.client.request.duration")
		})
	})

	t.Run("is cut short by AttemptTimeout and retried, until Timeout ends the call", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runOrders(t, ordersRun{faults: true, scenarios: []*fit.Scenario{delayPayments(8 * time.Second)}})

			require.Empty(t, res.payments)
			call := res.calls[0]
			require.Equal(t, http.StatusBadGateway, call.code)
			// The test accepts either error the handler can see, because
			// which one it sees is a race in net/http. Timeout ends the call
			// at 10s, as the second attempt's AttemptTimeout also ends.
			// For a RoundTripper that isn't one of its own, net/http puts
			// Timeout on the request's context as a deadline, and also
			// starts a timer whose goroutine records that the call timed
			// out. Both fire at 10s. net/http replaces the transport's
			// error with its own timeout error only if that goroutine has
			// recorded the timeout by the time the attempt returns.
			// Otherwise the handler sees the *fault.InjectedError of the
			// delay the call's deadline cut short.
			require.ErrorIs(t, call.err, context.DeadlineExceeded)
			require.NotErrorIs(t, call.err, fit.ErrInjected)
			var ue *url.Error
			require.ErrorAs(t, call.err, &ue)
			var ie *fault.InjectedError
			if errors.As(call.err, &ie) {
				require.Equal(t, paymentsPoint, ie.Point)
				require.ErrorIs(t, ie.Err, context.DeadlineExceeded)
			} else {
				require.True(t, ue.Timeout(), "error: %v", call.err)
			}

			spans := clientSpans(res.tel)
			require.Len(t, spans, 2, "the attempt AttemptTimeout cut, and the one Timeout cut")
			require.Equal(t, 5*time.Second, spans[0].EndTime().Sub(spans[0].StartTime()))
			require.Equal(t, 10*time.Second, spans[1].EndTime().Sub(spans[0].StartTime()), "Timeout ended the call")
			for _, span := range spans {
				require.Equal(t, attribute.StringValue(injectedErrorType), attrs(span)["error.type"])
				evs := spanEvents(span, evaluationEvent)
				require.Len(t, evs, 1)
				require.Equal(t, attribute.StringValue(paymentsPoint), eventAttrs(evs[0])["humus.fit.point"])
				require.Equal(t, attribute.Float64Value(8), eventAttrs(evs[0])["humus.fit.delay"])
			}
			rm := collect(t, t.Context(), res.tel)
			require.Equal(t, map[string]int64{"humus.circuit.outcome=failure": 2}, points(t, rm, "humus.circuit.calls"))
		})
	})
}

func TestClient_the_end_users_Classify_decides_an_injected_failure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		classify := httpclient.DefaultSettings().Classify
		spare := func(resp *http.Response, err error) httpclient.Classification {
			var ie *fault.InjectedError
			if errors.As(err, &ie) {
				return httpclient.Classification{Retry: retry.Stop, Breaker: circuit.Ignored}
			}
			return classify(resp, err)
		}
		res := runOrders(t, ordersRun{faults: true, classify: spare, scenarios: append(repeat(abortPayments, 6), nil)})

		for _, c := range res.calls[:6] {
			requireAborted(t, c.err)
		}
		require.Equal(t, http.StatusOK, res.calls[6].code, "the breaker opened")
		require.Len(t, res.payments, 1)
		require.Len(t, clientSpans(res.tel), 7, "one attempt per call")

		rm := collect(t, t.Context(), res.tel)
		require.Equal(t, map[string]int64{
			"humus.circuit.outcome=ignored": 6,
			"humus.circuit.outcome=success": 1,
		}, points(t, rm, "humus.circuit.calls"))
		require.Equal(t, breakerIs(1, 0, 0), state(t, rm))
	})
}

func TestClient_ForwardScenario(t *testing.T) {
	// requireSent fails t unless payments was called once, with tenant=acme
	// and a traceparent of the attempt's span, and returns what it saw and
	// the attempt's span.
	requireSent := func(t *testing.T, res ordersResult) (paymentsRequest, sdktrace.ReadOnlySpan) {
		t.Helper()
		require.Equal(t, http.StatusOK, res.calls[0].code, "error: %v", res.calls[0].err)
		require.True(t, res.calls[0].scenario, "orders' handler didn't see the trusted scenario")
		require.Len(t, res.payments, 1)
		got := res.payments[0]
		require.Equal(t, "acme", got.tenant, "a baggage member other than the scenario was lost")
		spans := clientSpans(res.tel)
		require.Len(t, spans, 1)
		require.Equal(t, spans[0].SpanContext().TraceID(), got.traceparent.TraceID())
		require.Equal(t, spans[0].SpanContext().SpanID(), got.traceparent.SpanID(), "payments' traceparent isn't the attempt's span")
		require.Len(t, spanEvents(spans[0], evaluationEvent), 1)
		return got, spans[0]
	}

	t.Run("false strips the scenario", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runOrders(t, ordersRun{faults: true, scenarios: []*fit.Scenario{sparesPayments}})
			got, span := requireSent(t, res)

			require.False(t, got.scenario, "payments saw the scenario")
			strips := spanEvents(span, stripEvent)
			require.Len(t, strips, 1)
			require.Empty(t, strips[0].Attributes)
		})
	})

	t.Run("true sends the scenario on", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			res := runOrders(t, ordersRun{faults: true, forward: true, scenarios: []*fit.Scenario{sparesPayments}})
			got, span := requireSent(t, res)

			require.True(t, got.scenario, "payments didn't see the scenario")
			require.Empty(t, spanEvents(span, stripEvent))
		})
	})
}

func TestClient_without_failure_injection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		res := runOrders(t, ordersRun{scenarios: []*fit.Scenario{abortPayments}})

		require.Equal(t, http.StatusOK, res.calls[0].code, "error: %v", res.calls[0].err)
		require.Len(t, res.payments, 1)
		require.True(t, res.payments[0].scenario, "the scenario didn't reach payments as baggage")
		require.Equal(t, "acme", res.payments[0].tenant)
		spans := clientSpans(res.tel)
		require.Len(t, spans, 1)
		require.Empty(t, spanEvents(spans[0], evaluationEvent))
		require.Empty(t, spanEvents(spans[0], stripEvent))
	})
}
