// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command billing issues a month of invoices and charges every pending one, once.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/billing/app"
)

func main() {
	humus.Main(app.Init, app.Options()...)
}
