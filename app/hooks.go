// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"errors"
)

// HookFunc is a function that runs after the inner runtime completes.
// Hooks receive the context from the application lifecycle and return an error if they fail.
// All hooks will be executed even if previous hooks fail; errors are collected and joined.
type HookFunc func(context.Context) error

// HookRegistry collects post-run hooks during application initialization.
// Hooks are executed in the order they are registered.
type HookRegistry struct {
	hooks []HookFunc
}

// OnPostRun registers a hook to be executed after the inner runtime completes.
// Hooks are executed in the order they are registered.
// All hooks will run even if the runtime or previous hooks fail.
func (r *HookRegistry) OnPostRun(hook HookFunc) {
	r.hooks = append(r.hooks, hook)
}

// hookRuntime wraps an inner runtime and executes registered hooks after it completes.
// All hooks are executed even if the inner runtime or previous hooks fail.
// Errors from the runtime and hooks are collected and joined.
type hookRuntime struct {
	inner Runtime
	hooks []HookFunc
}

// Run executes the inner runtime and then runs all registered hooks.
// All hooks are executed even if the inner runtime or previous hooks fail.
// Returns a joined error containing all errors that occurred.
func (rt hookRuntime) Run(ctx context.Context) error {
	runtimeErr := rt.inner.Run(ctx)

	var hookErrors error
	for _, hook := range rt.hooks {
		if err := hook(ctx); err != nil {
			hookErrors = errors.Join(hookErrors, err)
		}
	}

	return errors.Join(runtimeErr, hookErrors)
}

// WithHooks wraps a builder function with post-run hook support.
// The provided function receives a context and HookRegistry, allowing it to register
// cleanup hooks during initialization. After the inner runtime completes, all registered
// hooks are executed in the order they were registered.
//
// All hooks will execute even if the inner runtime or previous hooks fail.
// Errors from the runtime and all hooks are collected and joined.
//
// Example usage:
//
//	builder := app.WithHooks(func(ctx context.Context, h *app.HookRegistry) (httpserver.App, error) {
//	    db, err := openDatabase(ctx)
//	    if err != nil {
//	        return nil, err
//	    }
//	    h.OnPostRun(func(ctx context.Context) error {
//	        return db.Close()
//	    })
//	    return buildApp(ctx, db)
//	})
func WithHooks[T Runtime](f func(context.Context, *HookRegistry) (T, error)) Builder[hookRuntime] {
	return BuilderFunc[hookRuntime](func(ctx context.Context) (hookRuntime, error) {
		registry := &HookRegistry{}

		inner, err := f(ctx, registry)
		if err != nil {
			return hookRuntime{}, err
		}

		return hookRuntime{
			inner: inner,
			hooks: registry.hooks,
		}, nil
	})
}
