// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/orderstatus/projector"
	"github.com/z5labs/humus/humustest"
)

type observed struct {
	tel     *humustest.Telemetry
	metrics metricdata.ResourceMetrics
	orders  []string
}

func observeThreeRecords(t *testing.T) observed {
	t.Helper()
	p := newProcessor(t, nil)
	c := p.cluster(t)
	projected, refused, malformed := "order-"+rand.Text(), "order-"+rand.Text(), "order-"+rand.Text()
	now := time.Now().UTC()
	p.produce(t, c,
		event(projected, projector.Placed, now),
		event(refused, projector.Shipped, now),
		&kgo.Record{Topic: eventsTopic, Key: []byte(malformed), Value: []byte("placed")},
	)

	tel := humustest.NewTelemetry()
	signal, status := p.start(t, map[string]string{"RETRY_ATTEMPTS": "2"}, humus.WithTelemetry(tel.Builder()))
	waitFor(t, "every record is settled", func() bool {
		return committed(c, eventsTopic) == 3 && committed(c, retryTopic) == 1
	})
	signal(syscall.SIGTERM)
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())

	require.Equal(t, projector.Placed, p.store.status(projected))
	require.Len(t, p.readAll(t, c, statusTopic), 1)
	require.Len(t, p.readAll(t, c, retryTopic), 1)
	require.Len(t, p.readAll(t, c, deadLetterTopic), 2)

	metrics, err := tel.Metrics(context.Background())
	require.NoError(t, err)
	return observed{tel: tel, metrics: metrics, orders: []string{projected, refused, malformed}}
}

func processSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, topic string, offset int64) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() != "process "+topic {
			continue
		}
		for _, kv := range s.Attributes() {
			if kv.Key == "messaging.kafka.offset" && kv.Value.AsInt64() == offset {
				found = append(found, s)
			}
		}
	}
	require.Len(t, found, 1, "process spans of %s, offset %d", topic, offset)
	return found[0]
}

func childNames(spans []sdktrace.ReadOnlySpan, parent sdktrace.ReadOnlySpan) []string {
	var names []string
	for _, s := range spans {
		if s.Parent().SpanID() == parent.SpanContext().SpanID() && s.SpanContext().TraceID() == parent.SpanContext().TraceID() {
			names = append(names, s.Name())
		}
	}
	return names
}

func childNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, parent sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name && s.Parent().SpanID() == parent.SpanContext().SpanID() {
			return s
		}
	}
	t.Fatalf("%s has no child named %s", parent.Name(), name)
	return nil
}

func TestInit_a_records_process_span_is_the_parent_of_every_record_it_produces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := observeThreeRecords(t)
		spans := o.tel.Spans()

		projected := processSpan(t, spans, eventsTopic, 0)
		require.Equal(t, []string{"send " + statusTopic}, childNames(spans, projected))

		refused := processSpan(t, spans, eventsTopic, 1)
		require.Equal(t, []string{"send " + retryTopic}, childNames(spans, refused))
		retry := childNamed(t, spans, refused, "send "+retryTopic)
		retried := processSpan(t, spans, retryTopic, 0)
		require.Equal(t, retry.SpanContext().SpanID(), retried.Parent().SpanID())
		require.Equal(t, refused.SpanContext().TraceID(), retried.SpanContext().TraceID())
		require.Equal(t, []string{"send " + deadLetterTopic}, childNames(spans, retried))

		malformed := processSpan(t, spans, eventsTopic, 2)
		require.Equal(t, []string{"send " + deadLetterTopic}, childNames(spans, malformed))
	})
}

func counted(t *testing.T, rm metricdata.ResourceMetrics, name string) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s is a %T", name, m.Data)
			for _, dp := range sum.DataPoints {
				reason, ok := dp.Attributes.Value("orderstatus.reason")
				require.True(t, ok, "a point of %s has no orderstatus.reason", name)
				got[reason.AsString()] += dp.Value
			}
		}
	}
	return got
}

func TestInit_counts_each_retry_and_dead_letter_by_reason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := observeThreeRecords(t)

		require.Equal(t, map[string]int64{"invalid-transition": 1}, counted(t, o.metrics, "orderstatus.retries"))
		require.Equal(t, map[string]int64{"invalid-transition": 1, "malformed": 1}, counted(t, o.metrics, "orderstatus.dead_letters"))
	})
}

