// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the billing job's black-box cases, run against a deployment of it.
package cases

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

const (
	exitConfig   = 78
	period       = "2025-12"
	declinedCode = "card_declined"
)

type Target struct {
	Postgres string
	Schema   string
	WireMock *url.URL
	Client   *http.Client
	Key      string
	Run      func(ctx context.Context, databaseURL string, env map[string]string) (int, error)
	Timeout  time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "BillingAMonth", Run: BillingAMonth},
	{Name: "RunningAgain", Run: RunningAgain},
	{Name: "ADeclinedCard", Run: ADeclinedCard},
	{Name: "ABadSetting", Run: ABadSetting},
}

func BillingAMonth(t testing.TB, target Target) {
	ctx := bounded(t, target)
	db := newDatabase(ctx, t, target, "billing-")
	active := db.subscribe(ctx, t, "a", "b", "c")
	inactive := db.unsubscribed(ctx, t, "gone")

	require.Equal(t, 0, db.run(ctx, t, target, period))

	invoices := db.invoices(ctx, t)
	require.Equal(t, active, accounts(invoices))
	for _, inv := range invoices {
		require.Equal(t, period, inv.Period)
		require.Equal(t, "charged", inv.State)
		require.Equal(t, "ch_"+inv.ID, inv.ChargeID)
	}
	charges := db.journal(ctx, t, target)
	requireSentOnce(t, target, invoices, charges)
	for _, c := range charges {
		require.NotEqual(t, inactive, c.account(t))
	}
}

func RunningAgain(t testing.TB, target Target) {
	ctx := bounded(t, target)
	db := newDatabase(ctx, t, target, "billing-")
	db.subscribe(ctx, t, "a", "b", "c")
	require.Equal(t, 0, db.run(ctx, t, target, period))
	billed := db.invoices(ctx, t)
	require.Len(t, billed, 3)

	require.Equal(t, 0, db.run(ctx, t, target, period))

	require.Equal(t, billed, db.invoices(ctx, t))
	requireSentOnce(t, target, billed, db.journal(ctx, t, target))
}

func ADeclinedCard(t testing.TB, target Target) {
	ctx := bounded(t, target)
	db := newDatabase(ctx, t, target, "declined-")
	db.subscribe(ctx, t, "a")

	require.Equal(t, 0, db.run(ctx, t, target, period))

	invoices := db.invoices(ctx, t)
	require.Len(t, invoices, 1)
	require.Equal(t, "declined", invoices[0].State)
	require.Equal(t, declinedCode, invoices[0].DeclineCode)
	require.Empty(t, invoices[0].ChargeID)
	requireSentOnce(t, target, invoices, db.journal(ctx, t, target))

	require.Equal(t, 0, db.run(ctx, t, target, period))

	require.Equal(t, invoices, db.invoices(ctx, t))
	requireSentOnce(t, target, invoices, db.journal(ctx, t, target))
}

func ABadSetting(t testing.TB, target Target) {
	ctx := bounded(t, target)
	db := newDatabase(ctx, t, target, "billing-")
	db.subscribe(ctx, t, "a", "b")
	now := time.Now().UTC()
	unended := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01")

	for _, setting := range []string{unended, "last-month"} {
		require.Equal(t, exitConfig, db.run(ctx, t, target, setting), "BILLING_PERIOD=%s", setting)
		require.Empty(t, db.invoices(ctx, t), "BILLING_PERIOD=%s", setting)
		require.Empty(t, db.journal(ctx, t, target), "BILLING_PERIOD=%s", setting)
	}
}

func bounded(t testing.TB, target Target) context.Context {
	require.Positive(t, target.Timeout, "Target.Timeout")
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	t.Cleanup(cancel)
	return ctx
}

type database struct {
	name string
	url  string
	conn *pgx.Conn
}

