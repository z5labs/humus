// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command ledger serves the wallet's ledger: its accounts and the transfers between them, on PostgreSQL.
package main

import (
	"github.com/z5labs/humus/example/wallet/ledger/app"
	"github.com/z5labs/humus/grpc/grpcserver"
)

func main() {
	grpcserver.Main(app.Init)
}
