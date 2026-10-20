// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command catalog serves the shoppingcart's catalog: the products it sells and their prices, on PostgreSQL.
package main

import (
	"github.com/z5labs/humus/example/shoppingcart/catalog/app"
	"github.com/z5labs/humus/httpserver"
)

func main() {
	httpserver.Main(app.Init)
}
