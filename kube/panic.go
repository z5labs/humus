// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"context"
	"fmt"
	"runtime/debug"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// panicked is a panic guard recovered: its value, and the stack of the
// goroutine where it was recovered, which still holds the panic's frames.
type panicked struct {
	value any
	stack []byte
}

// guard calls f, and returns the panic it recovers, or nil when f returns.
func guard(f func()) (p *panicked) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		// The stack is taken here, in the deferred call, where it still
		// holds the frames of the panic.
		p = &panicked{value: v, stack: debug.Stack()}
	}()
	f()
	return nil
}

// recordPanic records p as an exception event on the reconcile's span, with
// exception.type, exception.message and exception.stacktrace, sets the
// span's status to Error, and emits one ERROR record, "reconcile panicked",
// in the span's context, with the object's attrs and the same three.
func (c *Controller) recordPanic(ctx context.Context, attrs []attribute.KeyValue, p *panicked) {
	exception := []attribute.KeyValue{
		semconv.ExceptionType(fmt.Sprintf("%T", p.value)),
		semconv.ExceptionMessage(fmt.Sprint(p.value)),
		semconv.ExceptionStacktrace(string(p.stack)),
	}
	span := trace.SpanFromContext(ctx)
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(exception...))
	span.SetStatus(codes.Error, "reconcile panicked")
	c.emit(ctx, log.SeverityError, "reconcile panicked", append(attrs[:len(attrs):len(attrs)], exception...)...)
}
