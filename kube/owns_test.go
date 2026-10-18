// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clienttesting "k8s.io/client-go/testing"
	kptr "k8s.io/utils/ptr"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// deployments is the resource the controllers under test own.
var deployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// controlledBy returns the controller owner reference that names the
// Memcached default/name.
func controlledBy(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "cache.example.com/v1alpha1",
		Kind:       "Memcached",
		Name:       name,
		UID:        types.UID(name + "-uid"),
		Controller: kptr.To(true),
	}
}

// deployment returns the metadata of a Deployment named name, in the
// default namespace, with refs.
func deployment(name string, refs ...metav1.OwnerReference) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", OwnerReferences: refs},
	}
}

// createDeployment adds d to the fakes, as the API server would on a create.
func (fc *fakeCluster) createDeployment(t *testing.T, d *metav1.PartialObjectMetadata) {
	t.Helper()
	require.NoError(t, fc.meta.Tracker().Create(deployments, d, d.Namespace))
}

// changeDeployment changes the default/name Deployment's labels, as a hand
// edit of it would.
func (fc *fakeCluster) changeDeployment(t *testing.T, name string) {
	t.Helper()
	got, err := fc.meta.Tracker().Get(deployments, "default", name)
	require.NoError(t, err)
	d := got.(*metav1.PartialObjectMetadata).DeepCopy()
	d.Labels = map[string]string{"changed": time.Now().String()}
	require.NoError(t, fc.meta.Tracker().Update(deployments, d, "default"))
}

// deleteDeployment deletes the default/name Deployment.
func (fc *fakeCluster) deleteDeployment(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, fc.meta.Tracker().Delete(deployments, "default", name))
}

func TestOwns_reconciles_the_owner(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ref  metav1.OwnerReference
	}{
		{"its controller owner reference names it", controlledBy("cache")},
		{"at another version of its group", func() metav1.OwnerReference {
			ref := controlledBy("cache")
			ref.APIVersion = "cache.example.com/v1beta1"
			return ref
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fc := newFakeCluster(t, memcachedObject("cache", 3), memcachedObject("other", 1))
				fc.createDeployment(t, deployment("cache", tc.ref))
				var got reconciles
				s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
				sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Owns(deployments)))
				time.Sleep(settle)
				before := got.count()
				require.Equal(t, 4, before, "each Memcached once for itself and once for its status")

				fc.changeDeployment(t, "cache")
				time.Sleep(settle)
				require.Equal(t, before+1, got.count(), "a change to the Deployment reconciles its owner")
				require.Equal(t, "cache", got.objs[before].Name)

				fc.deleteDeployment(t, "cache")
				time.Sleep(settle)
				require.Equal(t, before+2, got.count(), "the Deployment's deletion reconciles its owner")
				require.Equal(t, "cache", got.objs[before+1].Name)

				sv.cancel()
				require.NoError(t, sv.wait(t))
			})
		})
	}

	t.Run("when the owned resource is added", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			fc := newFakeCluster(t, memcachedObject("cache", 3))
			var got reconciles
			s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
			sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Owns(deployments)))
			time.Sleep(settle)
			before := got.count()

			fc.createDeployment(t, deployment("cache", controlledBy("cache")))
			time.Sleep(settle)
			require.Equal(t, before+1, got.count(), "the Deployment's creation reconciles its owner")

			sv.cancel()
			require.NoError(t, sv.wait(t))
		})
	})
}

