// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/telemetry"
)

// TestInit_calls_carts_for_each_RPC runs Init.

// Init is the end user's Init: it sources the carts dependency's Settings,
// builds its Client with NewClient, and hands the Client to generated code,
// in the end user's service, which it serves with grpcserver.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := grpcclient.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	cartsSettings, err := config.Struct(func(r *config.Resolver) grpcclient.Settings {
		c := d
		// No .Default: Addr is required, and carts serves TLS, so
		// CARTS_CA_FILE is too.
		c.Addr = r.Get(env("CARTS_ADDR").Validate(grpcclient.CheckAddr))
		c.TLS = r.Get(env("CARTS_CA_FILE").Parse(readFile(s.Host.FS)).Parse(clientTLS).Validate(grpcclient.CheckTLS))
		c.Timeout = r.Get(env("CARTS_TIMEOUT").Parse(time.ParseDuration).Default(d.Timeout).Validate(grpcclient.CheckTimeout))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}
	serverSettings, err := config.Struct(func(r *config.Resolver) grpcserver.Settings {
		return grpcserver.Settings{
			Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
			TLS:  r.Get(env("GRPC_TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(grpcserver.CheckTLS)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err
	}

	// One Client for the dependency, built once and shared by every RPC
	// the service serves.
	carts, err := grpcclient.NewClient(s, cartsSettings)
	if err != nil {
		return nil, err
	}

	api := &api{carts: testpb.NewTestServiceClient(carts)}
	svc, err := grpcserver.NewService(s, serverSettings)
	if err != nil {
		return nil, err
	}
	testpb.RegisterTestServiceServer(svc, api)
	return []humus.Service{svc}, nil
}

// api is the end user's service: grpc-go's interop/grpc_testing
// TestService, whose UnaryCall asks carts, the dependency, for its username,
// and answers with it.
type api struct {
	testpb.UnimplementedTestServiceServer

	carts testpb.TestServiceClient
}

func (a *api) UnaryCall(ctx context.Context, _ *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	// The call to carts ends with the inbound RPC's context.
	resp, err := a.carts.UnaryCall(ctx, &testpb.SimpleRequest{FillUsername: true})
	if err != nil {
		return nil, err
	}
	return &testpb.SimpleResponse{Username: "orders, via " + resp.GetUsername()}, nil
}

// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

// clientTLS returns a client's TLS config that trusts only the CA
// certificates in pem.
func clientTLS(pem []byte) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("holds no PEM certificate")
	}
	return &tls.Config{RootCAs: roots}, nil
}

// serverTLS returns a server's TLS config for a PEM file that holds its
// certificate chain and its private key.
func serverTLS(pem []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// buildTelemetry is the service's telemetry.Builder, which names the
// service.
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "orders"
	return telemetry.New(ctx, s, dial, p.Stdout)
}

// A service that calls a gRPC dependency builds the dependency's Client in
// its Init, and hands it to generated code, as Init does. This example has
// no Output comment, so it is compiled but never run: running it would exit
// the test binary.
func ExampleNewClient() {
	grpcserver.Main(Init, humus.WithTelemetry(buildTelemetry))
}
