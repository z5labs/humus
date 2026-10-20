//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/cases"
	catalogapp "github.com/z5labs/humus/example/shoppingcart/catalog/app"
	"github.com/z5labs/humus/example/shoppingcart/internal/postgrestest"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	wiremockImage    = "docker.io/wiremock/wiremock:3.13.2"
	wiremockPort     = "8443/tcp"
	wiremockKeystore = "/home/wiremock/keystore.p12"
	wiremockKeySet   = "/home/wiremock/__files/jwks.json"
	waitLimit        = 2 * time.Minute
)

func TestMain(m *testing.M) {
	postgrestest.Run(m)
}

func writeFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func makeKeySet(t *testing.T, dir string) (*idp, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyFile := writeFile(t, dir, "signing.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "go", "run", filepath.Join("..", "cmd", "carttoken"), "jwks", "-key", keyFile)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "carttoken jwks: %s", stderr.String())

	var set jose.JSONWebKeySet
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &set), "%s", stdout.String())
	require.Len(t, set.Keys, 1)
	return &idp{kid: set.Keys[0].KeyID, key: key}, stdout.Bytes()
}

func startWireMock(t *testing.T, ca *humustest.CA, host string, keySet []byte) *url.URL {
	t.Helper()
	mappings, err := filepath.Glob(filepath.Join("..", "deploy", "kind", "wiremock", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, mappings)
	keystore, password := ca.Issue(t, host, "localhost", "127.0.0.1").PKCS12(t)

	files := []testcontainers.ContainerFile{
		{Reader: bytes.NewReader(keystore), ContainerFilePath: wiremockKeystore, FileMode: 0o644},
		{Reader: bytes.NewReader(keySet), ContainerFilePath: wiremockKeySet, FileMode: 0o644},
	}
	for _, m := range mappings {
		files = append(files, testcontainers.ContainerFile{
			HostFilePath:      m,
			ContainerFilePath: "/home/wiremock/mappings/" + filepath.Base(m),
			FileMode:          0o644,
		})
	}

	c, err := testcontainers.Run(t.Context(), wiremockImage,
		testcontainers.WithExposedPorts(wiremockPort),
		testcontainers.WithFiles(files...),
		testcontainers.WithCmd(
			"--disable-http",
			"--https-port", "8443",
			"--https-keystore", wiremockKeystore,
			"--keystore-type", "PKCS12",
			"--keystore-password", password,
			"--key-manager-password", password,
		),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/__admin/health").
			WithPort(wiremockPort).
			WithTLS(true, &tls.Config{RootCAs: ca.Pool(), ServerName: host}).
			WithStartupTimeout(waitLimit)),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting WireMock in a container")
	port, err := c.MappedPort(t.Context(), wiremockPort)
	require.NoError(t, err)
	return &url.URL{Scheme: "https", Host: net.JoinHostPort(host, port.Port())}
}

type started struct {
	name   string
	ready  *url.URL
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

func startApp(t *testing.T, name string, init func(context.Context, *humus.Setup) ([]humus.Service, error), faults humus.Option, envs ...map[string]string) started {
	t.Helper()
	env := map[string]string{}
	for _, e := range envs {
		maps.Copy(env, e)
	}
	p, status := humustest.Start(t, init,
		humustest.WithApp(httpserver.Defaults(), humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder), faults),
		humustest.WithEnv(env),
	)
	return started{
		name:   name,
		ready:  &url.URL{Scheme: "https", Host: env["ADDR"], Path: "/health/readiness"},
		p:      p,
		status: status,
	}
}

func (s started) waitReady(t *testing.T, client *http.Client) {
	t.Helper()
	require.Eventually(t, func() bool {
		resp, err := client.Get(s.ready.String())
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, waitLimit, 100*time.Millisecond, "the %s didn't become ready", s.name)
}

func (s started) stop(t *testing.T) {
	t.Helper()
	<-s.p.Notified(syscall.SIGTERM)
	s.p.Signal(syscall.SIGTERM)
	select {
	case st := <-s.status:
		require.Zero(t, st.Code(), "the %s's cause: %v", s.name, st.Cause())
	case <-time.After(waitLimit):
		t.Fatalf("the %s didn't exit", s.name)
	}
}

func TestInit_passes_the_shared_cases_against_PostgreSQL_and_WireMock(t *testing.T) {
	carts := postgrestest.NewDatabase(t)
	carts.Migrated(t, "carts")
	catalogDB := postgrestest.NewDatabase(t)
	catalogDB.Migrated(t, "catalog")
	pg, err := url.Parse(carts.AppURL)
	require.NoError(t, err)

	dir := t.TempDir()
	ca := humustest.NewCA(t)
	provider, keySet := makeKeySet(t, dir)
	keySetURL := startWireMock(t, ca, pg.Hostname(), keySet).JoinPath(jwksPath)

	cartAddr, catalogAddr := freeAddr(t), freeAddr(t)
	cartCert, cartKey := pemPair(t, ca.Issue(t, "localhost", "127.0.0.1"))
	clientCert, clientKey := pemPair(t, ca.Issue(t, "shoppingcart"))
	catalogCert, catalogKey := pemPair(t, ca.Issue(t, "localhost", "127.0.0.1"))
	caFile := writeFile(t, dir, "ca.crt", caPEM(ca.Issue(t, "unused")))

	tokens := map[string]string{
		"JWKS_URL":     keySetURL.String(),
		"JWKS_CA_FILE": caFile,
		"JWT_ISSUER":   issuer,
		"JWT_AUDIENCE": audience,
	}
	catalog := startApp(t, "catalog", catalogapp.Init, humus.WithFaults(catalogapp.Faults), tokens, map[string]string{
		"ADDR":              catalogAddr,
		"TLS_CERT_FILE":     writeFile(t, dir, "catalog.crt", catalogCert),
		"TLS_KEY_FILE":      writeFile(t, dir, "catalog.key", catalogKey),
		"CLIENT_CA_FILE":    caFile,
		"DATABASE_URL_FILE": writeFile(t, dir, "catalog-database-url", []byte(catalogDB.AppURL+"\n")),
	})
	cart := startApp(t, "cart", Init, humus.WithFaults(Faults), tokens, map[string]string{
		"FAULT_TRUST_SCOPE":        trustScope,
		"ADDR":                     cartAddr,
		"TLS_CERT_FILE":            writeFile(t, dir, "cart.crt", cartCert),
		"TLS_KEY_FILE":             writeFile(t, dir, "cart.key", cartKey),
		"DATABASE_URL_FILE":        writeFile(t, dir, "cart-database-url", []byte(carts.AppURL+"\n")),
		"CATALOG_URL":              "https://" + catalogAddr,
		"CATALOG_CA_FILE":          caFile,
		"CATALOG_CLIENT_CERT_FILE": writeFile(t, dir, "cart-catalog.crt", clientCert),
		"CATALOG_CLIENT_KEY_FILE":  writeFile(t, dir, "cart-catalog.key", clientKey),
	})

	client := func() *http.Client {
		var protocols http.Protocols
		protocols.SetHTTP2(true)
		return &http.Client{
			Timeout: waitLimit,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: ca.Pool()},
				Protocols:       &protocols,
			},
		}
	}
	catalog.waitReady(t, client())
	cart.waitReady(t, client())

	target := cases.Target{
		Cart:          &url.URL{Scheme: "https", Host: cartAddr},
		CartClient:    client(),
		Catalog:       &url.URL{Scheme: "https", Host: catalogAddr},
		CatalogClient: client(),
		Token:         provider.sign,
		CatalogPoint:  "http:" + catalogAddr,
		ScenarioKey:   scenarioKey,
		TrustScope:    trustScope,
		Timeout:       waitLimit,
	}
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, target)
		})
	}

	cart.stop(t)
	catalog.stop(t)
}
