// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package payments_test

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/fit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"

	"github.com/z5labs/humus/example/job/billing/payments"
)

type sent struct {
	method         string
	path           string
	authorization  string
	idempotencyKey string
	contentType    string
	baggage        string
	body           map[string]any
}

type api struct {
	mu     sync.Mutex
	sent   []sent
	answer func(n int, r *http.Request, w http.ResponseWriter)
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.sent = append(a.sent, sent{
		method:         r.Method,
		path:           r.URL.Path,
		authorization:  r.Header.Get("Authorization"),
		idempotencyKey: r.Header.Get("Idempotency-Key"),
		contentType:    r.Header.Get("Content-Type"),
		baggage:        r.Header.Get("baggage"),
		body:           body,
	})
	n := len(a.sent)
	a.mu.Unlock()
	a.answer(n, r, w)
}

func (a *api) requests() []sent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sent(nil), a.sent...)
}

func answering(status int, body string) func(int, *http.Request, http.ResponseWriter) {
	return func(_ int, _ *http.Request, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

type env struct {
	host *humustest.Host
	ca   *humustest.CA
	key  string
	api  *api
}

func newEnv(t *testing.T, answer func(int, *http.Request, http.ResponseWriter)) *env {
	t.Helper()
	e := &env{
		host: humustest.NewHost(t),
		ca:   humustest.NewCA(t),
		key:  rand.Text(),
		api:  &api{answer: answer},
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

func (e *env) settings(t *testing.T) httpclient.Settings {
	t.Helper()
	u, err := url.Parse("https://payments:8443")
	require.NoError(t, err)
	settings := httpclient.DefaultSettings()
	settings.BaseURL = u
	settings.TLS = &tls.Config{RootCAs: e.ca.Pool()}
	return settings
}

func (e *env) client(t *testing.T) *payments.Client {
	t.Helper()
	return e.clientWith(t, &humus.Setup{Host: e.host.Host(), Telemetry: telemetry.Noop()}, e.settings(t))
}

func (e *env) clientWith(t *testing.T, s *humus.Setup, settings httpclient.Settings) *payments.Client {
	t.Helper()
	c, err := payments.NewClient(s, settings, e.key)
	require.NoError(t, err)
	return c
}

func newCharge() payments.Charge {
	return payments.Charge{ID: uuid.New(), AccountID: "acct-" + rand.Text(), AmountCents: 4999, Currency: "USD"}
}

func TestClient_Charge_the_request(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, answering(http.StatusCreated, `{"id":"ch_1"}`))
		ch := newCharge()

		_, err := e.client(t).Charge(t.Context(), ch)
		require.NoError(t, err)

		got := e.api.requests()
		require.Len(t, got, 1)
		require.Equal(t, http.MethodPost, got[0].method)
		require.Equal(t, "/v1/charges", got[0].path)
		require.Equal(t, "Bearer "+e.key, got[0].authorization)
		require.Equal(t, ch.ID.String(), got[0].idempotencyKey)
		require.Equal(t, "application/json", got[0].contentType)
		require.Equal(t, map[string]any{"account": ch.AccountID, "amount": float64(4999), "currency": "USD"}, got[0].body)
	})
}

func TestClient_Charge_the_answers(t *testing.T) {
	testCases := []struct {
		name     string
		status   int
		body     string
		want     payments.Result
		pending  bool
		stop     bool
		attempts int
	}{
		{name: "201 is charged", status: 201, body: `{"id":"ch_1"}`, want: payments.Result{State: payments.Charged, ChargeID: "ch_1"}, attempts: 1},
		{name: "200 with an earlier answer is charged", status: 200, body: `{"id":"ch_0"}`, want: payments.Result{State: payments.Charged, ChargeID: "ch_0"}, attempts: 1},
		{name: "201 without a charge ID failed", status: 201, body: `{}`, want: payments.Result{State: payments.Failed, Answer: "201 Created: {}"}, attempts: 1},
		{name: "402 is declined", status: 402, body: `{"code":"card_declined"}`, want: payments.Result{State: payments.Declined, DeclineCode: "card_declined"}, attempts: 1},
		{name: "409 is retried and left pending", status: 409, body: `{}`, pending: true, attempts: 3},
		{name: "401 is left pending and stops the run", status: 401, pending: true, stop: true, attempts: 1},
		{name: "403 is left pending and stops the run", status: 403, pending: true, stop: true, attempts: 1},
		{name: "422 failed", status: 422, body: `{"error":"bad currency"}`, want: payments.Result{State: payments.Failed, Answer: `422 Unprocessable Entity: {"error":"bad currency"}`}, attempts: 1},
		{name: "400 failed", status: 400, want: payments.Result{State: payments.Failed, Answer: "400 Bad Request"}, attempts: 1},
		{name: "404 failed", status: 404, want: payments.Result{State: payments.Failed, Answer: "404 Not Found"}, attempts: 1},
		{name: "429 is retried and left pending", status: 429, pending: true, attempts: 3},
		{name: "502 is retried and left pending", status: 502, pending: true, attempts: 3},
		{name: "503 is retried and left pending", status: 503, pending: true, attempts: 3},
		{name: "504 is retried and left pending", status: 504, pending: true, attempts: 3},
		{name: "500 is left pending", status: 500, pending: true, attempts: 1},
		{name: "501 is left pending", status: 501, pending: true, attempts: 1},
		{name: "507 is left pending", status: 507, pending: true, attempts: 1},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEnv(t, answering(tc.status, tc.body))

				got, err := e.client(t).Charge(t.Context(), newCharge())

				require.Len(t, e.api.requests(), tc.attempts)
				if !tc.pending {
					require.NoError(t, err)
					require.Equal(t, tc.want, got)
					return
				}
				require.Error(t, err)
				require.Zero(t, got)
				require.Equal(t, tc.stop, errors.Is(err, payments.ErrStop), "err: %v", err)
			})
		})
	}
}

