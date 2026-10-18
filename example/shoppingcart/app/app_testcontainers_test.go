//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

// The test in this file runs the whole service once, Init as main runs it,
// on humus.OSHost(), in real time, against Postgres in a container started
// with testcontainers-go, with the schema applied from schema.sql. Beside
// it, the test's identity provider serves its key set on the OS Host too,
// which the service reaches through JWKS_URL and JWKS_CA_FILE. It opens
// real sockets, so it sits behind the testcontainers build tag and runs in
// a CI job of its own, outside the coverage gate:
//
//	go -C example test -race -tags testcontainers ./...

import (
	"context"
	"crypto/rand"
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
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// postgresImage is the Postgres the test runs, the official image pinned to
// an exact release.
const postgresImage = "docker.io/library/postgres:18.0"

// waitLimit bounds every wait of the test for the service.
const waitLimit = 30 * time.Second

// startPostgres starts Postgres with a password new for the test, applies
// schema.sql, and returns its URL, which sets every field pgx uses.
func startPostgres(t *testing.T) string {
	t.Helper()
	c, err := tcpostgres.Run(t.Context(), postgresImage,
		tcpostgres.WithDatabase("carts"),
		tcpostgres.WithUsername("carts"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.WithInitScripts(filepath.Join("..", "database", "schema.sql")),
		tcpostgres.BasicWaitStrategies(),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting Postgres in a container")
	url, err := c.ConnectionString(t.Context(), "sslmode=disable")
	require.NoError(t, err)
	return url
}

// waitListening waits for something to listen on addr.
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

// freeAddr returns a loopback address with a port nothing listens on.
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

	// The identity provider, on the OS Host, with a certificate from the
	// same CA, which the service trusts through JWKS_CA_FILE.
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
	token := provider.token(t, endpoint.ScopeWrite)

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

	// Without a token, the service answers 401.
	token = ""
	code, body = do(http.MethodPost, "/carts", "")
	require.Equal(t, http.StatusUnauthorized, code, "%s", body)

	// The drain is httpserver.Main's 5s, so the exit takes that long.
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

// protocolsHTTP2 returns the protocols of a client that speaks HTTP/2 alone.
func protocolsHTTP2() *http.Protocols {
	var p http.Protocols
	p.SetHTTP2(true)
	return &p
}
