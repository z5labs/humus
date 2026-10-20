//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/example/shoppingcart/internal/postgrestest"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const waitLimit = 30 * time.Second

func TestMain(m *testing.M) {
	postgrestest.Run(m)
}

func startPostgres(t *testing.T) string {
	t.Helper()
	db := postgrestest.NewDatabase(t)
	db.Migrated(t, "carts")
	return db.AppURL
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, waitLimit, 50*time.Millisecond)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func TestInit_serves_carts_from_Postgres_on_the_OS_Host(t *testing.T) {
	url := startPostgres(t)

	ca := humustest.NewCA(t)
	bundle, password := ca.Issue(t, "localhost", "127.0.0.1").PKCS12(t)
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.p12")
	require.NoError(t, os.WriteFile(certFile, bundle, 0o600))
	addr := freeAddr(t)

	provider := newIdP(t)
	idpCert := ca.Issue(t, "127.0.0.1")
	idpCAFile := filepath.Join(dir, "idp-ca.pem")
	require.NoError(t, os.WriteFile(idpCAFile, caPEM(idpCert), 0o600))
	idpAddr := freeAddr(t)
	stopIdP := provider.serve(t, humus.OSHost(), idpAddr, idpCert)
	waitListening(t, idpAddr)

	p, status := httpservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{
			"ADDR":                addr,
			"TLS_PKCS12_FILE":     certFile,
			"TLS_PKCS12_PASSWORD": password,
			"DATABASE_URL":        url,
			"JWKS_URL":            "https://" + idpAddr + jwksPath,
			"JWKS_CA_FILE":        idpCAFile,
			"JWT_ISSUER":          issuer,
			"JWT_AUDIENCE":        audience,
		}),
	)

	client := &http.Client{
		Timeout: waitLimit,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: ca.Pool()},
			Protocols:       protocolsHTTP2(),
		},
	}
	base := "https://" + addr
	token := provider.token(t, shopper, endpoint.ScopeWrite)

	do := func(method, path, body string) (int, []byte) {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, r)
		require.NoError(t, err)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		require.Equal(t, 2, resp.ProtoMajor)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, b
	}

	waitListening(t, addr)

	code, body := do(http.MethodPost, "/carts", "")
	require.Equal(t, http.StatusCreated, code, "%s", body)
	var cart endpoint.Cart
	require.NoError(t, json.Unmarshal(body, &cart))

	code, body = do(http.MethodPost, "/carts/"+cart.CartID.String()+"/items", `{"productId":"sku-1","quantity":3,"unitPrice":4.5}`)
	require.Equal(t, http.StatusCreated, code, "%s", body)

	code, body = do(http.MethodGet, "/carts/"+cart.CartID.String(), "")
	require.Equal(t, http.StatusOK, code, "%s", body)
	require.NoError(t, json.Unmarshal(body, &cart))
	require.Len(t, cart.Items, 1)
	require.Equal(t, 3, cart.Items[0].Quantity)
	require.InDelta(t, 4.5, cart.Items[0].UnitPrice, 1e-9)

	code, body = do(http.MethodDelete, "/carts/"+cart.CartID.String(), "")
	require.Equal(t, http.StatusNoContent, code, "%s", body)
	code, body = do(http.MethodGet, "/carts/"+cart.CartID.String(), "")
	require.Equal(t, http.StatusNotFound, code, "%s", body)

	token = ""
	code, body = do(http.MethodPost, "/carts", "")
	require.Equal(t, http.StatusUnauthorized, code, "%s", body)

	<-p.Notified(syscall.SIGTERM)
	p.Signal(syscall.SIGTERM)
	select {
	case st := <-status:
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the service didn't exit")
	}
	stopIdP()
}

func protocolsHTTP2() *http.Protocols {
	var p http.Protocols
	p.SetHTTP2(true)
	return &p
}
