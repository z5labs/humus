//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package postgres_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/z5labs/bedrock/fit"

	"github.com/z5labs/humus/example/databases/postgres"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
)

const postgresImage = "docker.io/library/postgres:18.0"

var server struct {
	once      sync.Once
	url       string
	container *tcpostgres.PostgresContainer
	err       error
}

func TestMain(m *testing.M) {
	m.Run()
	if server.container != nil {
		_ = server.container.Terminate(context.Background())
	}
}

func startServer() error {
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("admin"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
	)
	if c != nil {
		server.container = c
	}
	if err != nil {
		return err
	}
	server.url, err = c.ConnectionString(ctx, "sslmode=disable")
	return err
}

type statements struct {
	mu   sync.Mutex
	seen []string
}

func (s *statements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, data.SQL)
	for _, arg := range data.Args {
		s.seen = append(s.seen, fmt.Sprint(arg))
	}
	return ctx
}

func (s *statements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *statements) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

type env struct {
	admin      *pgxpool.Pool
	server     *postgres.Server
	statements *statements
	codec      jsonscenario.Codec
	name       string
}

func newEnv(t *testing.T) env {
	t.Helper()
	server.once.Do(func() {
		server.err = errors.New("starting PostgreSQL did not finish")
		server.err = startServer()
	})
	require.NoError(t, server.err, "starting PostgreSQL in a container")

	cfg, err := pgxpool.ParseConfig(server.url)
	require.NoError(t, err)
	stmts := &statements{}
	cfg.ConnConfig.Tracer = stmts
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	fs := fault.DefaultSettings()
	fs.Decoder = codec
	faults, err := fault.New(fs)
	require.NoError(t, err)

	name := "db_" + strings.ToLower(rand.Text())
	e := env{admin: pool, server: postgres.New(pool, faults), statements: stmts, codec: codec, name: name}
	t.Cleanup(func() {
		ident := pgx.Identifier{name}.Sanitize()
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident)
	})
	return e
}

func (e env) aborting(t *testing.T, point string) context.Context {
	t.Helper()
	ctx, err := fit.ContextWithScenario(t.Context(), e.codec, fit.Scenario{Rules: []fit.Rule{
		{Point: point, Fault: fit.Fault{Abort: true}},
	}})
	require.NoError(t, err)
	return ctx
}

func (e env) connect(ctx context.Context, password string) (*pgx.Conn, error) {
	u, err := url.Parse(server.url)
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(e.name, password)
	u.Path = "/" + e.name
	return pgx.Connect(ctx, u.String())
}

func (e env) requireLogsIn(t *testing.T, password string) {
	t.Helper()
	conn, err := e.connect(t.Context(), password)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.Background()) }()
	var user, owner string
	err = conn.QueryRow(t.Context(),
		`SELECT current_user, pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()`,
	).Scan(&user, &owner)
	require.NoError(t, err)
	require.Equal(t, e.name, user)
	require.Equal(t, e.name, owner)
	_, err = conn.Exec(t.Context(), `CREATE TABLE owned (id int)`+"; "+`DROP TABLE owned`)
	require.NoError(t, err)
}

func (e env) requireRefused(t *testing.T, password string) {
	t.Helper()
	conn, err := e.connect(t.Context(), password)
	if conn != nil {
		_ = conn.Close(context.Background())
	}
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "28P01", pgErr.Code)
}

