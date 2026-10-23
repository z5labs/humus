// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package worker_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus/example/rabbitmq/mailer/mail"
	"github.com/z5labs/humus/example/rabbitmq/mailer/provider"
	"github.com/z5labs/humus/queue/rabbitmq"
)

const workerScope = "github.com/z5labs/humus/example/rabbitmq/mailer/worker"

func (h *harness) process(t *testing.T, m rabbitmq.Message) (sdktrace.ReadOnlySpan, error) {
	t.Helper()
	ctx, span := h.sdk.TracerProvider.Tracer("test").Start(t.Context(), "process "+mail.SendQueue)
	err := h.worker.Process(ctx, m)
	span.End()
	spans := h.tel.Spans()
	require.Len(t, spans, 1)
	return spans[0], err
}

func (h *harness) requests(t *testing.T) map[string]int64 {
	t.Helper()
	rm, err := h.tel.Metrics(t.Context())
	require.NoError(t, err)
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != workerScope {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "mail.requests" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "mail.requests is a %T", m.Data)
			require.True(t, sum.IsMonotonic)
			for _, dp := range sum.DataPoints {
				require.Equal(t, 1, dp.Attributes.Len(), "mail.requests has attributes besides mail.outcome")
				outcome, ok := dp.Attributes.Value("mail.outcome")
				require.True(t, ok)
				counts[outcome.AsString()] += dp.Value
			}
		}
	}
	return counts
}

func (h *harness) records() []sdklog.Record {
	var out []sdklog.Record
	for _, r := range h.tel.Logs() {
		if r.InstrumentationScope().Name == workerScope {
			out = append(out, r)
		}
	}
	return out
}

func attrsOf(r sdklog.Record) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value
		return true
	})
	return out
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestProcess_observes_each_request_by_its_outcome(t *testing.T) {
	const personal = `{"to":"ada@example.com","subject":"Your order","text":"It shipped."}`
	unreachable := errors.New("provider: it answered 503 Service Unavailable")
	cases := map[string]struct {
		maxAttempts int
		result      provider.Result
		err         error
		message     rabbitmq.Message
		outcome     string
		attempt     int64
		severity    log.Severity
	}{
		"a request sent": {
			maxAttempts: 5,
			result:      provider.Result{ID: "msg_1"},
			message:     delivery(personal, nil),
			outcome:     "sent",
			attempt:     1,
			severity:    log.SeverityInfo,
		},
		"a request refused": {
			maxAttempts: 5,
			result:      provider.Result{Refused: true, Reason: "the address does not exist"},
			message:     delivery(personal, map[string]any{mail.AttemptHeader: int64(2)}),
			outcome:     "refused",
			attempt:     2,
			severity:    log.SeverityInfo,
		},
		"a request retried": {
			maxAttempts: 5,
			err:         unreachable,
			message:     delivery(personal, nil),
			outcome:     "retried",
			attempt:     1,
			severity:    log.SeverityInfo,
		},
		"a request that gave up": {
			maxAttempts: 3,
			err:         unreachable,
			message:     delivery(personal, map[string]any{mail.AttemptHeader: int64(3)}),
			outcome:     "gave_up",
			attempt:     3,
			severity:    log.SeverityWarn,
		},
		"a malformed request": {
			maxAttempts: 5,
			message:     delivery(`{"to":"ada@example.com","text":"It shipped."}`, map[string]any{mail.AttemptHeader: int64(2)}),
			outcome:     "malformed",
			attempt:     2,
			severity:    log.SeverityWarn,
		},
		"a request whose attempt is malformed": {
			maxAttempts: 5,
			message:     delivery(personal, map[string]any{mail.AttemptHeader: "two"}),
			outcome:     "malformed",
			attempt:     1,
			severity:    log.SeverityWarn,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, tc.maxAttempts)
				h.provider.result = tc.result
				h.provider.err = tc.err

				span, err := h.process(t, tc.message)

				require.NoError(t, err)
				attempt, ok := spanAttr(span, "mail.attempt")
				require.True(t, ok, "the span has no mail.attempt")
				require.Equal(t, tc.attempt, attempt.AsInt64())
				outcome, ok := spanAttr(span, "mail.outcome")
				require.True(t, ok, "the span has no mail.outcome")
				require.Equal(t, tc.outcome, outcome.AsString())

				require.Equal(t, map[string]int64{tc.outcome: 1}, h.requests(t))

				records := h.records()
				require.Len(t, records, 1)
				r := records[0]
				require.Equal(t, tc.severity, r.Severity())
				require.Equal(t, "mail request "+tc.outcome, r.Body().AsString())
				require.Equal(t, map[string]attribute.Value{
					"messaging.message.id": attribute.StringValue(tc.message.MessageID),
					"mail.attempt":         attribute.Int64Value(tc.attempt),
					"mail.outcome":         attribute.StringValue(tc.outcome),
				}, attrsOf(r))
				require.Equal(t, span.SpanContext().TraceID(), r.TraceID())
			})
		})
	}
}

func TestProcess_logs_no_MessageId_for_a_request_without_one(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 5)
		m := delivery(validBody, nil)
		m.MessageID = ""

		_, err := h.process(t, m)

		require.NoError(t, err)
		records := h.records()
		require.Len(t, records, 1)
		require.Equal(t, map[string]attribute.Value{
			"mail.attempt": attribute.Int64Value(1),
			"mail.outcome": attribute.StringValue("malformed"),
		}, attrsOf(records[0]))
	})
}

func TestProcess_observes_no_outcome_when_its_publish_fails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 5)
		h.provider.err = errors.New("provider: it answered 503 Service Unavailable")
		h.retries.fail = failing(mail.RetryExchange, errors.New("rabbitmq: the broker nacked the publish"))

		span, err := h.process(t, delivery(validBody, map[string]any{mail.AttemptHeader: int64(2)}))

		require.Error(t, err)
		attempt, ok := spanAttr(span, "mail.attempt")
		require.True(t, ok, "the span has no mail.attempt")
		require.Equal(t, int64(2), attempt.AsInt64())
		_, ok = spanAttr(span, "mail.outcome")
		require.False(t, ok, "the span has a mail.outcome")
		require.Empty(t, h.requests(t))
		require.Empty(t, h.records())
	})
}
