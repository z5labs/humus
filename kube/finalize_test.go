// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"
	kptr "k8s.io/utils/ptr"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// finalizerName is the finalizer the controllers under test add.
const finalizerName = "cache.example.com/finalizer"

// noCleanup is a Finalize's f that has nothing to clean up.
func noCleanup(context.Context, kube.Object[MemcachedSpec, MemcachedStatus]) error {
	return nil
}

// cleanups records each call of a Finalize's f, and the reconcile's span its
// context carried.
type cleanups struct {
	mu    sync.Mutex
	objs  []kube.Object[MemcachedSpec, MemcachedStatus]
	at    []time.Time
	spans []bool
}

func (c *cleanups) add(ctx context.Context, obj kube.Object[MemcachedSpec, MemcachedStatus]) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objs = append(c.objs, obj)
	c.at = append(c.at, time.Now())
	c.spans = append(c.spans, spanFromContext(ctx))
	return len(c.objs)
}

func (c *cleanups) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.objs)
}

func (c *cleanups) times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.at...)
}

// cleanup returns a Finalize's f that records each call in got, and fails
// the first failures of them.
func cleanup(got *cleanups, failures int) func(context.Context, kube.Object[MemcachedSpec, MemcachedStatus]) error {
	return func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) error {
		if got.add(ctx, m) <= failures {
			return errors.New("the backups weren't deleted")
		}
		return nil
	}
}

func TestFinalize_adds_the_finalizer_before_the_first_reconcile(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var (
			got     reconciles
			cleaned cleanups
		)
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Finalize(finalizerName, cleanup(&cleaned, 0))))
		time.Sleep(settle)

		applies := fc.metadataApplies()
		require.Len(t, applies, 1, "the finalizer is added once")
		a := applies[0]
		require.Equal(t, "default", a.namespace)
		require.Equal(t, "cache", a.name)
		require.Equal(t, controllerName, a.options.FieldManager)
		require.Equal(t, kptr.To(true), a.options.Force)
		require.Equal(t, map[string]any{
			"apiVersion": "cache.example.com/v1alpha1",
			"kind":       "Memcached",
			"metadata": map[string]any{
				"name":       "cache",
				"namespace":  "default",
				"uid":        "cache-uid",
				"finalizers": []any{finalizerName},
			},
		}, a.patch)
		require.Equal(t, []string{finalizerName}, fc.object(t, "cache").GetFinalizers())

		require.Equal(t, 2, got.count(), "once for the object, and once for its own status")
		require.Equal(t, []string{finalizerName}, got.objs[0].Finalizers, "the first reconcile is of the object with its finalizer")
		require.Zero(t, cleaned.count(), "f is called only for an object being deleted")

		var spans int
		for _, span := range tel.Spans() {
			if span.Name() == "reconcile Memcached" {
				require.Equal(t, codes.Unset, span.Status().Code)
				spans++
			}
		}
		require.Equal(t, 3, spans, "adding the finalizer, and the two reconciles")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestFinalize_calls_f_in_place_of_the_Reconciler(t *testing.T) {
	t.Parallel()

	t.Run("and the deletion completes", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t, memcachedObject("cache", 3))
			var (
				got     reconciles
				cleaned cleanups
			)
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Finalize(finalizerName, cleanup(&cleaned, 0))))
			time.Sleep(settle)
			require.Equal(t, 2, got.count())

			fc.deleting(t, "cache")
			time.Sleep(settle)

			require.Equal(t, 2, got.count(), "the Reconciler isn't called for an object being deleted")
			require.Equal(t, 1, cleaned.count(), "f is called once")
			obj := cleaned.objs[0]
			require.Equal(t, "cache", obj.Name)
			require.Equal(t, "default", obj.Namespace)
			require.NotNil(t, obj.DeletionTimestamp)
			require.Equal(t, int32(3), obj.Spec.Size)
			require.Equal(t, int32(3), obj.Status.ReadyReplicas, "f is handed the object as the cache holds it")
			require.True(t, cleaned.spans[0], "f's context carries the reconcile's span")

			applies := fc.metadataApplies()
			require.Len(t, applies, 2, "the finalizer is added, then removed")
			require.Equal(t, controllerName, applies[1].options.FieldManager)
			require.Equal(t, kptr.To(true), applies[1].options.Force)
			require.Equal(t, map[string]any{
				"apiVersion": "cache.example.com/v1alpha1",
				"kind":       "Memcached",
				"metadata":   map[string]any{"name": "cache", "namespace": "default", "uid": "cache-uid"},
			}, applies[1].patch, "the metadata is applied without the finalizer")
			require.True(t, fc.gone("cache"), "the deletion completes")

			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})

	t.Run("while another finalizer holds it", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			obj := memcachedObject("cache", 3)
			obj.SetFinalizers([]string{"example.com/someone-elses"})
			fc := newFakeCluster(t, obj)
			var (
				got     reconciles
				cleaned cleanups
			)
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Finalize(finalizerName, cleanup(&cleaned, 0))))
			time.Sleep(settle)
			require.Equal(t, []string{"example.com/someone-elses", finalizerName}, fc.object(t, "cache").GetFinalizers())

			fc.deleting(t, "cache")
			time.Sleep(settle)

			require.Equal(t, 1, cleaned.count(), "f is called once, and not again while the other finalizer holds the object")
			require.Equal(t, 2, got.count())
			require.Equal(t, []string{"example.com/someone-elses"}, fc.object(t, "cache").GetFinalizers(),
				"only the controller's finalizer is removed")

			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})

	t.Run("only when its finalizer holds the object", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			// Deleted before its first reconcile, so the finalizer was
			// never added.
			obj := memcachedObject("cache", 3)
			obj.SetDeletionTimestamp(kptr.To(metav1.NewTime(time.Now())))
			obj.SetFinalizers([]string{"example.com/someone-elses"})
			fc := newFakeCluster(t, obj)
			var (
				got     reconciles
				cleaned cleanups
			)
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Finalize(finalizerName, cleanup(&cleaned, 0))))
			time.Sleep(settle)

			require.Zero(t, cleaned.count())
			require.Zero(t, got.count())
			require.Empty(t, fc.metadataApplies(), "the finalizer isn't added to an object being deleted")

			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})
}

