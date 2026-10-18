// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
)

// verdict is the error the breaker layer passes to Ticket.Done. It carries
// the outcome the layer has already decided, so the breaker's classifier
// only reads it back.
type verdict struct {
	outcome circuit.Outcome
}

func (v verdict) Error() string {
	return "httpclient: an attempt counted as " + v.outcome.String()
}

// countVerdict is the breaker's classifier: the outcome of a verdict. The
// breaker layer settles every ticket with a verdict, so anything else is
// humus's bug.
func countVerdict(_ context.Context, err error) circuit.Outcome {
	var v verdict
	if !errors.As(err, &v) {
		panic(fmt.Sprintf("httpclient: a ticket settled with a %T", err))
	}
	return v.outcome
}

// settle settles t with o.
func settle(t *circuit.Ticket, o circuit.Outcome) {
	t.Done(verdict{outcome: o})
}

// readFailed is how a body whose read failed counts: Ignored when the
// caller gave up, and Failure otherwise, the call's deadline included,
// since the dependency sent its headers and then failed.
func readFailed(call context.Context) circuit.Outcome {
	if errors.Is(call.Err(), context.Canceled) {
		return circuit.Ignored
	}
	return circuit.Failure
}

// newObserver returns the breaker's observer for the dependency m measures.
// It counts each transition in humus.circuit.transitions, and emits one log
// record for it, in the context of the call that made it, without its
// cancellation: the call has often ended by then, as a call whose deadline
// passed has, and an exporter handed a done context would drop the record.
// The record keeps the call's span. Its bodies are circuit breaker open at
// WARN, and circuit breaker half-open and circuit breaker closed at INFO.
// Its attributes are the dependency's server.address and server.port, the
// state the breaker left and the counts of that state.
func newObserver(logger log.Logger, m *instruments) func(context.Context, circuit.Transition) {
	return func(ctx context.Context, t circuit.Transition) {
		ctx = context.WithoutCancel(ctx)
		m.transition(ctx, t)

		severity := log.SeverityInfo
		if t.To == circuit.Open {
			severity = log.SeverityWarn
		}
		var r log.Record
		r.SetTimestamp(time.Now())
		r.SetSeverity(severity)
		r.SetSeverityText(severity.String())
		r.SetBody(attribute.StringValue("circuit breaker " + t.To.String()))
		r.AddAttributes(m.server...)
		r.AddAttributes(
			previousStateKey.String(t.From.String()),
			attribute.Int("humus.circuit.successes", t.Counts.Successes),
			attribute.Int("humus.circuit.failures", t.Counts.Failures),
			attribute.Int("humus.circuit.consecutive_successes", t.Counts.ConsecutiveSuccesses),
			attribute.Int("humus.circuit.consecutive_failures", t.Counts.ConsecutiveFailures),
			attribute.Int("humus.circuit.timed_out", t.Counts.TimedOut),
		)
		logger.Emit(ctx, r)
	}
}

// ticketBody is a response's body that settles its attempt's ticket: with
// outcome once service code reads it to the end or closes it, and as
// readFailed decides once a read fails. A body that is never closed leaves
// its ticket unsettled.
type ticketBody struct {
	io.ReadCloser
	call    context.Context
	ticket  *circuit.Ticket
	outcome circuit.Outcome
	once    sync.Once
}

// newTicketBody returns body, settling t once it is read or closed. A 101
// Switching Protocols response's body, which net/http makes writable,
// stays writable.
func newTicketBody(body io.ReadCloser, call context.Context, t *circuit.Ticket, outcome circuit.Outcome) io.ReadCloser {
	b := &ticketBody{ReadCloser: body, call: call, ticket: t, outcome: outcome}
	if w, ok := body.(io.ReadWriteCloser); ok {
		return &ticketReadWriter{ticketBody: b, w: w}
	}
	return b
}

func (b *ticketBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		b.settle(b.outcome)
	default:
		b.settle(readFailed(b.call))
	}
	return n, err
}

// Close settles the ticket before it closes the body, so a read that fails
// because the body was closed doesn't count as the dependency failing. The
// body is closed even if settling panics, in BreakerPolicy.
func (b *ticketBody) Close() (err error) {
	defer func() { err = b.ReadCloser.Close() }()
	b.settle(b.outcome)
	return nil
}

func (b *ticketBody) settle(o circuit.Outcome) {
	b.once.Do(func() { settle(b.ticket, o) })
}

// ticketReadWriter is a ticketBody for a 101 Switching Protocols response.
type ticketReadWriter struct {
	*ticketBody
	w io.Writer
}

func (b *ticketReadWriter) Write(p []byte) (int, error) {
	return b.w.Write(p)
}
