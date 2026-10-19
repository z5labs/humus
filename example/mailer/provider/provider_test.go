// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package provider_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/mailer/mail"
	"github.com/z5labs/humus/example/mailer/provider"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

type received struct {
	method        string
	path          string
	contentType   string
	key           string
	authorization string
	body          map[string]string
}

type recorder struct {
	mu       sync.Mutex
	requests []received
	answer   func(n int, w http.ResponseWriter)
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]string
	_ = json.Unmarshal(b, &body)
	rec.mu.Lock()
	rec.requests = append(rec.requests, received{
		method:        r.Method,
		path:          r.URL.Path,
		contentType:   r.Header.Get("Content-Type"),
		key:           r.Header.Get("Idempotency-Key"),
		authorization: r.Header.Get("Authorization"),
		body:          body,
	})
	n := len(rec.requests)
	rec.mu.Unlock()
	rec.answer(n, w)
}

func (rec *recorder) all() []received {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]received(nil), rec.requests...)
}

func answer(code int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

func always(a func(http.ResponseWriter)) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) { a(w) }
}

func startProvider(t *testing.T, h *humustest.Host, ca *humustest.CA, handler http.Handler) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "provider")
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	select {
	case <-h.Listening("provider:8443"):
	case st := <-status:
		t.Fatalf("the provider exited before it listened: %v", st.Cause())
	}
	return func() {
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}

func settings(t *testing.T, ca *humustest.CA) provider.Settings {
	t.Helper()
	u, err := url.Parse("https://provider:8443")
	require.NoError(t, err)
	d := httpclient.DefaultSettings()
	return provider.Settings{
		URL:            u,
		CA:             ca.Pool(),
		Timeout:        d.Timeout,
		AttemptTimeout: d.AttemptTimeout,
		MaxAttempts:    d.MaxAttempts,
		From:           "mailer@example.com",
		APIKey:         rand.Text(),
	}
}

func run(t *testing.T, h *humustest.Host, settings provider.Settings, f func(ctx context.Context, c *provider.Client)) {
	t.Helper()
	_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		c, err := provider.New(s, settings)
		if err != nil {
			return nil, err
		}
		return []humus.Service{job.NewService(s, func(ctx context.Context) error {
			f(ctx, c)
			return nil
		})}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

type sendResult struct {
	result provider.Result
	err    error
}

func sendOnce(t *testing.T, h *humustest.Host, settings provider.Settings, r mail.Request) sendResult {
	t.Helper()
	var got sendResult
	run(t, h, settings, func(ctx context.Context, c *provider.Client) {
		got.result, got.err = c.Send(ctx, r)
	})
	return got
}

func request() mail.Request {
	return mail.Request{
		MessageID:     "7f6c1f0e-2b3a-4c5d-8e9f-0a1b2c3d4e5f",
		CorrelationID: "order-42",
		To:            "ada@example.com",
		Subject:       "Your order has shipped",
		Text:          "Order 42 is on its way.",
	}
}

func TestSend_sends_the_idempotency_key_the_bearer_token_and_the_body(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: always(answer(http.StatusAccepted, `{"id":"msg_1"}`))}
		stop := startProvider(t, h, ca, rec)
		defer stop()
		s := settings(t, ca)

		got := sendOnce(t, h, s, request())

		require.NoError(t, got.err)
		require.Equal(t, []received{{
			method:        http.MethodPost,
			path:          "/v1/messages",
			contentType:   "application/json",
			key:           "7f6c1f0e-2b3a-4c5d-8e9f-0a1b2c3d4e5f",
			authorization: "Bearer " + s.APIKey,
			body: map[string]string{
				"from":    "mailer@example.com",
				"to":      "ada@example.com",
				"subject": "Your order has shipped",
				"text":    "Order 42 is on its way.",
			},
		}}, rec.all())
	})
}

func TestSend_joins_its_path_onto_the_URLs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: always(answer(http.StatusAccepted, `{"id":"msg_1"}`))}
		stop := startProvider(t, h, ca, rec)
		defer stop()
		s := settings(t, ca)
		s.URL = s.URL.JoinPath("mail")

		got := sendOnce(t, h, s, request())

		require.NoError(t, got.err)
		require.Len(t, rec.all(), 1)
		require.Equal(t, "/mail/v1/messages", rec.all()[0].path)
	})
}

func TestSend_a_202_is_sent_with_its_ID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: always(answer(http.StatusAccepted, `{"id":"msg_7f6c1f0e"}`))}
		stop := startProvider(t, h, ca, rec)
		defer stop()

		got := sendOnce(t, h, settings(t, ca), request())

		require.NoError(t, got.err)
		require.Equal(t, provider.Result{ID: "msg_7f6c1f0e"}, got.result)
	})
}

func TestSend_a_422_is_refused_with_its_reason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: always(answer(http.StatusUnprocessableEntity, `{"reason":"ada@example.invalid can't receive email"}`))}
		stop := startProvider(t, h, ca, rec)
		defer stop()

		got := sendOnce(t, h, settings(t, ca), request())

		require.NoError(t, got.err)
		require.Equal(t, provider.Result{Refused: true, Reason: "ada@example.invalid can't receive email"}, got.result)
		require.Len(t, rec.all(), 1)
	})
}

func TestSend_an_answer_it_cant_read_is_an_error(t *testing.T) {
	testCases := []struct {
		name string
		code int
		body string
	}{
		{name: "a 202 without an id", code: http.StatusAccepted, body: `{}`},
		{name: "a 202 that isn't JSON", code: http.StatusAccepted, body: `accepted`},
		{name: "a 422 without a reason", code: http.StatusUnprocessableEntity, body: `{}`},
		{name: "a 401 that isn't JSON", code: http.StatusUnauthorized, body: `unauthorized`},
		{name: "a 301", code: http.StatusMovedPermanently, body: `{}`},
		{name: "a 501", code: http.StatusNotImplemented, body: `{"reason":"no"}`},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				ca := humustest.NewCA(t)
				rec := &recorder{answer: always(answer(tc.code, tc.body))}
				stop := startProvider(t, h, ca, rec)
				defer stop()

				got := sendOnce(t, h, settings(t, ca), request())

				require.Error(t, got.err)
				require.Zero(t, got.result)
			})
		})
	}
}

