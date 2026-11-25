// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/http/httpserver/httpservertest"
	"github.com/z5labs/humus/http3/http3client"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// Init is the end user's Init: it sources the payments dependency's
// Settings, builds its client with NewClient, which calls payments over
// HTTP/3, and hands the client to the handler of an httpserver Service.
func Init(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
	d := http3client.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	paymentsSettings, err := config.Struct(func(r *config.Resolver) http3client.Settings {
		c := d
		// No .Default: BaseURL is required. HTTP/3 is always TLS, and with no
		// PAYMENTS_CA_FILE the client trusts the system's roots.
		c.BaseURL = r.Get(env("PAYMENTS_URL").Parse(url.Parse).Validate(http3client.CheckBaseURL))
		c.TLS = r.Get(env("PAYMENTS_CA_FILE").Parse(readFile(h.Filesystem)).Parse(trusting).Default(nil).Validate(http3client.CheckTLS))
		c.Timeout = r.Get(env("PAYMENTS_TIMEOUT").Parse(time.ParseDuration).Default(d.Timeout).Validate(http3client.CheckTimeout))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}
	serverSettings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		c := httpserver.DefaultSettings()
		c.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		c.TLS = r.Get(env("TLS_PEM_FILE").Parse(readFile(h.Filesystem)).Parse(serverTLS).Validate(httpserver.CheckTLS))
		return c
	}).Read(ctx)
	if err != nil {
		return nil, err
	}

	// One client for the dependency, built once and shared by every
	// request the handler serves. It dials over UDP through h, and never
	// falls back to TCP.
	payments, err := http3client.NewClient(s, h, paymentsSettings)
	if err != nil {
		return nil, err
	}

	api := newAPI(payments, paymentsSettings.BaseURL)
	svc, err := httpserver.NewService(ctx, s, h, serverSettings, api)
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
		_, _ = fmt.Fprintf(w, "%s over %s", readAll(resp.Body), resp.Proto)
	})
	return mux
}

// readAll returns what r holds, up to its first error.
func readAll(r io.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}

// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.Filesystem) func(name string) ([]byte, error) {
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

func keyPairPEM(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var b []byte
	for _, der := range cert.Certificate {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

func caPEM(c *humustest.Certificate) []byte {
	chain := c.TLS().Certificate
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]})
}

func TestInit_calls_payments_over_HTTP3(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		orders := ca.Issue(t, "orders")
		h := humustest.NewHost(t, hosttest.WithFiles(map[string][]byte{
			"tls.pem":         keyPairPEM(t, orders),
			"payments-ca.pem": caPEM(orders),
		}))
		serveH3(t, h, ca.Issue(t, "payments"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "charge for "+r.URL.Path+": paid")
		}))

		p, status := httpservertest.Start(t, h.Host(), Init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{
				"ADDR":             ":9443",
				"TLS_PEM_FILE":     "tls.pem",
				"PAYMENTS_URL":     "https://payments:8443/v1",
				"PAYMENTS_CA_FILE": "payments-ca.pem",
			}),
		)
		<-h.Listening("orders:9443")

		resp, err := httpservertest.NewClient(h, ca.Pool()).Get("https://orders:9443/orders/7/payment")
		require.NoError(t, err)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", b)
		require.Equal(t, "charge for /v1/charges/7: paid over HTTP/3.0", string(b))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, p.Stderr())
	})

	t.Run("fails with exit 78 when PAYMENTS_URL is not set", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			_, status := httpservertest.Start(t, h.Host(), Init,
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			)
			st := <-status
			require.Equal(t, 78, int(st.Code()), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "PAYMENTS_URL")
		})
	})

	t.Run("fails with exit 78 when PAYMENTS_URL is http", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			_, status := httpservertest.Start(t, h.Host(), Init,
				humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
				humustest.WithEnv(map[string]string{"PAYMENTS_URL": "http://payments:8080"}),
			)
			st := <-status
			require.Equal(t, 78, int(st.Code()), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "PAYMENTS_URL")
			require.ErrorContains(t, st.Cause(), "HTTP/3 has no plain-text form")
		})
	})
}
