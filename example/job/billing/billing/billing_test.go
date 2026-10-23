// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package billing_test

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/circuit"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"

	"github.com/z5labs/humus/example/job/billing/billing"
	"github.com/z5labs/humus/example/job/billing/payments"
	"github.com/z5labs/humus/example/job/billing/postgres"
)

var _ billing.Invoices = (*postgres.Store)(nil)

type subscription struct {
	accountID   string
	amountCents int64
	currency    string
	active      bool
}

type invoice struct {
	postgres.Invoice
	issued      int
	state       string
	chargeID    string
	declineCode string
	answer      string
}

type store struct {
	mu            sync.Mutex
	subscriptions []subscription
	invoices      []*invoice
	issueErr      error
	settleErr     error
}

func (s *store) Issue(_ context.Context, period time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.issueErr != nil {
		return 0, s.issueErr
	}
	var issued int64
	for _, sub := range s.subscriptions {
		if !sub.active || s.find(sub.accountID, period) != nil {
			continue
		}
		s.invoices = append(s.invoices, &invoice{
			Invoice: postgres.Invoice{
				ID:          uuid.New(),
				AccountID:   sub.accountID,
				Period:      period,
				AmountCents: sub.amountCents,
				Currency:    sub.currency,
			},
			issued: len(s.invoices),
			state:  "pending",
		})
		issued++
	}
	return issued, nil
}

func (s *store) find(accountID string, period time.Time) *invoice {
	for _, inv := range s.invoices {
		if inv.AccountID == accountID && inv.Period.Equal(period) {
			return inv
		}
	}
	return nil
}

func (s *store) Pending(context.Context) ([]postgres.Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []*invoice
	for _, inv := range s.invoices {
		if inv.state == "pending" {
			pending = append(pending, inv)
		}
	}
	slices.SortFunc(pending, func(a, b *invoice) int {
		return cmp.Or(a.Period.Compare(b.Period), cmp.Compare(a.issued, b.issued), strings.Compare(a.AccountID, b.AccountID))
	})
	invoices := make([]postgres.Invoice, 0, len(pending))
	for _, inv := range pending {
		invoices = append(invoices, inv.Invoice)
	}
	return invoices, nil
}

func (s *store) SettleCharged(_ context.Context, id uuid.UUID, chargeID string) (bool, error) {
	return s.settle(id, func(inv *invoice) { inv.state, inv.chargeID = "charged", chargeID })
}

func (s *store) SettleDeclined(_ context.Context, id uuid.UUID, declineCode string) (bool, error) {
	return s.settle(id, func(inv *invoice) { inv.state, inv.declineCode = "declined", declineCode })
}

func (s *store) SettleFailed(_ context.Context, id uuid.UUID, answer string) (bool, error) {
	return s.settle(id, func(inv *invoice) { inv.state, inv.answer = "failed", answer })
}

func (s *store) settle(id uuid.UUID, set func(*invoice)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settleErr != nil {
		return false, s.settleErr
	}
	for _, inv := range s.invoices {
		if inv.ID == id && inv.state == "pending" {
			set(inv)
			return true, nil
		}
	}
	return false, nil
}

func (s *store) all() []invoice {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := make([]invoice, 0, len(s.invoices))
	for _, inv := range s.invoices {
		all = append(all, *inv)
	}
	return all
}

func (s *store) of(t *testing.T, accountID string) []invoice {
	t.Helper()
	var of []invoice
	for _, inv := range s.all() {
		if inv.AccountID == accountID {
			of = append(of, inv)
		}
	}
	return of
}

type sent struct {
	idempotencyKey string
	account        string
}

type api struct {
	mu     sync.Mutex
	sent   []sent
	answer func(r *http.Request, account string, w http.ResponseWriter)
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.sent = append(a.sent, sent{idempotencyKey: r.Header.Get("Idempotency-Key"), account: body.Account})
	a.mu.Unlock()
	a.answer(r, body.Account, w)
}

func (a *api) requests() []sent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sent(nil), a.sent...)
}

