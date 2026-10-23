//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package migrate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus/example/http/shoppingcart/internal/postgrestest"
)

func TestMain(m *testing.M) {
	postgrestest.Run(m)
}

type service struct {
	migrations int32
	tables     []string
	writes     []string
}

var services = map[string]service{
	"carts": {
		migrations: 1,
		tables:     []string{"cart_items", "carts"},
		writes: []string{
			`INSERT INTO carts (owner) VALUES ('shopper')`,
			`INSERT INTO cart_items (cart_id, product_id, quantity, unit_price) SELECT id, 'sku-1', 1, 9.99 FROM carts`,
			`UPDATE carts SET updated_at = now()`,
			`UPDATE cart_items SET quantity = 2`,
			`DELETE FROM cart_items`,
			`DELETE FROM carts`,
		},
	},
	"catalog": {
		migrations: 1,
		tables:     []string{"products"},
		writes: []string{
			`INSERT INTO products (id, unit_price) VALUES ('sku-1', 9.99)`,
			`UPDATE products SET unit_price = 1`,
			`DELETE FROM products`,
		},
	},
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close(context.Background())) })
	return conn
}

func version(t *testing.T, conn *pgx.Conn) int32 {
	t.Helper()
	var v int32
	require.NoError(t, conn.QueryRow(t.Context(), `SELECT version FROM public.schema_version`).Scan(&v))
	return v
}

func tables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), `
		SELECT tablename FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'schema_version'
		ORDER BY tablename`)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return names
}

func requireDenied(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a PostgreSQL error, got %v", err)
	require.Equal(t, "42501", pgErr.Code, "want insufficient_privilege, got %v", err)
}

func TestInit_applies_each_services_migrations_and_a_second_run_does_nothing(t *testing.T) {
	for name, svc := range services {
		t.Run(name, func(t *testing.T) {
			db := postgrestest.NewDatabase(t)

			db.Migrated(t, name)

			owner := connect(t, db.OwnerURL)
			require.Equal(t, svc.migrations, version(t, owner))
			require.Equal(t, svc.tables, tables(t, owner))

			app := connect(t, db.AppURL)
			_, err := app.Exec(t.Context(), svc.writes[0])
			require.NoError(t, err)

			db.Migrated(t, name)

			require.Equal(t, svc.migrations, version(t, owner))
			require.Equal(t, svc.tables, tables(t, owner))
			var rows int
			require.NoError(t, owner.QueryRow(t.Context(), `SELECT count(*) FROM `+svc.tables[len(svc.tables)-1]).Scan(&rows))
			require.Equal(t, 1, rows, "the second run kept what the first run's tables held")
		})
	}
}

func TestInit_two_runs_at_once_both_succeed_and_each_migration_runs_once(t *testing.T) {
	for name, svc := range services {
		t.Run(name, func(t *testing.T) {
			db := postgrestest.NewDatabase(t)

			first, second := db.Migrate(t, name), db.Migrate(t, name)

			for _, st := range []bedrock.ExitStatus{<-first, <-second} {
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
			}
			owner := connect(t, db.OwnerURL)
			require.Equal(t, svc.migrations, version(t, owner))
			require.Equal(t, svc.tables, tables(t, owner))
		})
	}
}

func TestInit_grants_the_app_role_read_and_write_and_nothing_more(t *testing.T) {
	for name, svc := range services {
		t.Run(name, func(t *testing.T) {
			db := postgrestest.NewDatabase(t)
			db.Migrated(t, name)
			app := connect(t, db.AppURL)
			ctx := t.Context()

			for _, stmt := range svc.writes {
				_, err := app.Exec(ctx, stmt)
				require.NoError(t, err, stmt)
			}
			for _, table := range svc.tables {
				_, err := app.Exec(ctx, `SELECT * FROM `+table)
				require.NoError(t, err, table)

				_, err = app.Exec(ctx, `ALTER TABLE `+table+` ADD COLUMN extra INTEGER`)
				requireDenied(t, err)
				_, err = app.Exec(ctx, `DROP TABLE `+table)
				requireDenied(t, err)
				_, err = app.Exec(ctx, `TRUNCATE `+table)
				requireDenied(t, err)
			}
			_, err := app.Exec(ctx, `CREATE TABLE extra (id INTEGER)`)
			requireDenied(t, err)
			_, err = app.Exec(ctx, `SELECT * FROM public.schema_version`)
			requireDenied(t, err)
			_, err = app.Exec(ctx, `UPDATE public.schema_version SET version = 0`)
			requireDenied(t, err)

			owner := connect(t, db.OwnerURL)
			require.Equal(t, svc.tables, tables(t, owner))
		})
	}
}
