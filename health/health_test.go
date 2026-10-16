// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package health_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/health"
)

// result is what a monitor in the tables below reports.
type result struct {
	healthy bool
	err     error
}

// monitors returns a monitor reporting each result, and the record of which
// of them were checked, by index, in order.
func monitors(results ...result) ([]health.Monitor, *[]int) {
	var checked []int
	ms := make([]health.Monitor, len(results))
	for i, r := range results {
		ms[i] = health.MonitorFunc(func(ctx context.Context) (bool, error) {
			checked = append(checked, i)
			return r.healthy, r.err
		})
	}
	return ms, &checked
}

var (
	errFirst  = errors.New("first failed to check health status")
	errMiddle = errors.New("middle failed to check health status")
	errLast   = errors.New("last failed to check health status")
)

var (
	healthy   = result{healthy: true}
	unhealthy = result{}
)

func TestAndMonitor_Healthy(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		results     []result
		wantHealthy bool
		wantErr     error
		wantChecked []int
	}{
		{
			name:        "is healthy with no monitors",
			wantHealthy: true,
		},
		{
			name:        "is healthy if every monitor is healthy",
			results:     []result{healthy, healthy, healthy},
			wantHealthy: true,
			wantChecked: []int{0, 1, 2},
		},
		{
			name:        "is unhealthy at the first monitor that is unhealthy",
			results:     []result{healthy, unhealthy, healthy},
			wantChecked: []int{0, 1},
		},
		{
			name:        "returns the error of the first monitor",
			results:     []result{{err: errFirst}, healthy, healthy},
			wantErr:     errFirst,
			wantChecked: []int{0},
		},
		{
			name:        "returns the error of a monitor in the middle",
			results:     []result{healthy, {err: errMiddle}, {err: errLast}},
			wantErr:     errMiddle,
			wantChecked: []int{0, 1},
		},
		{
			name:        "returns the error of the last monitor",
			results:     []result{healthy, healthy, {err: errLast}},
			wantErr:     errLast,
			wantChecked: []int{0, 1, 2},
		},
		{
			name:        "returns the error of a monitor that also reports healthy",
			results:     []result{healthy, {healthy: true, err: errMiddle}, healthy},
			wantHealthy: true,
			wantErr:     errMiddle,
			wantChecked: []int{0, 1},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ms, checked := monitors(tc.results...)

			gotHealthy, err := health.And(ms...).Healthy(t.Context())

			require.Equal(t, tc.wantErr, err)
			require.Equal(t, tc.wantHealthy, gotHealthy)
			require.Equal(t, tc.wantChecked, *checked)
		})
	}

	t.Run("is unhealthy if one of its Binary monitors is", func(t *testing.T) {
		t.Parallel()

		var a, b, c health.Binary
		a.MarkHealthy()
		c.MarkHealthy()

		gotHealthy, err := health.And(&a, &b, &c).Healthy(t.Context())

		require.NoError(t, err)
		require.False(t, gotHealthy)
	})
}

func TestOrMonitor_Healthy(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		results     []result
		wantHealthy bool
		wantErrs    []error
		wantChecked []int
	}{
		{
			name: "is unhealthy with no monitors",
		},
		{
			name:        "is unhealthy if every monitor is unhealthy",
			results:     []result{unhealthy, unhealthy, unhealthy},
			wantChecked: []int{0, 1, 2},
		},
		{
			name:        "is healthy at the first monitor that is healthy",
			results:     []result{unhealthy, healthy, unhealthy},
			wantHealthy: true,
			wantChecked: []int{0, 1},
		},
		{
			name:        "is healthy after the first monitor returns an error",
			results:     []result{{err: errFirst}, healthy, unhealthy},
			wantHealthy: true,
			wantChecked: []int{0, 1},
		},
		{
			name:        "returns the error of the first monitor",
			results:     []result{{err: errFirst}, unhealthy, unhealthy},
			wantErrs:    []error{errFirst},
			wantChecked: []int{0, 1, 2},
		},
		{
			name:        "returns the error of a monitor in the middle",
			results:     []result{unhealthy, {err: errMiddle}, unhealthy},
			wantErrs:    []error{errMiddle},
			wantChecked: []int{0, 1, 2},
		},
		{
			name:        "returns the error of the last monitor",
			results:     []result{unhealthy, unhealthy, {err: errLast}},
			wantErrs:    []error{errLast},
			wantChecked: []int{0, 1, 2},
		},
		{
			name:        "returns every monitor's error, joined",
			results:     []result{{err: errFirst}, {err: errMiddle}, {healthy: true, err: errLast}},
			wantErrs:    []error{errFirst, errMiddle, errLast},
			wantChecked: []int{0, 1, 2},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ms, checked := monitors(tc.results...)

			gotHealthy, err := health.Or(ms...).Healthy(t.Context())

			if tc.wantErrs == nil {
				require.NoError(t, err)
			} else {
				require.Equal(t, errors.Join(tc.wantErrs...), err)
			}
			require.Equal(t, tc.wantHealthy, gotHealthy)
			require.Equal(t, tc.wantChecked, *checked)
		})
	}

	t.Run("is unhealthy if all of its Binary monitors are", func(t *testing.T) {
		t.Parallel()

		var a, b, c health.Binary

		gotHealthy, err := health.Or(&a, &b, &c).Healthy(t.Context())

		require.NoError(t, err)
		require.False(t, gotHealthy)
	})
}

