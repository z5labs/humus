// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package queue is the contract between the end user's code and any message
// broker: what processing a message returns, and when the message is
// acknowledged.
//
// The end user implements one interface, [Processor], whatever the broker,
// and [ProcessorFunc] turns a function into one:
//
//	var p queue.Processor[Order] = queue.ProcessorFunc[Order](func(ctx context.Context, o Order) error {
//		return store.Save(ctx, o) // idempotent: under queue.AtLeastOnce, o may come again
//	})
//
// A [Delivery] says when a message is acknowledged, relative to processing
// it: [AtLeastOnce] after Process has returned nil for it, [AtMostOnce]
// before Process is called. [CheckDelivery] rejects any other value,
// including the zero Delivery, which is not a default.
//
// queue has no consumer of its own. Each broker package, such as
// queue/kafka, provides its Service, which takes a Delivery and a Processor
// and implements the contract below against its own client. queue depends
// on nothing but the standard library.
//
// # The contract
//
// Every broker package's Service does the following.
//
//   - A processing error under AtLeastOnce ends the Service. The message
//     isn't acknowledged, the Service stops as it would on a signal, and its
//     Serve returns the error, so the App exits 1. The message is delivered
//     again when the App restarts, to it or to another replica.
//   - A processing error under AtMostOnce is logged and counted, and
//     processing goes on. The message was already acknowledged, so stopping
//     wouldn't bring it back.
//   - No panic is recovered. A panicking Processor crashes the process.
//   - Shutdown drains. On Serve's context a Service stops taking messages,
//     lets the ones in flight finish, acknowledges them, and returns.
//     Process's context is not cancelled when shutdown starts, only when the
//     grace period runs out.
//   - Retries are the Processor's. A Processor that wants to retry a
//     transient failure does so itself, for example with bedrock's retry
//     module, before it returns an error. Neither queue nor a broker package
//     retries for it.
//
// So a Processor under AtLeastOnce must be idempotent: a message whose
// processing failed, or was cut short by a crash or a rebalance, is
// delivered again, and may be processed more than once. A Processor that
// would rather skip a message that always fails than stop on it returns nil,
// after storing the message wherever it keeps dead letters.
package queue
