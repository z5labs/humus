// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fault_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
)

// The failure injection page of the docs site quotes the code in this file
// and in app_test.go, and TestDocs_code_samples checks that every quote
// still matches it. app_test.go runs Init and Faults as a whole App.

// Faults is the orders service's failure injection: a scenario is JSON in
// the baggage member acme.fault, and an operator sets MaxDelay with
// FAULT_MAX_DELAY.
func Faults(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
	codec, err := jsonscenario.New("acme.fault")
	if err != nil {
		return fault.Settings{}, err
	}
	d := fault.DefaultSettings()
	return config.Struct(func(r *config.Resolver) fault.Settings {
		c := d
		c.Decoder = codec
		c.MaxDelay = r.Get(config.Env(s.Process.Environment, "FAULT_MAX_DELAY").
			Parse(time.ParseDuration).Default(d.MaxDelay).Validate(fault.CheckMaxDelay))
		return c
	}).Read(ctx)
}

// Init is the orders service's Init. Its API trusts the scenario of a
// caller whose client certificate the mesh's CA verified, and its client of
// payments, another service of the end user's, sends the scenario on.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	payments, err := config.Struct(func(r *config.Resolver) httpclient.Settings {
		c := httpclient.DefaultSettings()
		c.BaseURL = r.Get(env("PAYMENTS_URL").Parse(url.Parse).Validate(httpclient.CheckBaseURL))
		c.TLS = r.Get(env("PAYMENTS_CA_FILE").Parse(readFile(s.Host.FS)).Parse(trusting).Validate(httpclient.CheckTLS))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err
	}
	api, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		c := httpserver.DefaultSettings()
		c.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		c.TLS = r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err
	}
	meshCAs, err := env("MESH_CA_FILE").Parse(readFile(s.Host.FS)).Parse(certPool).Read(ctx)
	if err != nil {
		return nil, err
	}

	payments.ForwardScenario = true // payments is ours, and honors acme.fault too
	client, err := httpclient.NewClient(s, payments)
	if err != nil {
		return nil, err
	}

	api.TLS.ClientAuth = tls.VerifyClientCertIfGiven
	api.TLS.ClientCAs = meshCAs
	api.TrustScenario = func(r *http.Request) bool {
		return r.TLS != nil && len(r.TLS.VerifiedChains) > 0
	}
	svc, err := httpserver.NewService(s, api, newAPI(client, payments.BaseURL))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// newAPI returns the orders API. GET /orders/{id}/payment answers with what
// payments says of the order's charge.
func newAPI(payments *http.Client, base *url.URL) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}/payment", func(w http.ResponseWriter, r *http.Request) {
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

// certPool returns the pool of the CA certificates in pem.
func certPool(pem []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("holds no PEM certificate")
	}
	return pool, nil
}

// trusting returns a client's TLS config that trusts only the CA
// certificates in pem.
func trusting(pem []byte) (*tls.Config, error) {
	roots, err := certPool(pem)
	if err != nil {
		return nil, err
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

// A service turns failure injection on by passing its Faults function to
// its Main with humus.WithFaults: here httpserver.Main, since orders serves
// HTTP behind a load balancer, and humus.Main takes the same option. This
// example has no Output comment, so it is compiled but never run: running
// it would exit the test binary.
func Example_main() {
	httpserver.Main(Init, humus.WithFaults(Faults))
}
