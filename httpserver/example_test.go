// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/telemetry"
)

// TestInit_serves_over_TLS_with_the_defaults runs Init.

// Init is the end user's Init: it sources the Settings, builds the Service
// with NewService and returns it, with any other Service the process runs.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		return httpserver.Settings{
			// No .Default: Addr and TLS are required, so an unset ADDR or
			// TLS_PEM_FILE fails as not found.
			Addr:              r.Get(env("ADDR").Validate(httpserver.CheckAddr)),
			TLS:               r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS)),
			HTTP2Only:         r.Get(env("HTTP2_ONLY").Parse(strconv.ParseBool).Default(d.HTTP2Only)),
			ReadHeaderTimeout: r.Get(env("READ_HEADER_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadHeaderTimeout).Validate(httpserver.CheckReadHeaderTimeout)),
			ReadTimeout:       r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(httpserver.CheckReadTimeout)),
			WriteTimeout:      r.Get(env("WRITE_TIMEOUT").Parse(time.ParseDuration).Default(d.WriteTimeout).Validate(httpserver.CheckWriteTimeout)),
			IdleTimeout:       r.Get(env("IDLE_TIMEOUT").Parse(time.ParseDuration).Default(d.IdleTimeout).Validate(httpserver.CheckIdleTimeout)),
			MaxHeaderBytes:    r.Get(env("MAX_HEADER_BYTES").Parse(strconv.Atoi).Default(d.MaxHeaderBytes).Validate(httpserver.CheckMaxHeaderBytes)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", greet)

	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// greet answers GET /hello.
func greet(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "hello")
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

// buildTelemetry is the service's telemetry.Builder, which names the
// service.
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "greeter"
	return telemetry.New(ctx, s, dial, p.Stdout)
}

// An HTTP service's main is one call to httpserver.Main, with an Init that
// returns the Service. This example has no Output comment, so it is compiled
// but never run: running it would exit the test binary.
func ExampleMain() {
	httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
}
