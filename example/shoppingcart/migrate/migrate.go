// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package migrate is migrate's Init: it applies a service's migrations, then grants the service's role read and write on its tables.
package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/jackc/pgx/v5"
	tern "github.com/jackc/tern/v2/migrate"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	catalogdb "github.com/z5labs/humus/example/shoppingcart/catalog/database"
	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

const (
	versionTable = "public.schema_version"
	grantLock    = int64(4217863019455312)
)

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}
	return []humus.Service{job.NewService(s, func(ctx context.Context) error {
		return run(ctx, s, set)
	})}, nil
}

type settings struct {
	Database   *pgx.ConnConfig
	Migrations fs.FS
	AppRole    string
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }

	return config.Struct(func(r *config.Resolver) settings {
		return settings{
			Database:   r.Get(file("DATABASE_URL_FILE").Parse(databaseURL)),
			Migrations: r.Get(env("MIGRATIONS").Parse(migrations)),
			AppRole:    r.Get(env("APP_ROLE").Validate(notEmpty)),
		}
	}).Read(ctx)
}

func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

func databaseURL(b []byte) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(string(bytes.TrimSpace(b)))
	if err != nil {
		return nil, errors.New("must hold a PostgreSQL URL")
	}
	return cfg, nil
}

func migrations(name string) (fs.FS, error) {
	switch name {
	case "carts":
		return fs.Sub(database.Migrations, "migrations")
	case "catalog":
		return fs.Sub(catalogdb.Migrations, "migrations")
	}
	return nil, errors.New("must be carts or catalog")
}

func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func run(ctx context.Context, s *humus.Setup, set settings) (err error) {
	cfg := set.Database
	cfg.DialFunc = s.Host.Dial
	cfg.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	defer func() {
		err = errors.Join(err, conn.Close(context.WithoutCancel(ctx)))
	}()

	m, err := tern.NewMigrator(ctx, conn, versionTable)
	if err != nil {
		return fmt.Errorf("preparing %s: %w", versionTable, err)
	}
	if err := m.LoadMigrations(set.Migrations); err != nil {
		return fmt.Errorf("loading the migrations: %w", err)
	}
	if err := m.Migrate(ctx); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	if err := grant(ctx, conn, set.AppRole); err != nil {
		return fmt.Errorf("granting %s read and write: %w", set.AppRole, err)
	}
	return nil
}

func grant(ctx context.Context, conn *pgx.Conn, role string) error {
	r := pgx.Identifier{role}.Sanitize()
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, grantLock); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+r+`;
			REVOKE ALL ON `+versionTable+` FROM `+r)
		return err
	})
}

func BuildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "migrate"
	return telemetry.New(ctx, s, dial, p.Stdout)
}
