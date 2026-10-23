//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/rabbitmq/mailer/app"
	"github.com/z5labs/humus/humustest"
)

const (
	httpclientScope = "github.com/z5labs/humus/httpclient"
	rabbitmqScope   = "github.com/z5labs/humus/queue/rabbitmq"
	workerScope     = "github.com/z5labs/humus/example/rabbitmq/mailer/worker"
)

type tracedRequester struct {
	ch     *amqp.Channel
	user   string
	tracer trace.Tracer
}

func newTracedRequester(t *testing.T, broker *rabbitMQ, l login) *tracedRequester {
	t.Helper()
	ch, err := connect(t, broker, l).Channel()
	require.NoError(t, err)
	require.NoError(t, ch.Confirm(false))
	return &tracedRequester{ch: ch, user: l.username, tracer: sdktrace.NewTracerProvider().Tracer("publisher")}
}

type published struct {
	messageID string
	span      trace.SpanContext
}

func (r *tracedRequester) publish(t *testing.T, body map[string]string, rules ...fit.Rule) published {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	ctx, span := r.tracer.Start(t.Context(), "send mail", trace.WithSpanKind(trace.SpanKindProducer))
	defer span.End()

	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	headers := amqp.Table{}
	for k, v := range carrier {
		headers[k] = v
	}
	if len(rules) > 0 {
		headers["baggage"] = scenario(t, rules...)
	}
	messageID := rand.Text()
	confirm, err := r.ch.PublishWithDeferredConfirmWithContext(ctx, "mail", "mail.send", true, false, amqp.Publishing{
		Headers:       headers,
		ContentType:   "application/json",
		DeliveryMode:  amqp.Persistent,
		MessageId:     messageID,
		CorrelationId: rand.Text(),
		UserId:        r.user,
		Body:          b,
	})
	require.NoError(t, err)
	acked, err := confirm.WaitContext(t.Context())
	require.NoError(t, err)
	require.True(t, acked, "the broker nacked the request %s", messageID)
	return published{messageID: messageID, span: span.SpanContext()}
}

type personal struct {
	values []string
}

func (p *personal) request(to string) map[string]string {
	subject := "Your order " + rand.Text() + " has shipped"
	text := "It arrives on Tuesday, " + rand.Text() + "."
	p.values = append(p.values, to, subject, text)
	return map[string]string{"to": to, "subject": subject, "text": text}
}

func processSpans(spans []sdktrace.ReadOnlySpan, messageID string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() != "process mail.send" {
			continue
		}
		if v, ok := attr(s.Attributes(), "messaging.message.id"); ok && v.AsString() == messageID {
			out = append(out, s)
		}
	}
	return out
}

