// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package health

import (
	"context"
	"errors"
	"sync/atomic"
)

// Monitor represents anything which can report its current state of health.
type Monitor interface {
	Healthy(ctx context.Context) (bool, error)
}

// MonitorFunc adapts a function to a [Monitor]. Its Healthy method calls the
// function with the context it is given.
type MonitorFunc func(ctx context.Context) (bool, error)

// Healthy implements the [Monitor] interface by calling f(ctx).
func (f MonitorFunc) Healthy(ctx context.Context) (bool, error) {
	return f(ctx)
}

// Probes holds the monitors an orchestrator checks: Liveness, whether the
// process is still working, and Readiness, whether it can serve traffic
// right now. See the package documentation for what each one should check.
type Probes struct {
	Liveness  Monitor
	Readiness Monitor
}

// Binary is a [Monitor] which simply has 2 states: healthy or unhealthy.
// It is safe for concurrent use. The zero value represents an unhealthy state.
type Binary struct {
	healthy atomic.Bool
}

// MarkUnhealthy changes the state to unhealthy.
func (b *Binary) MarkUnhealthy() {
	b.healthy.Store(false)
}

// MarkHealthy changes the state to healthy.
func (b *Binary) MarkHealthy() {
	b.healthy.Store(true)
}

// Healthy implements the [Monitor] interface.
func (b *Binary) Healthy(ctx context.Context) (bool, error) {
	return b.healthy.Load(), nil
}

// AndMonitor is a collection of [Monitor]s which follows
// the logical AND (&&) semantics for determining its own healthy/unhealthy state.
//
// It checks its monitors in order and fails fast: at the first one that is
// unhealthy or returns an error, it returns that monitor's result without
// checking the rest. An AndMonitor with no monitors is healthy.
type AndMonitor []Monitor

// And is a simple helper for initializing an [AndMonitor] in a more functional style.
func And(ms ...Monitor) AndMonitor {
	return AndMonitor(ms)
}

// Healthy implements the [Monitor] interface. It passes ctx to every monitor
// it checks.
func (am AndMonitor) Healthy(ctx context.Context) (bool, error) {
	for _, m := range am {
		healthy, err := m.Healthy(ctx)
		if !healthy || err != nil {
			return healthy, err
		}
	}
	return true, nil
}

// OrMonitor is a collection of [Monitor]s which follows
// the logical OR (||) semantics for determining its own healthy/unhealthy state.
//
// It is healthy at the first of its monitors, in order, that is healthy.
// Otherwise it is unhealthy, and the errors any of its monitors returned are
// collected and returned as a single joined error via [errors.Join]. An
// OrMonitor with no monitors is unhealthy.
type OrMonitor []Monitor

// Or is a simple helper for initializing an [OrMonitor] in a more functional style.
func Or(ms ...Monitor) OrMonitor {
	return OrMonitor(ms)
}

// Healthy implements the [Monitor] interface. It passes ctx to every monitor
// it checks.
func (om OrMonitor) Healthy(ctx context.Context) (bool, error) {
	errs := make([]error, 0, len(om))
	for _, m := range om {
		healthy, err := m.Healthy(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if healthy {
			return true, nil
		}
	}
	return false, errors.Join(errs...)
}
