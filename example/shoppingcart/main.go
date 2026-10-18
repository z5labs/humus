// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command shoppingcart is humus's reference service: a REST API for shopping
// carts, on Postgres. Its Init is in package app, which documents every
// variable it reads.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/app"
	"github.com/z5labs/humus/httpserver"
)

func main() {
	httpserver.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry))
}
