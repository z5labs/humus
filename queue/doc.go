// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package queue provides support for creating message queue processing services.
//
// The queue package implements a three-phase message processing pattern:
//
//   - [Consumer]: retrieves messages from a queue
//   - [Processor]: executes business logic on messages
//   - [Acknowledger]: confirms successful processing back to the queue
//
// [Runtime] implementations orchestrate these three phases. When a [Consumer]
// returns [ErrEndOfQueue], it signals the [Runtime] to shut down gracefully.
package queue