func (a *api) to(account string) []sent {
	var to []sent
	for _, s := range a.requests() {
		if s.account == account {
			to = append(to, s)
		}
	}
	return to
}

func charging(r *http.Request, account string, w http.ResponseWriter) {
	if strings.HasPrefix(account, "declined-") {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"code":"card_declined"}`)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"id":"ch_`+r.Header.Get("Idempotency-Key")+`"}`)
}

type env struct {
	host  *humustest.Host
	ca    *humustest.CA
	store *store
	api   *api
	tel   *humustest.Telemetry
	key   string
}

func newEnv(t *testing.T, answer func(*http.Request, string, http.ResponseWriter)) *env {
	t.Helper()
	e := &env{
		host:  humustest.NewHost(t),
		ca:    humustest.NewCA(t),
		store: &store{},
		api:   &api{answer: answer},
		tel:   humustest.NewTelemetry(),
		key:   rand.Text(),
	}
	l, err := e.host.Listen("tcp", ":8443")
	require.NoError(t, err)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	srv := &http.Server{
		Handler:   e.api,
		Protocols: &protocols,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{e.ca.Issue(t, "payments").TLS()}},
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		require.ErrorIs(t, <-done, http.ErrServerClosed)
	})
	return e
}

func (e *env) subscribe(accountIDs ...string) {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	for _, id := range accountIDs {
		e.store.subscriptions = append(e.store.subscriptions, subscription{accountID: id, amountCents: 4999, currency: "USD", active: true})
	}
}

func (e *env) run(t *testing.T, period time.Time, concurrency int) billing.Run {
	t.Helper()
	u, err := url.Parse("https://payments:8443")
	require.NoError(t, err)
	settings := httpclient.DefaultSettings()
	settings.BaseURL = u
	settings.TLS = &tls.Config{RootCAs: e.ca.Pool()}
	sdk := e.sdk(t)
	client, err := payments.NewClient(&humus.Setup{Host: e.host.Host(), Telemetry: sdk}, settings, e.key)
	require.NoError(t, err)
	return billing.Run{Invoices: e.store, Payments: client, Period: period, Concurrency: concurrency, Telemetry: sdk}
}

func (e *env) sdk(t *testing.T) telemetry.SDK {
	t.Helper()
	sdk, err := e.tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	return sdk
}

func month(year int, m time.Month) time.Time {
	return time.Date(year, m, 1, 0, 0, 0, 0, time.UTC)
}

func TestRun_a_month_billed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, charging)
		e.subscribe("acct-1", "acct-2", "acct-3")
		e.store.subscriptions = append(e.store.subscriptions, subscription{accountID: "acct-inactive", amountCents: 999, currency: "USD", active: false})

		err := e.run(t, time.Time{}, 4).Run(t.Context())

		require.NoError(t, err)
		invoices := e.store.all()
		require.Len(t, invoices, 3)
		for _, inv := range invoices {
			require.Equal(t, month(1999, time.December), inv.Period, "the month before the run started, in UTC")
			require.Equal(t, "charged", inv.state)
			require.Equal(t, "ch_"+inv.ID.String(), inv.chargeID)
			to := e.api.to(inv.AccountID)
			require.Len(t, to, 1)
			require.Equal(t, inv.ID.String(), to[0].idempotencyKey)
		}
		require.Empty(t, e.api.to("acct-inactive"))
	})
}

func TestRun_the_period_given(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, charging)
		e.subscribe("acct-1")

		err := e.run(t, month(1999, time.June), 1).Run(t.Context())

		require.NoError(t, err)
		invoices := e.store.of(t, "acct-1")
		require.Len(t, invoices, 1)
		require.Equal(t, month(1999, time.June), invoices[0].Period)
	})
}

func TestRun_the_same_month_run_again_sends_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, charging)
		e.subscribe("acct-1", "acct-2")
		run := e.run(t, month(1999, time.December), 2)
		require.NoError(t, run.Run(t.Context()))
		before := e.store.all()
		require.Len(t, e.api.requests(), 2)

		err := run.Run(t.Context())

		require.NoError(t, err)
		require.Equal(t, before, e.store.all())
		require.Len(t, e.api.requests(), 2)
	})
}

