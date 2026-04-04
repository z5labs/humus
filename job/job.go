// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package job

import "context"

// Handler represents the core logic of your job.
type Handler interface {
	Handle(context.Context) error
}

// HandlerFunc is an adapter to allow the use of ordinary functions as [Handler]s.
type HandlerFunc func(context.Context) error

// Handle implements the [Handler] interface.
func (f HandlerFunc) Handle(ctx context.Context) error {
	return f(ctx)
}

// App wraps a [Handler] and implements the bedrock.Runtime interface.
type App struct {
	h Handler
}

// NewApp initializes a new [App].
func NewApp(h Handler) *App {
	return &App{h: h}
}

// Run implements the bedrock.Runtime interface.
func (a *App) Run(ctx context.Context) error {
	return a.h.Handle(ctx)
}
