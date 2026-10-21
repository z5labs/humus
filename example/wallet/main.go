// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command wallet serves the wallet: each caller's own account, on the ledger, for a bearer token's subject.
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/wallet/app"
	"github.com/z5labs/humus/grpc/grpcserver"
)

func main() {
	grpcserver.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults))
}
