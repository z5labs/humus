// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// The tests in this file run inside synctest bubbles, on the same
// humustest Hosts and API server as the Client's other tests. A request's
// context carries the baggage member tenant=acme and, for most, a scenario
// written with jsonscenario, as the context of a reconcile that carries
// one would.

// scenarioKey is the baggage member the tests' codec reads.
const scenarioKey = "acme.fault"

// injectedErrorType is the error.type of an injected failure.
const injectedErrorType = "github.com/z5labs/humus/fault.InjectedError"

// The span events of failure injection.
const (
	evaluationEvent = "humus.fit.evaluation"
	stripEvent      = "humus.fit.strip"
)

// getPoint is the point of a GET of a ConfigMap.
const getPoint = "kube:configmaps:get"

// abort aborts point.
func abort(point string) *fit.Scenario {
	return &fit.Scenario{Rules: []fit.Rule{{Point: point, Fault: fit.Fault{Abort: true}}}}
}

// delay delays point by d.
func delay(point string, d time.Duration) *fit.Scenario {
	return &fit.Scenario{Rules: []fit.Rule{{Point: point, Fault: fit.Fault{Delay: d}}}}
}

// noRules is a scenario of no rules: it injects nothing, and every point
// is still evaluated.
var noRules = &fit.Scenario{}

// faultsCodec returns the tests' jsonscenario Codec.
func faultsCodec(t *testing.T) jsonscenario.Codec {
	t.Helper()
	c, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)
	return c
}

// faultsSetup returns a Setup on host whose telemetry tel records, with
// failure injection from the tests' codec.
func faultsSetup(t *testing.T, host humus.Host, tel *humustest.Telemetry) *humus.Setup {
	t.Helper()
	s := newSetup(t, host, tel)
	settings := fault.DefaultSettings()
	settings.Decoder = faultsCodec(t)
	in, err := fault.New(settings)
	require.NoError(t, err)
	s.Faults = in
	return s
}

// withScenario returns ctx with the baggage member tenant=acme and, unless
// sc is nil, sc written with the tests' codec.
func withScenario(t *testing.T, ctx context.Context, sc *fit.Scenario) context.Context {
	t.Helper()
	tenant, err := baggage.NewMemberRaw("tenant", "acme")
	require.NoError(t, err)
	b, err := baggage.New(tenant)
	require.NoError(t, err)
	ctx = baggage.ContextWithBaggage(ctx, b)
	if sc != nil {
		ctx, err = fit.ContextWithScenario(ctx, faultsCodec(t), *sc)
		require.NoError(t, err)
	}
	return ctx
}

// faultsClient is a Client with failure injection, the API server it
// reaches, and the telemetry it records.
type faultsClient struct {
	s   *humus.Setup
	c   *kube.Client
	kc  kubernetes.Interface
	api *apiServer
	tel *humustest.Telemetry
	tk  *tokens
}

// newFaultsClient starts an API server on a new Host and returns a Client
// of it with failure injection when faults is set, and a Token that counts
// its calls. It must be called inside a synctest bubble.
func newFaultsClient(t *testing.T, faults bool) faultsClient {
	t.Helper()
	h := humustest.NewHost(t)
	ca := humustest.NewCA(t)
	api := &apiServer{}
	t.Cleanup(startAPIServer(t, h, ca, server, api, "apiserver"))

	tel := humustest.NewTelemetry()
	s := newSetup(t, h.Host(), tel)
	if faults {
		s = faultsSetup(t, h.Host(), tel)
	}
	tk := &tokens{queue: []string{newToken(), newToken(), newToken()}}
	c, err := kube.NewClient(s, kube.Connection{
		Server: server,
		TLS:    &tls.Config{RootCAs: ca.Pool()},
		Token:  tk.token,
	})
	require.NoError(t, err)
	return faultsClient{s: s, c: c, kc: newClientset(t, c), api: api, tel: tel, tk: tk}
}

// do sends a request through the Client's transport, as client-go does,
// and drains and closes its response.
func (fc faultsClient) do(t *testing.T, ctx context.Context, method, path, contentType string) error {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		body = strings.NewReader(`{}`)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+server+path, body)
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := (&http.Client{Transport: fc.c.Config().Transport}).Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
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

