// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package health
package health

import (
	"context"
	"errors"
	"sync/atomic"
)

// Monitor
type Monitor interface {
	Healthy(context.Context) (bool, error)
}

// Binary
type Binary struct {
	healthy atomic.Bool
}

// MarkUnhealthy
func (b *Binary) MarkUnhealthy() {
	b.healthy.Swap(false)
}

// MarkHealthy
func (b *Binary) MarkHealthy() {
	b.healthy.Swap(true)
}

// Healthy implements the [Monitor] interface.
func (b *Binary) Healthy(ctx context.Context) (bool, error) {
	return b.healthy.Load(), nil
}

// AndMonitor
type AndMonitor []Monitor

// And
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

// OrMonitor
type OrMonitor []Monitor

// Or
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
