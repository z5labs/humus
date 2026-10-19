// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// The tests in this file run inside synctest bubbles, on a humustest Host.
// A Controller of Memcacheds on client-go's fakes, given a TrustScenario,
// reconciles the Memcached default/cache, whose uid is cache-uid, and its
// Reconciler sends requests through a Client of the API server below, as a
// Reconciler applies the children of its object. That API server records
// every body it receives.

// The paths of the requests the tests' Reconcilers send.
const (
	deploymentPath       = "/apis/apps/v1/namespaces/default/deployments/cache?fieldManager=test"
	deploymentStatusPath = "/apis/apps/v1/namespaces/default/deployments/cache/status?fieldManager=test"
	deploymentsPath      = "/apis/apps/v1/namespaces/default/deployments"
	memcachedPath        = "/apis/cache.example.com/v1alpha1/namespaces/default/memcacheds/cache?fieldManager=test"
	memcachedStatusPath  = "/apis/cache.example.com/v1alpha1/namespaces/default/memcacheds/cache/status?fieldManager=test"
)

// The content types of the requests the tests' Reconcilers send.
const (
	applyYAML  = "application/apply-patch+yaml"
	applyCBOR  = "application/apply-patch+cbor"
	jsonType   = "application/json"
	mergePatch = "application/merge-patch+json"
)

// childBody is a Deployment's apply, as JSON, whose controller owner
// reference is the Memcached cache, and whose body already gives the
// scenario's annotation a value, beside an annotation of its own.
const childBody = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"cache","namespace":"default",` +
	`"annotations":{"acme.example.com/failure-scenario":"stale","acme.example.com/team":"cache"},` +
	`"ownerReferences":[{"apiVersion":"cache.example.com/v1alpha1","kind":"Memcached","name":"cache","uid":"cache-uid","controller":true}]},` +
	`"spec":{"replicas":3}}`

// childBodyOwnedBy returns a Deployment's apply, as JSON, whose owner
// references are refs.
func childBodyOwnedBy(refs string) string {
	return `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"cache","namespace":"default","ownerReferences":` +
		refs + `},"spec":{"replicas":3}}`
}

// body is what the API server saw of one request.
type body struct {
	method      string
	path        string
	contentType string
	baggage     string
	body        []byte
}

// bodyServer records every request it receives, with its body, and
// answers with the body when it is JSON, or {} when it isn't.
type bodyServer struct {
	mu   sync.Mutex
	reqs []body
}

func (a *bodyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.reqs = append(a.reqs, body{
		method:      r.Method,
		path:        r.URL.RequestURI(),
		contentType: r.Header.Get("Content-Type"),
		baggage:     r.Header.Get("Baggage"),
		body:        b,
	})
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !json.Valid(b) {
		b = []byte(`{}`)
	}
	_, _ = w.Write(b)
}

func (a *bodyServer) received() []body {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]body(nil), a.reqs...)
}

// childClient is a Client with failure injection, the API server it
// reaches, and the telemetry its Setup records.
type childClient struct {
	s   *humus.Setup
	c   *kube.Client
	kc  kubernetes.Interface
	api *bodyServer
	tel *humustest.Telemetry
}

// newChildClient starts a bodyServer on a new Host and returns a Client of
// it with failure injection. It must be called inside a synctest bubble.
func newChildClient(t *testing.T) childClient {
	t.Helper()
	h := humustest.NewHost(t)
	ca := humustest.NewCA(t)
	api := &bodyServer{}
	t.Cleanup(startAPIServer(t, h, ca, server, api, "apiserver"))

	tel := humustest.NewTelemetry()
	s := faultsSetup(t, h.Host(), tel)
	c, err := kube.NewClient(s, kube.Connection{Server: server, TLS: &tls.Config{RootCAs: ca.Pool()}})
	require.NoError(t, err)
	return childClient{s: s, c: c, kc: newClientset(t, c), api: api, tel: tel}
}

// send sends a request with body through the Client's transport, as
// client-go does, and drains and closes its response.
func (cc childClient) send(ctx context.Context, method, path, contentType string, b []byte) error {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+server+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := (&http.Client{Transport: cc.c.Config().Transport}).Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// reconcileOnce runs a Controller of the Memcached cache, annotated with
// value unless it is empty, with a TrustScenario that trusts it, whose
// Reconciler calls do with the reconcile's context, on its first reconcile
// only. It returns do's error once the reconcile has settled.
func (cc childClient) reconcileOnce(t *testing.T, value string, do func(ctx context.Context) error) error {
	t.Helper()
	obj := memcachedObject("cache", 3)
	if value != "" {
		obj = annotated(obj, value)
	}
	fc := newFakeCluster(t, obj)
	var (
		once  sync.Once
		doErr error
		done  bool
	)
	rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(ctx context.Context, _ kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
		once.Do(func() {
			doErr = do(ctx)
			done = true
		})
		return kube.Result[MemcachedStatus]{}, nil
	})
	sv := serve(t, newController(t, cc.s, fc, testSettings(), rec, kube.TrustScenario(scenarioAnnotation, trustAll)))
	time.Sleep(settle)
	sv.cancel()
	require.NoError(t, sv.wait(t))
	require.True(t, done, "the Memcached was never reconciled")
	return doErr
}

// applySpans returns the PATCH spans tel recorded.
func applySpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, span := range tel.Spans() {
		if span.Name() == http.MethodPatch {
			out = append(out, span)
		}
	}
	return out
}

// annotationsOf returns the metadata.annotations of b, a JSON body.
func annotationsOf(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var obj struct {
		Metadata struct {
			Annotations map[string]any `json:"annotations"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(b, &obj), "the body is JSON: %s", b)
	return obj.Metadata.Annotations
}

