// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package queue

import (
	"context"
	"fmt"
)

// Processor processes one message. Under AtLeastOnce it must be idempotent:
// a message whose processing failed, or was cut short by a crash or a
// rebalance, is delivered again.
type Processor[T any] interface {
	Process(ctx context.Context, msg T) error
}

// ProcessorFunc adapts a function to a Processor.
type ProcessorFunc[T any] func(ctx context.Context, msg T) error

// Process implements the [Processor] interface by calling f(ctx, msg) and
// returning its error.
func (f ProcessorFunc[T]) Process(ctx context.Context, msg T) error {
	return f(ctx, msg)
}

// Delivery is when a message is acknowledged, relative to processing it.
type Delivery string

const (
	// AtLeastOnce acknowledges a message once Process has returned nil for
	// it. A message is never lost, and may be processed more than once.
	AtLeastOnce Delivery = "at-least-once"

	// AtMostOnce acknowledges a message before Process is called. A
	// message is never processed twice, and is lost if processing fails or
	// the process stops first.
	AtMostOnce Delivery = "at-most-once"
)

// CheckDelivery rejects a Delivery other than AtLeastOnce and AtMostOnce.
// The zero Delivery is rejected too: it is not a default. The error doesn't
// name a field, so a broker package's check can add the field's name, and a
// Delivery sourced from a variable can report the variable's.
func CheckDelivery(d Delivery) error {
	switch d {
	case AtLeastOnce, AtMostOnce:
		return nil
	}
	return fmt.Errorf("%q is not one of %s or %s", string(d), AtLeastOnce, AtMostOnce)
}
