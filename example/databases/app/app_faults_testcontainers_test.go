//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus/example/databases/cases"
	"github.com/z5labs/humus/fault/jsonscenario"
)

const untrustedPrefix = "databases-untrusted-"

var databases = schema.GroupVersionResource{Group: "postgres.example.com", Version: "v1alpha1", Resource: "databases"}

var faultCases = map[string]func(t *testing.T, target cases.Target){
	"an untrusted scenario is stripped":              stripsUntrusted,
	"a trusted scenario is carried on to the Secret": carriesToSecret,
}

func stripsUntrusted(t *testing.T, target cases.Target) {
	f := newFaults(t, target)
	ns := f.namespace(t, untrustedPrefix)
	value := f.scenario(t, fit.Rule{Point: "postgresql:*", Fault: fit.Fault{Abort: true}})

	f.create(t, ns, "orders", value)

	f.awaitReady(t, ns, "orders", metav1.ConditionTrue, "Provisioned")
	_, carried := f.secretScenario(t, ns, "orders-postgres")
	require.False(t, carried, "the Secret of an untrusted Database carries a scenario")
}

func carriesToSecret(t *testing.T, target cases.Target) {
	f := newFaults(t, target)
	ns := f.namespace(t, target.Prefix)
	want := fit.Scenario{Rules: []fit.Rule{{Point: "postgresql:*", Fault: fit.Fault{Delay: 10 * time.Millisecond}}}}
	value := f.scenario(t, want.Rules...)

	f.create(t, ns, "orders", value)

	f.awaitReady(t, ns, "orders", metav1.ConditionTrue, "Provisioned")
	f.await(t, "the Secret to carry the scenario", func(ctx context.Context) (bool, error) {
		_, carried := f.secretScenario(t, ns, "orders-postgres")
		return carried, nil
	})
	got, _ := f.secretScenario(t, ns, "orders-postgres")
	require.Equal(t, want, got)

	f.unannotate(t, ns, "orders")
	f.await(t, "the Secret to carry no scenario", func(ctx context.Context) (bool, error) {
		_, carried := f.secretScenario(t, ns, "orders-postgres")
		return !carried, nil
	})
}

type faults struct {
	kube       kubernetes.Interface
	dynamic    dynamic.Interface
	codec      jsonscenario.Codec
	annotation string
	timeout    time.Duration
}

func newFaults(t *testing.T, target cases.Target) *faults {
	t.Helper()
	kc, err := kubernetes.NewForConfig(target.Config)
	require.NoError(t, err)
	dc, err := dynamic.NewForConfig(target.Config)
	require.NoError(t, err)
	codec, err := jsonscenario.New(target.ScenarioKey)
	require.NoError(t, err)
	return &faults{kube: kc, dynamic: dc, codec: codec, annotation: target.Annotation, timeout: target.Timeout}
}

func (f *faults) scenario(t *testing.T, rules ...fit.Rule) string {
	t.Helper()
	b, err := f.codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	return b.String()
}

func (f *faults) namespace(t *testing.T, prefix string) string {
	t.Helper()
	ns, err := f.kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		err := f.kube.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		require.NoError(t, err)
		f.await(t, "the namespace "+ns.Name+" to be gone", func(ctx context.Context) (bool, error) {
			_, err := f.kube.CoreV1().Namespaces().Get(ctx, ns.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
	})
	return ns.Name
}

func (f *faults) create(t *testing.T, ns, name, scenario string) {
	t.Helper()
	db := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": databases.GroupVersion().String(),
		"kind":       "Database",
		"metadata": map[string]any{
			"name":        name,
			"annotations": map[string]any{f.annotation: scenario},
		},
		"spec": map[string]any{"name": "db_" + strings.ToLower(rand.Text()), "deletionPolicy": "Delete"},
	}}
	_, err := f.dynamic.Resource(databases).Namespace(ns).Create(t.Context(), db, metav1.CreateOptions{})
	require.NoError(t, err, "creating the Database %s/%s", ns, name)
}

func (f *faults) unannotate(t *testing.T, ns, name string) {
	t.Helper()
	patch := `{"metadata":{"annotations":{"` + f.annotation + `":null}}}`
	_, err := f.dynamic.Resource(databases).Namespace(ns).Patch(context.Background(), name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	require.NoError(t, err, "removing the scenario from the Database %s/%s", ns, name)
}

func (f *faults) awaitReady(t *testing.T, ns, name string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	f.await(t, "the Database "+ns+"/"+name+" to be Ready "+string(status)+" with "+reason, func(ctx context.Context) (bool, error) {
		db, err := f.dynamic.Resource(databases).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		conditions, _, err := unstructured.NestedSlice(db.Object, "status", "conditions")
		if err != nil {
			return false, err
		}
		for _, c := range conditions {
			m, ok := c.(map[string]any)
			if ok && m["type"] == "Ready" {
				return m["status"] == string(status) && m["reason"] == reason, nil
			}
		}
		return false, nil
	})
}

func (f *faults) secretScenario(t *testing.T, ns, name string) (fit.Scenario, bool) {
	t.Helper()
	var secret *corev1.Secret
	f.await(t, "the Secret "+ns+"/"+name, func(ctx context.Context) (bool, error) {
		s, err := f.kube.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		secret = s
		return err == nil, err
	})
	b, err := baggage.Parse(secret.Annotations[f.annotation])
	require.NoError(t, err, "the Secret's annotation")
	got, carried, err := f.codec.Decode(b)
	require.NoError(t, err, "the Secret's scenario")
	return got, carried
}

func (f *faults) await(t *testing.T, what string, done func(ctx context.Context) (bool, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	var last error
	for {
		ok, err := done(ctx)
		if ok {
			return
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "waiting for %s: %v (last error: %v)", what, context.Cause(ctx), last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}
