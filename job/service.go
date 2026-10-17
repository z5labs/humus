// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
)

// tracerName is the name of the tracer the run's span is started with.
const tracerName = "github.com/z5labs/humus/job"

// spanName is the name of the run's span.
const spanName = "job"

// errShutdown is the cause Shutdown cancels the run's context with. It wraps
// context.Canceled, so a run that returns context.Cause(ctx) after a bare
// Shutdown returns a cancellation, as one that returns ctx.Err() does.
var errShutdown = fmt.Errorf("job: the Service was shut down: %w", context.Canceled)

// Service is a humus.Service that runs a job once. It is safe for concurrent
// use.
type Service struct {
	run        func(ctx context.Context) error
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	env        bedrock.Environment

	// serving is set once: by the first Serve, to that Serve's call, or by
	// a Shutdown called before any Serve, to notServed.
	serving atomic.Pointer[call]
}

// call is one call of Serve: the function that cancels the run's context,
// and served, which is closed when Serve returns.
type call struct {
	cancel context.CancelCauseFunc
	served chan struct{}
}

// notServed is the call of a Service that was shut down before it was
// served.
var notServed = &call{}

// NewService returns a Service that runs run once, when its Serve is called.
// run's context is done when the App starts shutting down or when Shutdown is
// called, whichever comes first, and carries the span of the run.
//
// The span's parent is the trace context that s.Telemetry.Propagator
// extracts from s.Process.Environment, with each of the propagator's keys
// read as OpenTelemetry's environment-variable carriers name it: traceparent
// as TRACEPARENT. When the environment carries none, or a malformed one, the
// span is the root of a new trace.
//
// NewService panics if run is nil.
func NewService(s *humus.Setup, run func(ctx context.Context) error) *Service {
	if run == nil {
		panic("job: NewService called with a nil run")
	}
	return &Service{
		run:        run,
		tracer:     s.Telemetry.TracerProvider.Tracer(tracerName),
		propagator: s.Telemetry.Propagator,
		env:        s.Process.Environment,
	}
}

// Serve calls run on Serve's own goroutine, inside a span, and returns once
// run has returned:
//
//   - nil when run returned nil;
//   - nil when Shutdown was called before ctx was done and run returned an
//     error for which errors.Is(err, context.Canceled) holds, because
//     Shutdown stopped it;
//   - run's error, unchanged, otherwise. That includes a run that a signal
//     interrupted, which returns ctx's error.
//
// A Service is served once. A Serve called after Shutdown, or after an
// earlier Serve, returns nil at once without calling run.
func (s *Service) Serve(ctx context.Context) error {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	c := &call{cancel: cancel, served: make(chan struct{})}
	if !s.serving.CompareAndSwap(nil, c) {
		return nil
	}
	defer close(c.served)

	// The trace context of the process that started the job is the parent
	// of the run's span, and its baggage is on the run's context. When the
	// environment carries none, or a malformed one, Extract adds nothing to
	// runCtx, and the span is a new root.
	parentCtx := s.propagator.Extract(runCtx, envCarrier{env: s.env})
	spanCtx, span := s.tracer.Start(parentCtx, spanName, trace.WithSpanKind(trace.SpanKindInternal))
	err := s.run(spanCtx)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()

	if err == nil {
		return nil
	}
	// context.Cause reports whichever cancellation came first: Serve's
	// context's, after a signal, or errShutdown, after a bare Shutdown.
	if errors.Is(context.Cause(runCtx), errShutdown) && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Shutdown cancels run's context and waits for Serve to return. It returns
// nil once Serve has returned, or at once when Serve was never called or had
// already returned. If ctx is done first, it returns an error that wraps
// ctx's error, and Serve keeps waiting for run.
func (s *Service) Shutdown(ctx context.Context) error {
	s.serving.CompareAndSwap(nil, notServed)
	c := s.serving.Load()
	if c == notServed {
		return nil
	}
	c.cancel(errShutdown)

	select {
	case <-c.served:
		return nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("job: shutting down the Service: %w", err)
	}
	select {
	case <-c.served:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("job: shutting down the Service: %w", ctx.Err())
	}
}
