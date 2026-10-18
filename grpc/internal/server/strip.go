// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"

	"github.com/z5labs/humus/fault"
)

// stripEvent is the span event of a strip. It is humus's own, so it starts
// with humus.: OpenTelemetry has no convention for failure injection.
const stripEvent = "humus.fit.strip"

// Trust reports whether the failure scenario an RPC carries reaches the
// rest of the chain. ctx is the RPC's context as it arrived, scenario and
// all: it holds the RPC's peer and incoming metadata. method is the RPC's
// full method, such as "/grpc.testing.TestService/EmptyCall". A nil Trust
// trusts no caller.
type Trust func(ctx context.Context, method string) bool

// strip returns ctx with the scenario it carries stripped, with faults,
// and true, unless trust trusts the caller. trust is asked only when ctx
// carries a scenario, and the zero Injector, of an App without failure
// injection, finds none. A strip adds a humus.fit.strip event to the span
// in ctx. When nothing is stripped, it returns ctx itself, and false.
func strip(ctx context.Context, faults fault.Injector, trust Trust, method string) (context.Context, bool) {
	stripped, carried := faults.Strip(ctx)
	if !carried || (trust != nil && trust(ctx, method)) {
		return ctx, false
	}
	trace.SpanFromContext(stripped).AddEvent(stripEvent)
	return stripped, true
}

// StripUnary returns the unary interceptor that strips the failure
// scenario an RPC carries, with faults, unless trust trusts the caller, as
// the stream one does. The rest of the chain, the end user's interceptors
// and the handler, is given the stripped context. Every other baggage
// member is kept.
//
// It belongs inside the recovering interceptor New adds, so a panic of
// trust is recovered and recorded, and fails the RPC, as a panic of the
// handler does; and outside the end user's interceptors, so none of the
// end user's code sees a scenario it doesn't trust.
func StripUnary(faults fault.Injector, trust Trust) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, _ = strip(ctx, faults, trust, info.FullMethod)
		return handler(ctx, req)
	}
}

// StripStream returns the stream interceptor that strips the failure
// scenario a stream carries, with faults, unless trust trusts the caller.
// The rest of the chain, the end user's stream interceptors and the
// handler, is given a stream whose Context is the stripped one. Every
// other baggage member is kept.
//
// It belongs inside the recovering interceptor and the one that ends
// streams at Shutdown, which New adds, so a panic of trust is recovered,
// and the stripped context is still the one Shutdown cancels.
func StripStream(faults fault.Injector, trust Trust) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if ctx, stripped := strip(ss.Context(), faults, trust, info.FullMethod); stripped {
			ss = &serverStream{ServerStream: ss, ctx: ctx}
		}
		return handler(srv, ss)
	}
}
