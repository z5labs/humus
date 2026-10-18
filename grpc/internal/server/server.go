// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package server serves a *grpc.Server as a humus.Service.
//
// It is the code every gRPC server of the grpc module is served with: it
// listens through the Setup's Host, builds the *grpc.Server, serves it
// through the drain, and shuts it down with GracefulStop, then Stop once
// the grace period runs out. It knows nothing of TLS or of
// grpcserver.Settings: the transport credentials, if any, are among the
// options it is given. grpcserver.NewService builds on it with TLS
// required, and grpchealth.NewService builds its plaintext health server
// on it.
//
// Its outermost interceptors recover a panic of an RPC's handler, or of an
// interceptor inside them, and fail only that RPC, with codes.Internal and
// the message "internal error". The panic is recorded as an exception
// event on the RPC's span and as one ERROR record, in the RPC's context,
// through s.Telemetry's logger whose scope is the one New is given: the
// import path of the package building the server, so each package's panics
// are logged as its own.
//
// Inside the recovering stream interceptor, a second one gives every
// streaming RPC's handler a context of its own, derived from the RPC's,
// which Shutdown cancels when it starts. A stream whose handler then
// returns nil or a context error ends with codes.Unavailable and the
// message "server is shutting down". Unary RPCs are not cancelled.
//
// StripUnary and StripStream are the interceptors that strip the failure
// scenario an RPC carries from a caller the end user doesn't trust. New
// doesn't add them: a server that strips passes them in its options, so
// they run inside the recovery and the streams interceptor, and outside
// every interceptor of the end user's. grpcserver.NewService does;
// grpchealth's own server, which serves only health checks, doesn't.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/z5labs/humus"
)

// Server is a *grpc.Server, its listener, and the state that makes it a
// humus.Service. It is safe for concurrent use.
type Server struct {
	ln      net.Listener
	srv     *grpc.Server
	streams *streams

	// mu orders RegisterService against Serve, so a registration either
	// happens before Serve or is refused: grpc-go exits the process on one
	// that comes after. serving is set by the first Serve.
	mu      sync.Mutex
	serving bool

	// shutdown is set by the first Shutdown, and stopped is closed when it
	// returns.
	shutdown atomic.Bool
	stopped  chan struct{}
}

// New listens on addr with s.Host.Listen("tcp", addr), and builds the
// server with grpc.WaitForHandlers(true), a unary and a stream interceptor
// that recover a panic, a stream interceptor inside it that gives each
// stream's handler a context Shutdown cancels, and then opts. A failure to
// listen, such as EADDRINUSE, is its error. It registers a function with
// s.OnStop that closes the listener, so a listener whose Server never
// serves, because Init failed after New, is still closed.
//
// scope is the instrumentation scope of the logger a recovered panic is
// logged through, s.Telemetry.LoggerProvider.Logger(scope): the import path
// of the package building the server, such as
// "github.com/z5labs/humus/grpc/grpcserver".
func New(s *humus.Setup, scope, addr string, opts ...grpc.ServerOption) (*Server, error) {
	ln, err := s.Host.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s.OnStop(func(context.Context) error {
		// Serve and Shutdown close the listener, so it is usually closed
		// already.
		return closeListener(ln)
	})

	// WaitForHandlers comes first, so Stop waits for every handler to
	// return and no handler outlives Shutdown. An option in opts may
	// still replace it.
	//
	// The recovering interceptors come before opts, so they are the
	// outermost interceptors, and a panic of an interceptor in opts is
	// recovered too. A stats handler is not an interceptor, and stays
	// outside them, so the RPC's span records the Internal code.
	//
	// The interceptor that ends streams at Shutdown is inside the
	// recovery, so a panic of a handler it ran is still recovered, and
	// outside opts, so every interceptor of the end user's sees the
	// context Shutdown cancels.
	logger := s.Telemetry.LoggerProvider.Logger(scope)
	st := newStreams()
	all := make([]grpc.ServerOption, 0, 3+len(opts))
	all = append(all,
		grpc.WaitForHandlers(true),
		grpc.ChainUnaryInterceptor(recoverUnary(logger)),
		grpc.ChainStreamInterceptor(recoverStream(logger), st.interceptor()),
	)
	all = append(all, opts...)
	return &Server{ln: ln, srv: grpc.NewServer(all...), streams: st, stopped: make(chan struct{})}, nil
}

