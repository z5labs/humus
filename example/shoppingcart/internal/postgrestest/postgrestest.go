// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package postgrestest runs PostgreSQL in a container, and gives each test a database and roles of its own.
package postgrestest

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/migrate"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const image = "docker.io/library/postgres:18.0"

var server struct {
	once      sync.Once
	container *tcpostgres.PostgresContainer
	admin     *url.URL
	err       error
}

func Run(m *testing.M) {
	m.Run()
	if server.container != nil {
		_ = server.container.Terminate(context.Background())
	}
}

func start() error {
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
	)
	if c != nil {
		server.container = c
	}
	if err != nil {
		return err
	}
	conn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	server.admin, err = url.Parse(conn)
	return err
}

type Database struct {
	Name     string
	Owner    string
	OwnerURL string
	App      string
	AppURL   string
}

func NewDatabase(t *testing.T) Database {
	t.Helper()
	server.once.Do(func() {
		server.err = errors.New("starting PostgreSQL did not finish")
		server.err = start()
	})
	require.NoError(t, server.err, "starting PostgreSQL in a container")

	suffix := strings.ToLower(rand.Text())
	ownerPassword, appPassword := rand.Text(), rand.Text()
	db := Database{Name: "db_" + suffix, Owner: "owner_" + suffix, App: "app_" + suffix}
	db.OwnerURL = connURL(db.Name, db.Owner, ownerPassword)
	db.AppURL = connURL(db.Name, db.App, appPassword)

	admin, err := pgx.Connect(t.Context(), server.admin.String())
	require.NoError(t, err)
	defer func() { require.NoError(t, admin.Close(context.Background())) }()
	for _, stmt := range []string{
		`CREATE ROLE ` + db.Owner + ` LOGIN PASSWORD '` + ownerPassword + `'`,
		`CREATE ROLE ` + db.App + ` LOGIN PASSWORD '` + appPassword + `'`,
		`CREATE DATABASE ` + db.Name + ` OWNER ` + db.Owner,
	} {
		_, err := admin.Exec(t.Context(), stmt)
		require.NoError(t, err)
	}
	return db
}

func connURL(database, user, password string) string {
	u := *server.admin
	u.User = url.UserPassword(user, password)
	u.Path = "/" + database
	return u.String()
}

func (db Database) Migrate(t *testing.T, migrations string) <-chan bedrock.ExitStatus {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "database-url-*")
	require.NoError(t, err)
	_, err = f.WriteString(db.OwnerURL + "\n")
	require.NoError(t, errors.Join(err, f.Close()))

	_, status := humustest.Start(t, migrate.Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{
			"DATABASE_URL_FILE": f.Name(),
			"MIGRATIONS":        migrations,
			"APP_ROLE":          db.App,
		}),
	)
	return status
}

func (db Database) Migrated(t *testing.T, migrations string) {
	t.Helper()
	st := <-db.Migrate(t, migrations)
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}
