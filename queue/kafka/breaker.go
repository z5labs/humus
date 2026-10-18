// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
)

// classify is the breaker's classifier. It sees the result of one call to
// Produce, err, the join of each failed record's error, with the call's own
// context, which a caller giving up doesn't cancel, so it never has to tell
// a caller giving up from the cluster failing.
//
// A call is a Success when every record was acknowledged. It is Ignored
// when every record that failed failed for its own reason or its topic's,
// not the cluster's, so one topic's mistake doesn't open the breaker for
// the others. Anything else is the cluster not storing records in time, a
// Failure.
func classify(_ context.Context, err error) circuit.Outcome {
	switch {
	case err == nil:
		return circuit.Success
	case recordsOwn(err):
		return circuit.Ignored
	default:
		return circuit.Failure
	}
}

// recordsOwn reports whether err, or every error err joins, is a record's
// or its topic's own: a Kafka error code franz-go doesn't retry, such as
// MESSAGE_TOO_LARGE, INVALID_RECORD or TOPIC_AUTHORIZATION_FAILED, or
// UNKNOWN_TOPIC_OR_PARTITION or UNKNOWN_TOPIC_ID, which reach a record only
// once franz-go has given up on its topic.
func recordsOwn(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		errs := joined.Unwrap()
		for _, e := range errs {
			if !recordsOwn(e) {
				return false
			}
		}
		return len(errs) > 0
	}
	var ke *kerr.Error
	if !errors.As(err, &ke) {
		return false
	}
	return !ke.Retriable || ke == kerr.UnknownTopicOrPartition || ke == kerr.UnknownTopicID
}

// observe counts t, a transition of the breaker, in
// humus.circuit.transitions, and emits one log record for it, in the
// context of the call that made it: circuit breaker open at WARN, and
// circuit breaker half-open and circuit breaker closed at INFO. Its
// attributes are the cluster's, messaging.system and the first broker's
// server.address and server.port, the state the breaker left and the
// counts of that state.
func (p *Producer) observe(ctx context.Context, t circuit.Transition) {
	p.metrics.transition(ctx, t)

	severity := log.SeverityInfo
	if t.To == circuit.Open {
		severity = log.SeverityWarn
	}
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(severity)
	r.SetSeverityText(severity.String())
	r.SetBody(attribute.StringValue("circuit breaker " + t.To.String()))
	r.AddAttributes(p.metrics.cluster...)
	r.AddAttributes(
		previousStateKey.String(t.From.String()),
		attribute.Int("humus.circuit.successes", t.Counts.Successes),
		attribute.Int("humus.circuit.failures", t.Counts.Failures),
		attribute.Int("humus.circuit.consecutive_successes", t.Counts.ConsecutiveSuccesses),
		attribute.Int("humus.circuit.consecutive_failures", t.Counts.ConsecutiveFailures),
		attribute.Int("humus.circuit.timed_out", t.Counts.TimedOut),
	)
	p.logger.Emit(ctx, r)
}
