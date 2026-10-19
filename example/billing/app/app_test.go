// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/billing/billing"
	"github.com/z5labs/humus/example/billing/postgres"
	"github.com/z5labs/humus/humustest"
)

type memory struct {
	mu       sync.Mutex
	accounts []string
	invoices []*invoice
}

type invoice struct {
	postgres.Invoice
	state    string
	chargeID string
}

func (m *memory) open(context.Context, *humus.Setup, *pgxpool.Config) (billing.Invoices, error) {
	return m, nil
}

func (m *memory) Issue(_ context.Context, period time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var issued int64
	for _, account := range m.accounts {
		if m.find(account, period) {
			continue
		}
		m.invoices = append(m.invoices, &invoice{
			Invoice: postgres.Invoice{ID: uuid.New(), AccountID: account, Period: period, AmountCents: 4999, Currency: "USD"},
			state:   "pending",
		})
		issued++
	}
	return issued, nil
}

func (m *memory) find(account string, period time.Time) bool {
	for _, inv := range m.invoices {
		if inv.AccountID == account && inv.Period.Equal(period) {
			return true
		}
	}
	return false
}

func (m *memory) Pending(context.Context) ([]postgres.Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var pending []postgres.Invoice
	for _, inv := range m.invoices {
		if inv.state == "pending" {
			pending = append(pending, inv.Invoice)
		}
	}
	return pending, nil
}

func (m *memory) SettleCharged(_ context.Context, id uuid.UUID, chargeID string) (bool, error) {
	return m.settle(id, "charged", chargeID)
}

func (m *memory) SettleDeclined(_ context.Context, id uuid.UUID, _ string) (bool, error) {
	return m.settle(id, "declined", "")
}

func (m *memory) SettleFailed(_ context.Context, id uuid.UUID, _ string) (bool, error) {
	return m.settle(id, "failed", "")
}

func (m *memory) settle(id uuid.UUID, state, chargeID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inv := range m.invoices {
		if inv.ID == id && inv.state == "pending" {
			inv.state, inv.chargeID = state, chargeID
			return true, nil
		}
	}
	return false, nil
}

func (m *memory) all() []invoice {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := make([]invoice, 0, len(m.invoices))
	for _, inv := range m.invoices {
		all = append(all, *inv)
	}
	return all
}

type charge struct {
	authorization  string
	idempotencyKey string
	account        string
}

