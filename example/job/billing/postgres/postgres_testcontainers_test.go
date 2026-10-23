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
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/z5labs/bedrock/fit"

	"github.com/z5labs/humus/example/job/billing/postgres"
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

type database struct {
	pool  *pgxpool.Pool
	store *postgres.Store
	codec jsonscenario.Codec
}

func newDatabase(t *testing.T) database {
	t.Helper()
	server.once.Do(func() {
		server.err = errors.New("starting PostgreSQL did not finish")
		server.err = startServer()
	})
	require.NoError(t, server.err, "starting PostgreSQL in a container")

	ctx := t.Context()
	admin, err := pgx.Connect(ctx, server.url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	name := "billing_" + strings.ToLower(rand.Text())
	password := rand.Text()
	ident := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE ROLE "+ident+" LOGIN PASSWORD '"+strings.ReplaceAll(password, "'", "''")+"'")
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+ident+" OWNER "+ident)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.Background()
		_, err := admin.Exec(ctx, "DROP DATABASE "+ident+" WITH (FORCE)")
		require.NoError(t, err)
		_, err = admin.Exec(ctx, "DROP ROLE "+ident)
		require.NoError(t, err)
	})

	u, err := url.Parse(server.url)
	require.NoError(t, err)
	u.User = url.UserPassword(name, password)
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	schema, err := os.ReadFile("schema.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(schema))
	require.NoError(t, err)

	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	fs := fault.DefaultSettings()
	fs.Decoder = codec
	faults, err := fault.New(fs)
	require.NoError(t, err)

	return database{pool: pool, store: postgres.New(pool, faults), codec: codec}
}

func (db database) aborting(t *testing.T, point string) context.Context {
	t.Helper()
	ctx, err := fit.ContextWithScenario(t.Context(), db.codec, fit.Scenario{Rules: []fit.Rule{
		{Point: point, Fault: fit.Fault{Abort: true}},
	}})
	require.NoError(t, err)
	return ctx
}

func requireInjected(t *testing.T, err error, point string) {
	t.Helper()
	var injected *fault.InjectedError
	require.ErrorAs(t, err, &injected)
	require.Equal(t, point, injected.Point)
	require.ErrorIs(t, err, fit.ErrInjected)
}

func (db database) subscribe(t *testing.T, accountID string, amountCents int64, currency string, active bool) {
	t.Helper()
	_, err := db.pool.Exec(t.Context(),
		`INSERT INTO subscriptions (account_id, amount_cents, currency, active) VALUES ($1, $2, $3, $4)`,
		accountID, amountCents, currency, active,
	)
	require.NoError(t, err)
}

type row struct {
	AccountID   string
	Period      time.Time
	AmountCents int64
	Currency    string
	State       string
	ChargeID    *string
	DeclineCode *string
	Answer      *string
}

func (db database) invoice(t *testing.T, id uuid.UUID) row {
	t.Helper()
	var r row
	err := db.pool.QueryRow(t.Context(),
		`SELECT account_id, period, amount_cents, currency, state, charge_id, decline_code, answer FROM invoices WHERE id = $1`, id,
	).Scan(&r.AccountID, &r.Period, &r.AmountCents, &r.Currency, &r.State, &r.ChargeID, &r.DeclineCode, &r.Answer)
	require.NoError(t, err)
	return r
}

func (db database) invoices(t *testing.T) []row {
	t.Helper()
	rows, err := db.pool.Query(t.Context(),
		`SELECT account_id, period, amount_cents, currency, state, charge_id, decline_code, answer FROM invoices ORDER BY period, account_id`,
	)
	require.NoError(t, err)
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	require.NoError(t, err)
	return got
}

func month(year int, m time.Month) time.Time {
	return time.Date(year, m, 1, 0, 0, 0, 0, time.UTC)
}

func ptr(s string) *string {
	return &s
}

func TestStore_issuing_twice_issues_once(t *testing.T) {
	db := newDatabase(t)
	ctx := t.Context()
	db.subscribe(t, "acct-a", 1500, "USD", true)
	db.subscribe(t, "acct-b", 990, "EUR", true)
	db.subscribe(t, "acct-c", 4200, "USD", true)
	db.subscribe(t, "acct-gone", 700, "USD", false)
	march := month(2026, time.March)

	issued, err := db.store.Issue(ctx, march)
	require.NoError(t, err)
	require.EqualValues(t, 3, issued)

	issued, err = db.store.Issue(ctx, march)
	require.NoError(t, err)
	require.Zero(t, issued)

	require.Equal(t, []row{
		{AccountID: "acct-a", Period: march, AmountCents: 1500, Currency: "USD", State: "pending"},
		{AccountID: "acct-b", Period: march, AmountCents: 990, Currency: "EUR", State: "pending"},
		{AccountID: "acct-c", Period: march, AmountCents: 4200, Currency: "USD", State: "pending"},
	}, db.invoices(t))

	db.subscribe(t, "acct-d", 100, "USD", true)
	issued, err = db.store.Issue(ctx, march)
	require.NoError(t, err)
	require.EqualValues(t, 1, issued)
	require.Len(t, db.invoices(t), 4)
}

func TestStore_issues_only_a_month(t *testing.T) {
	db := newDatabase(t)
	db.subscribe(t, "acct-a", 1500, "USD", true)

	_, err := db.store.Issue(t.Context(), time.Date(2026, time.March, 15, 0, 0, 0, 0, time.UTC))
	require.ErrorContains(t, err, "issuing 2026-03")
	require.Empty(t, db.invoices(t))
}

