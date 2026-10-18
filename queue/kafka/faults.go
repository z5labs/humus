// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/fault"
)

// stripEvent is the span event of a strip. It is humus's own, so it starts
// with humus.: OpenTelemetry has no convention for failure injection.
const stripEvent = "humus.fit.strip"

// pointOf returns the injection point of topic: kafka:<topic>, such as
// kafka:orders.
func pointOf(topic string) string {
	return "kafka:" + topic
}

// strip returns ctx, a record's context under its process span, with the
// failure scenario it carries stripped, unless the Service's TrustScenario
// trusts m. TrustScenario is asked only when ctx carries a scenario, and
// the zero Injector, of an App without failure injection, finds none. A
// strip adds a humus.fit.strip event to the span in ctx. A panic in
// TrustScenario is not recovered.
func (s *Service) strip(ctx context.Context, m Message) context.Context {
	stripped, carried := s.faults.Strip(ctx)
	if !carried {
		return ctx
	}
	if trust := s.settings.TrustScenario; trust != nil && trust(m) {
		return ctx
	}
	trace.SpanFromContext(ctx).AddEvent(stripEvent)
	return stripped
}

// headersContext returns the context a record's headers are injected from:
// ctx, which carries the record's span, with the failure scenario stripped
// unless the Producer forwards it. A strip adds a humus.fit.strip event to
// the record's span.
func (p *Producer) headersContext(ctx context.Context, span trace.Span) context.Context {
	if p.forward {
		return ctx
	}
	stripped, carried := p.faults.Strip(ctx)
	if !carried {
		return ctx
	}
	span.AddEvent(stripEvent)
	return stripped
}

// injection is what failure injection makes of one call to Produce: the
// longest Delay of its records' faults, and the point it was at, and the
// error of the first record whose fault aborts, nil when none does.
type injection struct {
	delay      time.Duration
	delayPoint string
	abort      error
}

// evaluate evaluates each record's point, kafka:<topic>, once, with the
// record's own context, which carries its span, so each evaluation's
// humus.fit.evaluation event goes on that record's span.
func (p *Producer) evaluate(krs []*kgo.Record) injection {
	var inj injection
	for _, kr := range krs {
		point := pointOf(kr.Topic)
		f := p.faults.Eval(kr.Context, point)
		if f.Delay > inj.delay {
			inj.delay, inj.delayPoint = f.Delay, point
		}
		if f.Abort && inj.abort == nil {
			inj.abort = &fault.InjectedError{Point: point}
		}
	}
	return inj
}

// inject emulates inj for one call, whose context is call: it waits the
// longest Delay, bounded by call's deadline, DeliveryTimeout. It returns
// the error every record of the call fails with, unsent: a
// *fault.InjectedError matching call's cause when the Delay was cut short,
// or the abort's. It returns nil when the call goes on to be sent.
func (inj injection) inject(call context.Context) error {
	if err := (fit.Fault{Delay: inj.delay}).Wait(call); err != nil {
		return &fault.InjectedError{Point: inj.delayPoint, Err: err}
	}
	return inj.abort
}

// failUnsent fails every record of krs, none of which reached franz-go,
// with err. Each record's span records err, with its error.type, and ends.
// Neither messaging instrument counts a record that was never sent. It
// returns the call's error, joining each record's with its topic, as
// produceError does.
func failUnsent(krs []*kgo.Record, err error) error {
	errs := make([]error, len(krs))
	for i, kr := range krs {
		s := kr.Context.Value(sendingKey{}).(*sending)
		recordFailure(s.span, err)
		s.span.End()
		errs[i] = fmt.Errorf("kafka: producing to %s: %w", kr.Topic, err)
	}
	return errors.Join(errs...)
}
