// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver

import (
	"context"
	"errors"
	"net"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

type App struct {
	ls     net.Listener
	server *grpc.Server
}

func NewApp(ls net.Listener, s *grpc.Server) *App {
	return &App{
		ls:     ls,
		server: s,
	}
}

func (a *App) Run(ctx context.Context) error {
	eg, egCtx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		return a.server.Serve(a.ls)
	})
	eg.Go(func() error {
		<-egCtx.Done()

		a.server.GracefulStop()
		return nil
	})

	err := eg.Wait()
	if err == nil || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}