func TestStore_settling_never_overwrites_a_final_state(t *testing.T) {
	db := newDatabase(t)
	ctx := t.Context()
	db.subscribe(t, "acct-charged", 1500, "USD", true)
	db.subscribe(t, "acct-declined", 1500, "USD", true)
	db.subscribe(t, "acct-failed", 1500, "USD", true)
	_, err := db.store.Issue(ctx, month(2026, time.March))
	require.NoError(t, err)
	pending, err := db.store.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 3)
	charged, declined, failed := pending[0].ID, pending[1].ID, pending[2].ID

	settled, err := db.store.SettleCharged(ctx, charged, "ch_1")
	require.NoError(t, err)
	require.True(t, settled)
	settled, err = db.store.SettleDeclined(ctx, declined, "card_declined")
	require.NoError(t, err)
	require.True(t, settled)
	settled, err = db.store.SettleFailed(ctx, failed, "422 Unprocessable Entity: currency not supported")
	require.NoError(t, err)
	require.True(t, settled)

	final := map[uuid.UUID]row{
		charged:  {AccountID: "acct-charged", State: "charged", ChargeID: ptr("ch_1")},
		declined: {AccountID: "acct-declined", State: "declined", DeclineCode: ptr("card_declined")},
		failed:   {AccountID: "acct-failed", State: "failed", Answer: ptr("422 Unprocessable Entity: currency not supported")},
	}
	for id := range final {
		settled, err := db.store.SettleCharged(ctx, id, "ch_2")
		require.NoError(t, err)
		require.False(t, settled)
		settled, err = db.store.SettleDeclined(ctx, id, "insufficient_funds")
		require.NoError(t, err)
		require.False(t, settled)
		settled, err = db.store.SettleFailed(ctx, id, "500 Internal Server Error")
		require.NoError(t, err)
		require.False(t, settled)
	}
	for id, want := range final {
		got := db.invoice(t, id)
		require.Equal(t, want.AccountID, got.AccountID)
		require.Equal(t, want.State, got.State)
		require.Equal(t, want.ChargeID, got.ChargeID)
		require.Equal(t, want.DeclineCode, got.DeclineCode)
		require.Equal(t, want.Answer, got.Answer)
	}

	settled, err = db.store.SettleCharged(ctx, uuid.New(), "ch_3")
	require.NoError(t, err)
	require.False(t, settled)

	pending, err = db.store.Pending(ctx)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestStore_pending_invoices_come_oldest_first_across_periods(t *testing.T) {
	db := newDatabase(t)
	ctx := t.Context()
	db.subscribe(t, "acct-a", 1500, "USD", true)
	db.subscribe(t, "acct-b", 990, "EUR", true)
	march, april := month(2026, time.March), month(2026, time.April)

	_, err := db.store.Issue(ctx, april)
	require.NoError(t, err)
	_, err = db.store.Issue(ctx, march)
	require.NoError(t, err)

	pending, err := db.store.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 4)
	for _, inv := range pending {
		require.NotEqual(t, uuid.Nil, inv.ID)
	}
	type key struct {
		AccountID   string
		Period      time.Time
		AmountCents int64
		Currency    string
	}
	keys := make([]key, len(pending))
	for i, inv := range pending {
		keys[i] = key{inv.AccountID, inv.Period, inv.AmountCents, inv.Currency}
	}
	require.Equal(t, []key{
		{"acct-a", march, 1500, "USD"},
		{"acct-b", march, 990, "EUR"},
		{"acct-a", april, 1500, "USD"},
		{"acct-b", april, 990, "EUR"},
	}, keys)

	settled, err := db.store.SettleCharged(ctx, pending[0].ID, "ch_1")
	require.NoError(t, err)
	require.True(t, settled)

	rest, err := db.store.Pending(ctx)
	require.NoError(t, err)
	require.Equal(t, pending[1:], rest)
}

func TestStore_an_abort_at_postgresql_issue_fails_the_statement_and_changes_nothing(t *testing.T) {
	db := newDatabase(t)
	db.subscribe(t, "acct-a", 1500, "USD", true)
	db.subscribe(t, "acct-b", 990, "EUR", true)

	issued, err := db.store.Issue(db.aborting(t, "postgresql:issue"), month(2026, time.March))

	requireInjected(t, err, "postgresql:issue")
	require.Zero(t, issued)
	require.Empty(t, db.invoices(t))

	issued, err = db.store.Issue(db.aborting(t, "postgresql:settle"), month(2026, time.March))
	require.NoError(t, err)
	require.EqualValues(t, 2, issued)
}

func TestStore_an_abort_at_postgresql_settle_fails_the_statement_and_changes_nothing(t *testing.T) {
	db := newDatabase(t)
	db.subscribe(t, "acct-a", 1500, "USD", true)
	_, err := db.store.Issue(t.Context(), month(2026, time.March))
	require.NoError(t, err)
	pending, err := db.store.Pending(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	id := pending[0].ID
	ctx := db.aborting(t, "postgresql:settle")

	settled, err := db.store.SettleCharged(ctx, id, "ch_1")
	requireInjected(t, err, "postgresql:settle")
	require.False(t, settled)
	settled, err = db.store.SettleDeclined(ctx, id, "card_declined")
	requireInjected(t, err, "postgresql:settle")
	require.False(t, settled)
	settled, err = db.store.SettleFailed(ctx, id, "500 Internal Server Error")
	requireInjected(t, err, "postgresql:settle")
	require.False(t, settled)

	require.Equal(t, row{AccountID: "acct-a", Period: month(2026, time.March), AmountCents: 1500, Currency: "USD", State: "pending"}, db.invoice(t, id))

	settled, err = db.store.SettleCharged(db.aborting(t, "postgresql:issue"), id, "ch_1")
	require.NoError(t, err)
	require.True(t, settled)
	require.Equal(t, "charged", db.invoice(t, id).State)
}