// requireScenarioOnly fails t unless value is W3C baggage of exactly one
// member, the scenario's, whose value is scenario.
func requireScenarioOnly(t *testing.T, value any, scenario string) {
	t.Helper()
	s, ok := value.(string)
	require.True(t, ok, "the annotation is a string: %v", value)
	b, err := baggage.Parse(s)
	require.NoError(t, err)
	require.Equal(t, 1, b.Len(), "the annotation is %q", s)
	require.Equal(t, scenario, b.Member(scenarioKey).Value())
}

// scenarioMember returns the value of the scenario's member of an
// annotation that carries sc.
func scenarioMember(t *testing.T, sc *fit.Scenario) string {
	t.Helper()
	b, err := baggage.Parse(scenarioValue(t, sc))
	require.NoError(t, err)
	return b.Member(scenarioKey).Value()
}

func TestClient_carries_a_reconciles_scenario_on_to_the_child_it_applies(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cc := newChildClient(t)
		err := cc.reconcileOnce(t, scenarioValue(t, noRules), func(ctx context.Context) error {
			// A member of the Reconciler's own, which is no part of the
			// scenario.
			region, err := baggage.NewMemberRaw("region", "eu")
			if err != nil {
				return err
			}
			b, err := baggage.FromContext(ctx).SetMember(region)
			if err != nil {
				return err
			}
			ctx = baggage.ContextWithBaggage(ctx, b)
			return cc.send(ctx, http.MethodPatch, deploymentPath, applyYAML, []byte(childBody))
		})
		require.NoError(t, err)

		reqs := cc.api.received()
		require.Len(t, reqs, 1)
		require.Equal(t, applyYAML, reqs[0].contentType)
		annotations := annotationsOf(t, reqs[0].body)
		requireScenarioOnly(t, annotations[scenarioAnnotation], scenarioMember(t, noRules))
		require.Equal(t, "cache", annotations["acme.example.com/team"], "every other annotation is kept")
		require.JSONEq(t, childBodyWith(t, annotations[scenarioAnnotation].(string)), string(reqs[0].body),
			"only the annotation is changed")

		sent, err := baggage.Parse(reqs[0].baggage)
		require.NoError(t, err)
		require.Empty(t, sent.Member(scenarioKey).Key(), "the scenario reached the API server in %q", reqs[0].baggage)
		require.Equal(t, "eu", sent.Member("region").Value(), "every other member is kept")

		spans := applySpans(cc.tel)
		require.Len(t, spans, 1)
		var names []string
		for _, ev := range spans[0].Events() {
			names = append(names, ev.Name)
		}
		require.Equal(t, []string{evaluationEvent, stripEvent}, names, "setting the annotation adds no event")
	})
}

