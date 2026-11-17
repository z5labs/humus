//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/cases"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	postgresImage = "docker.io/library/postgres:18.0"
	waitLimit     = 2 * time.Minute
)

func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting PostgreSQL in a container")

	adminURL, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	defer func() { _ = admin.Close(context.Background()) }()
	password := rand.Text()
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE polls LOGIN PASSWORD '%s'`, password))
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `CREATE DATABASE polls OWNER polls`)
	require.NoError(t, err)

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	appURL := fmt.Sprintf("postgres://polls:%s@%s/polls?sslmode=disable", password, net.JoinHostPort(host, port.Port()))

	conn, err := pgx.Connect(ctx, appURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.Background()) }()
	schema, err := os.ReadFile(filepath.Join("..", "store", "schema.sql"))
	require.NoError(t, err)
	_, err = conn.Exec(ctx, string(schema))
	require.NoError(t, err)
	return appURL
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
	a := l.Addr().String()
	require.NoError(t, l.Close())
	return a
}

type replica struct {
	addr   string
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

func (r replica) waitReady(t *testing.T, client *http.Client) {
	t.Helper()
	ready := "https://" + r.addr + "/health/readiness"
	require.Eventually(t, func() bool {
		resp, err := client.Get(ready)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, waitLimit, 100*time.Millisecond, "the replica at %s didn't become ready", r.addr)
}

func (r replica) stop(t *testing.T) {
	t.Helper()
	<-r.p.Notified(syscall.SIGTERM)
	r.p.Signal(syscall.SIGTERM)
	select {
	case st := <-r.status:
		require.Zero(t, st.Code(), "the replica at %s's cause: %v", r.addr, st.Cause())
	case <-time.After(waitLimit):
		t.Fatalf("the replica at %s didn't exit", r.addr)
	}
}

func TestInit_passes_the_shared_cases_on_two_replicas_against_PostgreSQL(t *testing.T) {
	databaseURL := startPostgres(t)

	dir := t.TempDir()
	ca := humustest.NewCA(t)
	trust := humustest.NewCA(t)
	certPEM, keyPEM := pemPair(t, ca.Issue(t, "localhost", "127.0.0.1"))
	files := map[string]string{
		"TLS_CERT_FILE":       writeFile(t, dir, "tls.crt", certPEM),
		"TLS_KEY_FILE":        writeFile(t, dir, "tls.key", keyPEM),
		"DATABASE_URL_FILE":   writeFile(t, dir, "database-url", []byte(databaseURL+"\n")),
		"FAULT_TRUST_CA_FILE": writeFile(t, dir, "fault-ca.crt", caPEM(trust.Issue(t, "cases"))),
	}

	var protocols http.Protocols
	protocols.SetHTTP2(true)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.Pool()},
		Protocols:       &protocols,
	}
	trusted := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: ca.Pool(), Certificates: []tls.Certificate{trust.Issue(t, "cases").TLS()}},
		Protocols:       &protocols,
	}
	client := &http.Client{Transport: transport, Timeout: waitLimit}

	var (
		running  []replica
		replicas []*url.URL
	)
	for range 2 {
		a := freeAddr(t)
		env := map[string]string{"ADDR": a}
		for name, file := range files {
			env[name] = file
		}
		p, status := humustest.Start(t, Init,
			humustest.WithApp(httpserver.Defaults(), humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(Faults)),
			humustest.WithEnv(env),
		)
		running = append(running, replica{addr: a, p: p, status: status})
		replicas = append(replicas, &url.URL{Scheme: "https", Host: a})
	}
	for _, r := range running {
		r.waitReady(t, client)
	}

	browser := func(transport *http.Transport) func() *http.Client {
		return func() *http.Client {
			jar, err := cookiejar.New(nil)
			require.NoError(t, err)
			return &http.Client{Transport: transport, Jar: jar}
		}
	}
	target := cases.Target{
		Replicas:       replicas,
		Browser:        browser(transport),
		TrustedBrowser: browser(trusted),
		ScenarioKey:    scenarioKey,
		Timeout:        waitLimit,
	}
	t.Run("cases", func(t *testing.T) {
		for _, c := range cases.Cases {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})

	for _, r := range running {
		r.stop(t)
	}
}
