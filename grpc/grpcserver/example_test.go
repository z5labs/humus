// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"context"
	"crypto/tls"
	"io"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/telemetry"
)

// The tests in main_test.go run Init.

// Init is the end user's Init: it sources the Settings, builds the Service
// with NewService, registers the end user's service on it and returns it,
// with any other Service the process runs.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) grpcserver.Settings {
		return grpcserver.Settings{
			// No .Default: both fields are required, so an unset variable fails as not found.
			Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
			// readFile and serverTLS are the end user's own helpers. The
			// bundle's password is read only once the bundle has been.
			TLS: r.Get(env("GRPC_TLS_P12_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(bundle []byte) config.Value[*tls.Config] {
				return env("GRPC_TLS_P12_PASSWORD").Parse(serverTLS(bundle))
			}).Validate(grpcserver.CheckTLS)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	svc, err := grpcserver.NewService(s, settings)
	if err != nil {
		return nil, err
	}
	testpb.RegisterTestServiceServer(svc, greeter{})
	return []humus.Service{svc}, nil
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

// serverTLS returns a parser for config.Value.Parse that decodes bundle, a
// PKCS#12 bundle, with the password it is given, into a server's TLS
// config.
func serverTLS(bundle []byte) func(password string) (*tls.Config, error) {
	return func(password string) (*tls.Config, error) {
		key, leaf, chain, err := pkcs12.DecodeChain(bundle, password)
		if err != nil {
			return nil, err
		}
		cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
		for _, c := range chain {
			cert.Certificate = append(cert.Certificate, c.Raw)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
	}
}

// buildTelemetry is the service's telemetry.Builder, which names the
// service.
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "greeter"
	return telemetry.New(ctx, s, dial, p.Stdout)
}

// A gRPC service's main is one call to grpcserver.Main, with an Init that
// returns the Service. This example has no Output comment, so it is compiled
// but never run: running it would exit the test binary.
func ExampleMain() {
	grpcserver.Main(Init, humus.WithTelemetry(buildTelemetry))
}