type api struct {
	mu      sync.Mutex
	charges []charge
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/charges" || json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.charges = append(a.charges, charge{
		authorization:  r.Header.Get("Authorization"),
		idempotencyKey: r.Header.Get("Idempotency-Key"),
		account:        body.Account,
	})
	a.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"id":"ch_`+r.Header.Get("Idempotency-Key")+`"}`)
}

func (a *api) sent() []charge {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]charge(nil), a.charges...)
}

type target struct {
	host     *humustest.Host
	store    *memory
	api      *api
	key      string
	password string
	env      map[string]string
}

func newJob(t *testing.T, accounts ...string) *target {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "payments.internal")
	chain := cert.TLS().Certificate
	j := &target{
		store:    &memory{accounts: accounts},
		api:      &api{},
		key:      rand.Text(),
		password: rand.Text(),
	}
	j.host = humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"secrets/database-url": []byte("postgres://billing:" + j.password + "@db.internal:5432/billing?sslmode=disable\n"),
		"secrets/api-key":      []byte(j.key + "\n"),
		"payments-ca.pem":      pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]}),
	}))
	j.env = map[string]string{
		"DATABASE_URL_FILE":     "secrets/database-url",
		"PAYMENTS_URL":          "https://payments.internal:8443",
		"PAYMENTS_API_KEY_FILE": "secrets/api-key",
		"PAYMENTS_CA_FILE":      "payments-ca.pem",
	}

	l, err := j.host.Listen("tcp", "payments.internal:8443")
	require.NoError(t, err)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	srv := &http.Server{
		Handler:   j.api,
		Protocols: &protocols,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		require.ErrorIs(t, <-done, http.ErrServerClosed)
	})
	return j
}

func (j *target) run(t *testing.T, init func(context.Context, *humus.Setup) ([]humus.Service, error), env map[string]string) bedrock.ExitStatus {
	t.Helper()
	all := maps.Clone(j.env)
	for name, value := range env {
		if value == "" {
			delete(all, name)
			continue
		}
		all[name] = value
	}
	_, status := humustest.Start(t, init,
		humustest.WithApp(Options()...),
		humustest.WithApp(humus.WithHost(j.host.Host())),
		humustest.WithEnv(all),
	)
	return <-status
}

func (j *target) inMemory(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, j.store.open)
}

func TestInit_bills_a_month(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1", "acct-2", "acct-3")

		st := j.run(t, j.inMemory, nil)

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		invoices := j.store.all()
		require.Len(t, invoices, 3)
		sent := j.api.sent()
		require.Len(t, sent, 3)
		for _, inv := range invoices {
			require.Equal(t, time.Date(1999, time.December, 1, 0, 0, 0, 0, time.UTC), inv.Period)
			require.Equal(t, "charged", inv.state)
			require.Equal(t, "ch_"+inv.ID.String(), inv.chargeID)
		}
		for _, c := range sent {
			require.Equal(t, "Bearer "+j.key, c.authorization)
		}
	})
}

func TestInit_bills_the_period_given_with_the_concurrency_given(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1", "acct-2")

		st := j.run(t, j.inMemory, map[string]string{"BILLING_PERIOD": "1999-06", "BILLING_CONCURRENCY": "1"})

		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		for _, inv := range j.store.all() {
			require.Equal(t, time.Date(1999, time.June, 1, 0, 0, 0, 0, time.UTC), inv.Period)
			require.Equal(t, "charged", inv.state)
		}
		require.Len(t, j.api.sent(), 2)
	})
}

func TestInit_a_bad_setting_exits_78(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"DATABASE_URL_FILE missing":            {map[string]string{"DATABASE_URL_FILE": ""}, "config DATABASE_URL_FILE: not found"},
		"PAYMENTS_URL missing":                 {map[string]string{"PAYMENTS_URL": ""}, "config PAYMENTS_URL: not found"},
		"PAYMENTS_API_KEY_FILE missing":        {map[string]string{"PAYMENTS_API_KEY_FILE": ""}, "config PAYMENTS_API_KEY_FILE: not found"},
		"PAYMENTS_URL in plain text":           {map[string]string{"PAYMENTS_URL": "http://payments.internal:8443"}, "config PAYMENTS_URL: must be an https URL"},
		"PAYMENTS_URL relative":                {map[string]string{"PAYMENTS_URL": "/v1"}, "config PAYMENTS_URL:"},
		"BILLING_PERIOD not a month":           {map[string]string{"BILLING_PERIOD": "June"}, "config BILLING_PERIOD: must be a month, YYYY-MM"},
		"BILLING_PERIOD month 13":              {map[string]string{"BILLING_PERIOD": "1999-13"}, "config BILLING_PERIOD: must be a month, YYYY-MM"},
		"BILLING_PERIOD this month":            {map[string]string{"BILLING_PERIOD": "2000-01"}, "config BILLING_PERIOD: 2000-01 hasn't ended"},
		"BILLING_PERIOD next month":            {map[string]string{"BILLING_PERIOD": "2000-02"}, "config BILLING_PERIOD: 2000-02 hasn't ended"},
		"BILLING_CONCURRENCY 0":                {map[string]string{"BILLING_CONCURRENCY": "0"}, "config BILLING_CONCURRENCY: must be from 1 to 32"},
		"BILLING_CONCURRENCY 33":               {map[string]string{"BILLING_CONCURRENCY": "33"}, "config BILLING_CONCURRENCY: must be from 1 to 32"},
		"PAYMENTS_MAX_ATTEMPTS 0":              {map[string]string{"PAYMENTS_MAX_ATTEMPTS": "0"}, "config PAYMENTS_MAX_ATTEMPTS:"},
		"PAYMENTS_TIMEOUT not a duration":      {map[string]string{"PAYMENTS_TIMEOUT": "soon"}, "config PAYMENTS_TIMEOUT:"},
		"PAYMENTS_CA_FILE not found":           {map[string]string{"PAYMENTS_CA_FILE": "nowhere.pem"}, "config PAYMENTS_CA_FILE:"},
		"PAYMENTS_API_KEY_FILE not found":      {map[string]string{"PAYMENTS_API_KEY_FILE": "secrets/nowhere"}, "config PAYMENTS_API_KEY_FILE:"},
		"OTEL_TRACES_EXPORTER not an exporter": {map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"}, "config OTEL_TRACES_EXPORTER:"},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				j := newJob(t, "acct-1")

				st := j.run(t, j.inMemory, tc.env)

				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), tc.want)
				require.Empty(t, j.store.all())
				require.Empty(t, j.api.sent())
			})
		})
	}
}

func TestInit_without_its_settings_exits_78_naming_each(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init,
			humustest.WithApp(Options()...),
			humustest.WithApp(humus.WithHost(humustest.NewHost(t).Host())),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		for _, name := range []string{"DATABASE_URL_FILE", "PAYMENTS_URL", "PAYMENTS_API_KEY_FILE"} {
			require.ErrorContains(t, st.Cause(), "config "+name+": not found")
		}
	})
}

func TestInit_never_repeats_its_secrets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1")
		password := rand.Text()
		require.NoError(t, os.WriteFile(j.host.Path("secrets/bad-url"), []byte("postgres://billing:"+password+"@db.internal:port/billing"), 0o600))

		st := j.run(t, j.inMemory, map[string]string{"DATABASE_URL_FILE": "secrets/bad-url"})

		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), "config DATABASE_URL_FILE: must hold a PostgreSQL URL")
		require.NotContains(t, st.Cause().Error(), password)
		require.NotContains(t, st.Cause().Error(), j.key)
	})
}

func TestInit_reads_no_secret_from_the_environment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1")

		st := j.run(t, j.inMemory, map[string]string{
			"DATABASE_URL_FILE":     "",
			"PAYMENTS_API_KEY_FILE": "",
			"DATABASE_URL":          "postgres://billing:" + j.password + "@db.internal:5432/billing?sslmode=disable",
			"PAYMENTS_API_KEY":      j.key,
		})

		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), "config DATABASE_URL_FILE: not found")
		require.ErrorContains(t, st.Cause(), "config PAYMENTS_API_KEY_FILE: not found")
	})
}

func TestInit_dials_PostgreSQL_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		j := newJob(t, "acct-1")
		l, err := j.host.Listen("tcp", "db.internal:5432")
		require.NoError(t, err)
		var (
			mu      sync.Mutex
			dialled int
		)
		accepted := make(chan struct{})
		go func() {
			defer close(accepted)
			for {
				conn, err := l.Accept()
				if errors.Is(err, net.ErrClosed) {
					return
				}
				if err != nil {
					continue
				}
				mu.Lock()
				dialled++
				mu.Unlock()
				_ = conn.Close()
			}
		}()

		st := j.run(t, Init, nil)

		require.NoError(t, l.Close())
		<-accepted
		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		require.True(t, strings.HasPrefix(st.Cause().Error(), "serve: "), "cause: %v", st.Cause())
		mu.Lock()
		defer mu.Unlock()
		require.Positive(t, dialled)
		require.Empty(t, j.api.sent())
	})
}
