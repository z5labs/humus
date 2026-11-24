// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver

import (
	"time"

	"github.com/z5labs/humus"
)

// Defaults returns a gRPC service's options over humus.Main's: a drain of
// humus.DrainDelay(5 * time.Second) and a 15s grace period. The host and
// the telemetry Builder stay humus.Main's. A main on a host of its own
// spreads them before its own options:
// humus.MainOn(acquire, init, append(grpcserver.Defaults(), opts...)...).
func Defaults() []humus.Option {
	return []humus.Option{
		humus.WithDrain(humus.DrainDelay(5 * time.Second)),
		humus.WithShutdownGracePeriod(15 * time.Second),
	}
}

// Main is humus.Main with Defaults applied before opts, so the end user's
// options win over the package's. It never returns.
func Main(init humus.Init[humus.Host], opts ...humus.Option) {
	humus.Main(init, mainOptions(opts...)...)
}

// mainOptions returns the options Main passes to humus.Main for opts:
// Defaults, then opts in order.
func mainOptions(opts ...humus.Option) []humus.Option {
	return append(Defaults(), opts...)
}
