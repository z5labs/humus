// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"

	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// The keys of the attributes humus defines for the Producer's instruments
// and the breaker's log records.
const (
	stateKey         = attribute.Key("humus.circuit.state")
	previousStateKey = attribute.Key("humus.circuit.previous_state")
	outcomeKey       = attribute.Key("humus.circuit.outcome")
)

// outcomeRejected is the humus.circuit.outcome of a call the breaker didn't
// admit.
const outcomeRejected = "rejected"

// states are the states humus.circuit.state observes at each collection.
var states = [...]circuit.State{circuit.Closed, circuit.Open, circuit.HalfOpen}

// instruments are humus's own instruments of one Producer's breaker, and
// the attributes of its cluster, which every data point and every log
// record of the breaker carries. There is no humus.retry.decisions:
// franz-go retries inside a produce request, and exposes no decision.
type instruments struct {
	cluster     []attribute.KeyValue
	state       metric.Int64ObservableUpDownCounter
	transitions metric.Int64Counter
	calls       metric.Int64Counter
}

// newInstruments creates the Producer's instruments from meter, for the
// cluster whose first seed broker is broker, a host:port. It registers no
// callback: observeState does, once nothing after it can fail.
func newInstruments(meter metric.Meter, broker string) (*instruments, error) {
	m := &instruments{cluster: clusterAttrs(broker)}
	var errs [3]error
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
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}
	return m, nil
}

// clusterAttrs returns the attributes of the cluster whose first seed
// broker is broker: messaging.system, and broker's host and port as
// server.address and server.port. They name the seed the end user listed
// first, not the broker that answered. CheckBrokers accepts only host:port,
// with a port from 1 to 65535, so broker always has both.
func clusterAttrs(broker string) []attribute.KeyValue {
	host, port, _ := net.SplitHostPort(broker)
	n, _ := strconv.Atoi(port)
	return []attribute.KeyValue{
		semconv.MessagingSystemKafka,
		semconv.ServerAddress(host),
		semconv.ServerPort(n),
	}
}

// with returns the measurement option of the cluster's attributes and kvs.
func (m *instruments) with(kvs ...attribute.KeyValue) metric.MeasurementOption {
	return metric.WithAttributes(slices.Concat(m.cluster, kvs)...)
}

// observeState registers the callback of humus.circuit.state, which reads
// b's state once at each collection, and observes 1 for it and 0 for the
// other two. It calls nothing of the end user's and changes nothing. It is
// registered with Meter.RegisterCallback, since the SDK keeps only the
// callback option of an instrument's first creation, and the second
// Producer of an App creates it again. It is never unregistered, so the
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
// so a call counts once every one of its records has its result, which may
// be after Produce returns, and a ticket settled by Release counts nothing.
func (m *instruments) countCalls(classify func(context.Context, error) circuit.Outcome) func(context.Context, error) circuit.Outcome {
	return func(ctx context.Context, err error) circuit.Outcome {
		o := classify(ctx, err)
		m.calls.Add(ctx, 1, m.with(outcomeKey.String(o.String())))
		return o
	}
}

// rejected counts a call the breaker didn't admit in humus.circuit.calls.
func (m *instruments) rejected(ctx context.Context) {
	m.calls.Add(ctx, 1, m.with(outcomeKey.String(outcomeRejected)))
}
