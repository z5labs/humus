// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/internal/defaults"
	"github.com/z5labs/humus/telemetry"
)

// Defaults returns Main's defaults for an App on h with init:
//
//   - Host: h.
//   - Telemetry: a Builder that calls telemetry.New with
//     telemetry.DefaultSettings(), the App's host and the Process's stdout.
//   - ShutdownGracePeriod: 20s.
//   - Drain: none.
//   - Init: init.
//   - Faults: none, so failure injection is off.
func Defaults[H Network](h H, init Init[H]) App[H] {
	return App[H]{
		Host:                h,
		ShutdownGracePeriod: defaults.ShutdownGracePeriod,
		Telemetry:           defaults.Telemetry,
		Init:                init,
	}
}

// Main is MainOn on OSHost: it runs the App that init and opts describe on
// the OS process and the OS's network and filesystem, and exits the process
// with the App's status. It never returns.
//
// The App starts from Defaults(OSHost(), init), and opts change it in order.
// HUMUS_SHUTDOWN_GRACE_PERIOD and HUMUS_SHUTDOWN_DRAIN_DELAY override the
// grace period and the drain, as for any App. If Apply fails, Main writes
// its error to the Process's stderr and exits ExitConfig. It panics if init
// is nil.
func Main(init Init[Host], opts ...Option) {
	bedrock.Main(mainApp("Main", acquireOSHost, init, opts...))
}

// acquireOSHost is Main's acquire.
func acquireOSHost() (Host, error) { return OSHost(), nil }

// MainOn runs the App that init and opts describe on the OS process and on
// the host that acquire returns, and exits the process with the App's
// status. It never returns.
//
// The App starts from Defaults, and opts are applied, in order, before
// acquire is called. If Apply fails, MainOn writes its error to the
// Process's stderr and exits ExitConfig, without calling acquire. Otherwise
// acquire is called once, and an error from it exits as an error from Init
// does: ExitConfig when a *config.Error is in its chain, and ExitFailure
// otherwise. MainOn never releases the host: a host that needs an orderly
// close registers it in Init with Setup.OnStop. It panics if init or acquire
// is nil.
func MainOn[H Network](acquire func() (H, error), init Init[H], opts ...Option) {
	if acquire == nil {
		panic("humus: MainOn called with a nil acquire")
	}
	bedrock.Main(mainApp("MainOn", acquire, init, opts...))
}

// mainApp returns the bedrock.App that Main and MainOn, named by caller,
// run for acquire, init and opts. It panics if init is nil.
func mainApp[H Network](caller string, acquire func() (H, error), init Init[H], opts ...Option) bedrock.App {
	if init == nil {
		panic("humus: " + caller + " called with a nil init")
	}
	return acquiringApp[H]{acquire: acquire, init: init, opts: opts}
}

// acquiringApp is the bedrock.App that Main and MainOn run: it applies opts
// to Defaults, acquires the host and then runs the App on it.
type acquiringApp[H Network] struct {
	acquire func() (H, error)
	init    Init[H]
	opts    []Option
}

// Run implements bedrock.App.
func (m acquiringApp[H]) Run(ctx context.Context, p bedrock.Process) bedrock.ExitStatus {
	var zero H
	a := Defaults(zero, m.init)
	if err := a.Apply(m.opts...); err != nil {
		return fail(p.Stderr, ExitConfig, err)
	}
	h, err := m.acquire()
	if err != nil {
		code := ExitFailure
		if _, ok := errors.AsType[*config.Error](err); ok {
			code = ExitConfig
		}
		return fail(p.Stderr, code, fmt.Errorf("acquire the host: %w", err))
	}
	a.Host = h
	return a.Run(ctx, p)
}

// Option changes an App. It isn't generic, so one Option works for an App
// on any host, and only this package's With functions make one: the zero
// Option is not an Option, and Apply rejects it.
type Option struct {
	apply func(t optionTarget) error
}

// optionTarget is what an Option changes: an *App[H] for some H.
type optionTarget interface {
	setShutdownGracePeriod(d time.Duration)
	setDrain(d Drain)
	setTelemetry(b telemetry.Builder)
	setFaults(f any, host reflect.Type) error
}

func (a *App[H]) setShutdownGracePeriod(d time.Duration) { a.ShutdownGracePeriod = d }
func (a *App[H]) setDrain(d Drain)                       { a.Drain = d }
func (a *App[H]) setTelemetry(b telemetry.Builder)       { a.Telemetry = b }

func (a *App[H]) setFaults(f any, host reflect.Type) error {
	faults, ok := f.(func(context.Context, *Setup, H) (fault.Settings, error))
	if !ok {
		return fmt.Errorf("humus: WithFaults was made for an App on %v, not on %v", host, reflect.TypeFor[H]())
	}
	a.Faults = faults
	return nil
}

// Apply applies opts to the App in order. If one of them is the zero
// Option, or was made for an App on another host type, Apply returns an
// error and changes nothing. For another host type, the error names both
// types.
func (a *App[H]) Apply(opts ...Option) error {
	b := *a
	for i, opt := range opts {
		if opt.apply == nil {
			return fmt.Errorf("humus: option %d is the zero Option: only humus's With functions make an Option", i)
		}
		if err := opt.apply(&b); err != nil {
			return err
		}
	}
	*a = b
	return nil
}

// WithShutdownGracePeriod sets App.ShutdownGracePeriod, the grace period
// when HUMUS_SHUTDOWN_GRACE_PERIOD is not set.
func WithShutdownGracePeriod(d time.Duration) Option {
	return Option{apply: func(t optionTarget) error {
		t.setShutdownGracePeriod(d)
		return nil
	}}
}

// WithDrain sets App.Drain, the drain when HUMUS_SHUTDOWN_DRAIN_DELAY is not
// set. WithDrain(nil) removes a drain that an earlier Option set.
func WithDrain(d Drain) Option {
	return Option{apply: func(t optionTarget) error {
		t.setDrain(d)
		return nil
	}}
}

// WithTelemetry sets App.Telemetry.
func WithTelemetry(b telemetry.Builder) Option {
	return Option{apply: func(t optionTarget) error {
		t.setTelemetry(b)
		return nil
	}}
}

// WithFaults sets App.Faults. H is inferred from f, and the Option it
// returns is for an App on H only: Apply rejects it for an App on another
// host type.
func WithFaults[H Network](f func(ctx context.Context, s *Setup, h H) (fault.Settings, error)) Option {
	host := reflect.TypeFor[H]()
	return Option{apply: func(t optionTarget) error {
		return t.setFaults(f, host)
	}}
}