func TestClient_Charge_a_transport_error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, func(_ int, _ *http.Request, w http.ResponseWriter) {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = conn.Close()
		})

		got, err := e.client(t).Charge(t.Context(), newCharge())

		require.Error(t, err)
		require.NotErrorIs(t, err, payments.ErrStop)
		require.Zero(t, got)
		require.GreaterOrEqual(t, len(e.api.requests()), 3)
	})
}

func TestClient_Charge_retries_a_POST_with_the_same_key(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		charged := map[string]int{}
		e := newEnv(t, func(n int, r *http.Request, w http.ResponseWriter) {
			if n <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			key := r.Header.Get("Idempotency-Key")
			mu.Lock()
			charged[key]++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"ch_`+key+`"}`)
		})
		ch := newCharge()

		got, err := e.client(t).Charge(t.Context(), ch)

		require.NoError(t, err)
		require.Equal(t, payments.Result{State: payments.Charged, ChargeID: "ch_" + ch.ID.String()}, got)
		sent := e.api.requests()
		require.Len(t, sent, 3)
		for _, s := range sent {
			require.Equal(t, ch.ID.String(), s.idempotencyKey)
			require.Equal(t, sent[0].body, s.body)
		}
		require.Equal(t, map[string]int{ch.ID.String(): 1}, charged)
	})
}

func TestClient_Charge_the_breaker(t *testing.T) {
	t.Run("opens after repeated failures and rejects the next call", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, answering(http.StatusInternalServerError, ""))
			c := e.client(t)

			for range 5 {
				_, err := c.Charge(t.Context(), newCharge())
				require.Error(t, err)
				require.NotErrorIs(t, err, payments.ErrStop)
			}
			require.Len(t, e.api.requests(), 5)

			got, err := c.Charge(t.Context(), newCharge())

			require.ErrorIs(t, err, payments.ErrStop)
			require.ErrorIs(t, err, circuit.ErrRejected)
			require.Zero(t, got)
			require.Len(t, e.api.requests(), 5)
		})
	})

	t.Run("stays closed through answers that are 4xx", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t, func(n int, r *http.Request, w http.ResponseWriter) {
				if n <= 20 {
					answering(http.StatusUnprocessableEntity, "")(n, r, w)
					return
				}
				answering(http.StatusCreated, `{"id":"ch_1"}`)(n, r, w)
			})
			c := e.client(t)

			for range 20 {
				got, err := c.Charge(t.Context(), newCharge())
				require.NoError(t, err)
				require.Equal(t, payments.Failed, got.State)
			}
			got, err := c.Charge(t.Context(), newCharge())

			require.NoError(t, err)
			require.Equal(t, payments.Charged, got.State)
		})
	})
}

func TestClient_never_forwards_a_scenario(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, answering(http.StatusCreated, `{"id":"ch_1"}`))
		codec, err := jsonscenario.New("acme.fault")
		require.NoError(t, err)
		fs := fault.DefaultSettings()
		fs.Decoder = codec
		faults, err := fault.New(fs)
		require.NoError(t, err)
		settings := e.settings(t)
		settings.ForwardScenario = true
		c := e.clientWith(t, &humus.Setup{Host: e.host.Host(), Telemetry: telemetry.Noop(), Faults: faults}, settings)

		tenant, err := baggage.NewMemberRaw("tenant", "acme")
		require.NoError(t, err)
		b, err := baggage.New(tenant)
		require.NoError(t, err)
		ctx := baggage.ContextWithBaggage(t.Context(), b)
		ctx, err = fit.ContextWithScenario(ctx, codec, fit.Scenario{Rules: []fit.Rule{{Point: "postgresql:settle", Fault: fit.Fault{Abort: true}}}})
		require.NoError(t, err)

		_, err = c.Charge(ctx, newCharge())
		require.NoError(t, err)

		sent := e.api.requests()
		require.Len(t, sent, 1)
		got, err := baggage.Parse(sent[0].baggage)
		require.NoError(t, err)
		require.Equal(t, "acme", got.Member("tenant").Value())
		require.Empty(t, got.Member("acme.fault").Key())
	})
}

func TestClassify(t *testing.T) {
	testCases := []struct {
		status int
		want   httpclient.Classification
	}{
		{status: 200, want: httpclient.Classification{Retry: retry.Success, Breaker: circuit.Success}},
		{status: 201, want: httpclient.Classification{Retry: retry.Success, Breaker: circuit.Success}},
		{status: 400, want: httpclient.Classification{Retry: retry.Success, Breaker: circuit.Success}},
		{status: 401, want: httpclient.Classification{Retry: retry.Success, Breaker: circuit.Success}},
		{status: 402, want: httpclient.Classification{Retry: retry.Success, Breaker: circuit.Success}},
		{status: 403, want: httpclient.Classification{Retry: retry.Success, Breaker: circuit.Success}},
		{status: 409, want: httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}},
		{status: 422, want: httpclient.Classification{Retry: retry.Success, Breaker: circuit.Success}},
		{status: 429, want: httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}},
		{status: 500, want: httpclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}},
		{status: 501, want: httpclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}},
		{status: 502, want: httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}},
		{status: 503, want: httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}},
		{status: 504, want: httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}},
		{status: 505, want: httpclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}},
		{status: 599, want: httpclient.Classification{Retry: retry.Stop, Breaker: circuit.Failure}},
	}
	for _, tc := range testCases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: http.NoBody}
			require.Equal(t, tc.want, payments.Classify(resp, nil))
		})
	}

	t.Run("a transport error", func(t *testing.T) {
		require.Equal(t, httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}, payments.Classify(nil, errors.New("connection reset")))
	})
}
