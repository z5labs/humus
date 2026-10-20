// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package latch_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/shoppingcart/internal/latch"
	"github.com/z5labs/humus/health"
)

var _ health.Monitor = (*latch.Monitor)(nil)

type check struct {
	mu    sync.Mutex
	err   error
	calls atomic.Int64
}

func (c *check) run(context.Context) error {
	c.calls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *check) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func TestMonitor_is_unhealthy_with_the_checks_error_until_it_first_succeeds(t *testing.T) {
	down := errors.New("the database is down")
	c := &check{err: down}
	m := latch.New(c.run)

	for range 3 {
		ok, err := m.Healthy(t.Context())
		require.ErrorIs(t, err, down)
		require.False(t, ok)
	}
	require.EqualValues(t, 3, c.calls.Load())

	c.fail(nil)
	ok, err := m.Healthy(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
}

func TestMonitor_stays_healthy_without_checking_again_once_it_has_succeeded(t *testing.T) {
	c := &check{}
	m := latch.New(c.run)

	ok, err := m.Healthy(t.Context())
	require.NoError(t, err)
	require.True(t, ok)

	c.fail(errors.New("the database is down"))
	for range 3 {
		ok, err := m.Healthy(t.Context())
		require.NoError(t, err)
		require.True(t, ok)
	}
	require.EqualValues(t, 1, c.calls.Load())
}

func TestMonitor_passes_the_callers_context_to_its_check(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "probe")
	var got any
	m := latch.New(func(ctx context.Context) error {
		got = ctx.Value(key{})
		return ctx.Err()
	})

	ok, err := m.Healthy(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "probe", got)
}

func TestMonitor_is_safe_for_concurrent_probes(t *testing.T) {
	c := &check{err: errors.New("the database is down")}
	m := latch.New(c.run)

	var wg sync.WaitGroup
	for i := range 16 {
		if i == 8 {
			c.fail(nil)
		}
		wg.Go(func() {
			_, _ = m.Healthy(t.Context())
		})
	}
	wg.Wait()

	ok, err := m.Healthy(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
}
