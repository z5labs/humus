// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scopeName is the instrumentation scope of the Client's own telemetry: the
// span of a rejected call, humus's own instruments, and the breaker's log
// records.
const scopeName = "github.com/z5labs/humus/grpc/grpcclient"

// breakerOutcome is the breaker's classifier. Every result the Client
// settles a ticket with is a *result, already classified, so it returns
// the Breaker of its Classification.
func breakerOutcome(_ context.Context, err error) circuit.Outcome {
	r, ok := err.(*result)
	if !ok {
		panic(fmt.Sprintf("grpcclient: a breaker's result is a %T", err))
	}
	return r.class.Breaker
}

// rejection is the Classification of a call the breaker rejected, which
// the Client decides without asking Classify: the call ends. Its Breaker is
// never counted, since a rejected call holds no ticket.
var rejection = Classification{Retry: retry.Stop, Breaker: circuit.Ignored}

// rejectedError is the error of a call the breaker rejected. It matches
// circuit.ErrRejected with errors.Is, and *circuit.RejectedError with
// errors.As, and its gRPC status is Unavailable, with the rejection's
// message, as for a server the Client can't reach.
type rejectedError struct {
	rejected *circuit.RejectedError
}

func (e *rejectedError) Error() string { return e.rejected.Error() }

func (e *rejectedError) Unwrap() error { return e.rejected }

// GRPCStatus is the status that status.FromError and status.Code read.
func (e *rejectedError) GRPCStatus() *status.Status {
	return status.New(codes.Unavailable, e.rejected.Error())
}

// reject is the result of a call to method that the breaker rejected with
// err. The call never reached grpc-go, so otelgrpc made no span for it: the
// Client starts one of its own, a child of the span in ctx, named and
// attributed as otelgrpc names an attempt's span, with Addr as its server,
// and ends it at once, with status Error and the rejection's message.
func (c *Client) reject(ctx context.Context, method string, err error) *result {
	var rejected *circuit.RejectedError
	if !errors.As(err, &rejected) {
		// circuit's Allow returns nothing else, so this is a change in
		// bedrock that humus hasn't caught up with.
		panic(fmt.Sprintf("grpcclient: the breaker rejected a call with a %T", err))
	}
	rerr := &rejectedError{rejected: rejected}

	// As otelgrpc's ParseFullMethod: a method without its leading slash
	// is the span's name, and rpc.method, and one with none is the name
	// alone.
	name, attrs := method, []attribute.KeyValue(nil)
	if rest, ok := strings.CutPrefix(method, "/"); ok {
		name, attrs = rest, []attribute.KeyValue{semconv.RPCMethod(rest)}
	}
	attrs = append(attrs,
		semconv.RPCSystemNameGRPC,
		semconv.ServerAddress(c.host),
		semconv.ServerPort(c.port),
		semconv.RPCResponseStatusCode(strings.ToUpper(codes.Unavailable.String())),
	)
	_, span := c.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	span.SetStatus(otelcodes.Error, rerr.Error())
	span.End()

	return &result{err: rerr, class: rejection}
}

// observe counts t, a transition of the breaker, in
// humus.circuit.transitions, and emits one log record for it, in the
// context of the call that made it: circuit breaker open at WARN, and
// circuit breaker half-open and circuit breaker closed at INFO. Its
// attributes are Addr's host and port, the state the breaker left and the
// counts of that state.
func (c *Client) observe(ctx context.Context, t circuit.Transition) {
	c.metrics.transition(ctx, t)

	severity := log.SeverityInfo
	if t.To == circuit.Open {
		severity = log.SeverityWarn
	}
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(severity)
	r.SetSeverityText(severity.String())
	r.SetBody(attribute.StringValue("circuit breaker " + t.To.String()))
	r.AddAttributes(c.metrics.server...)
	r.AddAttributes(
		previousStateKey.String(t.From.String()),
		attribute.Int("humus.circuit.successes", t.Counts.Successes),
		attribute.Int("humus.circuit.failures", t.Counts.Failures),
		attribute.Int("humus.circuit.consecutive_successes", t.Counts.ConsecutiveSuccesses),
		attribute.Int("humus.circuit.consecutive_failures", t.Counts.ConsecutiveFailures),
		attribute.Int("humus.circuit.timed_out", t.Counts.TimedOut),
	)
	c.logger.Emit(ctx, r)
}

// stream is a grpc.ClientStream that opened through the breaker. It holds
// the open's ticket, and settles it once, with how the stream ended: when
// RecvMsg returns an error, io.EOF counting as nil, or, for an RPC with one
// response, when RecvMsg returns it.
type stream struct {
	grpc.ClientStream

	c      *Client
	ctx    context.Context
	method string

	// oneResponse is set for an RPC whose server sends one response, a
	// client-streaming RPC: its first RecvMsg that succeeds is its end.
	oneResponse bool

	ticket  *circuit.Ticket
	settled sync.Once
	// stop stops the function that releases the ticket when ctx is
	// cancelled.
	stop func() bool
}

// watch returns s, opened to method with ctx, wrapped to settle t when it
// ends. A caller that cancels ctx before the stream ends gave up, which
// says nothing about the dependency, so the ticket is released, counting
// nothing. Release calls no function of the end user's, so it is safe on
// the goroutine context.AfterFunc starts. A stream whose ctx reaches its
// deadline is settled by the RecvMsg that sees it, on the caller's
// goroutine.
func (c *Client) watch(ctx context.Context, desc *grpc.StreamDesc, method string, s grpc.ClientStream, t *circuit.Ticket) grpc.ClientStream {
	w := &stream{
		ClientStream: s,
		c:            c,
		ctx:          ctx,
		method:       method,
		oneResponse:  !desc.ServerStreams,
		ticket:       t,
	}
	w.stop = context.AfterFunc(ctx, func() {
		if errors.Is(ctx.Err(), context.Canceled) {
			w.settled.Do(t.Release)
		}
	})
	return w
}

// RecvMsg receives a message, and settles the stream's ticket once the
// stream has ended.
func (s *stream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	switch {
	case err != nil:
		s.settle(err)
	case s.oneResponse:
		s.settle(nil)
	}
	return err
}

// settle settles the stream's ticket, once, with how the stream ended:
// err, nil for io.EOF. A stream that failed once its caller cancelled ctx
// is released, counting nothing, as watch releases it, whichever of the
// two runs first. Any other error once ctx has ended is the Client's to
// classify, as for a unary attempt, and Classify is asked about the rest,
// with the stream's trailer. A panic in Classify passes to the caller of
// RecvMsg, and the ticket is released.
func (s *stream) settle(err error) {
	s.settled.Do(func() {
		s.stop()
		defer s.ticket.Release()
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if err != nil && errors.Is(s.ctx.Err(), context.Canceled) {
			return
		}
		var class Classification
		if err != nil && ended(s.ctx) {
			class = decided(s.ctx)
		} else {
			class = s.c.classify(s.method, err, s.Trailer())
			mustBeValid(class)
		}
		s.ticket.Done(&result{err: err, class: class})
	})
}