func TestBinary(t *testing.T) {
	t.Parallel()

	t.Run("is unhealthy as its zero value", func(t *testing.T) {
		t.Parallel()

		var b health.Binary

		gotHealthy, err := b.Healthy(t.Context())

		require.NoError(t, err)
		require.False(t, gotHealthy)
	})

	t.Run("is safe to mark and check from many goroutines", func(t *testing.T) {
		t.Parallel()

		var b health.Binary
		var wg sync.WaitGroup
		for i := range 64 {
			wg.Go(func() {
				for range 100 {
					if i%2 == 0 {
						b.MarkHealthy()
					} else {
						b.MarkUnhealthy()
					}
					_, err := b.Healthy(t.Context())
					if err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		wg.Wait()

		b.MarkHealthy()
		gotHealthy, err := b.Healthy(t.Context())
		require.NoError(t, err)
		require.True(t, gotHealthy)
	})
}

type ctxKey struct{}

func TestMonitorFunc_Healthy(t *testing.T) {
	t.Parallel()

	t.Run("calls the func with the ctx it is given", func(t *testing.T) {
		t.Parallel()

		ctx := context.WithValue(t.Context(), ctxKey{}, "given")
		var got context.Context
		m := health.MonitorFunc(func(ctx context.Context) (bool, error) {
			got = ctx
			return true, nil
		})

		gotHealthy, err := m.Healthy(ctx)

		require.NoError(t, err)
		require.True(t, gotHealthy)
		require.Same(t, ctx, got)
	})

	t.Run("returns the func's result", func(t *testing.T) {
		t.Parallel()

		m := health.MonitorFunc(func(ctx context.Context) (bool, error) {
			return false, errFirst
		})

		gotHealthy, err := m.Healthy(t.Context())

		require.ErrorIs(t, err, errFirst)
		require.False(t, gotHealthy)
	})
}

// TestCombinators_pass_on_ctx checks that And and Or hand the ctx they are
// given to every monitor they check, nested ones included, so a caller's
// deadline reaches each check.
func TestCombinators_pass_on_ctx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "given"))
	defer cancel()

	var got []context.Context
	record := func(healthy bool) health.Monitor {
		return health.MonitorFunc(func(ctx context.Context) (bool, error) {
			got = append(got, ctx)
			return healthy, nil
		})
	}

	m := health.And(
		record(true),
		health.Or(record(false), record(true)),
		health.And(record(true)),
	)

	gotHealthy, err := m.Healthy(ctx)

	require.NoError(t, err)
	require.True(t, gotHealthy)
	require.Len(t, got, 4)
	for i, c := range got {
		require.Same(t, ctx, c, "monitor %d was checked with a different ctx", i)
	}
}

func TestProbes(t *testing.T) {
	t.Parallel()

	var started health.Binary
	probes := health.Probes{
		Liveness:  health.And(),
		Readiness: health.And(&started),
	}

	live, err := probes.Liveness.Healthy(t.Context())
	require.NoError(t, err)
	require.True(t, live)

	ready, err := probes.Readiness.Healthy(t.Context())
	require.NoError(t, err)
	require.False(t, ready)

	started.MarkHealthy()

	ready, err = probes.Readiness.Healthy(t.Context())
	require.NoError(t, err)
	require.True(t, ready)
}
