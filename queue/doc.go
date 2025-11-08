// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package queue provides support for creating message queue processing services.
//
// The queue package implements a three-phase message processing pattern that separates
// concerns for consuming, processing, and acknowledging messages from a queue:
//
//   - Consumer: retrieves messages from a queue
//   - Processor: executes business logic on messages
//   - Acknowledger: confirms successful processing back to the queue
//
// Runtime implementations orchestrate these three phases and handle the application
// lifecycle. When a Consumer returns the EOQ error, it signals that the queue is
// exhausted and the Runtime should shut down gracefully. This is particularly useful
// for finite queues or batch processing scenarios.
//
// # Example Usage
//
// Here's a typical Runtime implementation that coordinates the three phases:
//
//	type MyRuntime struct {
//	    consumer     queue.Consumer[Message]
//	    processor    queue.Processor[Message]
//	    acknowledger queue.Acknowledger[Message]
//	}
//
//	func (r *MyRuntime) ProcessQueue(ctx context.Context) error {
//	    for {
//	        // Phase 1: Consume a message
//	        msg, err := r.consumer.Consume(ctx)
//	        if errors.Is(err, queue.EOQ) {
//	            // Queue is exhausted, shut down gracefully
//	            return nil
//	        }
//	        if err != nil {
//	            return fmt.Errorf("consume failed: %w", err)
//	        }
//
//	        // Phase 2: Process the message
//	        if err := r.processor.Process(ctx, msg); err != nil {
//	            return fmt.Errorf("process failed: %w", err)
//	        }
//
//	        // Phase 3: Acknowledge successful processing
//	        if err := r.acknowledger.Acknowledge(ctx, msg); err != nil {
//	            return fmt.Errorf("acknowledge failed: %w", err)
//	        }
//	    }
//	}
//
// The application is then built using the Builder and Run functions, which automatically
// include OpenTelemetry instrumentation, panic recovery, and graceful shutdown handling:
//
//	func main() {
//	    configReader := ... // io.Reader with YAML config
//	    queue.Run(configReader, buildApp)
//	}
//
//	func buildApp(ctx context.Context, cfg Config) (*queue.App, error) {
//	    runtime := &MyRuntime{
//	        consumer:     ...,
//	        processor:    ...,
//	        acknowledger: ...,
//	    }
//	    return queue.NewApp(runtime), nil
//	}
//
// # Processing Semantics
//
// The queue package provides two built-in item processors that implement different
// delivery guarantee semantics:
//
// AtMostOnce guarantees that each message is processed at most once.
// Messages are acknowledged immediately after consumption, before processing. If
// processing fails, the message is lost and will not be retried. Use this for
// non-critical data where performance is more important than reliability:
//
//	processor := queue.ProcessAtMostOnce(consumer, processor, acknowledger)
//	for {
//	    err := processor.ProcessItem(ctx)
//	    if errors.Is(err, queue.EOQ) {
//	        return nil
//	    }
//	    // Continue even on errors - message already acknowledged
//	}
//
// AtLeastOnce guarantees that each message is processed at least once.
// Messages are acknowledged only after successful processing. If processing fails,
// the message will be redelivered for retry. Your Processor must be idempotent to
// handle duplicate messages correctly. Use this for critical data where reliability
// is more important than avoiding duplicate processing:
//
//	processor := queue.ProcessAtLeastOnce(consumer, processor, acknowledger)
//	for {
//	    err := processor.ProcessItem(ctx)
//	    if errors.Is(err, queue.EOQ) {
//	        return nil
//	    }
//	    if err != nil {
//	        // Message not acknowledged, will be retried
//	        return err
//	    }
//	}
//
// Both processors automatically instrument operations with OpenTelemetry tracing
// and logging. They can be used with any Runtime implementation by calling their
// ProcessItem method in a loop until EOQ is returned.
package queue