// allEvents returns the events named name of every span tel recorded.
func allEvents(tel *humustest.Telemetry, name string) []sdktrace.Event {
	var evs []sdktrace.Event
	for _, span := range tel.Spans() {
		evs = append(evs, spanEvents(span, name)...)
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

// spanAttrs returns span's attributes by key.
func spanAttrs(span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value)
	for _, kv := range span.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}

// requireInjected fails t unless err holds a *fault.InjectedError at point,
// which errors.As finds, whose Err is nil for an abort, or matches cause.
func requireInjected(t *testing.T, err error, point string, cause error) {
	t.Helper()
	var ie *fault.InjectedError
	require.ErrorAs(t, err, &ie)
	require.Equal(t, point, ie.Point)
	if cause == nil {
		require.NoError(t, ie.Err, "an abort's Err is nil")
		return
	}
	require.ErrorIs(t, ie.Err, cause, "Err is the context's cause")
}

func TestClient_evaluates_one_point_for_each_request(t *testing.T) {
	t.Parallel()

	const apply = "application/apply-patch+yaml"
	for _, tc := range []struct {
		name        string
		method      string
		path        string
		contentType string
		point       string
	}{
		{"get", http.MethodGet, "/api/v1/namespaces/default/configmaps/settings", "", "kube:configmaps:get"},
		{"head", http.MethodHead, "/api/v1/namespaces/default/configmaps/settings", "", "kube:configmaps:get"},
		{"list", http.MethodGet, "/api/v1/namespaces/default/configmaps", "", "kube:configmaps:list"},
		{"watch", http.MethodGet, "/api/v1/namespaces/default/configmaps?watch=true", "", "kube:configmaps:watch"},
		{"watch of one object", http.MethodGet, "/api/v1/namespaces/default/configmaps?watch=1&fieldSelector=metadata.name%3Dsettings", "", "kube:configmaps:watch"},
		{"watch on the legacy path", http.MethodGet, "/api/v1/watch/namespaces/default/configmaps", "", "kube:configmaps:watch"},
		{"create", http.MethodPost, "/api/v1/namespaces/default/configmaps", "application/json", "kube:configmaps:create"},
		{"update", http.MethodPut, "/api/v1/namespaces/default/configmaps/settings", "application/json", "kube:configmaps:update"},
		{"patch", http.MethodPatch, "/api/v1/namespaces/default/configmaps/settings", "application/merge-patch+json", "kube:configmaps:patch"},
		{"server-side apply", http.MethodPatch, "/api/v1/namespaces/default/configmaps/settings?fieldManager=test", apply, "kube:configmaps:patch"},
		{"delete", http.MethodDelete, "/api/v1/namespaces/default/configmaps/settings", "", "kube:configmaps:delete"},
		{"deletecollection", http.MethodDelete, "/api/v1/namespaces/default/configmaps", "", "kube:configmaps:deletecollection"},
		{"a method the API server has no verb for", http.MethodOptions, "/api/v1/namespaces/default/configmaps", "", "kube:configmaps:options"},
		{"a grouped resource", http.MethodGet, "/apis/apps/v1/namespaces/default/deployments/web", "", "kube:deployments.apps:get"},
		{"an apply of a grouped resource", http.MethodPatch, "/apis/apps/v1/namespaces/default/deployments/web?fieldManager=test", apply, "kube:deployments.apps:patch"},
		{"a custom resource", http.MethodGet, "/apis/cache.example.com/v1/namespaces/default/memcacheds", "", "kube:memcacheds.cache.example.com:list"},
		{"a subresource", http.MethodPatch, "/apis/cache.example.com/v1/namespaces/default/memcacheds/m/status?fieldManager=test", apply, "kube:memcacheds.cache.example.com/status:patch"},
		{"a core subresource", http.MethodGet, "/api/v1/namespaces/default/pods/web-0/log", "", "kube:pods/log:get"},
		{"a cluster-scoped resource", http.MethodGet, "/api/v1/nodes/node-1", "", "kube:nodes:get"},
		{"a grouped cluster-scoped resource", http.MethodGet, "/apis/rbac.authorization.k8s.io/v1/clusterroles", "", "kube:clusterroles.rbac.authorization.k8s.io:list"},
		{"a cluster-scoped watch", http.MethodGet, "/apis/coordination.k8s.io/v1/leases?watch=true", "", "kube:leases.coordination.k8s.io:watch"},
		{"a namespace", http.MethodGet, "/api/v1/namespaces/default", "", "kube:namespaces:get"},
		{"a namespace's subresource", http.MethodPut, "/api/v1/namespaces/default/finalize", "application/json", "kube:namespaces/finalize:update"},
		{"a proxy subresource", http.MethodGet, "/api/v1/namespaces/default/services/web/proxy/healthz", "", "kube:services/proxy:get"},
		{"a proxy on the legacy path", http.MethodGet, "/api/v1/proxy/namespaces/default/services/web/healthz", "", "kube:services:proxy"},
		{"discovery of the groups", http.MethodGet, "/apis", "", "kube:/apis:get"},
		{"discovery of the core group", http.MethodGet, "/api", "", "kube:/api:get"},
		{"discovery of a version", http.MethodGet, "/api/v1", "", "kube:/api/v1:get"},
		{"discovery of a group", http.MethodGet, "/apis/apps", "", "kube:/apis/apps:get"},
		{"discovery of a group's version", http.MethodGet, "/apis/apps/v1", "", "kube:/apis/apps/v1:get"},
		{"a watch of nothing", http.MethodGet, "/api/v1/watch", "", "kube:/api/v1/watch:get"},
		{"the version", http.MethodGet, "/version", "", "kube:/version:get"},
		{"the root", http.MethodGet, "/", "", "kube:/:get"},
		{"a non-resource POST", http.MethodPost, "/livez", "application/json", "kube:/livez:post"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fc := newFaultsClient(t, true)
				// A watch has no span, so its evaluation lands on the
				// caller's.
				ctx, span := fc.s.Telemetry.TracerProvider.Tracer("test").Start(withScenario(t, t.Context(), noRules), "caller")
				require.NoError(t, fc.do(t, ctx, tc.method, tc.path, tc.contentType))
				span.End()
				require.Len(t, fc.api.received(), 1)

				evs := allEvents(fc.tel, evaluationEvent)
				require.Len(t, evs, 1, "one evaluation for one request")
				require.Equal(t, attribute.StringValue(tc.point), eventAttrs(evs[0])["humus.fit.point"])
			})
		})
	}
}

