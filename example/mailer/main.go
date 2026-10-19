// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command mailer sends the emails other services ask for over RabbitMQ.
package main

import (
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/mailer/app"
)

func main() {
	humus.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults), humus.WithShutdownGracePeriod(25*time.Second))
}
