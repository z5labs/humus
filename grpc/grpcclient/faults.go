// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"context"
	"strings"
	"time"

	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/fault"
)

// stripEvent is the span event of a strip. It is humus's own, so it starts
// with humus.: OpenTelemetry has no convention for failure injection.
const stripEvent = "humus.fit.strip"

// pointOf returns the injection point of method: grpc:<service>/<method>,
// the full method without its leading slash, as rpc.method writes it, such
// as grpc:payments.v1.Payments/Charge.
func pointOf(method string) string {
	return "grpc:" + strings.TrimPrefix(method, "/")
}

// injectedError is the error of an attempt that a fault ended: an abort,
// whose gRPC status is Unavailable, or a Delay its context cut short, whose
// status is DeadlineExceeded or Canceled, as grpc-go would have ended the
// attempt. It matches the *fault.InjectedError it wraps with errors.As, and
// what that matches with errors.Is: fit.ErrInjected for an abort, the
// context's cause for a Delay.
type injectedError struct {
	injected *fault.InjectedError
	code     codes.Code
}

func (e *injectedError) Error() string { return e.injected.Error() }

func (e *injectedError) Unwrap() error { return e.injected }

// GRPCStatus is the status that status.FromError and status.Code read.
func (e *injectedError) GRPCStatus() *status.Status {
	return status.New(e.code, e.injected.Error())
}

// inject is failure injection's seam in an attempt to method, which the
// breaker has admitted and grpc-go is about to be given. It evaluates the
// attempt's point with the Setup's Injector, in ctx, so the evaluation is
// an event on the caller's span, and waits the Fault's Delay, inside
// Timeout. An abort, or a Delay ctx cuts short, ends the attempt: inject
// returns its *injectedError, after a span of the Client's own for it, and
// grpc-go is never called. Otherwise, unless ForwardScenario is set, it
// strips the scenario from ctx, with a humus.fit.strip event on the
// caller's span, and returns the context to call grpc-go with, from which
// otelgrpc injects every other baggage member. With failure injection off,
// it returns ctx itself.
func (c *Client) inject(ctx context.Context, method string) (context.Context, error) {
	start := time.Now()
	point := pointOf(method)
	f := c.faults.Eval(ctx, point)
	if err := f.Wait(ctx); err != nil {
		code := status.FromContextError(ctx.Err()).Code()
		return nil, c.injected(ctx, method, start, &fault.InjectedError{Point: point, Err: err}, code)
	}
	if f.Abort {
		return nil, c.injected(ctx, method, start, &fault.InjectedError{Point: point}, codes.Unavailable)
	}
	if c.forward {
		return ctx, nil
	}
	stripped, carried := c.faults.Strip(ctx)
	if carried {
		trace.SpanFromContext(ctx).AddEvent(stripEvent)
	}
	return stripped, nil
}

// injected is the error of an attempt to method, started at start, that
// the fault ie names ended with code. The attempt never reached grpc-go,
// so otelgrpc made no span for it: the Client records one of its own, as
// for a rejected call, from start to now, with ie's error.type.
func (c *Client) injected(ctx context.Context, method string, start time.Time, ie *fault.InjectedError, code codes.Code) error {
	err := &injectedError{injected: ie, code: code}
	c.ownSpan(ctx, method, start, code, err, semconv.ErrorTypeKey.String(ie.ErrorType()))
	return err
}
