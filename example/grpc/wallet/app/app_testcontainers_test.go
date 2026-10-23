//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/grpc/wallet/cases"
	"github.com/z5labs/humus/humustest"
)

const (
	postgresImage = "docker.io/library/postgres:18.0"
	postgresSSL   = "/tmp/testcontainers-go/postgres"
	waitLimit     = 2 * time.Minute
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func startPostgres(t *testing.T, ca *humustest.CA, file func(string, []byte) string) map[string]string {
	t.Helper()
	ctx := t.Context()

	cert := ca.Issue(t, "localhost", "127.0.0.1").TLS()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	caFile := file("postgres/ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[len(cert.Certificate)-1]}))
	certFile := file("postgres/server.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))
	keyFile := file("postgres/server.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))

	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
		tcpostgres.WithSSLCert(caFile, certFile, keyFile),
		testcontainers.WithCmdArgs(
			"-c", "ssl=on",
			"-c", "ssl_cert_file="+postgresSSL+"/server.cert",
			"-c", "ssl_key_file="+postgresSSL+"/server.key",
		),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting PostgreSQL with TLS in a container")

	adminURL, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	defer func() { _ = admin.Close(context.Background()) }()

	name := "ledger_" + strings.ToLower(rand.Text())
	password := rand.Text()
	ident := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, ident, password))
	require.NoError(t, err)
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, ident, ident))
	require.NoError(t, err)

	port, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	host, err := c.Host(ctx)
	require.NoError(t, err)
	addr := net.JoinHostPort(host, port.Port())

	owner, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", name, password, addr, name))
	require.NoError(t, err)
	defer func() { _ = owner.Close(context.Background()) }()
	schema, err := os.ReadFile(filepath.Join("..", "ledger", "store", "schema.sql"))
	require.NoError(t, err)
	_, err = owner.Exec(ctx, string(schema))
	require.NoError(t, err)

	return map[string]string{
		"POSTGRES_URL":           fmt.Sprintf("postgres://%s@%s/%s", name, addr, name),
		"POSTGRES_PASSWORD_FILE": file("secrets/postgres-password", []byte(password+"\n")),
	}
}

func onOSHost(t *testing.T) place {
	return place{
		host: humus.OSHost(),
		dir:  t.TempDir(),
		addr: func(string) string { return freeAddr(t) },
		postgres: func(ca *humustest.CA, file func(string, []byte) string) map[string]string {
			return startPostgres(t, ca, file)
		},
		timeout: waitLimit,
	}
}

func TestInit_passes_the_shared_cases_against_PostgreSQL(t *testing.T) {
	h := startHarness(t, onOSHost(t))

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, h.target)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, h.target)
		})
	}

	h.stop(t)
}

func TestInit_keeps_every_watch_open_through_a_rollout_of_the_ledger_while_a_connection_to_each_replica_is_in_its_handshake(t *testing.T) {
	h := startHarness(t, onOSHost(t))
	for _, r := range h.ledger.replicas {
		conn, err := net.Dial("tcp", r.addr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
	}

	cases.ARolloutOfTheLedger(t, h.target)

	h.stop(t)
}