func TestFinalize_retries_an_error_then_removes_the_finalizer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var (
			got     reconciles
			cleaned cleanups
		)
		settings := testSettings()
		settings.RetryBaseDelay = time.Second
		settings.RetryMaxDelay = 4 * time.Second
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, settings, readyReplicas(&got), kube.Finalize(finalizerName, cleanup(&cleaned, 3))))
		time.Sleep(settle)

		fc.deleting(t, "cache")
		time.Sleep(settle)

		require.Equal(t, 4, cleaned.count(), "three failures, then a success")
		at := cleaned.times()
		var gaps []time.Duration
		for i := 1; i < len(at); i++ {
			gaps = append(gaps, at[i].Sub(at[i-1]))
		}
		require.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, gaps, "retried with backoff, as a reconcile is")
		require.Len(t, fc.metadataApplies(), 2, "the finalizer is removed only once f returns nil")
		require.True(t, fc.gone("cache"))
		require.Equal(t, 2, got.count(), "the Reconciler is never called for it")
		require.False(t, sv.returned(), "an error from f never ends the Service")

		var failed int
		for _, span := range tel.Spans() {
			if span.Name() == "reconcile Memcached" && span.Status().Code == codes.Error {
				require.Equal(t, "the backups weren't deleted", span.Status().Description)
				failed++
			}
		}
		require.Equal(t, 3, failed, "each call of f is a reconcile's span")
		var warned int
		for _, r := range tel.Logs() {
			if r.Body().AsString() != "reconcile failed" {
				continue
			}
			warned++
			require.Equal(t, log.SeverityWarn, r.Severity())
			require.True(t, r.TraceID().IsValid(), "in the span's context")
			attrs := recordAttrs(&r)
			require.Equal(t, "default", attrs["k8s.namespace.name"])
			require.Equal(t, "cache", attrs["humus.kube.object.name"])
			require.Equal(t, "the backups weren't deleted", attrs["exception.message"])
		}
		require.Equal(t, 3, warned, "a failed f is the reconcile failed record")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestFinalize_retries_a_failed_apply(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		// The first two applies of the metadata are refused.
		var refused atomic.Int32
		fc.dyn.PrependReactor("patch", Memcached.Plural, func(a clienttesting.Action) (bool, runtime.Object, error) {
			if p, ok := a.(clienttesting.PatchActionImpl); !ok || p.Subresource != "" || refused.Load() >= 2 {
				return false, nil, nil
			}
			refused.Add(1)
			return true, nil, apierrors.NewForbidden(Memcached.GVR().GroupResource(), "cache", errors.New("no RBAC"))
		})
		var got reconciles
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Finalize(finalizerName, noCleanup)))
		time.Sleep(settle)

		require.Equal(t, int32(2), refused.Load())
		require.Equal(t, []string{finalizerName}, fc.object(t, "cache").GetFinalizers(), "the add is retried")
		require.Equal(t, 2, got.count(), "the Reconciler waits for the finalizer")
		for _, obj := range got.objs {
			require.Equal(t, []string{finalizerName}, obj.Finalizers)
		}
		var failed int
		for _, span := range tel.Spans() {
			if span.Status().Code == codes.Error {
				require.Contains(t, span.Status().Description, "kube: adding the finalizer cache.example.com/finalizer of default/cache: ")
				failed++
			}
		}
		require.Equal(t, 2, failed)

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestFinalize_reports_a_finalizer_another_field_manager_set(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// Set by hand, so the controller's apply doesn't own it.
		obj := memcachedObject("cache", 3)
		obj.SetFinalizers([]string{finalizerName})
		fc := newFakeCluster(t, obj)
		var (
			got     reconciles
			cleaned cleanups
		)
		tel := humustest.NewTelemetry()
		s := newSetup(t, humus.Host{Dial: noDial}, tel)
		sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Finalize(finalizerName, cleanup(&cleaned, 0))))
		time.Sleep(settle)
		require.Empty(t, fc.metadataApplies(), "the object already has the finalizer")
		require.Equal(t, 2, got.count())

		fc.deleting(t, "cache")
		time.Sleep(settle)

		require.Greater(t, cleaned.count(), 1, "the removal is retried, with f")
		require.False(t, fc.gone("cache"))
		var failed bool
		for _, span := range tel.Spans() {
			if span.Status().Code == codes.Error {
				require.Equal(t, "kube: removing the finalizer cache.example.com/finalizer of default/cache: another field manager also set it, so it is still on the object",
					span.Status().Description)
				failed = true
			}
		}
		require.True(t, failed)

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}
