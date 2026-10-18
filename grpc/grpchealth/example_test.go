// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpchealth_test

import (
	"context"
	"crypto/tls"
	"io"

	"github.com/z5labs/bedrock/config"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/health"
)

// TestInit_answers_the_probes_on_the_Service_port runs Init.

// Init is the end user's Init for a gRPC service on a cluster whose grpc
// probes can use TLS: it registers the health server on its own
// grpcserver.Service, next to its own services, and returns the health
// Service whose Probes the server answers from next to it.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) grpcserver.Settings {
		return grpcserver.Settings{
			Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
			// readFile and serverTLS are the end user's own helpers. The
			// config requires no client certificate, so the kubelet's
			// probes reach the Service.
			TLS: r.Get(env("GRPC_TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(grpcserver.CheckTLS)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	svc, err := grpcserver.NewService(s, settings)
	if err != nil {
		return nil, err
	}
	testpb.RegisterTestServiceServer(svc, greeter{}) // the end user's own service
	hs := health.NewService()
	healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
	return []humus.Service{svc, hs}, nil
}

// greeter is the end user's service: grpc-go's interop/grpc_testing
// TestService, whose UnaryCall answers with the username "greeter".
type greeter struct {
	testpb.UnimplementedTestServiceServer
}

func (greeter) UnaryCall(context.Context, *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	return &testpb.SimpleResponse{Username: "greeter"}, nil
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

// serverTLS returns a server's TLS config for a PEM file that holds its
// certificate chain and its private key.
func serverTLS(pem []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// A gRPC service answers gRPC health v1 on its own port by registering
// NewServer's server on its grpcserver.Service, next to its own services,
// as Init does. Its probes are grpc probes with service: liveness or
// readiness, over TLS. This example has no Output comment, so it is
// compiled but never run: running it would exit the test binary.
func ExampleNewServer() {
	grpcserver.Main(Init)
}

// TestProbesInit_answers_the_probes_on_a_plaintext_port runs ProbesInit.

// ProbesInit is the end user's Init for a gRPC service on a cluster whose
// grpc probes can't use TLS: it serves its own services on a
// grpcserver.Service, over TLS, and the probes on a plaintext port of their
// own. It returns the two Services, and no health Service: the probes
// Service owns readiness.
func ProbesInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	type settings struct {
		grpc   grpcserver.Settings
		probes grpchealth.Settings
	}
	cfg, err := config.Struct(func(r *config.Resolver) settings {
		return settings{
			grpc: grpcserver.Settings{
				Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
				TLS:  r.Get(env("GRPC_TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(grpcserver.CheckTLS)),
			},
			probes: grpchealth.Settings{
				// No .Default: the field is required.
				Addr: r.Get(env("PROBES_ADDR").Validate(grpchealth.CheckAddr)),
			},
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	svc, err := grpcserver.NewService(s, cfg.grpc)
	if err != nil {
		return nil, err
	}
	testpb.RegisterTestServiceServer(svc, greeter{}) // the end user's own service
	probes, err := grpchealth.NewService(s, cfg.probes)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, probes}, nil
}

// A gRPC service on a cluster whose grpc probes can't use TLS, which is
// most clusters today, builds the probes Service in its Init, next to its
// grpcserver.Service, as ProbesInit does. Its probes are grpc probes with
// service: liveness or readiness, on the probes port, in plain text. This
// example has no Output comment, so it is compiled but never run: running
// it would exit the test binary.
func ExampleNewService() {
	grpcserver.Main(ProbesInit)
}
