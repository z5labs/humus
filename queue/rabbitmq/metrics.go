// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"errors"
	"slices"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// The keys of the attributes humus defines for the Publisher's instruments
// and the breaker's log records.
const (
	stateKey         = attribute.Key("humus.circuit.state")
	previousStateKey = attribute.Key("humus.circuit.previous_state")
	outcomeKey       = attribute.Key("humus.circuit.outcome")
	decisionKey      = attribute.Key("humus.retry.decision")
)

// outcomeRejected is the humus.circuit.outcome of an attempt the breaker
// didn't admit.
const outcomeRejected = "rejected"

// states are the states humus.circuit.state observes at each collection.
var states = [...]circuit.State{circuit.Closed, circuit.Open, circuit.HalfOpen}

// instruments are humus's own instruments of one Publisher, and the
// attributes of its broker, Connection.Addr's host and port, which every
// data point and every log record of the breaker carries. The Service's
// connection retries record none of them.
type instruments struct {
	server      []attribute.KeyValue
	state       metric.Int64ObservableUpDownCounter
	transitions metric.Int64Counter
	calls       metric.Int64Counter
	decisions   metric.Int64Counter
}

// newInstruments creates the Publisher's instruments from meter, for the
// broker at host and port. It registers no callback: observeState does,
// once nothing after it can fail.
func newInstruments(meter metric.Meter, host string, port int) (*instruments, error) {
	m := &instruments{server: []attribute.KeyValue{semconv.ServerAddress(host), semconv.ServerPort(port)}}
	var errs [4]error
	m.state, errs[0] = meter.Int64ObservableUpDownCounter("humus.circuit.state",
		metric.WithUnit("{breaker}"),
		metric.WithDescription("Circuit breakers in each state. Only the current state's value is non-zero."),
	)
	m.transitions, errs[1] = meter.Int64Counter("humus.circuit.transitions",
		metric.WithUnit("{transition}"),
		metric.WithDescription("Changes of state of a circuit breaker."),
	)
	m.calls, errs[2] = meter.Int64Counter("humus.circuit.calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("Calls through a circuit breaker, by how each counted."),
	)
	m.decisions, errs[3] = meter.Int64Counter("humus.retry.decisions",
		metric.WithUnit("{decision}"),
		metric.WithDescription("What retry decided after each attempt."),
	)
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}
	return m, nil
}

// with returns the measurement option of the broker's attributes and kvs.
func (m *instruments) with(kvs ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributes(slices.Concat(m.server, kvs)...)
}

// observeState registers the callback of humus.circuit.state, which reads
// b's state once at each collection, and observes 1 for it and 0 for the
// other two. It calls nothing of the end user's and changes nothing. It is
// registered with Meter.RegisterCallback, since the SDK keeps only the
// callback option of an instrument's first creation, and the second
// Publisher of an App creates it again. It is never unregistered, so the
// App's final collection, after the OnStop functions, has the state the App
// stopped in.
func (m *instruments) observeState(meter metric.Meter, b *circuit.Breaker) error {
	_, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		current := b.State()
		for _, s := range states {
			var n int64
			if s == current {
				n = 1
			}
			o.ObserveInt64(m.state, n, m.with(stateKey.String(s.String())))
		}
		return nil
	}, m.state)
	return err
}

// transition counts t in humus.circuit.transitions.
func (m *instruments) transition(ctx context.Context, t circuit.Transition) {
	m.transitions.Add(ctx, 1, m.with(
		stateKey.String(t.To.String()),
		previousStateKey.String(t.From.String()),
	))
}

// countCalls returns classify, wrapped to count the Outcome it returns in
// humus.circuit.calls. The breaker calls its classifier from Ticket.Done,
// so an attempt counts once it has its result, and a ticket settled by
// Release counts nothing.
func (m *instruments) countCalls(classify func(context.Context, error) circuit.Outcome) func(context.Context, error) circuit.Outcome {
	return func(ctx context.Context, err error) circuit.Outcome {
		o := classify(ctx, err)
		m.calls.Add(ctx, 1, m.with(outcomeKey.String(o.String())))
		return o
	}
}

// rejected counts an attempt the breaker didn't admit in
// humus.circuit.calls.
func (m *instruments) rejected(ctx context.Context) {
	m.calls.Add(ctx, 1, m.with(outcomeKey.String(outcomeRejected)))
}

// done calls a.Done with err, counts the Decision it returns in
// humus.retry.decisions, and returns it.
func (m *instruments) done(ctx context.Context, a *retry.Attempt, err error) retry.Decision {
	d := a.Done(err)
	m.decisions.Add(ctx, 1, m.with(decisionKey.String(d.String())))
	return d
}
