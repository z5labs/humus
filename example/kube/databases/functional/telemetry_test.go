//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package functional

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	telemetryTimeout = 4 * time.Minute
	pollInterval     = 2 * time.Second
	logLookback      = time.Hour
	controllerName   = "databases-controller"
)

var databases = schema.GroupVersionResource{Group: "postgres.example.com", Version: "v1alpha1", Resource: "databases"}

func TestDatabases_sends_its_telemetry_to_the_LGTM_stack_on_kind(t *testing.T) {
	for _, d := range []string{"databases", "otel-collector", "loki", "tempo", "mimir"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	tempo := forward(t, "tempo", 3200)
	loki := forward(t, "loki", 3100)
	mimir := forward(t, "mimir", 9009)

	ctx, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	t.Cleanup(cancel)

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	start := time.Now()
	ns, name := applyDatabase(ctx, t)
	t.Logf("applied the Database %s/%s", ns, name)

	eventually(ctx, t, "the Database's reconcile trace in Tempo", func(ctx context.Context) (bool, error) {
		q := url.Values{
			"q":     {fmt.Sprintf(`{ resource.service.name = "databases" && span.k8s.namespace.name = %q && span.humus.kube.object.name = %q }`, ns, name)},
			"start": {strconv.FormatInt(start.Add(-time.Minute).Unix(), 10)},
			"end":   {strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)},
		}
		var res struct {
			Traces []json.RawMessage `json:"traces"`
		}
		ok, err := getJSON(ctx, client, "http://"+tempo+"/api/search?"+q.Encode(), &res)
		return ok && len(res.Traces) > 0, err
	})

	eventually(ctx, t, "the controller's records in Loki", func(ctx context.Context) (bool, error) {
		q := url.Values{
			"query": {`{service_name="databases"}`},
			"start": {strconv.FormatInt(start.Add(-logLookback).UnixNano(), 10)},
			"limit": {"100"},
		}
		var res struct {
			Data struct {
				Result []json.RawMessage `json:"result"`
			} `json:"data"`
		}
		ok, err := getJSON(ctx, client, "http://"+loki+"/loki/api/v1/query_range?"+q.Encode(), &res)
		return ok && len(res.Data.Result) > 0, err
	})

	controller := fmt.Sprintf(`job="databases", humus_kube_controller_name=%q`, controllerName)
	for _, c := range []struct {
		what  string
		query string
	}{
		{"humus.kube.controller.queue.depth", `humus_kube_controller_queue_depth{` + controller + `}`},
		{"humus.kube.controller.queue.adds", `humus_kube_controller_queue_adds{` + controller + `} > 0`},
		{"humus.kube.controller.queue.duration", `humus_kube_controller_queue_duration_count{` + controller + `} > 0`},
		{"humus.kube.controller.reconcile.duration", `humus_kube_controller_reconcile_duration_count{` + controller + `, humus_kube_controller_reconcile_outcome="success"} > 0`},
		{"humus.kube.controller.reconcile.unfinished_duration", `humus_kube_controller_reconcile_unfinished_duration{` + controller + `}`},
		{"humus.kube.controller.reconcile.longest_unfinished_duration", `humus_kube_controller_reconcile_longest_unfinished_duration{` + controller + `}`},
		{"the controller containers' restarts", `k8s_container_restarts{k8s_namespace_name="databases", k8s_container_name="databases"}`},
	} {
		eventually(ctx, t, c.what+" in Mimir", func(ctx context.Context) (bool, error) {
			var res struct {
				Data struct {
					Result []json.RawMessage `json:"result"`
				} `json:"data"`
			}
			ok, err := getJSON(ctx, client, "http://"+mimir+"/prometheus/api/v1/query?"+url.Values{"query": {c.query}}.Encode(), &res)
			return ok && len(res.Data.Result) > 0, err
		})
	}
}

func applyDatabase(ctx context.Context, t *testing.T) (string, string) {
	t.Helper()
	cfg := kubeconfig(t)
	kc, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dc, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)

	ns, err := kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "databases-telemetry-"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		deleteNamespace(ctx, t, kc, ns.Name)
	})

	db := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": databases.GroupVersion().String(),
		"kind":       "Database",
		"metadata":   map[string]any{"name": "orders"},
		"spec":       map[string]any{"name": "db_" + strings.ToLower(rand.Text()), "deletionPolicy": "Delete"},
	}}
	_, err = dc.Resource(databases).Namespace(ns.Name).Create(ctx, db, metav1.CreateOptions{})
	require.NoError(t, err, "creating the Database %s/orders", ns.Name)
	return ns.Name, "orders"
}

func deleteNamespace(ctx context.Context, t *testing.T, kc kubernetes.Interface, name string) {
	t.Helper()
	err := kc.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	require.NoError(t, err, "deleting the namespace %s", name)
	eventually(ctx, t, "the namespace "+name+" to be gone", func(ctx context.Context) (bool, error) {
		_, err := kc.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func getJSON(ctx context.Context, client *http.Client, u string, v any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return true, json.Unmarshal(body, v)
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("GET %s: %s: %s", u, resp.Status, body)
	}
}

func eventually(ctx context.Context, t *testing.T, what string, found func(ctx context.Context) (bool, error)) {
	t.Helper()
	var last error
	for {
		ok, err := found(ctx)
		if ok && err == nil {
			return
		}
		if err != nil {
			last = err
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			require.FailNowf(t, "not found", "%s: %v (last error: %v)", what, context.Cause(ctx), last)
		}
	}
}
