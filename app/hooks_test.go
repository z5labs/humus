// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// mockRuntime is a test helper that implements the Runtime interface.
type mockRuntime struct {
	runCalled bool
	runErr    error
	runFunc   func(context.Context) error
}

func (m *mockRuntime) Run(ctx context.Context) error {
	m.runCalled = true
	if m.runFunc != nil {
		return m.runFunc(ctx)
	}
	return m.runErr
}

func TestWithHooks_hooksRunInOrder(t *testing.T) {
	var order []int
	builder := WithHooks(func(ctx context.Context, h *HookRegistry) (Runtime, error) {
		h.OnPostRun(func(ctx context.Context) error {
			order = append(order, 1)
			return nil
		})
		h.OnPostRun(func(ctx context.Context) error {
			order = append(order, 2)
			return nil
		})
		h.OnPostRun(func(ctx context.Context) error {
			order = append(order, 3)
			return nil
		})
		return &mockRuntime{}, nil
	})

	rt, err := builder.Build(context.Background())
	require.NoError(t, err)

	err = rt.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int{1, 2, 3}, order)
}

func TestWithHooks_hooksRunAfterRuntime(t *testing.T) {
	var order []string
	mock := &mockRuntime{
		runFunc: func(ctx context.Context) error {
			order = append(order, "runtime")
			return nil
		},
	}

	builder := WithHooks(func(ctx context.Context, h *HookRegistry) (Runtime, error) {
		h.OnPostRun(func(ctx context.Context) error {
			order = append(order, "hook")
			return nil
		})
		return mock, nil
	})

	rt, err := builder.Build(context.Background())
	require.NoError(t, err)

	err = rt.Run(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"runtime", "hook"}, order)
}

func TestWithHooks_runtimeErrorDoesNotStopHooks(t *testing.T) {
	hookCalled := false
	mock := &mockRuntime{
		runErr: errors.New("runtime error"),
	}

	builder := WithHooks(func(ctx context.Context, h *HookRegistry) (Runtime, error) {
		h.OnPostRun(func(ctx context.Context) error {
			hookCalled = true
			return nil
		})
		return mock, nil
	})

	rt, err := builder.Build(context.Background())
	require.NoError(t, err)

	err = rt.Run(context.Background())
	require.Error(t, err)
	require.True(t, hookCalled)
}

func TestWithHooks_hookErrorDoesNotStopOtherHooks(t *testing.T) {
	var called []int

	builder := WithHooks(func(ctx context.Context, h *HookRegistry) (Runtime, error) {
		h.OnPostRun(func(ctx context.Context) error {
			called = append(called, 1)
			return errors.New("hook 1 error")
		})
		h.OnPostRun(func(ctx context.Context) error {
			called = append(called, 2)
			return nil
		})
		return &mockRuntime{}, nil
	})

	rt, err := builder.Build(context.Background())
	require.NoError(t, err)

	err = rt.Run(context.Background())
	require.Error(t, err)
	require.Equal(t, []int{1, 2}, called)
}

func TestWithHooks_collectsAllErrors(t *testing.T) {
	runtimeErr := errors.New("runtime error")
	hook1Err := errors.New("hook 1 error")
	hook2Err := errors.New("hook 2 error")

	mock := &mockRuntime{runErr: runtimeErr}

	builder := WithHooks(func(ctx context.Context, h *HookRegistry) (Runtime, error) {
		h.OnPostRun(func(ctx context.Context) error {
			return hook1Err
		})
		h.OnPostRun(func(ctx context.Context) error {
			return nil // This one succeeds
		})
		h.OnPostRun(func(ctx context.Context) error {
			return hook2Err
		})
		return mock, nil
	})

	rt, err := builder.Build(context.Background())
	require.NoError(t, err)

	err = rt.Run(context.Background())
	require.Error(t, err)

	// Verify all errors are present
	require.ErrorIs(t, err, runtimeErr)
	require.ErrorIs(t, err, hook1Err)
	require.ErrorIs(t, err, hook2Err)
}

func TestWithHooks_builderErrorPreventsRuntimeCreation(t *testing.T) {
	expectedErr := errors.New("build error")

	builder := WithHooks(func(ctx context.Context, h *HookRegistry) (Runtime, error) {
		h.OnPostRun(func(ctx context.Context) error {
			return nil
		})
		return nil, expectedErr
	})

	_, err := builder.Build(context.Background())
	require.Error(t, err)
	require.Equal(t, expectedErr, err)
}

func TestWithHooks_noHooksRegistered(t *testing.T) {
	mock := &mockRuntime{}

	builder := WithHooks(func(ctx context.Context, h *HookRegistry) (Runtime, error) {
		// No hooks registered
		return mock, nil
	})

	rt, err := builder.Build(context.Background())
	require.NoError(t, err)

	err = rt.Run(context.Background())
	require.NoError(t, err)
	require.True(t, mock.runCalled)
}

func TestWithHooks_contextPropagatedToHooks(t *testing.T) {
	type ctxKey string
	key := ctxKey("test")
	value := "test-value"

	var hookCtxValue string

	mock := &mockRuntime{}
	builder := WithHooks(func(ctx context.Context, h *HookRegistry) (Runtime, error) {
		h.OnPostRun(func(ctx context.Context) error {
			hookCtxValue = ctx.Value(key).(string)
			return nil
		})
		return mock, nil
	})

	rt, err := builder.Build(context.Background())
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), key, value)
	err = rt.Run(ctx)
	require.NoError(t, err)
	require.Equal(t, value, hookCtxValue)
}