// childBodyWith returns childBody with the scenario's annotation set to
// value.
func childBodyWith(t *testing.T, value string) string {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(childBody), &obj))
	obj["metadata"].(map[string]any)["annotations"].(map[string]any)[scenarioAnnotation] = value
	b, err := json.Marshal(obj)
	require.NoError(t, err)
	return string(b)
}

func TestClient_carries_a_scenario_on_through_a_clientsets_apply(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cc := newChildClient(t)
		err := cc.reconcileOnce(t, scenarioValue(t, noRules), func(ctx context.Context) error {
			_, err := cc.kc.AppsV1().Deployments("default").Apply(ctx,
				appsv1ac.Deployment("cache", "default").
					WithOwnerReferences(metav1ac.OwnerReference().
						WithAPIVersion("cache.example.com/v1alpha1").WithKind("Memcached").WithName("cache").WithUID("cache-uid").
						WithController(true).WithBlockOwnerDeletion(true)).
					WithSpec(appsv1ac.DeploymentSpec().WithReplicas(3)),
				metav1.ApplyOptions{FieldManager: "test"},
			)
			return err
		})
		require.NoError(t, err)

		reqs := cc.api.received()
		require.Len(t, reqs, 1)
		requireScenarioOnly(t, annotationsOf(t, reqs[0].body)[scenarioAnnotation], scenarioMember(t, noRules))
	})
}

