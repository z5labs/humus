// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// The tests in this file run inside synctest bubbles, on client-go's fakes,
// as the Controller's other tests do. Each object's annotation is written
// as an experiment's tool writes it: the W3C baggage that fit and the
// tests' jsonscenario codec leave, beside the member tenant=acme.

// scenarioAnnotation is the annotation the tests' TrustScenarios read.
const scenarioAnnotation = "acme.example.com/failure-scenario"

// ownPoint is an injection point of the end user's own, which the tests'
// Reconcilers and Finalizes evaluate with s.Faults.Eval.
const ownPoint = "acme:own"

// trustAll trusts every object.
func trustAll(metav1.Object) bool { return true }

// scenarioValue returns the value of an annotation that carries sc: the
// W3C baggage of the member tenant=acme and, unless sc is nil, sc written
// with the tests' codec.
func scenarioValue(t *testing.T, sc *fit.Scenario) string {
	t.Helper()
	return baggage.FromContext(withScenario(t, context.Background(), sc)).String()
}

// annotated returns obj with the annotation scenarioAnnotation set to
// value.
func annotated(obj *unstructured.Unstructured, value string) *unstructured.Unstructured {
	obj.SetAnnotations(map[string]string{scenarioAnnotation: value})
	return obj
}

// seen is what one reconcile's context carried: its baggage's members, by
// key, and the Fault s.Faults evaluates at ownPoint with it.
type seen struct {
	name    string
	members map[string]string
	fault   fit.Fault
}

// sightings records what each reconcile's context carried.
type sightings struct {
	mu   sync.Mutex
	seen []seen
}

// add records ctx, the context of a reconcile of the object name, and
// evaluates ownPoint with s.Faults in it.
func (sg *sightings) add(ctx context.Context, s *humus.Setup, name string) {
	members := map[string]string{}
	for _, m := range baggage.FromContext(ctx).Members() {
		members[m.Key()] = m.Value()
	}
	f := s.Faults.Eval(ctx, ownPoint)
	sg.mu.Lock()
	defer sg.mu.Unlock()
	sg.seen = append(sg.seen, seen{name: name, members: members, fault: f})
}

// of returns what the reconciles of the object name carried, in order.
func (sg *sightings) of(name string) []seen {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	var out []seen
	for _, s := range sg.seen {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

// sighting is a Reconciler that records what each reconcile's context
// carried in sg, and settles.
func sighting(s *humus.Setup, sg *sightings) kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus] {
	return func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
		sg.add(ctx, s, m.Name)
		return kube.Result[MemcachedStatus]{}, nil
	}
}

// truster is a TrustScenario's trust that trusts the objects it names, and
// records each object it is asked about.
type truster struct {
	trusted []string

	mu    sync.Mutex
	asked []metav1.Object
}

func (tr *truster) trust(obj metav1.Object) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.asked = append(tr.asked, obj)
	return slices.Contains(tr.trusted, obj.GetName())
}

// names returns the names of the objects trust was asked about, in order.
func (tr *truster) names() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []string
	for _, obj := range tr.asked {
		out = append(out, obj.GetName())
	}
	return out
}

// reconcileSpans returns the "reconcile Memcached" spans of the object
// name.
func reconcileSpans(tel *humustest.Telemetry, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, span := range tel.Spans() {
		if span.Name() == "reconcile Memcached" && spanAttrs(span)["humus.kube.object.name"].AsString() == name {
			out = append(out, span)
		}
	}
	return out
}

// requireNoScenario fails t unless every reconcile in got carried no
// baggage at all, and evaluated ownPoint to the zero Fault.
func requireNoScenario(t *testing.T, got []seen) {
	t.Helper()
	require.NotEmpty(t, got, "the object was reconciled")
	for _, s := range got {
		require.Empty(t, s.members, "no scenario, and no other member, joins %s's reconcile", s.name)
		require.Zero(t, s.fault, "%s's reconcile evaluates no fault", s.name)
	}
}

