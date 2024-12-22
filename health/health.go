// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package health provides utilities for monitoring the healthiness of an application.
package health

import (
	"context"
	"errors"
	"sync/atomic"
)

// Monitor represents anything which can report its current state of health.
type Monitor interface {
	Healthy(context.Context) (bool, error)
}

// Binary is a [Monitor] which simply has 2 states: healthy or unhealthy.
// It is safe for concurrent use. The zero value represents an unhealthy state.
type Binary struct {
	healthy atomic.Bool
}

// MarkUnhealthy changes the state to unhealthy.
func (b *Binary) MarkUnhealthy() {
	b.healthy.Swap(false)
}

// MarkHealthy changes that state to healthy.
func (b *Binary) MarkHealthy() {
	b.healthy.Swap(true)
}

// Healthy implements the [Monitor] interface.
func (b *Binary) Healthy(ctx context.Context) (bool, error) {
	return b.healthy.Load(), nil
}

// AndMonitor is a collection of [Monitor]s which follows
// the logical AND (&&) semantics for determining its own healthy/unhealthy state.
//
// In the case of an error it will fail fast.
type AndMonitor []Monitor

// And is a simple helper for initializing a [AndMonitor] in a more functional style.
func And(ms ...Monitor) AndMonitor {
	return AndMonitor(ms)
}

// Healthy implements the [Monitor] interface.
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
// It will check all [Monitor]s and if any errors are encountered they will be collected
// and returned as a single joined error via [errors.Join].
type OrMonitor []Monitor

// Or is a simple helper for initializing a [OrMonitor] in a more functional style.
func Or(ms ...Monitor) OrMonitor {
	return OrMonitor(ms)
}

// Healthy implements the [Monitor] interface.
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
