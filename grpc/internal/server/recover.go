// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errPanicked is the error an RPC that panicked fails with. Its message is
// fixed, so the panic's value never reaches the client.
var errPanicked = status.Error(codes.Internal, "internal error")

// recoverUnary returns a unary interceptor that recovers a panic of the
// rest of the chain, the end user's interceptors and the handler, records
// it with recordPanic, and fails the RPC with errPanicked.
func recoverUnary(logger log.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if v := recover(); v != nil {
				recordPanic(ctx, logger, info.FullMethod, v)
				resp, err = nil, errPanicked
			}
		}()
		return handler(ctx, req)
	}
}

// recoverStream returns a stream interceptor that recovers a panic of the
// rest of the chain, the end user's interceptors and the handler, records
// it with recordPanic, and ends the stream with errPanicked.
func recoverStream(logger log.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if v := recover(); v != nil {
				recordPanic(ss.Context(), logger, info.FullMethod, v)
				err = errPanicked
			}
		}()
		return handler(srv, ss)
	}
}

// recordPanic records the panic v of the RPC to fullMethod as an exception
// event on the RPC's span and as one ERROR record, emitted in ctx, so the
// record carries the span's trace and span IDs. The span's status is left
// to otelgrpc, which sets it to Error from the Internal code the RPC fails
// with.
//
// It must be called from the deferred function that recovered v, where the
// stack still holds the frames of the panic.
func recordPanic(ctx context.Context, logger log.Logger, fullMethod string, v any) {
	exception := []attribute.KeyValue{
		semconv.ExceptionType(fmt.Sprintf("%T", v)),
		semconv.ExceptionMessage(fmt.Sprint(v)),
		semconv.ExceptionStacktrace(string(debug.Stack())),
	}
	trace.SpanFromContext(ctx).AddEvent(semconv.ExceptionEventName, trace.WithAttributes(exception...))

	// rpc.method is the method without its leading slash, as on the span.
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(log.SeverityError)
	r.SetSeverityText(log.SeverityError.String())
	r.SetBody(attribute.StringValue("RPC panicked"))
	r.AddAttributes(semconv.RPCMethod(strings.TrimPrefix(fullMethod, "/")))
	r.AddAttributes(exception...)
	logger.Emit(ctx, r)
}
