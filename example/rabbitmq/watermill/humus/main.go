// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command humus is the invoicer the Watermill migration guide ends with: a queue.Processor on humus's queue/rabbitmq.
package main

import (
	"github.com/z5labs/humus"
)

func main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}
