// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"errors"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// classifyBreaker is the breaker's classifier. It sees the result of one
// attempt of a Publish, with the Publish's context, which is the caller's
// with Timeout. What each result means is the post's circuit column:
//
//   - confirmed: Success;
//   - returned unroutable, ErrUnroutable, or a channel error, such as 404
//     NOT_FOUND, 403 ACCESS_REFUSED or 406 PRECONDITION_FAILED: Success,
//     since the broker answered;
//   - nacked by the broker, or lost: a dropped connection, a channel closed
//     without the broker's error, a failed dial or a failed handshake:
//     Failure;
//   - an attempt that ended with ctx: Failure when ctx's deadline passed,
//     whether Timeout's or the caller's, and Ignored when the caller
//     cancelled it, which says nothing about the broker;
//   - the Publisher stopped: Ignored, for the same reason;
//   - anything else: Failure.
func classifyBreaker(ctx context.Context, err error) circuit.Outcome {
	switch {
	case err == nil:
		return circuit.Success
	case errors.Is(err, ErrUnroutable):
		return circuit.Success
	case errors.Is(err, errNacked):
		return circuit.Failure
	case errors.Is(err, errStopped):
		return circuit.Ignored
	}
	if _, ok := errors.AsType[*lostError](err); ok {
		return circuit.Failure
	}
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return circuit.Ignored
	case ctx.Err() != nil:
		return circuit.Failure
	}
	if _, ok := errors.AsType[*amqp.Error](err); ok {
		// Only an error the broker closed a channel with, on a connection
		// still open, reaches here unmarked: closedError marks the rest
		// lost.
		return circuit.Success
	}
	return circuit.Failure
}

// observe counts t, a transition of the breaker, in
// humus.circuit.transitions, and emits one log record for it, in the
// context of the publish that made it: circuit breaker open at WARN, and
// circuit breaker half-open and circuit breaker closed at INFO. Its
// attributes are server.address and server.port, from Connection.Addr, the
// state the breaker left and the counts of that state.
func (p *Publisher) observe(ctx context.Context, t circuit.Transition) {
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
	r.AddAttributes(
		semconv.ServerAddress(p.serverAddr),
		semconv.ServerPort(p.serverPort),
		previousStateKey.String(t.From.String()),
		attribute.Int("humus.circuit.successes", t.Counts.Successes),
		attribute.Int("humus.circuit.failures", t.Counts.Failures),
		attribute.Int("humus.circuit.consecutive_successes", t.Counts.ConsecutiveSuccesses),
		attribute.Int("humus.circuit.consecutive_failures", t.Counts.ConsecutiveFailures),
		attribute.Int("humus.circuit.timed_out", t.Counts.TimedOut),
	)
	p.logger.Emit(ctx, r)
}
