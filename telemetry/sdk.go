// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry

import (
	"context"
	"net"

	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// SDK is the set of OpenTelemetry providers an App hands its Services. It
// replaces OpenTelemetry's globals: every humus package takes one, and
// humus never reads or sets the globals.
//
// SDK is a plain struct, so a Builder that needs something Settings does
// not cover, such as a metric view or another reader, can build its own
// value, Shutdown included.
type SDK struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	LoggerProvider log.LoggerProvider
	Propagator     propagation.TextMapPropagator

	// Shutdown flushes and stops the providers. It is called once, after
	// the App has stopped; Init and the Services never call it.
	Shutdown func(ctx context.Context) error
}

// Noop returns an SDK whose providers discard everything and whose Shutdown
// does nothing and returns nil. Its propagator is still W3C trace context
// plus baggage, so a request's trace passes through a service whose
// telemetry is off. Unlike traces at ExporterNone, a no-op TracerProvider
// makes no IDs of its own: it passes on the span context a request brought.
func Noop() SDK {
	return SDK{
		TracerProvider: tracenoop.NewTracerProvider(),
		MeterProvider:  metricnoop.NewMeterProvider(),
		LoggerProvider: lognoop.NewLoggerProvider(),
		Propagator:     newPropagator(),
		Shutdown:       func(context.Context) error { return nil },
	}
}

// newPropagator returns W3C trace context plus baggage.
func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

// Dialer opens connections, like (*net.Dialer).DialContext. It is the Dial
// field of humus.Host, so the OTLP exporters reach the collector through the
// App's Host.
type Dialer func(ctx context.Context, network, address string) (net.Conn, error)

// Builder builds an App's SDK. The end user writes it: it sources Settings
// from p however it likes and calls New with dial and p.Stdout.
type Builder func(ctx context.Context, p bedrock.Process, dial Dialer) (SDK, error)

// NoopBuilder is a Builder that returns Noop() and a nil error, for tests
// that do not assert on telemetry.
func NoopBuilder(context.Context, bedrock.Process, Dialer) (SDK, error) {
	return Noop(), nil
}
