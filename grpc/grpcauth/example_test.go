// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcauth_test

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"strings"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/grpc/grpcauth"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/health"
)

// Claims are the end user's principal: whatever their identity provider
// puts in a token.
type Claims struct {
	Subject string
	Scope   string // space-separated, as RFC 9068 has it
}

// HasScope reports whether the claims grant scope, so that RequireScopes
// can require it.
func (c Claims) HasScope(scope string) bool { return slices.Contains(strings.Fields(c.Scope), scope) }

// Tokens verify the end user's bearer tokens, such as an auth/jwt
// Verifier does. A token that authenticates no one is an error wrapping
// auth.ErrUnauthenticated; any other error is the server's.
type Tokens interface {
	Verify(ctx context.Context, token string) (Claims, error)
}

// greeterService is the end user's service: grpc-go's interop/grpc_testing
// TestService, whose UnaryCall greets the token's subject, and whose
// StreamingOutputCall changes something, so it needs a scope.
type greeterService struct {
	testpb.UnimplementedTestServiceServer

	bearer auth.Scheme[Claims]
}

func (g greeterService) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	claims, _ := g.bearer.From(ctx) // always there: the Default requires bearer
	return &testpb.SimpleResponse{Username: claims.Subject}, nil
}

// SecuredInit serves the greeter over TLS, with every method requiring a
// bearer token, StreamingOutputCall requiring the "greetings:write" scope
// too, and gRPC health on the same Service opened for the kubelet's probes.
func SecuredInit(tokens Tokens, tlsConfig *tls.Config) func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		bearer := auth.Bearer("bearer", tokens.Verify).Format("JWT")

		unary, stream, err := grpcauth.Interceptors(s,
			grpcauth.Default(auth.Require(bearer)),
			grpcauth.Method(testpb.TestService_StreamingOutputCall_FullMethodName, auth.RequireScopes(bearer, "greetings:write")),
			grpcauth.Method(healthpb.Health_Check_FullMethodName, auth.Disabled()), // the kubelet presents no credentials
			grpcauth.Method(healthpb.Health_Watch_FullMethodName, auth.Disabled()),
			grpcauth.Method(healthpb.Health_List_FullMethodName, auth.Disabled()),
		)
		if err != nil {
			return nil, err
		}
		svc, err := grpcserver.NewService(s, grpcserver.Settings{Addr: ":9090", TLS: tlsConfig}, unary, stream)
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, greeterService{bearer: bearer})
		hs := health.NewService()
		healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
		return []humus.Service{svc, hs}, nil
	}
}

// unconfigured is Tokens that verify nothing.
type unconfigured struct{}

func (unconfigured) Verify(context.Context, string) (Claims, error) {
	return Claims{}, errors.New("no identity provider is configured")
}

// A gRPC Service whose methods require a bearer token, with Health opened
// by name. This example has no Output comment, so it is compiled but never
// run: running it would exit the test binary.
func ExampleInterceptors() {
	grpcserver.Main(SecuredInit(unconfigured{}, &tls.Config{}))
}
