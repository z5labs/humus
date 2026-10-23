// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command migrate applies a shoppingcart service's migrations before the service starts.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/shoppingcart/migrate"
)

func main() {
	humus.Main(migrate.Init, humus.WithTelemetry(migrate.BuildTelemetry))
}