// TestClient_evaluates_the_point_of_a_clientsets_apply shows the point of
// a server-side apply from client-go's own clientset.
func TestClient_evaluates_the_point_of_a_clientsets_apply(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fc := newFaultsClient(t, true)
		ctx := withScenario(t, t.Context(), noRules)
		_, err := fc.kc.CoreV1().ConfigMaps("default").Apply(ctx, corev1ac.ConfigMap("settings", "default"), metav1.ApplyOptions{FieldManager: "test"})
		require.NoError(t, err)

		spans := fc.tel.Spans()
		require.Len(t, spans, 1)
		require.Equal(t, http.MethodPatch, spans[0].Name())
		evs := spanEvents(spans[0], evaluationEvent)
		require.Len(t, evs, 1, "the evaluation is on the request's span")
		require.Equal(t, attribute.StringValue("kube:configmaps:patch"), eventAttrs(evs[0])["humus.fit.point"])
	})
}

func TestClient_an_aborted_request(t *testing.T) {
	t.Parallel()

	t.Run("sends nothing, calls no Token, and fails with an InjectedError", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFaultsClient(t, true)
			ctx := withScenario(t, t.Context(), abort(getPoint))

			err := getConfigMap(ctx, fc.kc, "settings")
			requireInjected(t, err, getPoint, nil)
			require.ErrorIs(t, err, fit.ErrInjected)
			require.Empty(t, fc.api.received(), "an aborted request reached the API server")
			require.Zero(t, fc.tk.calls.Load(), "an aborted request called Token")

			spans := fc.tel.Spans()
			require.Len(t, spans, 1)
			require.Equal(t, attribute.StringValue(injectedErrorType), spanAttrs(spans[0])["error.type"])
			evs := spanEvents(spans[0], evaluationEvent)
			require.Len(t, evs, 1)
			ev := eventAttrs(evs[0])
			require.Equal(t, attribute.StringValue(getPoint), ev["humus.fit.point"])
			require.Equal(t, attribute.StringValue(getPoint), ev["humus.fit.rule"])
			require.Equal(t, attribute.BoolValue(true), ev["humus.fit.abort"])
			require.Empty(t, spanEvents(spans[0], stripEvent), "an aborted request sends nothing to strip")

			// A point the scenario doesn't name goes on as usual.
			_, err = fc.kc.CoreV1().ConfigMaps("default").List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, fc.api.received(), 1)
			require.EqualValues(t, 1, fc.tk.calls.Load())
		})
	})

	t.Run("with a body, closes it and sends nothing", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFaultsClient(t, true)
			ctx := withScenario(t, t.Context(), abort("kube:configmaps:create"))

			_, err := fc.kc.CoreV1().ConfigMaps("default").Create(ctx, newConfigMap("settings"), metav1.CreateOptions{})
			requireInjected(t, err, "kube:configmaps:create", nil)
			require.Empty(t, fc.api.received())
			require.Zero(t, fc.tk.calls.Load())
		})
	})

	t.Run("is a GET client-go doesn't retry", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFaultsClient(t, true)
			ctx := withScenario(t, t.Context(), abort(getPoint))

			start := time.Now()
			requireInjected(t, getConfigMap(ctx, fc.kc, "settings"), getPoint, nil)
			require.Zero(t, time.Since(start), "client-go waited to retry")
			require.Len(t, fc.tel.Spans(), 1, "client-go sent the GET again")
			require.Len(t, allEvents(fc.tel, evaluationEvent), 1)
		})
	})
}

