// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// panicSettings returns testSettings with a backoff short enough that settle
// outlasts RetryMaxDelay many times over, so a retry would be seen.
func panicSettings() kube.Settings {
	s := testSettings()
	s.RetryBaseDelay = time.Second
	s.RetryMaxDelay = 4 * time.Second
	return s
}

// outOfRange panics as an index out of range does, for an object of a size
// the end user's code didn't expect.
func outOfRange(m kube.Object[MemcachedSpec, MemcachedStatus]) {
	var replicas []int32
	_ = replicas[m.Spec.Size]
}

// resize changes the size of the Memcached default/name, as kubectl edit
// would.
func (fc *fakeCluster) resize(t *testing.T, name string, size int64) {
	t.Helper()
	u := fc.object(t, name).DeepCopy()
	u.Object["spec"].(map[string]any)["size"] = size
	require.NoError(t, fc.dyn.Tracker().Update(Memcached.GVR(), u, "default"))
}

// relabel changes the labels of the Memcached default/name.
func (fc *fakeCluster) relabel(t *testing.T, name string, labels map[string]string) {
	t.Helper()
	u := fc.object(t, name).DeepCopy()
	u.SetLabels(labels)
	require.NoError(t, fc.dyn.Tracker().Update(Memcached.GVR(), u, "default"))
}

// panickedSpans returns the reconcile spans whose status is the panic's
// Error.
func panickedSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, span := range tel.Spans() {
		if span.Name() == "reconcile Memcached" && span.Status().Code == codes.Error && span.Status().Description == "reconcile panicked" {
			out = append(out, span)
		}
	}
	return out
}

// requirePanicRecorded checks tel holds want panics of the object default/name,
// each recorded as the package doc says: an exception event on an Error
// span, and one ERROR record, "reconcile panicked", in its context, with
// the stack of the panic, and no "reconcile failed".
func requirePanicRecorded(t *testing.T, tel *humustest.Telemetry, name string, want int) {
	t.Helper()
	const message = "runtime error: index out of range ["

	spans := panickedSpans(tel)
	require.Len(t, spans, want)
	for _, span := range spans {
		attrs := map[string]string{}
		for _, kv := range span.Attributes() {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		require.Equal(t, name, attrs["humus.kube.object.name"])

		events := span.Events()
		require.Len(t, events, 1)
		require.Equal(t, "exception", events[0].Name)
		ev := map[string]string{}
		for _, kv := range events[0].Attributes {
			ev[string(kv.Key)] = kv.Value.String()
		}
		require.Equal(t, "runtime.boundsError", ev["exception.type"])
		require.Contains(t, ev["exception.message"], message)
		require.Contains(t, ev["exception.stacktrace"], "kube_test.outOfRange", "the stack of the panic")
	}

	var panicked int
	for _, r := range tel.Logs() {
		switch r.Body().AsString() {
		case "reconcile failed":
			if recordAttrs(&r)["humus.kube.object.name"] == name {
				t.Fatalf("a panic is no reconcile failed record: %v", recordAttrs(&r))
			}
		case "reconcile panicked":
			panicked++
			require.Equal(t, log.SeverityError, r.Severity())
			require.True(t, r.TraceID().IsValid(), "in the span's context")
			attrs := recordAttrs(&r)
			require.Equal(t, "default", attrs["k8s.namespace.name"])
			require.Equal(t, name, attrs["humus.kube.object.name"])
			require.Equal(t, "runtime.boundsError", attrs["exception.type"])
			require.Contains(t, attrs["exception.message"], message)
			require.Contains(t, attrs["exception.stacktrace"], "kube_test.outOfRange")
		}
	}
	require.Equal(t, want, panicked, "one record per panic")
}

func TestController_recovers_a_panic_in_Reconcile_and_never_retries_it(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3), memcachedObject("other", 2))
		var cache, other reconciles
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			if m.Name == "cache" {
				cache.add(m)
				outOfRange(m)
			}
			return readyReplicas(&other)(ctx, m)
		})
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, panicSettings(), rec))
		time.Sleep(settle)

		require.Equal(t, 1, cache.count(), "no retry, though RetryMaxDelay has passed many times over")
		requirePanicRecorded(t, tel, "cache", 1)
		require.Equal(t, 2, other.count(), "the other object is still reconciled, and its status")
		for _, a := range fc.statusApplies() {
			require.NotEqual(t, "cache", a.name, "no status is applied after a panic")
		}
		require.Nil(t, fc.object(t, "cache").Object["status"])
		require.False(t, sv.returned(), "a panic never ends the Service")

		fc.resize(t, "cache", 4)
		time.Sleep(settle)
		require.Equal(t, 2, cache.count(), "reconciled again when it changes, and only once")
		requirePanicRecorded(t, tel, "cache", 2)
		require.False(t, sv.returned())

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestController_reconciles_a_panicked_object_changed_while_it_ran(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var got reconciles
		started := make(chan struct{})
		release := make(chan struct{})
		rec := kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus](func(_ context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
			if got.add(m) == 1 {
				close(started)
				<-release
				outOfRange(m)
			}
			return kube.Result[MemcachedStatus]{}, nil
		})
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, panicSettings(), rec))

		<-started
		fc.resize(t, "cache", 5)
		time.Sleep(time.Second)
		require.Equal(t, 1, got.count(), "one object is never reconciled twice at once")
		close(release)
		time.Sleep(settle)

		require.Equal(t, 2, got.count(), "the change made while it ran reconciles it again, once")
		require.Equal(t, int32(5), got.objs[1].Spec.Size)
		requirePanicRecorded(t, tel, "cache", 1)

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestFinalize_recovers_a_panic_in_f_and_keeps_the_finalizer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3), memcachedObject("other", 2))
		var (
			got     reconciles
			cleaned cleanups
		)
		f := func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) error {
			if cleaned.add(ctx, m) == 1 {
				outOfRange(m)
			}
			return nil
		}
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, panicSettings(), readyReplicas(&got), kube.Finalize(finalizerName, f)))
		time.Sleep(settle)
		require.Equal(t, 4, got.count(), "each object, and each one's status")

		fc.deleting(t, "cache")
		time.Sleep(settle)

		require.Equal(t, 1, cleaned.count(), "no retry, though RetryMaxDelay has passed many times over")
		requirePanicRecorded(t, tel, "cache", 1)
		require.Equal(t, []string{finalizerName}, fc.object(t, "cache").GetFinalizers(), "the finalizer still holds the object")
		require.Len(t, fc.metadataApplies(), 2, "each object's finalizer is added, and neither is removed")
		require.False(t, sv.returned(), "a panic of f never ends the Service")

		fc.relabel(t, "other", map[string]string{"changed": "yes"})
		time.Sleep(settle)
		require.Equal(t, 5, got.count(), "the other object is still reconciled")

		fc.relabel(t, "cache", map[string]string{"changed": "yes"})
		time.Sleep(settle)
		require.Equal(t, 2, cleaned.count(), "its deletion waits for its next reconcile")
		require.True(t, fc.gone("cache"), "which removes the finalizer")
		require.Equal(t, 5, got.count(), "the Reconciler is never called for it")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}
