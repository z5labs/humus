// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package latch is a health.Monitor that is unhealthy until its check first succeeds, then healthy for good.
package latch

import (
	"context"
	"sync/atomic"
)

type Monitor struct {
	check   func(context.Context) error
	turn    chan struct{}
	reached atomic.Bool
}

func New(check func(ctx context.Context) error) *Monitor {
	return &Monitor{check: check, turn: make(chan struct{}, 1)}
}

func (m *Monitor) Healthy(ctx context.Context) (bool, error) {
	if m.reached.Load() {
		return true, nil
	}
	select {
	case m.turn <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-m.turn }()
	if m.reached.Load() {
		return true, nil
	}
	if err := m.check(ctx); err != nil {
		return false, err
	}
	m.reached.Store(true)
	return true, nil
}
