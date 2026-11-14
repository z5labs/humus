// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command step1 is the users service after the Echo migration guide's first step: its Echo engine, run on humus.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
)

func main() {
	httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
}
