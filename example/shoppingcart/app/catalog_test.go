// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

type fakeCatalog struct {
	mu     sync.Mutex
	prices map[string]float64
	err    error
}

func fakePrices() *fakeCatalog {
	return &fakeCatalog{prices: map[string]float64{"sku-1": 9.99, "sku-2": 1.5}}
}

func (c *fakeCatalog) Price(_ context.Context, productID string) (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	price, ok := c.prices[productID]
	if !ok {
		return 0, endpoint.ErrProductNotFound
	}
	return price, nil
}

func (c *fakeCatalog) set(productID string, price float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prices[productID] = price
}

func (c *fakeCatalog) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *fakeCatalog) open(*humus.Setup, httpclient.Settings) (endpoint.Catalog, error) {
	return c, nil
}

func onFakes(catalog *fakeCatalog) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
		return initWith(ctx, setup, inMemory, catalog.open)
	}
}

type catalogServer struct {
	prices map[string]float64

	mu    sync.Mutex
	peers []string
}

func (c *catalogServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		c.mu.Lock()
		c.peers = append(c.peers, r.TLS.PeerCertificates[0].DNSNames...)
		c.mu.Unlock()
	}
	id := r.PathValue("productId")
	price, ok := c.prices[id]
	if !ok {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404,"detail":"There is no product."}`))
		return
	}
	b, err := json.Marshal(map[string]any{"productId": id, "unitPrice": price})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (c *catalogServer) clients() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.peers...)
}

func (c *catalogServer) serve(t *testing.T, host humus.Host, addr string, cert *humustest.Certificate, clientCAs *x509.CertPool) (stop func()) {
	t.Helper()
	proc, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = addr
		settings.TLS = &tls.Config{
			Certificates: []tls.Certificate{cert.TLS()},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    clientCAs,
		}
		mux := http.NewServeMux()
		mux.Handle("GET /products/{productId}", c)
		svc, err := httpserver.NewService(s, settings, mux)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
	return func() {
		t.Helper()
		<-proc.Notified(syscall.SIGTERM)
		proc.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "the catalog's cause: %v", st.Cause())
	}
}

func pemPair(t *testing.T, c *humustest.Certificate) (certPEM, keyPEM []byte) {
	t.Helper()
	cert := c.TLS()
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}
