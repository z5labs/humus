// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command orderstatus projects orders' events into their statuses.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/orderstatus/app"
)

func main() {
	humus.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry))
}