func TestTrustScenario_carries_a_trusted_objects_scenario_into_its_reconciles(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		value := scenarioValue(t, abort(ownPoint))
		fc := newFakeCluster(t, annotated(memcachedObject("cache", 3), value))
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)

		var (
			got      sightings
			finalize sightings
		)
		f := func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) error {
			finalize.add(ctx, s, m.Name)
			return nil
		}
		tr := &truster{trusted: []string{"cache"}}
		sv := serve(t, newController(t, s, fc, testSettings(), sighting(s, &got),
			kube.Finalize(finalizerName, f),
			kube.TrustScenario(scenarioAnnotation, tr.trust),
		))
		time.Sleep(settle)

		want, err := baggage.Parse(value)
		require.NoError(t, err)
		scenario := want.Member(scenarioKey).Value()
		require.NotEmpty(t, scenario)

		reconciled := got.of("cache")
		require.Len(t, reconciled, 1, "the finalizer's pass, then the Reconciler's")
		require.Equal(t, map[string]string{scenarioKey: scenario}, reconciled[0].members,
			"the Decoder's member joins the reconcile's context, and tenant=acme doesn't")
		require.True(t, reconciled[0].fault.Abort, "the end user's own point meets the scenario")
		require.Equal(t, []string{"cache", "cache"}, tr.names(), "asked once a reconcile: the finalizer's, and the Reconciler's")
		require.Equal(t, value, tr.asked[0].GetAnnotations()[scenarioAnnotation], "trust is handed the object")

		spans := reconcileSpans(tel, "cache")
		require.Len(t, spans, 2)
		evs := spanEvents(spans[1], evaluationEvent)
		require.Len(t, evs, 1, "the Reconciler's evaluation is on the reconcile's span")
		require.Equal(t, ownPoint, eventAttrs(evs[0])["humus.fit.point"].AsString())
		require.True(t, eventAttrs(evs[0])["humus.fit.abort"].AsBool())
		require.Empty(t, allEvents(tel, stripEvent), "nothing is stripped")

		fc.deleting(t, "cache")
		time.Sleep(settle)

		finalized := finalize.of("cache")
		require.Len(t, finalized, 1)
		require.Equal(t, map[string]string{scenarioKey: scenario}, finalized[0].members, "f's context carries the scenario")
		require.True(t, finalized[0].fault.Abort)
		require.True(t, fc.gone("cache"))

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestTrustScenario_drops_every_other_member_of_the_annotation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		value := scenarioValue(t, noRules) + ",region=eu,acme.other=1"
		fc := newFakeCluster(t, annotated(memcachedObject("cache", 3), value))
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)

		var got sightings
		sv := serve(t, newController(t, s, fc, testSettings(), sighting(s, &got),
			kube.TrustScenario(scenarioAnnotation, trustAll),
		))
		time.Sleep(settle)

		reconciled := got.of("cache")
		require.Len(t, reconciled, 1)
		require.Equal(t, []string{scenarioKey}, slices.Sorted(maps.Keys(reconciled[0].members)),
			"only the Decoder's member joins: tenant, region and acme.other are dropped")
		require.Zero(t, reconciled[0].fault, "a scenario of no rules injects nothing")
		require.Len(t, allEvents(tel, evaluationEvent), 1, "and is still evaluated")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestTrustScenario_strips_an_untrusted_objects_scenario_and_an_annotation_that_isnt_baggage(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t,
			annotated(memcachedObject("untrusted", 3), scenarioValue(t, abort(ownPoint))),
			annotated(memcachedObject("garbled", 3), "not baggage"),
		)
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)

		var got sightings
		tr := &truster{trusted: []string{"garbled"}}
		sv := serve(t, newController(t, s, fc, testSettings(), sighting(s, &got),
			kube.TrustScenario(scenarioAnnotation, tr.trust),
		))
		time.Sleep(settle)

		requireNoScenario(t, got.of("untrusted"))
		requireNoScenario(t, got.of("garbled"))
		require.Equal(t, []string{"untrusted"}, tr.names(), "an annotation that isn't baggage carries no scenario to ask about")

		spans := reconcileSpans(tel, "untrusted")
		require.Len(t, spans, 1)
		strips := spanEvents(spans[0], stripEvent)
		require.Len(t, strips, 1, "the untrusted scenario is stripped on the reconcile's span")
		require.Empty(t, strips[0].Attributes, "as a server's strip, with no attributes")
		require.Empty(t, spanEvents(spans[0], evaluationEvent), "and never evaluated")

		spans = reconcileSpans(tel, "garbled")
		require.Len(t, spans, 1)
		strips = spanEvents(spans[0], stripEvent)
		require.Len(t, strips, 1, "the annotation that isn't baggage is stripped on the reconcile's span")
		attrs := eventAttrs(strips[0])
		require.Len(t, attrs, 2, "error.type and error.message: %v", strips[0].Attributes)
		require.NotEmpty(t, attrs["error.type"].AsString())
		require.Contains(t, attrs["error.message"].AsString(), "invalid")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestTrustScenario_never_asks_about_an_object_without_a_scenario(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		other := memcachedObject("other-annotation", 3)
		other.SetAnnotations(map[string]string{"acme.example.com/owner": scenarioValue(t, abort(ownPoint))})
		fc := newFakeCluster(t,
			memcachedObject("plain", 3),
			other,
			annotated(memcachedObject("empty", 3), ""),
			annotated(memcachedObject("tenant", 3), scenarioValue(t, nil)),
		)
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)

		var got sightings
		tr := &truster{}
		sv := serve(t, newController(t, s, fc, testSettings(), sighting(s, &got),
			kube.TrustScenario(scenarioAnnotation, tr.trust),
		))
		time.Sleep(settle)

		for _, name := range []string{"plain", "other-annotation", "empty", "tenant"} {
			requireNoScenario(t, got.of(name))
		}
		require.Empty(t, tr.names(), "no annotation carries a scenario")
		require.Empty(t, allEvents(tel, stripEvent), "and nothing is recorded")
		require.Empty(t, allEvents(tel, evaluationEvent))

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestTrustScenario_an_owned_resources_change_carries_the_owners_scenario(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		value := scenarioValue(t, abort(ownPoint))
		fc := newFakeCluster(t, annotated(memcachedObject("cache", 3), value))
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)

		var got sightings
		tr := &truster{trusted: []string{"cache"}}
		sv := serve(t, newController(t, s, fc, testSettings(), sighting(s, &got),
			kube.Owns(deployments),
			kube.TrustScenario(scenarioAnnotation, tr.trust),
		))
		time.Sleep(settle)
		require.Len(t, got.of("cache"), 1)

		// The Deployment's own annotation carries another scenario, which
		// isn't read.
		d := deployment("cache", controlledBy("cache"))
		d.Annotations = map[string]string{scenarioAnnotation: scenarioValue(t, delay(ownPoint, time.Second))}
		fc.createDeployment(t, d)
		time.Sleep(settle)
		fc.changeDeployment(t, "cache")
		time.Sleep(settle)

		want, err := baggage.Parse(value)
		require.NoError(t, err)
		reconciled := got.of("cache")
		require.Len(t, reconciled, 3, "once for itself, and once for each change to its Deployment")
		for _, r := range reconciled {
			require.Equal(t, map[string]string{scenarioKey: want.Member(scenarioKey).Value()}, r.members, "the owner's scenario")
			require.Equal(t, fit.Fault{Abort: true}, r.fault, "and not the Deployment's delay")
		}
		require.Len(t, tr.asked, 3)
		for _, obj := range tr.asked {
			require.Equal(t, value, obj.GetAnnotations()[scenarioAnnotation], "trust is asked about the owner")
			require.Equal(t, "Memcached", obj.(runtime.Object).GetObjectKind().GroupVersionKind().Kind)
		}

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestTrustScenario_recovers_a_panic_in_trust_and_never_retries_it(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		value := scenarioValue(t, abort(ownPoint))
		fc := newFakeCluster(t,
			annotated(memcachedObject("cache", 3), value),
			annotated(memcachedObject("other", 2), value),
		)
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)

		var got sightings
		tr := &truster{trusted: []string{"other"}}
		trust := func(obj metav1.Object) bool {
			trusted := tr.trust(obj)
			if obj.GetName() == "cache" {
				outOfRange(kube.Object[MemcachedSpec, MemcachedStatus]{Spec: MemcachedSpec{Size: 3}})
			}
			return trusted
		}
		sv := serve(t, newController(t, s, fc, panicSettings(), sighting(s, &got),
			kube.Finalize(finalizerName, noCleanup),
			kube.TrustScenario(scenarioAnnotation, trust),
		))
		time.Sleep(settle)

		askedAbout := func(name string) int {
			var n int
			for _, asked := range tr.names() {
				if asked == name {
					n++
				}
			}
			return n
		}
		require.Equal(t, 1, askedAbout("cache"), "no retry, though RetryMaxDelay has passed many times over")
		requirePanicRecorded(t, tel, "cache", 1)
		require.Empty(t, got.of("cache"), "the Reconciler isn't called")
		for _, a := range fc.metadataApplies() {
			require.NotEqual(t, "cache", a.name, "no finalizer is applied after a panic")
		}
		require.Empty(t, fc.object(t, "cache").GetFinalizers())
		require.Len(t, got.of("other"), 1, "the other object is still reconciled")
		require.True(t, got.of("other")[0].fault.Abort, "with its scenario")

		counts, _ := outcomes(t, controllerMetrics(t, tel))
		require.Equal(t, uint64(1), counts["panic"], "the panic's outcome")
		require.False(t, sv.returned(), "a panic in trust never ends the Service")

		fc.relabel(t, "cache", map[string]string{"changed": "yes"})
		time.Sleep(settle)
		require.Equal(t, 2, askedAbout("cache"), "asked again when it changes, and only once")
		requirePanicRecorded(t, tel, "cache", 2)

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestTrustScenario_reads_nothing_in_an_App_without_failure_injection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t,
			annotated(memcachedObject("cache", 3), scenarioValue(t, abort(ownPoint))),
			annotated(memcachedObject("garbled", 3), "not baggage"),
		)
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)

		var got sightings
		tr := &truster{trusted: []string{"cache", "garbled"}}
		sv := serve(t, newController(t, s, fc, testSettings(), sighting(s, &got),
			kube.TrustScenario(scenarioAnnotation, tr.trust),
		))
		time.Sleep(settle)

		requireNoScenario(t, got.of("cache"))
		requireNoScenario(t, got.of("garbled"))
		require.Empty(t, tr.names(), "trust is never asked")
		require.Empty(t, allEvents(tel, stripEvent), "and nothing is recorded, not even the annotation that isn't baggage")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestTrustScenario_reads_nothing_without_the_Part(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t,
			annotated(memcachedObject("cache", 3), scenarioValue(t, abort(ownPoint))),
			annotated(memcachedObject("garbled", 3), "not baggage"),
		)
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)

		var got sightings
		sv := serve(t, newController(t, s, fc, testSettings(), sighting(s, &got)))
		time.Sleep(settle)

		requireNoScenario(t, got.of("cache"))
		requireNoScenario(t, got.of("garbled"))
		require.Empty(t, allEvents(tel, stripEvent))

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestTrustScenario_carries_a_Kinds_objects_scenario(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		value := scenarioValue(t, abort(ownPoint))
		fc := newConfigMapCluster(t,
			annotated(configMapObject("trusted", map[string]any{"a": "1"}), value),
			annotated(configMapObject("untrusted", map[string]any{"a": "1"}), value),
		)
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)

		var got sightings
		rec := kube.KindReconcilerFunc[corev1.ConfigMap](func(ctx context.Context, cm corev1.ConfigMap) (time.Duration, error) {
			got.add(ctx, s, cm.Name)
			return 0, nil
		})
		tr := &truster{trusted: []string{"trusted"}}
		sv := serve(t, newKindController(t, s, fc, ConfigMaps, rec, kube.TrustScenario(scenarioAnnotation, tr.trust)))
		time.Sleep(settle)

		reconciled := got.of("trusted")
		require.Len(t, reconciled, 1)
		require.Equal(t, []string{scenarioKey}, slices.Sorted(maps.Keys(reconciled[0].members)))
		require.True(t, reconciled[0].fault.Abort)
		requireNoScenario(t, got.of("untrusted"))
		require.ElementsMatch(t, []string{"trusted", "untrusted"}, tr.names())
		require.Len(t, allEvents(tel, stripEvent), 1, "the untrusted ConfigMap's scenario is stripped")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}
