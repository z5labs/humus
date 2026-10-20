// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command databases provisions a database, its role and a Secret in PostgreSQL for each Database.
package main

import (
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/databases/app"
)

func main() {
	humus.Main(app.Init, humus.WithShutdownGracePeriod(20*time.Second))
}
