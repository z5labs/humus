// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command step2 is the users API after the Huma migration guide's second step: /v1/users/{id} on openapi/v3, the rest on Huma.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
)

func main() {
	httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
}
