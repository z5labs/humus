// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command polls serves the polls service's hypermedia UI, with live results on every replica, on PostgreSQL.
package main

import (
	"github.com/z5labs/humus/example/http/polls/app"
	"github.com/z5labs/humus/httpserver"
)

func main() {
	httpserver.Main(app.Init)
}
