// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/telemetry"
)

// TestInit_calls_payments_for_each_request runs Init.

// Init is the end user's Init: it sources the payments dependency's
// Settings, builds its client with NewClient, and hands the client to the
// handler of an httpserver Service.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := httpclient.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	paymentsSettings, err := config.Struct(func(r *config.Resolver) httpclient.Settings {
		c := d
		// No .Default: BaseURL is required, and the dependency is https,
		// so PAYMENTS_CA_FILE is too.
		c.BaseURL = r.Get(env("PAYMENTS_URL").Parse(url.Parse).Validate(httpclient.CheckBaseURL))
		c.TLS = r.Get(env("PAYMENTS_CA_FILE").Parse(readFile(s.Host.FS)).Parse(trusting).Validate(httpclient.CheckTLS))
		c.Timeout = r.Get(env("PAYMENTS_TIMEOUT").Parse(time.ParseDuration).Default(d.Timeout).Validate(httpclient.CheckTimeout))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}
	serverSettings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		c := httpserver.DefaultSettings()
		c.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		c.TLS = r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err
	}

	// One client for the dependency, built once and shared by every
	// request the handler serves.
	payments, err := httpclient.NewClient(s, paymentsSettings)
	if err != nil {
		return nil, err
	}

	api := newAPI(payments, paymentsSettings.BaseURL)
	svc, err := httpserver.NewService(s, serverSettings, api)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// newAPI returns the end user's handler. GET /orders/{id}/payment answers
// with what the payments dependency says of the order's charge.
func newAPI(payments *http.Client, base *url.URL) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}/payment", func(w http.ResponseWriter, r *http.Request) {
		// The call ends with the request's context, and service code joins
		// its paths onto the BaseURL.
		u := base.JoinPath("charges", r.PathValue("id"))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := payments.Do(req)
		if err != nil {
			http.Error(w, "payments is unavailable", http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			http.Error(w, fmt.Sprintf("payments answered %d", resp.StatusCode), http.StatusBadGateway)
			return
		}
		_, _ = io.Copy(w, resp.Body)
	})
	return mux
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

// trusting returns a client's TLS config that trusts only the CA
// certificates in pem.
func trusting(pem []byte) (*tls.Config, error) {
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

// A service that calls a dependency builds the dependency's client in its
// Init, and hands it to the code that calls it. This example has no Output
// comment, so it is compiled but never run: running it would exit the test
// binary.
func Example_main() {
	httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
}