func TestClient_a_delayed_request(t *testing.T) {
	t.Parallel()

	t.Run("waits the delay in the request's span, before it is sent", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFaultsClient(t, true)
			ctx := withScenario(t, t.Context(), delay(getPoint, 2*time.Second))

			start := time.Now()
			require.NoError(t, getConfigMap(ctx, fc.kc, "settings"))
			require.Equal(t, 2*time.Second, time.Since(start))
			require.Len(t, fc.api.received(), 1)

			spans := fc.tel.Spans()
			require.Len(t, spans, 1)
			require.Equal(t, 2*time.Second, spans[0].EndTime().Sub(spans[0].StartTime()), "the delay is in the request's span")
			evs := spanEvents(spans[0], evaluationEvent)
			require.Len(t, evs, 1)
			require.Equal(t, attribute.Float64Value(2), eventAttrs(evs[0])["humus.fit.delay"])
			require.Equal(t, attribute.BoolValue(false), eventAttrs(evs[0])["humus.fit.abort"])
		})
	})

	t.Run("cut short by the request's context, fails with an InjectedError", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFaultsClient(t, true)
			ctx, cancel := context.WithTimeout(withScenario(t, t.Context(), delay(getPoint, 5*time.Second)), time.Second)
			defer cancel()

			start := time.Now()
			err := getConfigMap(ctx, fc.kc, "settings")
			require.Equal(t, time.Second, time.Since(start))
			requireInjected(t, err, getPoint, context.DeadlineExceeded)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, fit.ErrInjected)
			require.Empty(t, fc.api.received())
			require.Zero(t, fc.tk.calls.Load())

			spans := fc.tel.Spans()
			require.Len(t, spans, 1)
			require.Equal(t, attribute.StringValue(injectedErrorType), spanAttrs(spans[0])["error.type"])
		})
	})

	t.Run("cut short by a rest.Config's Timeout, times out", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFaultsClient(t, true)
			cfg := fc.c.Config()
			cfg.Timeout = time.Second
			kc, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err)
			ctx := withScenario(t, t.Context(), delay(getPoint, 5*time.Second))

			start := time.Now()
			err = getConfigMap(ctx, kc, "settings")
			require.Equal(t, time.Second, time.Since(start))
			// The error is the InjectedError or net/http's timeout error,
			// whichever timer net/http saw first. Both match
			// DeadlineExceeded.
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Empty(t, fc.api.received())

			spans := fc.tel.Spans()
			require.Len(t, spans, 1)
			require.Equal(t, attribute.StringValue(injectedErrorType), spanAttrs(spans[0])["error.type"])
		})
	})
}

