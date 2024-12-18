// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"github.com/z5labs/humus/noop"
	"golang.org/x/sync/errgroup"
)

type AppOptions struct {
	errorLogHandler slog.Handler
}

type AppOption interface {
	ApplyAppOption(*AppOptions)
}

type appOptionFunc func(*AppOptions)

func (f appOptionFunc) ApplyAppOption(ao *AppOptions) {
	f(ao)
}

func ErrorLog(h slog.Handler) AppOption {
	return appOptionFunc(func(ao *AppOptions) {
		ao.errorLogHandler = h
	})
}

// App
type App struct {
	ls     net.Listener
	server *http.Server
}

// NewApp initializes a [App].
func NewApp(ls net.Listener, h http.Handler, opts ...AppOption) *App {
	ao := &AppOptions{
		errorLogHandler: noop.LogHandler{},
	}
	for _, opt := range opts {
		opt.ApplyAppOption(ao)
	}

	return &App{
		ls: ls,
		server: &http.Server{
			Handler:  h,
			ErrorLog: slog.NewLogLogger(ao.errorLogHandler, slog.LevelError),
		},
	}
}

// Run implements the [bedrock.App] interface.
func (a *App) Run(ctx context.Context) error {
	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		return a.server.Serve(a.ls)
	})
	eg.Go(func() error {
		<-egCtx.Done()

		return a.server.Shutdown(context.Background())
	})

	err := eg.Wait()
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