func childrenOf(spans []sdktrace.ReadOnlySpan, parent sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name && s.Parent().SpanID() == parent.SpanContext().SpanID() {
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
			out = append(out, string(kv.Key), kv.Value.String())
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

func TestInit_traces_logs_and_measures_each_request_without_its_address_or_body(t *testing.T) {
	broker := startRabbitMQ(t)
	provider := startWireMock(t)
	mailer := broker.addUser(t, `^$`, `^mail\.(retry|dead|events)$`, `^mail\.send$`)
	cased := broker.addUser(t, `^amq\.gen-.*$`, `^(mail|amq\.gen-.*)$`, `^(mail\.(events|dead)|amq\.gen-.*)$`)
	watcher := broker.addUser(t, `^amq\.gen-.*$`, `^amq\.gen-.*$`, `^(mail\.(events|dead|retry)|amq\.gen-.*)$`)

	m := newFaultsMailer(t, broker, provider, mailer, cased.username)
	m.env["MAIL_MAX_ATTEMPTS"] = "2"
	tel := humustest.NewTelemetry()
	p, status := humustest.Start(t, app.Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(tel.Builder()), humus.WithFaults(app.Faults)),
		humustest.WithEnv(m.env),
	)
	m.awaitReady(t)

	o := observe(t, broker, watcher, map[string]string{"mail.retry": "mail.send", "mail.dead": "", "mail.events": "#"})
	requester := newTracedRequester(t, broker, cased)
	pii := &personal{}

	sent := requester.publish(t, pii.request(address()))
	refused := requester.publish(t, pii.request(strings.ToLower(rand.Text())+"@example.invalid"))
	malformedBody := pii.request(address())
	delete(malformedBody, "subject")
	malformed := requester.publish(t, malformedBody)
	o.await(t, "the sent, refused and malformed requests' outcomes", func() bool {
		return len(o.of(sent.messageID, "mail.events")) > 0 &&
			len(o.of(refused.messageID, "mail.events")) > 0 &&
			len(o.of(malformed.messageID, "mail.dead")) > 0
	})

	gaveUp := requester.publish(t, pii.request(address()), abort("http:"+provider.Host))
	o.await(t, "the request that gave up to be dead-lettered", func() bool {
		return len(o.of(gaveUp.messageID, "mail.dead")) > 0
	})

	stop(t, p, status)

	spans := tel.Spans()
	t.Run("a request's process span is a child of its publisher's send span, with its provider attempts and its result under it", func(t *testing.T) {
		process := processSpans(spans, sent.messageID)
		require.Len(t, process, 1)
		ps := process[0]
		require.Equal(t, trace.SpanKindConsumer, ps.SpanKind())
		require.Equal(t, sent.span.TraceID(), ps.SpanContext().TraceID())
		require.Equal(t, sent.span.SpanID(), ps.Parent().SpanID())
		require.True(t, ps.Parent().IsRemote())

		require.NotEmpty(t, childrenOf(spans, ps, http.MethodPost), "no provider attempt under the process span")
		require.Len(t, childrenOf(spans, ps, "send mail.events"), 1)
	})

	t.Run("the process span carries mail.attempt and mail.outcome", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			messageID string
			attempt   int64
			outcome   string
			result    string
		}{
			{"sent", sent.messageID, 1, "sent", "send mail.events"},
			{"refused", refused.messageID, 1, "refused", "send mail.events"},
			{"malformed", malformed.messageID, 1, "malformed", "send mail.dead"},
			{"retried", gaveUp.messageID, 1, "retried", "send mail.retry"},
			{"gave up", gaveUp.messageID, 2, "gave_up", "send mail.dead"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var found []sdktrace.ReadOnlySpan
				for _, s := range processSpans(spans, tc.messageID) {
					if v, ok := attr(s.Attributes(), "mail.attempt"); ok && v.AsInt64() == tc.attempt {
						found = append(found, s)
					}
				}
				require.Len(t, found, 1)
				outcome, ok := attr(found[0].Attributes(), "mail.outcome")
				require.True(t, ok, "the span has no mail.outcome")
				require.Equal(t, tc.outcome, outcome.AsString())
				require.Len(t, childrenOf(spans, found[0], tc.result), 1)
			})
		}
	})

	t.Run("the parked request's next processing is a child of the publish that parked it", func(t *testing.T) {
		var first, second sdktrace.ReadOnlySpan
		for _, s := range processSpans(spans, gaveUp.messageID) {
			v, _ := attr(s.Attributes(), "mail.attempt")
			switch v.AsInt64() {
			case 1:
				first = s
			case 2:
				second = s
			}
		}
		require.NotNil(t, first)
		require.NotNil(t, second)
		park := childrenOf(spans, first, "send mail.retry")
		require.Len(t, park, 1)
		require.Equal(t, park[0].SpanContext().SpanID(), second.Parent().SpanID())
		require.Equal(t, gaveUp.span.TraceID(), second.SpanContext().TraceID())
	})

	rm, err := tel.Metrics(t.Context())
	require.NoError(t, err)

	t.Run("mail.requests counts each outcome", func(t *testing.T) {
		requests, ok := metricsOf(rm, workerScope)["mail.requests"]
		require.True(t, ok, "mail.requests was not recorded")
		require.Equal(t, map[string]float64{
			"sent":      1,
			"refused":   1,
			"malformed": 1,
			"retried":   1,
			"gave_up":   1,
		}, sumBy(requests, "mail.outcome"))
	})

	t.Run("the worker logs each outcome with the MessageId, the outcome and the attempt only", func(t *testing.T) {
		var n int
		for _, r := range tel.Logs() {
			if r.InstrumentationScope().Name != workerScope {
				continue
			}
			n++
			var keys []string
			r.WalkAttributes(func(kv attribute.KeyValue) bool {
				keys = append(keys, string(kv.Key))
				return true
			})
			require.ElementsMatch(t, []string{"messaging.message.id", "mail.attempt", "mail.outcome"}, keys)
		}
		require.Equal(t, 5, n)
	})

	t.Run("no span, log record or metric carries a request's address, subject or text", func(t *testing.T) {
		for _, w := range written(t, tel) {
			for _, v := range pii.values {
				require.NotContains(t, w, v)
			}
		}
	})

	t.Run("the provider client's breaker and retries are measured", func(t *testing.T) {
		ms := metricsOf(rm, httpclientScope)
		for _, name := range []string{"humus.circuit.state", "humus.circuit.calls", "humus.retry.decisions"} {
			m, ok := ms[name]
			require.True(t, ok, "%s was not recorded for the provider client", name)
			points, _ := dataPoints(m)
			require.NotEmpty(t, points, name)
			for _, set := range points {
				host, _ := set.Value("server.address")
				require.Equal(t, provider.Hostname(), host.AsString(), name)
			}
		}
		require.Positive(t, sumBy(ms["humus.circuit.calls"], "humus.circuit.outcome")["success"])
		require.Positive(t, sumBy(ms["humus.retry.decisions"], "humus.retry.decision")["succeeded"])
	})

	t.Run("every Publisher's breaker and retries are measured", func(t *testing.T) {
		ms := metricsOf(rm, rabbitmqScope)
		state, ok := ms["humus.circuit.state"]
		require.True(t, ok, "humus.circuit.state was not recorded for the Publishers")
		var breakers float64
		for _, v := range sumBy(state, "humus.circuit.state") {
			breakers += v
		}
		require.Equal(t, float64(3), breakers, "humus.circuit.state doesn't add up to three Publishers' breakers")

		calls, ok := ms["humus.circuit.calls"]
		require.True(t, ok, "humus.circuit.calls was not recorded for the Publishers")
		require.GreaterOrEqual(t, sumBy(calls, "humus.circuit.outcome")["success"], float64(5))
		decisions, ok := ms["humus.retry.decisions"]
		require.True(t, ok, "humus.retry.decisions was not recorded for the Publishers")
		require.GreaterOrEqual(t, sumBy(decisions, "humus.retry.decision")["succeeded"], float64(5))
	})
}