func newDatabase(ctx context.Context, t testing.TB, target Target, prefix string) database {
	t.Helper()
	server, err := url.Parse(target.Postgres)
	require.NoError(t, err, "Target.Postgres")

	admin, err := pgx.Connect(ctx, target.Postgres)
	require.NoError(t, err, "connecting to PostgreSQL")
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	name := prefix + strings.ToLower(rand.Text())
	ident := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE DATABASE "+ident)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), target.Timeout)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP DATABASE "+ident+" WITH (FORCE)")
		require.NoError(t, err)
	})

	own := *server
	own.Path = "/" + name
	conn, err := pgx.Connect(ctx, own.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	_, err = conn.Exec(ctx, target.Schema)
	require.NoError(t, err, "applying Target.Schema")

	return database{name: name, url: own.String(), conn: conn}
}

func (db database) account(suffix string) string {
	return db.name + "-" + suffix
}

func (db database) subscribe(ctx context.Context, t testing.TB, suffixes ...string) []string {
	t.Helper()
	return db.insert(ctx, t, true, suffixes)
}

func (db database) unsubscribed(ctx context.Context, t testing.TB, suffix string) string {
	t.Helper()
	return db.insert(ctx, t, false, []string{suffix})[0]
}

func (db database) insert(ctx context.Context, t testing.TB, active bool, suffixes []string) []string {
	t.Helper()
	var ids []string
	for _, suffix := range suffixes {
		id := db.account(suffix)
		_, err := db.conn.Exec(ctx,
			`INSERT INTO subscriptions (account_id, amount_cents, currency, active) VALUES ($1, $2, $3, $4)`,
			id, 4999, "USD", active,
		)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	return ids
}

func (db database) run(ctx context.Context, t testing.TB, target Target, billingPeriod string) int {
	t.Helper()
	code, err := target.Run(ctx, db.url, map[string]string{"BILLING_PERIOD": billingPeriod})
	require.NoError(t, err, "running the billing job")
	return code
}

type invoice struct {
	ID          string
	AccountID   string
	Period      string
	State       string
	ChargeID    string
	DeclineCode string
}

func (db database) invoices(ctx context.Context, t testing.TB) []invoice {
	t.Helper()
	rows, err := db.conn.Query(ctx, `
		SELECT id::text, account_id, to_char(period, 'YYYY-MM'), state, coalesce(charge_id, ''), coalesce(decline_code, '')
		FROM invoices ORDER BY account_id, period`,
	)
	require.NoError(t, err)
	invoices, err := pgx.CollectRows(rows, pgx.RowToStructByPos[invoice])
	require.NoError(t, err)
	return invoices
}

func accounts(invoices []invoice) []string {
	var ids []string
	for _, inv := range invoices {
		ids = append(ids, inv.AccountID)
	}
	return ids
}

type sent struct {
	Headers map[string]any `json:"headers"`
	Body    string         `json:"body"`
}

func (s sent) header(name string) string {
	for k, v := range s.Headers {
		if strings.EqualFold(k, name) {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func (s sent) account(t testing.TB) string {
	t.Helper()
	var body struct {
		Account string `json:"account"`
	}
	require.NoError(t, json.Unmarshal([]byte(s.Body), &body), "the body of a charge: %s", s.Body)
	return body.Account
}

func requireSentOnce(t testing.TB, target Target, invoices []invoice, charges []sent) {
	t.Helper()
	require.Len(t, charges, len(invoices), "one charge per invoice")
	byKey := make(map[string]sent, len(charges))
	for _, c := range charges {
		key := c.header("Idempotency-Key")
		require.NotContains(t, byKey, key, "a second charge with the key %s", key)
		byKey[key] = c
	}
	for _, inv := range invoices {
		c, ok := byKey[inv.ID]
		require.True(t, ok, "no charge with the key of invoice %s", inv.ID)
		require.Equal(t, inv.AccountID, c.account(t))
		require.Equal(t, "Bearer "+target.Key, c.header("Authorization"))
	}
}

func (db database) journal(ctx context.Context, t testing.TB, target Target) []sent {
	t.Helper()
	query, err := json.Marshal(map[string]any{
		"method": http.MethodPost,
		"url":    "/v1/charges",
		"bodyPatterns": []any{
			map[string]any{"matchesJsonPath": map[string]any{"expression": "$.account", "matches": "^" + db.name + "-.*"}},
		},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.WireMock.JoinPath("__admin", "requests", "find").String(), bytes.NewReader(query))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := target.Client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)

	var found struct {
		Requests []sent `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(body, &found))
	return found.Requests
}