func TestRun_a_declined_card_is_settled_declined_and_never_sent_again(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, charging)
		e.subscribe("declined-1")
		run := e.run(t, month(1999, time.December), 1)

		require.NoError(t, run.Run(t.Context()))
		require.NoError(t, run.Run(t.Context()))

		invoices := e.store.of(t, "declined-1")
		require.Len(t, invoices, 1)
		require.Equal(t, "declined", invoices[0].state)
		require.Equal(t, "card_declined", invoices[0].declineCode)
		require.Len(t, e.api.to("declined-1"), 1)
	})
}

func TestRun_a_failed_answer_is_settled_failed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, func(_ *http.Request, _ string, w http.ResponseWriter) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":"bad currency"}`)
		})
		e.subscribe("acct-1")

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.NoError(t, err)
		invoices := e.store.of(t, "acct-1")
		require.Len(t, invoices, 1)
		require.Equal(t, "failed", invoices[0].state)
		require.Equal(t, `422 Unprocessable Entity: {"error":"bad currency"}`, invoices[0].answer)
	})
}

func TestRun_an_invoice_left_pending_from_an_earlier_month_is_charged_by_a_later_run(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		down := true
		var mu sync.Mutex
		e := newEnv(t, func(r *http.Request, account string, w http.ResponseWriter) {
			mu.Lock()
			defer mu.Unlock()
			if down {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			charging(r, account, w)
		})
		e.subscribe("acct-1")
		require.Error(t, e.run(t, month(1999, time.November), 1).Run(t.Context()))
		november := e.store.of(t, "acct-1")
		require.Len(t, november, 1)
		require.Equal(t, "pending", november[0].state)
		mu.Lock()
		down = false
		mu.Unlock()

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.NoError(t, err)
		invoices := e.store.of(t, "acct-1")
		require.Len(t, invoices, 2)
		for _, inv := range invoices {
			require.Equal(t, "charged", inv.state)
		}
		keys := []string{}
		for _, s := range e.api.to("acct-1") {
			keys = append(keys, s.idempotencyKey)
		}
		require.Equal(t, []string{november[0].ID.String(), november[0].ID.String(), invoices[1].ID.String()}, keys, "the earlier month first, with its own key again")
	})
}

