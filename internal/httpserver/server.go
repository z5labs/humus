// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"

	"golang.org/x/sync/errgroup"
)

// App
type App struct {
	ls     net.Listener
	server *http.Server
}

// NewApp initializes a [App].
func NewApp(ls net.Listener, s *http.Server) *App {
	return &App{
		ls:     ls,
		server: s,
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
