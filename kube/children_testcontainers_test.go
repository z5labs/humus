//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

// The tests in this file run a whole App with failure injection, whose
// Controller reconciles Memcacheds in the k3s that
// kube_testcontainers_test.go starts, and carries the scenario a
// Memcached's annotation holds on to the Deployment its Reconciler applies.
// Only a real API server shows server-side apply removing the annotation
// once the experiment ends. kube_testcontainers_test.go says why they sit
// behind the testcontainers build tag.

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// deploymentWatcherName is the name of the second controller of the
// operator of two controllers, its Lease's and its field manager's.
const deploymentWatcherName = "deployment-watcher"

// Deployments is the Kind of the operator's second controller.
var Deployments = kube.Kind[appsv1.Deployment]{
	GVR:   appsv1.SchemeGroupVersion.WithResource("deployments"),
	Kind:  "Deployment",
	Scope: kube.Namespaced,
}

// createAnnotatedMemcached creates the Memcached default/name of r's
// group, annotated with a scenario of sc, and returns its client.
func createAnnotatedMemcached(t *testing.T, client *kube.Client, r kube.Resource[MemcachedSpec, MemcachedStatus], name string, sc *fit.Scenario) dynamic.ResourceInterface {
	t.Helper()
	crd, err := r.CRD()
	require.NoError(t, err)
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	memcacheds := dyn.Resource(r.GVR()).Namespace("default")
	obj := annotated(memcached(name, map[string]any{"size": int64(1), "image": "memcached:1.6"}), scenarioValue(t, sc))
	obj.SetAPIVersion(r.GVR().GroupVersion().String())
	_, err = memcacheds.Create(t.Context(), obj, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)
	return memcacheds
}

// deploymentAnnotation polls the Deployment default/name until done
// reports true of its scenario's annotation, and whether it has one.
func deploymentAnnotation(t *testing.T, kc kubernetes.Interface, name string, done func(value string, ok bool) bool) {
	t.Helper()
	err := apiwait.PollUntilContextTimeout(t.Context(), 250*time.Millisecond, appWaitLimit, true, func(ctx context.Context) (bool, error) {
		d, err := kc.AppsV1().Deployments("default").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		value, ok := d.Annotations[scenarioAnnotation]
		return done(value, ok), nil
	})
	require.NoError(t, err, "the Deployment's annotation never settled")
}

// onlyScenario returns a deploymentAnnotation condition: an annotation of
// exactly one member, the scenario's, whose value is scenario.
func onlyScenario(scenario string) func(string, bool) bool {
	return func(value string, ok bool) bool {
		if !ok {
			return false
		}
		b, err := baggage.Parse(value)
		return err == nil && b.Len() == 1 && b.Member(scenarioKey).Value() == scenario
	}
}

// noAnnotation is a deploymentAnnotation condition: no annotation at all.
func noAnnotation(_ string, ok bool) bool { return !ok }

func TestTrustScenario_an_annotated_Memcacheds_Deployment_carries_its_scenario_until_the_annotation_is_removed(t *testing.T) {
	client := k3sClient(t)
	kc, err := kubernetes.NewForConfig(client.Config())
	require.NoError(t, err)

	r := Memcached
	r.Group = "fitchild.example.com"
	const name = "fit-child"
	memcacheds := createAnnotatedMemcached(t, client, r, name, noRules)

	p, status := startFaultsController(t, r, controllerSettings(), kube.TrustScenario(scenarioAnnotation, trustAll))

	deploymentAnnotation(t, kc, name, onlyScenario(scenarioMember(t, noRules)))

	removeAnnotation(t, memcacheds, name)
	deploymentAnnotation(t, kc, name, noAnnotation)

	p.Signal(syscall.SIGTERM)
	st := appExit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

func TestTrustScenario_an_operator_of_two_controllers_carries_a_scenario_down(t *testing.T) {
	client := k3sClient(t)
	kc, err := kubernetes.NewForConfig(client.Config())
	require.NoError(t, err)

	r := Memcached
	r.Group = "fitoperator.example.com"
	const name = "fit-operator"
	sc := abort(ownPoint)
	memcacheds := createAnnotatedMemcached(t, client, r, name, sc)

	var watched sightings
	codec := faultsCodec(t)
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		client, err := kube.NewClient(s, cluster.conn)
		if err != nil {
			return nil, err
		}
		kc, err := kubernetes.NewForConfig(client.Config())
		if err != nil {
			return nil, err
		}
		memcachedCtrl, err := kube.NewController(s, client, controllerSettings(), r, deployer{kube: kc},
			kube.TrustScenario(scenarioAnnotation, trustAll),
		)
		if err != nil {
			return nil, err
		}
		settings := controllerSettings()
		settings.Name = deploymentWatcherName
		deploymentCtrl, err := kube.NewKindController(s, client, settings, Deployments,
			kube.KindReconcilerFunc[appsv1.Deployment](func(ctx context.Context, d appsv1.Deployment) (time.Duration, error) {
				watched.add(ctx, s, d.Name)
				return 0, nil
			}),
			kube.TrustScenario(scenarioAnnotation, trustAll),
		)
		if err != nil {
			return nil, err
		}
		return []humus.Service{memcachedCtrl, deploymentCtrl}, nil
	}, humustest.WithApp(
		humus.WithHost(humus.OSHost()),
		humus.WithFaults(func(context.Context, *humus.Setup) (fault.Settings, error) {
			settings := fault.DefaultSettings()
			settings.Decoder = codec
			return settings, nil
		}),
	))

	scenario := scenarioMember(t, sc)
	last := func() (seen, bool) {
		got := watched.of(name)
		if len(got) == 0 {
			return seen{}, false
		}
		return got[len(got)-1], true
	}

	// The Memcached's apply of its Deployment carries the scenario on, and
	// the Deployment's controller carries it into its reconciles, where its
	// own point meets it.
	err = apiwait.PollUntilContextTimeout(t.Context(), 250*time.Millisecond, appWaitLimit, true, func(context.Context) (bool, error) {
		s, ok := last()
		return ok && s.members[scenarioKey] == scenario, nil
	})
	require.NoError(t, err, "no reconcile of the Deployment carried the Memcached's scenario")
	s, _ := last()
	require.Equal(t, map[string]string{scenarioKey: scenario}, s.members, "only the scenario joins the Deployment's reconcile")
	require.True(t, s.fault.Abort, "the second controller's own point meets the scenario")

	// Ending the experiment on the Memcached ends it on the Deployment.
	removeAnnotation(t, memcacheds, name)
	deploymentAnnotation(t, kc, name, noAnnotation)
	err = apiwait.PollUntilContextTimeout(t.Context(), 250*time.Millisecond, appWaitLimit, true, func(context.Context) (bool, error) {
		s, ok := last()
		return ok && len(s.members) == 0, nil
	})
	require.NoError(t, err, "the Deployment was never reconciled without the scenario")
	s, _ = last()
	require.Zero(t, s.fault, "the Deployment's reconcile evaluates no fault once the experiment ends")

	p.Signal(syscall.SIGTERM)
	st := appExit(t, status)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}
