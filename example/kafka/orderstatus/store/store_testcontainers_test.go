//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package store_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/z5labs/humus/example/kafka/orderstatus/store"
	"github.com/z5labs/humus/humustest"
)

const postgresImage = "docker.io/library/postgres:18.0"

const sslDir = "/tmp/testcontainers-go/postgres"

var shared struct {
	once      sync.Once
	container *tcpostgres.PostgresContainer
	err       error
}

func TestMain(m *testing.M) {
	m.Run()
	if shared.container != nil {
		_ = shared.container.Terminate(context.Background())
	}
}

func startPostgres(ctx context.Context, opts ...testcontainers.ContainerCustomizer) (*tcpostgres.PostgresContainer, error) {
	opts = append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
	}, opts...)
	return tcpostgres.Run(ctx, postgresImage, opts...)
}

func sharedPostgres(t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	shared.once.Do(func() {
		shared.container, shared.err = startPostgres(context.Background())
	})
	require.NoError(t, shared.err, "starting PostgreSQL in a container")
	return shared.container
}

func database(t *testing.T, c *tcpostgres.PostgresContainer, tlsConfig *tls.Config) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()

	adminURL, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	admin, err := pgx.Connect(ctx, adminURL)
	require.NoError(t, err)
	defer func() { _ = admin.Close(context.Background()) }()

	name := "orders_" + strings.ToLower(rand.Text())
	password := rand.Text()
	ident := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, ident, password))
	require.NoError(t, err)
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, ident, ident))
	require.NoError(t, err)

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	url := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", name, password, net.JoinHostPort(host, port.Port()), name)
	cfg, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	cfg.ConnConfig.TLSConfig = tlsConfig

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(schema))
	require.NoError(t, err)
	return pool
}

func at(minute int) time.Time {
	return time.Date(2026, 10, 18, 14, minute, 0, 0, time.UTC)
}

func status(t *testing.T, pool *pgxpool.Pool, orderID string) store.Status {
	t.Helper()
	var s store.Status
	err := pool.QueryRow(t.Context(), `SELECT status, at FROM order_status WHERE order_id = $1`, orderID).Scan(&s.Status, &s.At)
	require.NoError(t, err)
	s.At = s.At.UTC()
	return s
}

func events(t *testing.T, s *store.Store, orderID string) []store.Event {
	t.Helper()
	got, err := s.Events(t.Context(), orderID)
	require.NoError(t, err)
	for i := range got {
		got[i].At = got[i].At.UTC()
	}
	return got
}

func TestStore_stores_an_event_once(t *testing.T) {
	pool := database(t, sharedPostgres(t), nil)
	s := store.New(pool)
	ctx := t.Context()

	placed := store.Event{Partition: 4, Offset: 1932, OrderID: "7f3c", Type: "placed", At: at(0)}

	applied, err := s.Applied(ctx, 4, 1932)
	require.NoError(t, err)
	require.False(t, applied)

	require.NoError(t, s.Apply(ctx, placed, store.Status{Status: "placed", At: at(0)}))

	applied, err = s.Applied(ctx, 4, 1932)
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = s.Applied(ctx, 3, 1932)
	require.NoError(t, err)
	require.False(t, applied)
	applied, err = s.Applied(ctx, 4, 1933)
	require.NoError(t, err)
	require.False(t, applied)

	again := placed
	again.Type = "paid"
	again.At = at(3)
	err = s.Apply(ctx, again, store.Status{Status: "paid", At: at(3)})
	require.ErrorIs(t, err, store.ErrApplied)

	require.Equal(t, []store.Event{placed}, events(t, s, "7f3c"))
	require.Equal(t, store.Status{Status: "placed", At: at(0)}, status(t, pool, "7f3c"))

	paid := store.Event{Partition: 4, Offset: 1940, OrderID: "7f3c", Type: "paid", At: at(3)}
	require.NoError(t, s.Apply(ctx, paid, store.Status{Status: "paid", At: at(3)}))
	require.Equal(t, []store.Event{placed, paid}, events(t, s, "7f3c"))
	require.Equal(t, store.Status{Status: "paid", At: at(3)}, status(t, pool, "7f3c"))
}

