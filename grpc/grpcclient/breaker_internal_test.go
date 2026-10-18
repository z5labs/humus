// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
)

// ctxLogger is a log.Logger that keeps the context of each record it is
// given.
type ctxLogger struct {
	embedded.Logger
	ctxs []context.Context
}

func (l *ctxLogger) Emit(ctx context.Context, _ log.Record) { l.ctxs = append(l.ctxs, ctx) }

func (*ctxLogger) Enabled(context.Context, log.EnabledParameters) bool { return true }

func TestObserve_emits_without_the_calls_cancellation(t *testing.T) {
	t.Parallel()

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	call, cancel := context.WithCancel(trace.ContextWithSpanContext(t.Context(), sc))
	cancel() // a call whose deadline passed, or whose caller gave up

	m, err := newInstruments(noop.NewMeterProvider().Meter(scopeName), "backend", 9090)
	require.NoError(t, err)
	logger := &ctxLogger{}
	c := &Client{metrics: m, logger: logger}
	c.observe(call, circuit.Transition{From: circuit.Closed, To: circuit.Open, Generation: 1})

	require.Len(t, logger.ctxs, 1)
	ctx := logger.ctxs[0]
	require.NoError(t, ctx.Err(), "an exporter handed a done context would drop the record")
	require.Equal(t, sc, trace.SpanContextFromContext(ctx), "the record keeps the call's span")
}
