//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/z5labs/humus/humustest"
)

const kubeScope = "github.com/z5labs/humus/kube"

func TestInit_traces_logs_and_measures_each_reconcile_without_a_credential(t *testing.T) {
	tel := humustest.NewTelemetry()
	c := startController(t, tel.Builder())
	dsn, err := url.Parse(c.target.Postgres)
	require.NoError(t, err)
	adminPassword, _ := dsn.User.Password()

	var ns, password string
	t.Run("a Database fails once, then is provisioned", func(t *testing.T) {
		f := newFaults(t, c.target)
		ns = f.namespace(t, c.target.Prefix)
		f.create(t, ns, "orders", f.scenario(t, fit.Rule{Point: "postgresql:*", Fault: fit.Fault{Abort: true}}))
		f.awaitReady(t, ns, "orders", metav1.ConditionFalse, "PostgresUnavailable")
		f.unannotate(t, ns, "orders")
		f.awaitReady(t, ns, "orders", metav1.ConditionTrue, "Provisioned")
		secret, err := f.kube.CoreV1().Secrets(ns).Get(t.Context(), "orders-postgres", metav1.GetOptions{})
		require.NoError(t, err)
		password = string(secret.Data["password"])
		require.NotEmpty(t, password)
	})

	c.stop(t)

	spans := tel.Spans()
	reconciles := reconcilesOf(spans, ns, "orders")
	require.NotEmpty(t, reconciles, "no reconcile span for the Database")

	t.Run("a reconcile's span has its admin operations' spans and its Secret's apply under it", func(t *testing.T) {
		var summaries []string
		applied := false
		for _, r := range reconciles {
			if r.Status().Code == codes.Error {
				continue
			}
			var admin, apply []sdktrace.ReadOnlySpan
			for _, s := range childrenOf(spans, r) {
				if v, ok := attr(s.Attributes(), "db.system.name"); ok && v.AsString() == "postgresql" {
					admin = append(admin, s)
					summary, _ := attr(s.Attributes(), "db.query.summary")
					summaries = append(summaries, summary.AsString())
				}
				if u, ok := attr(s.Attributes(), "url.full"); ok && s.Name() == "PATCH" && strings.Contains(u.AsString(), "/namespaces/"+ns+"/secrets/orders-postgres") {
					apply = append(apply, s)
				}
			}
			if len(admin) > 0 && len(apply) == 1 {
				applied = true
			}
		}
		require.True(t, applied, "no successful reconcile has both admin operations and the Secret's apply under it")
		require.Contains(t, summaries, "CREATE ROLE")
		require.Contains(t, summaries, "CREATE DATABASE")
	})

	t.Run("the failed reconcile's span is an error, and logged", func(t *testing.T) {
		failed := 0
		for _, r := range reconciles {
			if r.Status().Code == codes.Error {
				failed++
			}
		}
		require.Positive(t, failed)

		logged := 0
		for _, r := range tel.Logs() {
			if r.InstrumentationScope().Name != kubeScope || r.Body().AsString() != "reconcile failed" {
				continue
			}
			object, namespace := "", ""
			r.WalkAttributes(func(kv attribute.KeyValue) bool {
				switch kv.Key {
				case "humus.kube.object.name":
					object = kv.Value.AsString()
				case "k8s.namespace.name":
					namespace = kv.Value.AsString()
				}
				return true
			})
			if object == "orders" && namespace == ns {
				logged++
			}
		}
		require.Positive(t, logged, "no reconcile failed record for the Database")
	})

	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)

	t.Run("the humus.kube.controller instruments are recorded, with their outcomes", func(t *testing.T) {
		ms := metricsOf(rm, kubeScope)
		for _, name := range []string{
			"humus.kube.controller.queue.depth",
			"humus.kube.controller.queue.adds",
			"humus.kube.controller.queue.duration",
			"humus.kube.controller.reconcile.duration",
			"humus.kube.controller.reconcile.unfinished_duration",
			"humus.kube.controller.reconcile.longest_unfinished_duration",
		} {
			m, ok := ms[name]
			require.True(t, ok, "%s was not recorded", name)
			points, _ := dataPoints(m)
			require.NotEmpty(t, points, name)
			for _, set := range points {
				v, _ := set.Value("humus.kube.controller.name")
				require.Equal(t, controllerName, v.AsString(), name)
			}
		}
		outcomes := sumBy(ms["humus.kube.controller.reconcile.duration"], "humus.kube.controller.reconcile.outcome")
		require.Positive(t, outcomes["success"])
		require.Positive(t, outcomes["error"])
	})

	t.Run("no span attribute, log record or metric carries a password or the admin DSN", func(t *testing.T) {
		secrets := map[string]string{
			"the Database's password": password,
			"the admin's password":    adminPassword,
			"the admin DSN":           c.target.Postgres,
			"the service account's":   c.token,
		}
		for _, w := range written(t, tel) {
			for what, secret := range secrets {
				require.NotContains(t, w, secret, "%s was written", what)
			}
		}
	})
}

func reconcilesOf(spans []sdktrace.ReadOnlySpan, ns, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() != "reconcile Database" {
			continue
		}
		n, _ := attr(s.Attributes(), "humus.kube.object.name")
		namespace, _ := attr(s.Attributes(), "k8s.namespace.name")
		if n.AsString() == name && namespace.AsString() == ns {
			out = append(out, s)
		}
	}
	return out
}

func childrenOf(spans []sdktrace.ReadOnlySpan, parent sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Parent().SpanID() == parent.SpanContext().SpanID() {
			out = append(out, s)
		}
	}
	return out
}

func attr(kvs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func dataPoints(m metricdata.Metrics) (points []attribute.Set, values []float64) {
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			points, values = append(points, dp.Attributes), append(values, float64(dp.Value))
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			points, values = append(points, dp.Attributes), append(values, dp.Value)
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			points, values = append(points, dp.Attributes), append(values, float64(dp.Value))
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			points, values = append(points, dp.Attributes), append(values, dp.Value)
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			points, values = append(points, dp.Attributes), append(values, float64(dp.Count))
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			points, values = append(points, dp.Attributes), append(values, float64(dp.Count))
		}
	}
	return points, values
}

func metricsOf(rm metricdata.ResourceMetrics, scope string) map[string]metricdata.Metrics {
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			continue
		}
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func sumBy(m metricdata.Metrics, key string) map[string]float64 {
	out := map[string]float64{}
	points, values := dataPoints(m)
	for i, set := range points {
		v, _ := set.Value(attribute.Key(key))
		out[v.String()] += values[i]
	}
	return out
}

func written(t *testing.T, tel *humustest.Telemetry) []string {
	t.Helper()
	var out []string
	kvs := func(attrs []attribute.KeyValue) {
		for _, kv := range attrs {
			out = append(out, string(kv.Key), kv.Value.Emit())
		}
	}
	for _, s := range tel.Spans() {
		out = append(out, s.Name(), s.Status().Description)
		kvs(s.Attributes())
		for _, ev := range s.Events() {
			out = append(out, ev.Name)
			kvs(ev.Attributes)
		}
		for _, l := range s.Links() {
			kvs(l.Attributes)
		}
	}
	for _, r := range tel.Logs() {
		out = append(out, r.EventName(), r.Body().String())
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			out = append(out, string(kv.Key), kv.Value.String())
			return true
		})
	}
	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out = append(out, m.Name, m.Description)
			points, _ := dataPoints(m)
			for _, set := range points {
				kvs(set.ToSlice())
			}
		}
	}
	return out
}
