// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"context"
	"time"

	"github.com/z5labs/humus"
)

// Defaults sets an HTTP service's defaults over humus.Main's: a drain of
// humus.DrainDelay(5 * time.Second) and a 10s grace period. The Host and
// the telemetry Builder stay humus.Main's.
func Defaults() humus.Option {
	return func(a *humus.App) {
		a.Drain = humus.DrainDelay(5 * time.Second)
		a.ShutdownGracePeriod = 10 * time.Second
	}
}

// Main is humus.Main with Defaults applied before opts, so the end user's
// options win over the package's. It never returns.
func Main(init func(ctx context.Context, s *humus.Setup) ([]humus.Service, error), opts ...humus.Option) {
	humus.Main(init, mainOptions(opts...)...)
}

// mainOptions returns the options Main passes to humus.Main for opts:
// Defaults, then opts in order.
func mainOptions(opts ...humus.Option) []humus.Option {
	return append([]humus.Option{Defaults()}, opts...)
}