func TestSend_a_409_then_a_202_is_retried_and_sent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: func(n int, w http.ResponseWriter) {
			if n == 1 {
				answer(http.StatusConflict, `{"reason":"a request with this key is in flight"}`)(w)
				return
			}
			answer(http.StatusAccepted, `{"id":"msg_1"}`)(w)
		}}
		stop := startProvider(t, h, ca, rec)
		defer stop()

		got := sendOnce(t, h, settings(t, ca), request())

		require.NoError(t, got.err)
		require.Equal(t, provider.Result{ID: "msg_1"}, got.result)
		requests := rec.all()
		require.Len(t, requests, 2)
		require.Equal(t, requests[0], requests[1])
	})
}

func TestSend_a_409_counts_for_the_provider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: always(answer(http.StatusConflict, `{}`))}
		stop := startProvider(t, h, ca, rec)
		defer stop()
		s := settings(t, ca)
		s.MaxAttempts = 1

		var errs []error
		run(t, h, s, func(ctx context.Context, c *provider.Client) {
			for range 10 {
				_, err := c.Send(ctx, request())
				errs = append(errs, err)
			}
		})

		require.Len(t, rec.all(), 10)
		for _, err := range errs {
			require.Error(t, err)
			require.NotErrorIs(t, err, circuit.ErrRejected)
		}
	})
}

func TestSend_a_429_within_BackoffMax_is_retried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: func(n int, w http.ResponseWriter) {
			if n == 1 {
				w.Header().Set("Retry-After", "2")
				answer(http.StatusTooManyRequests, `{}`)(w)
				return
			}
			answer(http.StatusAccepted, `{"id":"msg_1"}`)(w)
		}}
		stop := startProvider(t, h, ca, rec)
		defer stop()

		var took time.Duration
		var got sendResult
		run(t, h, settings(t, ca), func(ctx context.Context, c *provider.Client) {
			start := time.Now()
			got.result, got.err = c.Send(ctx, request())
			took = time.Since(start)
		})

		require.NoError(t, got.err)
		require.Equal(t, provider.Result{ID: "msg_1"}, got.result)
		require.Len(t, rec.all(), 2)
		require.GreaterOrEqual(t, took, 2*time.Second)
	})
}

func TestSend_a_429_beyond_BackoffMax_is_an_error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: func(_ int, w http.ResponseWriter) {
			w.Header().Set("Retry-After", "60")
			answer(http.StatusTooManyRequests, `{}`)(w)
		}}
		stop := startProvider(t, h, ca, rec)
		defer stop()

		got := sendOnce(t, h, settings(t, ca), request())

		require.Error(t, got.err)
		require.Zero(t, got.result)
		require.Len(t, rec.all(), 1)
	})
}

func TestSend_5xx_answers_open_the_breaker_and_the_calls_after_are_shed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: always(answer(http.StatusServiceUnavailable, `{}`))}
		stop := startProvider(t, h, ca, rec)
		defer stop()

		var opened bool
		var sentBefore, sentAfter int
		var shed error
		run(t, h, settings(t, ca), func(ctx context.Context, c *provider.Client) {
			for range 10 {
				_, err := c.Send(ctx, request())
				if errors.Is(err, circuit.ErrRejected) {
					opened = true
					break
				}
			}
			sentBefore = len(rec.all())
			_, shed = c.Send(ctx, request())
			sentAfter = len(rec.all())
		})

		require.True(t, opened, "the breaker never opened")
		require.ErrorIs(t, shed, circuit.ErrRejected)
		require.Equal(t, sentBefore, sentAfter)
	})
}

func TestSend_trusts_the_systems_roots_without_a_CA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		rec := &recorder{answer: always(answer(http.StatusAccepted, `{"id":"msg_1"}`))}
		stop := startProvider(t, h, ca, rec)
		defer stop()
		s := settings(t, ca)
		s.CA = nil

		got := sendOnce(t, h, s, request())

		var untrusted *tls.CertificateVerificationError
		require.ErrorAs(t, got.err, &untrusted)
		require.Empty(t, rec.all())
	})
}

func TestNew_rejects_bad_settings(t *testing.T) {
	testCases := []struct {
		name   string
		change func(*provider.Settings)
		want   string
	}{
		{name: "no sender's address", change: func(s *provider.Settings) { s.From = "" }, want: "Settings.From"},
		{name: "no API key", change: func(s *provider.Settings) { s.APIKey = "" }, want: "Settings.APIKey"},
		{name: "a CA for an http URL", change: func(s *provider.Settings) { s.URL.Scheme = "http" }, want: "Settings.CA"},
		{name: "no URL", change: func(s *provider.Settings) { s.URL = nil }, want: "Settings.BaseURL"},
		{name: "no Timeout", change: func(s *provider.Settings) { s.Timeout = 0 }, want: "Settings.Timeout"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := humustest.NewHost(t)
				s := settings(t, humustest.NewCA(t))
				tc.change(&s)

				_, status := humustest.Start(t, func(_ context.Context, setup *humus.Setup) ([]humus.Service, error) {
					_, err := provider.New(setup, s)
					return nil, err
				}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
				st := <-status

				require.NotZero(t, st.Code())
				require.ErrorContains(t, st.Cause(), tc.want)
			})
		})
	}
}