// closeListener closes ln. A listener that is already closed is not an
// error.
func closeListener(ln net.Listener) error {
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("closing the listener: %w", err)
	}
	return nil
}

// Addr returns the address the Server listens on, with the port the Host
// picked when addr has port 0.
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// RegisterService registers impl as desc's service. It returns an error,
// and registers nothing, for each of the three misuses that make
// (*grpc.Server).RegisterService exit the process: a call after Serve, a
// service that is already registered, and an impl that doesn't implement
// desc.HandlerType. Each error names the service.
func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl any) error {
	if impl != nil && desc.HandlerType != nil {
		ht := reflect.TypeOf(desc.HandlerType).Elem()
		if it := reflect.TypeOf(impl); !it.Implements(ht) {
			return fmt.Errorf("service %q: the implementation, of type %v, does not implement %v", desc.ServiceName, it, ht)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serving {
		return fmt.Errorf("service %q: registered after Serve", desc.ServiceName)
	}
	if _, ok := s.srv.GetServiceInfo()[desc.ServiceName]; ok {
		return fmt.Errorf("service %q: already registered", desc.ServiceName)
	}
	s.srv.RegisterService(desc, impl)
	return nil
}

// GetServiceInfo returns the registered services, as
// (*grpc.Server).GetServiceInfo does.
func (s *Server) GetServiceInfo() map[string]grpc.ServiceInfo {
	return s.srv.GetServiceInfo()
}

// Serve serves the listener until Shutdown. It keeps serving after ctx is
// done, so the Server answers RPCs through the App's drain. It returns nil
// once Shutdown has stopped the server, including a Shutdown called before
// Serve.
func (s *Server) Serve(context.Context) error {
	s.mu.Lock()
	s.serving = true
	s.mu.Unlock()

	err := s.srv.Serve(s.ln)
	if err == nil || errors.Is(err, grpc.ErrServerStopped) {
		// Serve returns ErrServerStopped when Shutdown came first, and nil
		// when Shutdown stopped it.
		return nil
	}
	return fmt.Errorf("serving: %w", err)
}

// Shutdown ends every streaming RPC: before anything else, it cancels the
// context of every stream's handler, and of every stream that starts
// after, and a stream whose handler then returns nil or a context error
// ends with codes.Unavailable. Unary RPCs are not cancelled.
//
// Then it stops the server gracefully, with GracefulStop: it stops
// accepting connections, sends every client a GOAWAY, and waits for the
// RPCs in flight. If ctx is done first, or is done already, it calls Stop,
// which closes every connection and cancels every RPC, waits for every
// handler to return, and returns an error that wraps ctx's error.
//
// A call after the first one has returned returns nil. A call made while
// the first is still running waits for it, and, if its own ctx is done
// first, calls Stop and returns an error that wraps ctx's error, as the
// first does.
func (s *Server) Shutdown(ctx context.Context) error {
	if !s.shutdown.CompareAndSwap(false, true) {
		return s.awaitShutdown(ctx)
	}
	defer close(s.stopped)
	s.streams.stop()
	// Before Serve, the *grpc.Server doesn't know the listener, so neither
	// GracefulStop nor Stop closes it.
	defer func() { _ = closeListener(s.ln) }()

	if ctx.Err() == nil {
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			s.srv.GracefulStop()
		}()
		select {
		case <-stopped:
			return nil
		case <-ctx.Done():
		}
		// Stop ends the GracefulStop still running, and waits for every
		// handler, so stopped is closed soon after.
		s.srv.Stop()
		<-stopped
	} else {
		s.srv.Stop()
	}
	return fmt.Errorf("shutting down: %w", ctx.Err())
}

// awaitShutdown waits for the first Shutdown to return. If ctx is done
// first, it calls Stop, which ends the graceful stop the first Shutdown is
// waiting on, and returns an error that wraps ctx's error once the first
// Shutdown has returned.
func (s *Server) awaitShutdown(ctx context.Context) error {
	select {
	case <-s.stopped:
		return nil
	default:
	}
	select {
	case <-s.stopped:
		return nil
	case <-ctx.Done():
	}
	s.srv.Stop()
	<-s.stopped
	return fmt.Errorf("shutting down: %w", ctx.Err())
}
