// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/fault"
)

// stripEvent is the span event of a strip. It is humus's own, so it starts
// with humus.: OpenTelemetry has no convention for failure injection.
const stripEvent = "humus.fit.strip"

// pointOf returns the injection point of a publish of m:
// rabbitmq:<messaging.destination.name>, which is <exchange>:<routing key>,
// leaving out what is empty, such as rabbitmq:orders:accepted.
func pointOf(m Publishing) string {
	return "rabbitmq:" + sendDestinationName(m.Exchange, m.RoutingKey)
}

// strip returns ctx, a delivery's context under its process span, with the
// failure scenario it carries stripped, unless the Service's TrustScenario
// trusts m. TrustScenario is asked only when ctx carries a scenario, and
// the zero Injector, of an App without failure injection, finds none. A
// strip adds a humus.fit.strip event to the span in ctx. A panic in
// TrustScenario is not recovered.
func (svc *Service) strip(ctx context.Context, m Message) context.Context {
	stripped, carried := svc.faults.Strip(ctx)
	if !carried {
		return ctx
	}
	if trust := svc.settings.TrustScenario; trust != nil && trust(m) {
		return ctx
	}
	trace.SpanFromContext(ctx).AddEvent(stripEvent)
	return stripped
}

// headersContext returns the context a message's headers are injected
// from: ctx, which carries the Publish's span, with the failure scenario
// stripped unless the Publisher forwards it. A strip adds a
// humus.fit.strip event to the Publish's span, span.
func (p *Publisher) headersContext(ctx context.Context, span trace.Span) context.Context {
	if p.settings.ForwardScenario {
		return ctx
	}
	stripped, carried := p.faults.Strip(ctx)
	if !carried {
		return ctx
	}
	span.AddEvent(stripEvent)
	return stripped
}

// inject is failure injection's seam in an attempt to publish m, which the
// breaker has admitted and which has sent nothing yet. It evaluates m's
// point with the Setup's Injector, in ctx, the Publish's context, which
// carries its span and Timeout, so the evaluation is an event on the
// Publish's span, and waits the Fault's Delay, inside Timeout. It returns
// the attempt's error when a fault ended it, and nil when the attempt goes
// on to publish:
//
//   - an abort is marked lost, as a lost connection's error is, so classify
//     and classifyBreaker decide it as they decide one: Retry, and
//     Failure;
//   - a Delay ctx cuts short is not marked: the attempt ended with ctx, so
//     classify decides it Retry, which retry turns into Expired, and
//     classifyBreaker Failure when ctx's deadline passed, and Ignored when
//     the caller cancelled it.
//
// Both wrap a *fault.InjectedError naming the point, whose error.type is
// github.com/z5labs/humus/fault.InjectedError. With failure injection off,
// it evaluates nothing and returns nil.
func (p *Publisher) inject(ctx context.Context, m Publishing) error {
	point := pointOf(m)
	f := p.faults.Eval(ctx, point)
	if err := f.Wait(ctx); err != nil {
		return fmt.Errorf("rabbitmq: publishing to %s: %w", destination(m), &fault.InjectedError{Point: point, Err: err})
	}
	if f.Abort {
		return lost(fmt.Errorf("rabbitmq: publishing to %s: %w", destination(m), &fault.InjectedError{Point: point}))
	}
	return nil
}
