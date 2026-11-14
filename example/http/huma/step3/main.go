// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command step3 is the users API after the Huma migration guide's last step: every operation on openapi/v3, without Huma.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
)

func main() {
	httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
}