func TestClient_strips_the_scenario(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		fc := newFaultsClient(t, true)
		ctx := withScenario(t, t.Context(), abort("kube:secrets:get"))

		require.NoError(t, getConfigMap(ctx, fc.kc, "settings"))

		reqs := fc.api.received()
		require.Len(t, reqs, 1)
		b, err := baggage.Parse(reqs[0].baggage)
		require.NoError(t, err)
		require.Equal(t, 1, b.Len(), "the API server got %q", reqs[0].baggage)
		require.Equal(t, "acme", b.Member("tenant").Value(), "every other member is kept")
		require.Empty(t, b.Member(scenarioKey).Key(), "the scenario reached the API server")

		spans := fc.tel.Spans()
		require.Len(t, spans, 1)
		require.Contains(t, reqs[0].traceparent, spans[0].SpanContext().TraceID().String(), "the request keeps its span context")
		strips := spanEvents(spans[0], stripEvent)
		require.Len(t, strips, 1)
		require.Empty(t, strips[0].Attributes, "a strip has no attributes")
	})
}

// TestClient_a_watch_evaluates_on_the_span_in_its_context shows a watch,
// which has no span of its own, adding its evaluation to the span in its
// context, such as a reconcile's.
func TestClient_a_watch_evaluates_on_the_span_in_its_context(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		api := &apiServer{}
		defer startAPIServer(t, h, ca, server, api, "apiserver")()

		tel := humustest.NewTelemetry()
		s := faultsSetup(t, h.Host(), tel)
		c, err := kube.NewClient(s, kube.Connection{Server: server, TLS: &tls.Config{RootCAs: ca.Pool()}})
		require.NoError(t, err)
		kc := newClientset(t, c)

		ctx, span := s.Telemetry.TracerProvider.Tracer("test").Start(withScenario(t, t.Context(), noRules), "reconcile ConfigMap")
		w, err := kc.CoreV1().ConfigMaps("default").Watch(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		for range w.ResultChan() {
		}
		w.Stop()
		span.End()

		spans := tel.Spans()
		require.Len(t, spans, 1, "the watch has no span")
		require.Equal(t, "reconcile ConfigMap", spans[0].Name())
		evs := spanEvents(spans[0], evaluationEvent)
		require.Len(t, evs, 1)
		require.Equal(t, attribute.StringValue("kube:configmaps:watch"), eventAttrs(evs[0])["humus.fit.point"])

		reqs := api.received()
		require.Len(t, reqs, 1)
		require.True(t, reqs[0].watch)
		require.Empty(t, reqs[0].baggage, "a watch sends no baggage")
		require.Empty(t, spanEvents(spans[0], stripEvent), "a watch has nothing to strip")
	})
}

func TestClient_without_a_scenario(t *testing.T) {
	t.Parallel()

	t.Run("evaluates, strips and records nothing", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFaultsClient(t, true)
			ctx, span := fc.s.Telemetry.TracerProvider.Tracer("test").Start(withScenario(t, t.Context(), nil), "caller")

			require.NoError(t, getConfigMap(ctx, fc.kc, "settings"))
			w, err := fc.kc.CoreV1().ConfigMaps("default").Watch(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			for range w.ResultChan() {
			}
			w.Stop()
			span.End()

			require.Empty(t, allEvents(fc.tel, evaluationEvent))
			require.Empty(t, allEvents(fc.tel, stripEvent))
			reqs := fc.api.received()
			require.Len(t, reqs, 2)
			require.Equal(t, "tenant=acme", reqs[0].baggage, "the baggage is sent as it is")
		})
	})

	t.Run("in an App without failure injection, touches no request", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFaultsClient(t, false)
			ctx := withScenario(t, t.Context(), abort(getPoint))

			require.NoError(t, getConfigMap(ctx, fc.kc, "settings"), "a scenario aborted a request without failure injection")
			require.Empty(t, allEvents(fc.tel, evaluationEvent))
			require.Empty(t, allEvents(fc.tel, stripEvent))

			reqs := fc.api.received()
			require.Len(t, reqs, 1)
			b, err := baggage.Parse(reqs[0].baggage)
			require.NoError(t, err)
			require.Equal(t, 2, b.Len(), "nothing is stripped without failure injection")
		})
	})
}
