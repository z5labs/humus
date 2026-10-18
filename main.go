// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"time"

	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/internal/defaults"
	"github.com/z5labs/humus/telemetry"
)

// Main runs the App that init and opts describe on the OS process, and exits
// the process with the App's status. It never returns.
//
// The App starts from Main's defaults, and opts change them in order:
//
//   - Host: OSHost().
//   - Telemetry: a Builder that calls telemetry.New with
//     telemetry.DefaultSettings(), the Host's Dial and the Process's stdout.
//   - ShutdownGracePeriod: 20s.
//   - Drain: none.
//   - Faults: none, so failure injection is off.
//
// Init is init, and is set before opts are applied, so an Option sees it
// and may wrap it, for example to add a Service.
//
// HUMUS_SHUTDOWN_GRACE_PERIOD and HUMUS_SHUTDOWN_DRAIN_DELAY override the
// grace period and the drain, as for any App. Main applies every option
// before it runs anything, and is then bedrock.Main of the App. It panics if
// init or one of opts is nil.
func Main(init func(ctx context.Context, s *Setup) ([]Service, error), opts ...Option) {
	bedrock.Main(mainApp(init, opts...))
}

// mainApp returns the App that Main runs for init and opts: Main's defaults,
// with Init set to init, and then opts applied in order.
func mainApp(init func(ctx context.Context, s *Setup) ([]Service, error), opts ...Option) App {
	if init == nil {
		panic("humus: Main called with a nil init")
	}
	a := App{
		Host:                OSHost(),
		ShutdownGracePeriod: defaults.ShutdownGracePeriod,
		Telemetry:           defaults.Telemetry,
		Init:                init,
	}
	for _, opt := range opts {
		if opt == nil {
			panic("humus: Main called with a nil Option")
		}
		opt(&a)
	}
	return a
}

// Option changes fields of the App that Main or humustest builds.
type Option func(*App)

// WithHost sets App.Host.
func WithHost(h Host) Option {
	return func(a *App) { a.Host = h }
}

// WithTelemetry sets App.Telemetry.
func WithTelemetry(b telemetry.Builder) Option {
	return func(a *App) { a.Telemetry = b }
}

// WithShutdownGracePeriod sets App.ShutdownGracePeriod, the grace period
// when HUMUS_SHUTDOWN_GRACE_PERIOD is not set.
func WithShutdownGracePeriod(d time.Duration) Option {
	return func(a *App) { a.ShutdownGracePeriod = d }
}

// WithDrain sets App.Drain, the drain when HUMUS_SHUTDOWN_DRAIN_DELAY is not
// set. WithDrain(nil) removes a drain that an earlier Option set.
func WithDrain(d Drain) Option {
	return func(a *App) { a.Drain = d }
}

// WithFaults sets App.Faults.
func WithFaults(f func(ctx context.Context, s *Setup) (fault.Settings, error)) Option {
	return func(a *App) { a.Faults = f }
}