func TestRun_a_500_leaves_its_invoice_pending_with_an_error_that_counts_it(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, func(r *http.Request, account string, w http.ResponseWriter) {
			if account == "acct-500" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"boom"}`)
				return
			}
			charging(r, account, w)
		})
		e.subscribe("acct-1", "acct-500", "acct-3")

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.ErrorContains(t, err, "left 1 of 3 invoices pending")
		require.ErrorContains(t, err, `500 Internal Server Error: {"error":"boom"}`)
		for _, inv := range e.store.all() {
			want := "charged"
			if inv.AccountID == "acct-500" {
				want = "pending"
				require.ErrorContains(t, err, inv.ID.String())
			}
			require.Equal(t, want, inv.state, inv.AccountID)
		}
	})
}

func TestRun_never_more_charges_in_flight_than_the_concurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		inFlight, most := 0, 0
		e := newEnv(t, func(r *http.Request, account string, w http.ResponseWriter) {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()
			time.Sleep(time.Second)
			mu.Lock()
			inFlight--
			mu.Unlock()
			charging(r, account, w)
		})
		for i := range 10 {
			e.subscribe("acct-" + string(rune('a'+i)))
		}

		err := e.run(t, month(1999, time.December), 3).Run(t.Context())

		require.NoError(t, err)
		require.Len(t, e.api.requests(), 10)
		require.Equal(t, 3, most)
	})
}

func TestRun_the_api_down_opens_the_breaker_and_stops_the_run_with_every_unsent_invoice_pending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, func(_ *http.Request, _ string, w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		var accounts []string
		for i := range 10 {
			accounts = append(accounts, fmt.Sprintf("acct-%02d", i))
		}
		e.subscribe(accounts...)

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.ErrorIs(t, err, circuit.ErrRejected)
		require.ErrorIs(t, err, payments.ErrStop)
		require.ErrorContains(t, err, "leaving 10 of 10 invoices pending")
		for _, inv := range e.store.all() {
			require.Equal(t, "pending", inv.state, inv.AccountID)
		}
		var sentTo []string
		for _, s := range e.api.requests() {
			if !slices.Contains(sentTo, s.account) {
				sentTo = append(sentTo, s.account)
			}
		}
		require.Less(t, len(sentTo), len(accounts))
		require.Equal(t, accounts[:len(sentTo)], sentTo, "oldest first, and nothing after the breaker rejected a call")
	})
}

func TestRun_a_401_or_403_stops_the_run_after_the_charges_in_flight(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t, func(r *http.Request, account string, w http.ResponseWriter) {
					if account == "acct-3" {
						time.Sleep(500 * time.Millisecond)
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"error":"bad key"}`)
						return
					}
					time.Sleep(time.Second)
					charging(r, account, w)
				})
				e.subscribe("acct-1", "acct-2", "acct-3", "acct-4", "acct-5")

				err := e.run(t, month(1999, time.December), 3).Run(t.Context())

				require.ErrorIs(t, err, payments.ErrStop)
				require.ErrorContains(t, err, "leaving 3 of 5 invoices pending")
				require.ErrorContains(t, err, `{"error":"bad key"}`)
				for _, inv := range e.store.all() {
					want := "pending"
					if inv.AccountID == "acct-1" || inv.AccountID == "acct-2" {
						want = "charged"
					}
					require.Equal(t, want, inv.state, inv.AccountID)
				}
				require.Empty(t, e.api.to("acct-4"))
				require.Empty(t, e.api.to("acct-5"))
			})
		})
	}
}

func TestRun_a_failed_statement_stops_the_run(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, charging)
		e.subscribe("acct-1", "acct-2", "acct-3")
		e.store.settleErr = errors.New(rand.Text())

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.ErrorIs(t, err, e.store.settleErr)
		require.ErrorContains(t, err, "leaving 3 of 3 invoices pending")
		for _, inv := range e.store.all() {
			require.Equal(t, "pending", inv.state, inv.AccountID)
		}
		require.Len(t, e.api.requests(), 1)
	})
}

func TestRun_a_signal_while_a_charges_answer_is_lost_leaves_its_invoice_pending_for_the_next_run(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signal := errors.New("interrupt")
		ctx, cancel := context.WithCancelCause(t.Context())
		var mu sync.Mutex
		lost := true
		e := newEnv(t, func(r *http.Request, account string, w http.ResponseWriter) {
			mu.Lock()
			lose := lost
			mu.Unlock()
			if lose {
				cancel(signal)
				select {
				case <-r.Context().Done():
				case <-time.After(time.Minute):
				}
				return
			}
			charging(r, account, w)
		})
		e.subscribe("acct-1", "acct-2")

		err := e.run(t, month(1999, time.December), 1).Run(ctx)

		require.Equal(t, signal, err)
		first := e.store.of(t, "acct-1")
		require.Len(t, first, 1)
		require.Equal(t, "pending", first[0].state)
		require.Equal(t, "pending", e.store.of(t, "acct-2")[0].state)
		require.Len(t, e.api.requests(), 1)
		mu.Lock()
		lost = false
		mu.Unlock()

		err = e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.NoError(t, err)
		keys := []string{}
		for _, s := range e.api.to("acct-1") {
			keys = append(keys, s.idempotencyKey)
		}
		require.Equal(t, []string{first[0].ID.String(), first[0].ID.String()}, keys)
		require.Equal(t, "charged", e.store.of(t, "acct-1")[0].state)
		require.Equal(t, "ch_"+first[0].ID.String(), e.store.of(t, "acct-1")[0].chargeID)
	})
}