func TestOwns_queues_nothing(t *testing.T) {
	t.Parallel()

	notController := controlledBy("cache")
	notController.Controller = nil
	controllerFalse := controlledBy("cache")
	controllerFalse.Controller = kptr.To(false)
	otherGroup := controlledBy("cache")
	otherGroup.APIVersion = "other.example.com/v1alpha1"
	otherKind := controlledBy("cache")
	otherKind.Kind = "Redis"

	cases := []struct {
		name string
		refs []metav1.OwnerReference
	}{
		{"for an owner reference without controller", []metav1.OwnerReference{notController}},
		{"for an owner reference with controller: false", []metav1.OwnerReference{controllerFalse}},
		{"for an owner reference of another group", []metav1.OwnerReference{otherGroup}},
		{"for an owner reference of another kind", []metav1.OwnerReference{otherKind}},
		{"for a resource with no owner reference", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fc := newFakeCluster(t, memcachedObject("cache", 3))
				var got reconciles
				s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
				sv := serve(t, newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Owns(deployments)))
				time.Sleep(settle)
				before := got.count()

				fc.createDeployment(t, deployment("cache", tc.refs...))
				time.Sleep(settle)
				fc.changeDeployment(t, "cache")
				time.Sleep(settle)
				fc.deleteDeployment(t, "cache")
				time.Sleep(settle)
				require.Equal(t, before, got.count(), "neither the Deployment's creation, its change nor its deletion reconciles")

				sv.cancel()
				require.NoError(t, sv.wait(t))
			})
		})
	}
}

func TestOwns_watches_limited_to_the_namespace(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		var (
			mu         sync.Mutex
			namespaces []string
		)
		record := func(a clienttesting.Action) {
			mu.Lock()
			defer mu.Unlock()
			namespaces = append(namespaces, a.GetVerb()+" "+a.GetNamespace())
		}
		fc.meta.PrependReactor("list", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
			record(a)
			return false, nil, nil
		})
		fc.meta.PrependWatchReactor("deployments", func(a clienttesting.Action) (bool, watch.Interface, error) {
			record(a)
			return false, nil, nil
		})
		settings := testSettings()
		settings.Namespace = "default"
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		sv := serve(t, newController(t, s, fc, settings, readyReplicas(&reconciles{}), kube.Owns(deployments)))
		time.Sleep(settle)
		sv.cancel()
		require.NoError(t, sv.wait(t))

		mu.Lock()
		defer mu.Unlock()
		require.Contains(t, namespaces, "list default")
		require.Contains(t, namespaces, "watch default", "the informer watches")
		for _, got := range namespaces {
			require.Regexp(t, `^(list|watch) default$`, got, "every list and watch of the owned resource is in Settings.Namespace")
		}
	})
}

func TestOwns_Synced_waits_for_the_owned_cache(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		// The first list, Serve's check that the resource is served, is
		// answered at once. The informer's list waits for release, so its
		// cache doesn't sync until then.
		var lists atomic.Int32
		release := make(chan struct{})
		fc.meta.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
			if lists.Add(1) > 1 {
				<-release
			}
			return false, nil, nil
		})
		var got reconciles
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		ctrl := newController(t, s, fc, testSettings(), readyReplicas(&got), kube.Owns(deployments))
		sv := serve(t, ctrl)
		time.Sleep(settle)

		requireHealthy(t, ctrl.Synced(), false, "while the owned resource's cache hasn't synced")
		require.Zero(t, got.count(), "nothing is reconciled before every cache has synced")
		require.Empty(t, fc.holder(t), "no Lease is campaigned for before every cache has synced")

		close(release)
		time.Sleep(settle)
		requireHealthy(t, ctrl.Synced(), true, "once every cache has synced")
		require.Equal(t, 2, got.count())

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestOwns_Serve_fails_on_an_owned_resource_that_isnt_served(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 3))
		fc.meta.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(deployments.GroupResource(), "")
		})
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		ctrl := newController(t, s, fc, testSettings(), readyReplicas(&reconciles{}), kube.Owns(deployments))
		err := ctrl.Serve(t.Context())
		require.ErrorContains(t, err, "kube: listing apps/v1, Resource=deployments, which the Controller owns: ")
		require.True(t, apierrors.IsNotFound(err))
		require.Empty(t, fc.holder(t), "no Lease was campaigned for")
	})
}