func scopeMetric(rm metricdata.ResourceMetrics, scope, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func TestInit_records_its_Producers_breaker_instruments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := observeThreeRecords(t)
		const scope = "github.com/z5labs/humus/queue/kafka"

		state, ok := scopeMetric(o.metrics, scope, "humus.circuit.state")
		require.True(t, ok, "no humus.circuit.state")
		states := map[string]int64{}
		for _, dp := range state.Data.(metricdata.Sum[int64]).DataPoints {
			v, ok := dp.Attributes.Value("humus.circuit.state")
			require.True(t, ok)
			states[v.AsString()] += dp.Value
		}
		require.Positive(t, states["closed"])
		require.Zero(t, states["open"])
		require.Zero(t, states["half-open"])

		calls, ok := scopeMetric(o.metrics, scope, "humus.circuit.calls")
		require.True(t, ok, "no humus.circuit.calls")
		outcomes := map[string]int64{}
		for _, dp := range calls.Data.(metricdata.Sum[int64]).DataPoints {
			v, ok := dp.Attributes.Value("humus.circuit.outcome")
			require.True(t, ok)
			outcomes[v.AsString()] += dp.Value
		}
		require.Equal(t, map[string]int64{"success": 4}, outcomes)
	})
}

func metricAttributes(m metricdata.Metrics) []attribute.Set {
	var sets []attribute.Set
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			sets = append(sets, dp.Attributes)
		}
	}
	return sets
}

func TestInit_records_no_orders_ID_on_a_span_a_log_record_or_a_metric(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := observeThreeRecords(t)

		var written []string
		attrs := func(kvs []attribute.KeyValue) {
			for _, kv := range kvs {
				written = append(written, string(kv.Key), kv.Value.String())
			}
		}
		spans := o.tel.Spans()
		require.NotEmpty(t, spans)
		for _, s := range spans {
			written = append(written, s.Name(), s.Status().Description)
			attrs(s.Attributes())
			for _, ev := range s.Events() {
				written = append(written, ev.Name)
				attrs(ev.Attributes)
			}
			for _, l := range s.Links() {
				attrs(l.Attributes)
			}
		}
		logs := o.tel.Logs()
		require.NotEmpty(t, logs)
		for _, r := range logs {
			written = append(written, r.Body().String())
			r.WalkAttributes(func(kv attribute.KeyValue) bool {
				written = append(written, string(kv.Key), kv.Value.String())
				return true
			})
		}
		require.NotEmpty(t, o.metrics.ScopeMetrics)
		for _, sm := range o.metrics.ScopeMetrics {
			for _, m := range sm.Metrics {
				written = append(written, m.Name)
				for _, set := range metricAttributes(m) {
					attrs(set.ToSlice())
				}
			}
		}
		for _, w := range written {
			for _, id := range o.orders {
				require.NotContains(t, w, id)
			}
		}
	})
}

func TestInit_with_a_bad_telemetry_setting_exits_78(t *testing.T) {
	for name, value := range map[string]string{
		"OTEL_SDK_DISABLED":           "maybe",
		"OTEL_SERVICE_NAME":           "",
		"OTEL_TRACES_EXPORTER":        "zipkin",
		"OTEL_METRICS_EXPORTER":       "prometheus",
		"OTEL_LOGS_EXPORTER":          "file",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "collector",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newProcessor(t, nil)
				_, status := p.start(t, map[string]string{name: value}, humus.WithTelemetry(BuildTelemetry))
				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), "config "+name+":")
			})
		})
	}
}

func TestBuildTelemetry_names_the_service_orderstatus(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"by default":           {env: map[string]string{}, want: "orderstatus"},
		"unless it is renamed": {env: map[string]string{"OTEL_SERVICE_NAME": "orderstatus-canary"}, want: "orderstatus-canary"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_LOGS_EXPORTER": "none"}
			for k, v := range tc.env {
				env[k] = v
			}
			var stdout bytes.Buffer
			sdk, err := BuildTelemetry(t.Context(), bedrock.Process{Environment: bedrock.NewStaticEnvironment(env), Stdout: &stdout}, nil)
			require.NoError(t, err)

			_, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "probe")
			span.End()
			require.NoError(t, sdk.Shutdown(context.Background()))

			require.Contains(t, stdout.String(), `"Key":"service.name","Value":{"Type":"STRING","Value":"`+tc.want+`"}`)
		})
	}
}
