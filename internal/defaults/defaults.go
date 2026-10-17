// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package defaults holds the defaults of the App that humus.Main runs, so
// that humustest can run a test's App with the same ones without either
// package exporting them.
//
// It can't import the root package, which imports it, so it holds the
// values, and each of humus.Main and humustest assembles its App from them.
// The Host is not here: humus.Main's is humus.OSHost(), and humustest's is a
// Host of its own. Nor is the drain: the App that humus.Main runs has none.
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
// telemetry.DefaultSettings(), the Host's Dial and the Process's stdout.
// Logs go to stdout as JSON lines, and spans get real IDs without being
// exported.
func Telemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	return telemetry.New(ctx, telemetry.DefaultSettings(), dial, p.Stdout)
}
