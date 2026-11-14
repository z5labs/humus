// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command step3 is the users service the Fiber migration guide ends with: every route on openapi/v3, without Fiber.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
)

func main() {
	httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
}
