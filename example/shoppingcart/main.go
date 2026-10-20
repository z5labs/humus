// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command shoppingcart serves the shoppingcart's API for shopping carts, on PostgreSQL.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/app"
	"github.com/z5labs/humus/httpserver"
)

func main() {
	httpserver.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults))
}
