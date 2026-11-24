// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package telemetry_test

import (
	"context"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/telemetry"
)

// A Builder starts from DefaultSettings, changes the fields it sources, and
// hands the Network and the process's stdout to New.
func ExampleNew() {
	var build telemetry.Builder = func(ctx context.Context, p bedrock.Process, n telemetry.Network) (telemetry.SDK, error) {
		s := telemetry.DefaultSettings()
		s.ServiceName = "carts"
		return telemetry.New(ctx, s, n, p.Stdout)
	}
	_ = build
}
