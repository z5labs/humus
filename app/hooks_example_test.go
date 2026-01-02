// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"context"
	"fmt"

	"github.com/z5labs/humus/app"
)

// mockDB simulates a database connection for example purposes.
type mockDB struct {
	closed bool
}

func (db *mockDB) Close() error {
	db.closed = true
	fmt.Println("Closing database connection")
	return nil
}

// DatabaseApp represents an application that uses a database connection.
type DatabaseApp struct {
	db *mockDB
}

func (a DatabaseApp) Run(ctx context.Context) error {
	fmt.Println("Application running with database")
	return nil
}

// Example demonstrates using WithHooks for database cleanup.
func Example_withHooks() {
	builder := app.WithHooks(func(ctx context.Context, h *app.HookRegistry) (app.Runtime, error) {
		// Open database connection
		db := &mockDB{}

		// Register cleanup hook right next to resource creation
		h.OnPostRun(func(ctx context.Context) error {
			return db.Close()
		})

		// Return application
		return DatabaseApp{db: db}, nil
	})

	// Build and run the application
	_ = app.Run(context.Background(), builder)

	// Output:
	// Application running with database
	// Closing database connection
}

// Example_withHooksMultiple demonstrates multiple hooks executing in order.
func Example_withHooksMultiple() {
	builder := app.WithHooks(func(ctx context.Context, h *app.HookRegistry) (app.Runtime, error) {
		// Register hooks in order
		h.OnPostRun(func(ctx context.Context) error {
			fmt.Println("Cleanup step 1")
			return nil
		})

		h.OnPostRun(func(ctx context.Context) error {
			fmt.Println("Cleanup step 2")
			return nil
		})

		h.OnPostRun(func(ctx context.Context) error {
			fmt.Println("Cleanup step 3")
			return nil
		})

		return app.RuntimeFunc(func(ctx context.Context) error {
			fmt.Println("Application running")
			return nil
		}), nil
	})

	_ = app.Run(context.Background(), builder)

	// Output:
	// Application running
	// Cleanup step 1
	// Cleanup step 2
	// Cleanup step 3
}
