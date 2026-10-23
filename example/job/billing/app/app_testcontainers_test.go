//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/job/billing/app"
	"github.com/z5labs/humus/example/job/billing/cases"
	"github.com/z5labs/humus/humustest"
)

const (
	postgresImage = "docker.io/library/postgres:18.0"
	wiremockImage = "docker.io/wiremock/wiremock:3.13.2"
)

const (
	wiremockPort     = "8443/tcp"
	wiremockKeystore = "/home/wiremock/keystore.p12"
	waitLimit        = 2 * time.Minute
)

func startPostgres(t *testing.T) (*tcpostgres.PostgresContainer, string) {
	t.Helper()
	c, err := tcpostgres.Run(t.Context(), postgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("admin"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting PostgreSQL in a container")
	u, err := c.ConnectionString(t.Context(), "sslmode=disable")
	require.NoError(t, err)
	return c, u
}

func startWireMock(t *testing.T, ca *humustest.CA, host string) *url.URL {
	t.Helper()
	mappings, err := filepath.Glob(filepath.Join("..", "deploy", "kind", "wiremock", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, mappings)
	keystore, password := ca.Issue(t, host, "localhost", "127.0.0.1").PKCS12(t)

	files := []testcontainers.ContainerFile{{
		Reader:            bytes.NewReader(keystore),
		ContainerFilePath: wiremockKeystore,
		FileMode:          0o644,
	}}
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

func writeFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}

func caPEM(t *testing.T, ca *humustest.CA) []byte {
	t.Helper()
	chain := ca.Issue(t, "unused").TLS().Certificate
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]})
}

func runner(t *testing.T, dir string, env map[string]string) func(ctx context.Context, databaseURL string, extra map[string]string) (int, error) {
	return func(ctx context.Context, databaseURL string, extra map[string]string) (int, error) {
		f, err := os.CreateTemp(dir, "database-url-*")
		if err != nil {
			return 0, err
		}
		_, err = f.WriteString(databaseURL + "\n")
		if err := errors.Join(err, f.Close()); err != nil {
			return 0, err
		}

		all := maps.Clone(env)
		all["DATABASE_URL_FILE"] = f.Name()
		maps.Copy(all, extra)
		p, status := humustest.Start(t, app.Init,
			humustest.WithApp(app.Options()...),
			humustest.WithApp(humus.WithHost(humus.OSHost())),
			humustest.WithEnv(all),
		)

		select {
		case st := <-status:
			t.Logf("billing exited %d: %v", st.Code(), st.Cause())
			return st.Code(), nil
		case <-ctx.Done():
		}
		select {
		case <-p.Notified(syscall.SIGTERM):
			p.Signal(syscall.SIGTERM)
		case st := <-status:
			return st.Code(), context.Cause(ctx)
		}
		st := <-status
		return st.Code(), context.Cause(ctx)
	}
}

func TestInit_passes_the_shared_cases_against_PostgreSQL_and_WireMock(t *testing.T) {
	pg, postgresURL := startPostgres(t)
	host, err := pg.Host(t.Context())
	require.NoError(t, err)
	ca := humustest.NewCA(t)
	wiremock := startWireMock(t, ca, host)
	schema, err := os.ReadFile(filepath.Join("..", "postgres", "schema.sql"))
	require.NoError(t, err)

	dir := t.TempDir()
	key := rand.Text()
	target := cases.Target{
		Postgres: postgresURL,
		Schema:   string(schema),
		WireMock: wiremock,
		Client: &http.Client{
			Timeout:   waitLimit,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}},
		},
		Key: key,
		Run: runner(t, dir, map[string]string{
			"PAYMENTS_URL":          wiremock.String(),
			"PAYMENTS_API_KEY_FILE": writeFile(t, dir, "api-key", []byte(key+"\n")),
			"PAYMENTS_CA_FILE":      writeFile(t, dir, "payments-ca.pem", caPEM(t, ca)),
		}),
		Timeout: waitLimit,
	}

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
}
