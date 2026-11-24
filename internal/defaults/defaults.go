// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package defaults holds the values of humus.Defaults, the defaults of the
// App that humus.Main and humus.MainOn run, so that they are tested apart
// from the root package.
//
// The host is not here: it is an argument of humus.Defaults. Nor is the
// drain: the App that humus.Main runs has none.
package defaults

import (
	"context"
	"time"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/telemetry"
)

// ShutdownGracePeriod is the grace period when HUMUS_SHUTDOWN_GRACE_PERIOD
// is not set. With telemetry's 5s shutdown after it, it fits inside
// Kubernetes' default terminationGracePeriodSeconds of 30s.
const ShutdownGracePeriod = 20 * time.Second

// Telemetry is the App's telemetry.Builder: telemetry.New with
// telemetry.DefaultSettings(), the App's host and the Process's stdout.
// Logs go to stdout as JSON lines, and spans get real IDs without being
// exported.
func Telemetry(ctx context.Context, p bedrock.Process, n telemetry.Network) (telemetry.SDK, error) {
	return telemetry.New(ctx, telemetry.DefaultSettings(), n, p.Stdout)
}