func (e env) exists(t *testing.T) (role, database bool) {
	t.Helper()
	err := e.admin.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1), EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`,
		e.name,
	).Scan(&role, &database)
	require.NoError(t, err)
	return role, database
}

func (e env) requireNoPassword(t *testing.T, passwords ...string) {
	t.Helper()
	seen := e.statements.all()
	require.NotEmpty(t, seen)
	for _, s := range seen {
		for _, p := range passwords {
			require.NotContains(t, s, p)
		}
	}
}

func requireInjected(t *testing.T, err error, point string) {
	t.Helper()
	var injected *fault.InjectedError
	require.ErrorAs(t, err, &injected)
	require.Equal(t, point, injected.Point)
	require.ErrorIs(t, err, fit.ErrInjected)
}

func TestServer_a_provisioned_role_logs_in_and_owns_its_database(t *testing.T) {
	e := newEnv(t)
	password := rand.Text()

	require.NoError(t, e.server.Provision(t.Context(), e.name, password))

	e.requireLogsIn(t, password)
	e.requireNoPassword(t, password)
}

func TestServer_provisioning_again_with_a_new_password_replaces_the_old_one(t *testing.T) {
	e := newEnv(t)
	old, rotated := rand.Text(), rand.Text()
	require.NoError(t, e.server.Provision(t.Context(), e.name, old))
	e.requireLogsIn(t, old)

	require.NoError(t, e.server.Provision(t.Context(), e.name, rotated))

	e.requireLogsIn(t, rotated)
	e.requireRefused(t, old)
	e.requireNoPassword(t, old, rotated)
}

func TestServer_provisioning_hands_an_existing_database_to_its_role(t *testing.T) {
	e := newEnv(t)
	password := rand.Text()
	_, err := e.admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{e.name}.Sanitize())
	require.NoError(t, err)

	require.NoError(t, e.server.Provision(t.Context(), e.name, password))

	e.requireLogsIn(t, password)
}

func TestServer_dropping_removes_both_while_a_connection_is_open(t *testing.T) {
	e := newEnv(t)
	password := rand.Text()
	require.NoError(t, e.server.Provision(t.Context(), e.name, password))
	conn, err := e.connect(t.Context(), password)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	require.NoError(t, conn.Ping(t.Context()))

	require.NoError(t, e.server.Drop(t.Context(), e.name))

	role, database := e.exists(t)
	require.False(t, role)
	require.False(t, database)
	require.Error(t, conn.Ping(t.Context()))

	require.NoError(t, e.server.Drop(t.Context(), e.name))
}

func TestServer_dropping_what_was_never_provisioned_succeeds(t *testing.T) {
	e := newEnv(t)

	require.NoError(t, e.server.Drop(t.Context(), e.name))

	role, database := e.exists(t)
	require.False(t, role)
	require.False(t, database)
}

func TestServer_an_abort_at_postgresql_provision_changes_nothing(t *testing.T) {
	e := newEnv(t)
	password := rand.Text()

	err := e.server.Provision(e.aborting(t, "postgresql:provision"), e.name, password)

	requireInjected(t, err, "postgresql:provision")
	require.NotContains(t, err.Error(), password)
	role, database := e.exists(t)
	require.False(t, role)
	require.False(t, database)

	require.NoError(t, e.server.Provision(t.Context(), e.name, password))
	rotated := rand.Text()
	err = e.server.Provision(e.aborting(t, "postgresql:provision"), e.name, rotated)
	requireInjected(t, err, "postgresql:provision")
	e.requireLogsIn(t, password)
	e.requireRefused(t, rotated)

	require.NoError(t, e.server.Provision(e.aborting(t, "postgresql:drop"), e.name, rotated))
	e.requireLogsIn(t, rotated)
}

func TestServer_an_abort_at_postgresql_drop_changes_nothing(t *testing.T) {
	e := newEnv(t)
	password := rand.Text()
	require.NoError(t, e.server.Provision(t.Context(), e.name, password))

	err := e.server.Drop(e.aborting(t, "postgresql:drop"), e.name)

	requireInjected(t, err, "postgresql:drop")
	role, database := e.exists(t)
	require.True(t, role)
	require.True(t, database)
	e.requireLogsIn(t, password)

	require.NoError(t, e.server.Drop(e.aborting(t, "postgresql:provision"), e.name))
	role, database = e.exists(t)
	require.False(t, role)
	require.False(t, database)
}