func TestClient_sends_a_YAML_apply_of_a_child_on_as_JSON(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const yamlBody = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: cache
  namespace: default
  ownerReferences:
  - apiVersion: cache.example.com/v1alpha1
    kind: Memcached
    name: cache
    uid: cache-uid
    controller: true
spec:
  replicas: 3
`
		cc := newChildClient(t)
		err := cc.reconcileOnce(t, scenarioValue(t, noRules), func(ctx context.Context) error {
			return cc.send(ctx, http.MethodPatch, deploymentPath, applyYAML, []byte(yamlBody))
		})
		require.NoError(t, err)

		reqs := cc.api.received()
		require.Len(t, reqs, 1)
		require.True(t, json.Valid(reqs[0].body), "the body is sent on as JSON: %s", reqs[0].body)
		require.Equal(t, applyYAML, reqs[0].contentType, "JSON is YAML too")
		annotations := annotationsOf(t, reqs[0].body)
		requireScenarioOnly(t, annotations[scenarioAnnotation], scenarioMember(t, noRules))
		var got map[string]any
		require.NoError(t, json.Unmarshal(reqs[0].body, &got))
		delete(got["metadata"].(map[string]any), "annotations")
		rest, err := json.Marshal(got)
		require.NoError(t, err)
		require.JSONEq(t, childBodyOwnedBy(`[{"apiVersion":"cache.example.com/v1alpha1","kind":"Memcached","name":"cache","uid":"cache-uid","controller":true}]`),
			string(rest), "the rest of the body is as the YAML wrote it")
	})
}

func TestClient_an_aborted_apply_of_a_child_sends_nothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const point = "kube:deployments.apps:patch"
		cc := newChildClient(t)
		err := cc.reconcileOnce(t, scenarioValue(t, abort(point)), func(ctx context.Context) error {
			return cc.send(ctx, http.MethodPatch, deploymentPath, applyYAML, []byte(childBody))
		})
		requireInjected(t, err, point, nil)
		require.Empty(t, cc.api.received(), "an aborted apply reached the API server")
	})
}

func TestClient_leaves_every_other_request_as_it_is(t *testing.T) {
	t.Parallel()

	finalizerBody := `{"apiVersion":"cache.example.com/v1alpha1","kind":"Memcached","metadata":{"name":"cache","namespace":"default","uid":"cache-uid","finalizers":["cache.example.com/cleanup"]}}`
	statusBody := `{"apiVersion":"cache.example.com/v1alpha1","kind":"Memcached","metadata":{"name":"cache","namespace":"default"},"status":{"readyReplicas":1}}`
	// The CBOR of {"a": 1}, and the start of a protobuf body, with the
	// magic number the API server's protobuf serializer begins with.
	cbor := []byte{0xa1, 0x61, 0x61, 0x01}
	protobuf := []byte("k8s\x00\x0a\x0e\x0a\x07apps/v1\x12\x0aDeployment")

	for _, tc := range []struct {
		name        string
		noScenario  bool // the Memcached has no annotation
		outside     bool // sent outside the reconcile, with a context of its own
		method      string
		path        string
		contentType string
		body        []byte
	}{
		{name: "an apply in a reconcile with no scenario", noScenario: true, method: http.MethodPatch, path: deploymentPath, contentType: applyYAML, body: []byte(childBody)},
		{name: "an apply outside a reconcile", outside: true, method: http.MethodPatch, path: deploymentPath, contentType: applyYAML, body: []byte(childBody)},
		{name: "a create of a child", method: http.MethodPost, path: deploymentsPath, contentType: jsonType, body: []byte(childBody)},
		{name: "an update of a child", method: http.MethodPut, path: deploymentPath, contentType: jsonType, body: []byte(childBody)},
		{name: "a merge patch of a child", method: http.MethodPatch, path: deploymentPath, contentType: mergePatch, body: []byte(childBody)},
		{name: "a CBOR apply", method: http.MethodPatch, path: deploymentPath, contentType: applyCBOR, body: cbor},
		{name: "a protobuf body", method: http.MethodPatch, path: deploymentPath, contentType: applyYAML, body: protobuf},
		{name: "an apply to a child's subresource", method: http.MethodPatch, path: deploymentStatusPath, contentType: applyYAML, body: []byte(childBody)},
		{name: "the controller's apply of the object's finalizer", method: http.MethodPatch, path: memcachedPath, contentType: applyYAML, body: []byte(finalizerBody)},
		{name: "the controller's apply of the object's status", method: http.MethodPatch, path: memcachedStatusPath, contentType: applyYAML, body: []byte(statusBody)},
		{name: "an object another object controls", method: http.MethodPatch, path: deploymentPath, contentType: applyYAML,
			body: []byte(childBodyOwnedBy(`[{"apiVersion":"cache.example.com/v1alpha1","kind":"Memcached","name":"other","uid":"other-uid","controller":true}]`))},
		{name: "an object with only owner references that aren't a controller's", method: http.MethodPatch, path: deploymentPath, contentType: applyYAML,
			body: []byte(childBodyOwnedBy(`[{"apiVersion":"cache.example.com/v1alpha1","kind":"Memcached","name":"cache","uid":"cache-uid"},` +
				`{"apiVersion":"cache.example.com/v1alpha1","kind":"Memcached","name":"cache","uid":"cache-uid","controller":false}]`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				value := scenarioValue(t, noRules)
				if tc.noScenario {
					value = ""
				}
				cc := newChildClient(t)
				var err error
				if tc.outside {
					err = cc.send(withScenario(t, t.Context(), noRules), tc.method, tc.path, tc.contentType, tc.body)
				} else {
					err = cc.reconcileOnce(t, value, func(ctx context.Context) error {
						return cc.send(ctx, tc.method, tc.path, tc.contentType, tc.body)
					})
				}
				require.NoError(t, err)

				reqs := cc.api.received()
				require.Len(t, reqs, 1)
				require.Equal(t, tc.body, reqs[0].body, "the body is sent as it is")
				require.Equal(t, tc.contentType, reqs[0].contentType)
			})
		})
	}
}
