// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"net"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/internal/server"
)

// Service is a humus.Service that serves gRPC over TLS. It is a
// grpc.ServiceRegistrar, so generated code registers services on it:
//
//	cartpb.RegisterCartServiceServer(svc, carts)
type Service struct {
	srv *server.Server
}

// The Service is a grpc.ServiceRegistrar, and a humus.Service.
var (
	_ grpc.ServiceRegistrar = (*Service)(nil)
	_ humus.Service         = (*Service)(nil)
)

// NewService runs CheckAddr and CheckTLS on settings, reporting every bad
// field in one error that names each one, listens on settings.Addr through
// s.Host, and builds the server.
//
// The first of humus's options is a stats handler, otelgrpc's, that traces
// and measures every RPC with s.Telemetry's TracerProvider, MeterProvider
// and Propagator. A grpc.StatsHandler in opts is added after it.
//
// humus's options also add a unary and a stream interceptor, the
// outermost ones, that recover a panic of an RPC and fail only that RPC,
// with codes.Internal, as the package doc describes under Panics. Inside
// them, a second stream interceptor gives each stream's handler a context
// that Shutdown cancels, as the package doc describes under Streams at
// shutdown.
//
// opts come after humus's own options, and humus's transport credentials
// come after opts. An option that grpc-go keeps one value of, such as
// grpc.MaxRecvMsgSize, replaces humus's. An option that chains, such as
// grpc.ChainUnaryInterceptor, is added inside humus's. grpc.Creds in opts
// has no effect, so every connection is served with settings.TLS.
//
// The listener is closed by Shutdown, or by an OnStop function NewService
// registers with s, if the App never serves.
func NewService(s *humus.Setup, settings Settings, opts ...grpc.ServerOption) (*Service, error) {
	if err := checkSettings(settings); err != nil {
		return nil, fmt.Errorf("grpcserver: NewService: %w", err)
	}

	// The stats handler comes first, so it is the first one grpc-go calls,
	// and a stats handler in opts is a second one. Every provider and the
	// propagator are passed, so otelgrpc never falls back to
	// OpenTelemetry's globals.
	//
	// The credentials come last, so grpc-go keeps them over any grpc.Creds
	// in opts. credentials.NewTLS clones the config, so the end user's is
	// never changed.
	all := make([]grpc.ServerOption, 0, 1+len(opts)+1)
	all = append(all, grpc.StatsHandler(otelgrpc.NewServerHandler(
		otelgrpc.WithTracerProvider(s.Telemetry.TracerProvider),
		otelgrpc.WithMeterProvider(s.Telemetry.MeterProvider),
		otelgrpc.WithPropagators(s.Telemetry.Propagator),
	)))
	all = append(all, opts...)
	all = append(all, grpc.Creds(credentials.NewTLS(settings.TLS)))

	srv, err := server.New(s, settings.Addr, all...)
	if err != nil {
		return nil, fmt.Errorf("grpcserver: NewService: %w", err)
	}
	return &Service{srv: srv}, nil
}

// checkSettings runs every check on settings, and reports every failure in
// one error, each naming its field.
func checkSettings(settings Settings) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("Settings.Addr", CheckAddr(settings.Addr))
	check("Settings.TLS", CheckTLS(settings.TLS))
	return errors.Join(errs...)
}

// Addr returns the address the Service listens on, with the port the Host
// picked when settings.Addr has port 0.
func (svc *Service) Addr() net.Addr {
	return svc.srv.Addr()
}

// RegisterService implements grpc.ServiceRegistrar. It panics, instead of
// exiting the process as grpc-go does, if it is called after Serve, if
// desc's service is already registered, or if impl doesn't implement
// desc.HandlerType.
func (svc *Service) RegisterService(desc *grpc.ServiceDesc, impl any) {
	if err := svc.srv.RegisterService(desc, impl); err != nil {
		panic(fmt.Errorf("grpcserver: RegisterService: %w", err))
	}
}

// GetServiceInfo returns the registered services, like
// (*grpc.Server).GetServiceInfo, so a Service can be given to
// reflection.Register.
func (svc *Service) GetServiceInfo() map[string]grpc.ServiceInfo {
	return svc.srv.GetServiceInfo()
}

// Serve serves gRPC until Shutdown. It keeps serving after ctx is done,
// through the App's drain delay.
func (svc *Service) Serve(ctx context.Context) error {
	if err := svc.srv.Serve(ctx); err != nil {
		return fmt.Errorf("grpcserver: %w", err)
	}
	return nil
}

// Shutdown ends every streaming RPC: it cancels each stream handler's
// context, and a stream whose handler then returns nil or a context error
// ends with codes.Unavailable. It stops the server gracefully: it stops
// accepting connections, sends every client a GOAWAY, and waits for the
// RPCs in flight. If ctx is done first, it closes every connection,
// cancels every RPC, waits for every handler to return, and returns an
// error that wraps ctx's error.
func (svc *Service) Shutdown(ctx context.Context) error {
	if err := svc.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("grpcserver: %w", err)
	}
	return nil
}
