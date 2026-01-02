// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpc

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"

	"github.com/z5labs/humus/app"
	"github.com/z5labs/humus/config"

	"github.com/sourcegraph/conc/pool"
)

// Runtime represents a running gRPC application.
// It manages the lifecycle of the gRPC server and handles graceful shutdown.
type Runtime struct {
	ls  net.Listener
	api *Api
}

// Run starts the gRPC server and blocks until the context is cancelled or an error occurs.
// When the context is cancelled, the server performs a graceful shutdown.
// Returns nil if the server shuts down cleanly, or an error if the server fails to start or serve.
func (rt Runtime) Run(ctx context.Context) error {
	p := pool.New().WithContext(ctx)

	p.Go(func(ctx context.Context) error {
		return rt.api.server.Serve(rt.ls)
	})

	p.Go(func(ctx context.Context) error {
		<-ctx.Done()
		rt.api.server.GracefulStop()
		return nil
	})

	err := p.Wait()
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// Build creates an app.Builder for a gRPC application.
//
// The returned builder constructs a gRPC server that serves the provided API
// with OpenTelemetry instrumentation automatically applied via the Api's interceptors.
//
// Parameters:
//   - listener: Network listener configuration (address, TLS, etc.)
//   - api: The gRPC API to serve
//
// Example:
//
//	listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
//	    ln, err := net.Listen("tcp", ":9090")
//	    if err != nil {
//	        return config.Value[net.Listener]{}, err
//	    }
//	    return config.ValueOf(ln), nil
//	})
//	api := grpc.NewApi()
//	pb.RegisterMyServiceServer(api, implementation)
//	builder := grpc.Build(listener, api)
func Build(listener config.Reader[net.Listener], api *Api) app.Builder[Runtime] {
	return app.BuilderFunc[Runtime](func(ctx context.Context) (Runtime, error) {
		val, err := listener.Read(ctx)
		if err != nil {
			return Runtime{}, err
		}
		ln, ok := val.Value()
		if !ok {
			return Runtime{}, errors.New("listener not configured")
		}
		return Runtime{ls: ln, api: api}, nil
	})
}

// RunOptions holds configuration for [Run].
// These options control logging and other runtime behavior.
type RunOptions struct {
	logger *slog.Logger
}

// RunOption configures [Run] behavior.
// Use [LogHandler] to customize error logging.
type RunOption interface {
	ApplyRunOption(*RunOptions)
}

type runOptionFunc func(*RunOptions)

func (f runOptionFunc) ApplyRunOption(ro *RunOptions) {
	f(ro)
}

// LogHandler configures a custom log handler for errors during application startup and running.
// By default, errors are logged as JSON to stdout.
//
// Example:
//
//	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
//	grpc.Run(ctx, builder, grpc.LogHandler(handler))
func LogHandler(h slog.Handler) RunOption {
	return runOptionFunc(func(ro *RunOptions) {
		ro.logger = slog.New(h)
	})
}

// Run builds and runs a gRPC application using the provided builder.
//
// This function orchestrates the complete lifecycle of a gRPC application:
//  1. Builds the gRPC application using the provided builder
//  2. Runs the gRPC server
//  3. Logs any errors that occur
//
// The context should typically be context.Background(). Signal handling
// is performed by app.Run, which will cancel the context on SIGINT, SIGKILL,
// or SIGTERM.
//
// Example:
//
//	listener := config.ReaderFunc[net.Listener](func(ctx context.Context) (config.Value[net.Listener], error) {
//	    ln, err := net.Listen("tcp", ":9090")
//	    if err != nil {
//	        return config.Value[net.Listener]{}, err
//	    }
//	    return config.ValueOf(ln), nil
//	})
//	api := grpc.NewApi()
//	pb.RegisterMyServiceServer(api, implementation)
//	builder := grpc.Build(listener, api)
//	grpc.Run(context.Background(), builder)
func Run(ctx context.Context, builder app.Builder[Runtime], opts ...RunOption) error {
	ro := &RunOptions{
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{})),
	}
	for _, opt := range opts {
		opt.ApplyRunOption(ro)
	}

	err := app.Run(ctx, builder)
	if err != nil {
		app.LogError(ro.logger.Handler(), err)
	}
	return err
}