func TestStore_tells_the_second_of_two_writers_the_event_was_stored_already(t *testing.T) {
	pool := database(t, sharedPostgres(t), nil)
	s := store.New(pool)
	ctx := t.Context()

	existing := store.Event{Partition: 0, Offset: 1, OrderID: "existing", Type: "placed", At: at(0)}
	require.NoError(t, s.Apply(ctx, existing, store.Status{Status: "placed", At: at(0)}))

	testCases := []struct {
		name string
		hold string
		race store.Event
	}{
		{
			name: "a new order",
			hold: `INSERT INTO order_status (order_id, status, at) VALUES ('new', 'placed', now())`,
			race: store.Event{Partition: 0, Offset: 2, OrderID: "new", Type: "placed", At: at(1)},
		},
		{
			name: "an order with a status",
			hold: `SELECT 1 FROM order_status WHERE order_id = 'existing' FOR UPDATE`,
			race: store.Event{Partition: 0, Offset: 3, OrderID: "existing", Type: "paid", At: at(2)},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			holder, err := pool.Begin(ctx)
			require.NoError(t, err)
			_, err = holder.Exec(ctx, tc.hold)
			require.NoError(t, err)

			folded := store.Status{Status: tc.race.Type, At: tc.race.At}
			errs := make(chan error, 2)
			for range 2 {
				go func() { errs <- s.Apply(ctx, tc.race, folded) }()
			}

			require.Eventually(t, func() bool {
				var waiting int
				err := pool.QueryRow(ctx, `
					SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'`,
				).Scan(&waiting)
				return err == nil && waiting == 2
			}, 30*time.Second, 10*time.Millisecond, "both writers waiting on the order's status row")
			require.NoError(t, holder.Rollback(ctx))

			var stored, already int
			for range 2 {
				if err := <-errs; err != nil {
					require.ErrorIs(t, err, store.ErrApplied)
					already++
					continue
				}
				stored++
			}
			require.Equal(t, 1, stored)
			require.Equal(t, 1, already)

			var rows int
			err = pool.QueryRow(ctx,
				`SELECT count(*) FROM order_events WHERE source_partition = $1 AND source_offset = $2`,
				tc.race.Partition, tc.race.Offset,
			).Scan(&rows)
			require.NoError(t, err)
			require.Equal(t, 1, rows)
			require.Equal(t, store.Status{Status: tc.race.Type, At: tc.race.At}, status(t, pool, tc.race.OrderID))
		})
	}
}

func TestStore_returns_an_orders_events_by_at_then_offset(t *testing.T) {
	pool := database(t, sharedPostgres(t), nil)
	s := store.New(pool)
	ctx := t.Context()

	written := []store.Event{
		{Partition: 2, Offset: 10, OrderID: "a", Type: "placed", At: at(0)},
		{Partition: 2, Offset: 11, OrderID: "b", Type: "placed", At: at(1)},
		{Partition: 2, Offset: 12, OrderID: "a", Type: "cancelled", At: at(9)},
		{Partition: 2, Offset: 13, OrderID: "a", Type: "shipped", At: at(5)},
		{Partition: 2, Offset: 14, OrderID: "a", Type: "delivered", At: at(5)},
		{Partition: 2, Offset: 15, OrderID: "a", Type: "paid", At: at(3)},
	}
	for _, e := range written {
		require.NoError(t, s.Apply(ctx, e, store.Status{Status: e.Type, At: e.At}))
	}

	require.Equal(t, []store.Event{written[0], written[5], written[3], written[4], written[2]}, events(t, s, "a"))
	require.Equal(t, []store.Event{written[1]}, events(t, s, "b"))
	require.Empty(t, events(t, s, "c"))
}

func TestStore_connects_over_TLS(t *testing.T) {
	ctx := t.Context()
	ca := humustest.NewCA(t)
	dir := t.TempDir()

	cert := ca.Issue(t, "localhost").TLS()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	caFile := filepath.Join(dir, "ca.pem")
	certFile := filepath.Join(dir, "server.pem")
	keyFile := filepath.Join(dir, "server.key")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[len(cert.Certificate)-1]}), 0o600))
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600))

	c, err := startPostgres(ctx,
		tcpostgres.WithSSLCert(caFile, certFile, keyFile),
		testcontainers.WithCmdArgs(
			"-c", "ssl=on",
			"-c", "ssl_cert_file="+sslDir+"/server.cert",
			"-c", "ssl_key_file="+sslDir+"/server.key",
		),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting PostgreSQL with TLS in a container")

	pool := database(t, c, &tls.Config{RootCAs: ca.Pool(), ServerName: "localhost", MinVersion: tls.VersionTLS12})

	var ssl bool
	err = pool.QueryRow(ctx, `SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()`).Scan(&ssl)
	require.NoError(t, err)
	require.True(t, ssl, "the connection is over TLS")

	s := store.New(pool)
	placed := store.Event{Partition: 1, Offset: 7, OrderID: "tls", Type: "placed", At: at(0)}
	require.NoError(t, s.Apply(ctx, placed, store.Status{Status: "placed", At: at(0)}))
	require.Equal(t, []store.Event{placed}, events(t, s, "tls"))

	untrusted := humustest.NewCA(t)
	cfg := pool.Config().ConnConfig.Copy()
	cfg.TLSConfig = &tls.Config{RootCAs: untrusted.Pool(), ServerName: "localhost", MinVersion: tls.VersionTLS12}
	_, err = pgx.ConnectConfig(ctx, cfg)
	require.Error(t, err, "a client that doesn't trust the CA is refused")
}
