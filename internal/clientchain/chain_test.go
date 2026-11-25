// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package clientchain_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/fit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/internal/clientchain"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file build the chain on a bottom Transport of their
// own, which answers each attempt as the test says, so they need no
// network. httpclient's and http3client's tests run the chain over real
// connections.

// name is the client's package the tests' chains are built for.
const name = "someclient"

// bottom is a Transport that answers each attempt with answer, and counts
// the attempts and its CloseIdleConnections calls.
type bottom struct {
	answer   func(n int, r *http.Request) (*http.Response, error)
	attempts atomic.Int32
	closed   atomic.Int32

	mu      sync.Mutex
	headers []http.Header
	bodies  []string
}

func (b *bottom) RoundTrip(r *http.Request) (*http.Response, error) {
	n := int(b.attempts.Add(1))
	var body string
	if r.Body != nil {
		got, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		body = string(got)
	}
	b.mu.Lock()
	b.headers = append(b.headers, r.Header.Clone())
	b.bodies = append(b.bodies, body)
	b.mu.Unlock()
	return b.answer(n, r)
}

func (b *bottom) CloseIdleConnections() { b.closed.Add(1) }

// respond returns a response to r with code and body.
func respond(r *http.Request, code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Status:     http.StatusText(code),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// classify is the tests' Classify: a 503 and every error are retried, and
// count as failures, and every other status succeeds.
func classify(resp *http.Response, err error) clientchain.Classification {
	if err != nil || resp.StatusCode == http.StatusServiceUnavailable {
		return clientchain.Classification{Retry: retry.Retry, Breaker: circuit.Failure}
	}
	return clientchain.Classification{Retry: retry.Success, Breaker: circuit.Success}
}

// settings are the tests' Settings, as httpclient's defaults, for
// https://payments:8443.
func settings() clientchain.Settings {
	return clientchain.Settings{
		BaseURL:        &url.URL{Scheme: "https", Host: "payments:8443"},
		Timeout:        10 * time.Second,
		AttemptTimeout: 5 * time.Second,
		MaxAttempts:    3,
		BackoffBase:    100 * time.Millisecond,
		BackoffMax:     5 * time.Second,
		RetryBudget:    retry.BudgetConfig{MaxTokens: 10, TokenRatio: 0.1},
		Classify:       classify,
		Breaker:        circuit.Config{Window: 20, Probes: 1, OpenFor: 10 * time.Second, ProbeTimeout: 10 * time.Second},
		BreakerPolicy: circuit.Policy{
			Trip:  func(c circuit.Counts) bool { return c.ConsecutiveFailures >= 5 },
			Probe: func(circuit.Counts) circuit.State { return circuit.Closed },
		},
	}
}

// newClient returns the chain's client for b, built with s.
func newClient(t *testing.T, b *bottom, s clientchain.Settings) *http.Client {
	t.Helper()
	c, err := clientchain.New(&humus.Setup{Telemetry: telemetry.Noop()}, name, "github.com/z5labs/humus/internal/clientchain", s, b)
	require.NoError(t, err)
	return c
}

// do sends req with c and reads the whole response.
func do(c *http.Client, req *http.Request) (int, string, error) {
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

func get(t *testing.T, ctx context.Context, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	return req
}

func TestNew_sends_each_attempt_through_the_bottom_transport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &bottom{answer: func(n int, r *http.Request) (*http.Response, error) {
			if n < 3 {
				return respond(r, http.StatusServiceUnavailable, "busy"), nil
			}
			return respond(r, http.StatusOK, "paid"), nil
		}}
		c := newClient(t, b, settings())

		code, body, err := do(c, get(t, t.Context(), "https://payments:8443/charges/7"))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "paid", body)
		require.EqualValues(t, 3, b.attempts.Load(), "the 503s were retried")

		c.CloseIdleConnections()
		require.EqualValues(t, 1, b.closed.Load(), "CloseIdleConnections reaches the bottom transport")
	})
}

func TestNew_refuses_another_origin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &bottom{answer: func(_ int, r *http.Request) (*http.Response, error) { return respond(r, http.StatusOK, ""), nil }}
		c := newClient(t, b, settings())

		_, err := c.Transport.RoundTrip(get(t, t.Context(), "https://ledger:8443/"))
		require.EqualError(t, err, name+": request to https://ledger:8443 on a client for https://payments:8443")
		require.Zero(t, b.attempts.Load())
	})
}

func TestNew_follows_redirects_within_its_origin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &bottom{answer: func(_ int, r *http.Request) (*http.Response, error) {
			resp := respond(r, http.StatusFound, "")
			switch r.URL.Path {
			case "/away":
				resp.Header.Set("Location", "https://ledger:8443/")
			case "/loop":
				resp.Header.Set("Location", "/loop")
			case "/old":
				resp.Header.Set("Location", "/new")
			default:
				return respond(r, http.StatusOK, r.URL.Path), nil
			}
			return resp, nil
		}}
		c := newClient(t, b, settings())

		code, body, err := do(c, get(t, t.Context(), "https://payments:8443/old"))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "/new", body)

		code, _, err = do(c, get(t, t.Context(), "https://payments:8443/away"))
		require.NoError(t, err)
		require.Equal(t, http.StatusFound, code, "a redirect to another origin is returned as it came")

		_, _, err = do(c, get(t, t.Context(), "https://payments:8443/loop"))
		require.ErrorContains(t, err, "stopped after 10 redirects")
	})
}

func TestNew_retries_only_what_is_safe_to_send_twice(t *testing.T) {
	busy := func(_ int, r *http.Request) (*http.Response, error) {
		return respond(r, http.StatusServiceUnavailable, "busy"), nil
	}

	t.Run("a POST is sent once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: busy}
			c := newClient(t, b, settings())
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://payments:8443/charges", strings.NewReader("{}"))
			require.NoError(t, err)
			code, body, err := do(c, req)
			require.NoError(t, err)
			require.Equal(t, http.StatusServiceUnavailable, code)
			require.Equal(t, "busy", body)
			require.EqualValues(t, 1, b.attempts.Load())
		})
	})

	t.Run("a POST with an idempotency key is retried, with its body rewound", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: busy}
			c := newClient(t, b, settings())
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://payments:8443/charges", strings.NewReader("{}"))
			require.NoError(t, err)
			req.Header.Set("Idempotency-Key", "k1")
			code, _, err := do(c, req)
			require.NoError(t, err)
			require.Equal(t, http.StatusServiceUnavailable, code)
			require.Equal(t, []string{"{}", "{}", "{}"}, b.bodies)
		})
	})

	t.Run("a body that can't be rewound ends the call", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: busy}
			c := newClient(t, b, settings())
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://payments:8443/charges/7", strings.NewReader("{}"))
			require.NoError(t, err)
			errRewind := errors.New("gone")
			req.GetBody = func() (io.ReadCloser, error) { return nil, errRewind }
			_, _, err = do(c, req)
			require.ErrorIs(t, err, errRewind)
			require.ErrorContains(t, err, name+": rewinding the request's body")
		})
	})

	t.Run("a body too large to buffer is returned as it came", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			large := strings.Repeat("x", 64<<10+1)
			b := &bottom{answer: func(_ int, r *http.Request) (*http.Response, error) {
				return respond(r, http.StatusServiceUnavailable, large), nil
			}}
			c := newClient(t, b, settings())
			_, body, err := do(c, get(t, t.Context(), "https://payments:8443/"))
			require.NoError(t, err)
			require.Equal(t, large, body)
			require.EqualValues(t, 1, b.attempts.Load())
		})
	})

	t.Run("a body that fails while buffered fails once read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			errRead := errors.New("reset")
			b := &bottom{answer: func(n int, r *http.Request) (*http.Response, error) {
				resp := respond(r, http.StatusServiceUnavailable, "")
				resp.Body = io.NopCloser(io.MultiReader(strings.NewReader("bu"), failing{err: errRead}))
				return resp, nil
			}}
			s := settings()
			s.MaxAttempts = 2
			s.BackoffMax = time.Hour // the second attempt waits past the call's Timeout
			s.BackoffBase = time.Hour
			s.Timeout = time.Second
			c := newClient(t, b, s)
			resp, err := c.Do(get(t, t.Context(), "https://payments:8443/"))
			require.NoError(t, err)
			got, err := io.ReadAll(resp.Body)
			require.Equal(t, "bu", string(got))
			require.ErrorIs(t, err, errRead)
			require.NoError(t, resp.Body.Close())
		})
	})
}

// failing is a reader whose every read fails with err.
type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

func TestNew_breaker_rejects_once_open(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &bottom{answer: func(_ int, r *http.Request) (*http.Response, error) {
			return respond(r, http.StatusServiceUnavailable, ""), nil
		}}
		s := settings()
		s.MaxAttempts = 1
		c := newClient(t, b, s)
		for range 5 {
			code, _, err := do(c, get(t, t.Context(), "https://payments:8443/"))
			require.NoError(t, err)
			require.Equal(t, http.StatusServiceUnavailable, code)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://payments:8443/", strings.NewReader("{}"))
		require.NoError(t, err)
		_, _, err = do(c, req)
		require.ErrorIs(t, err, circuit.ErrRejected)
		require.EqualValues(t, 5, b.attempts.Load(), "a rejected attempt sends nothing")
	})
}

func TestNew_bounds_each_attempt_and_the_call(t *testing.T) {
	hang := func(_ int, r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}

	t.Run("by AttemptTimeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: hang}
			s := settings()
			s.MaxAttempts = 1
			s.AttemptTimeout = time.Second
			c := newClient(t, b, s)
			start := time.Now()
			_, _, err := do(c, get(t, t.Context(), "https://payments:8443/"))
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.ErrorContains(t, err, name+": the attempt had no response after 1s")
			require.Equal(t, time.Second, time.Since(start))
		})
	})

	t.Run("by Timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: hang}
			s := settings()
			s.Timeout = 2 * time.Second
			c := newClient(t, b, s)
			start := time.Now()
			_, _, err := do(c, get(t, t.Context(), "https://payments:8443/"))
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, 2*time.Second, time.Since(start))
		})
	})

	t.Run("by the caller", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: hang}
			c := newClient(t, b, settings())
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			_, _, err := do(c, get(t, ctx, "https://payments:8443/"))
			require.ErrorIs(t, err, context.DeadlineExceeded)
		})
	})
}

func TestNew_a_101_response_stays_writable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var written bytes.Buffer
		b := &bottom{answer: func(_ int, r *http.Request) (*http.Response, error) {
			resp := respond(r, http.StatusSwitchingProtocols, "")
			resp.Body = readWriter{Reader: strings.NewReader("hi"), w: &written}
			return resp, nil
		}}
		c := newClient(t, b, settings())
		// http.Client hides the body's Write behind its Timeout's timer, so
		// the test sends through the chain itself.
		resp, err := c.Transport.RoundTrip(get(t, t.Context(), "https://payments:8443/"))
		require.NoError(t, err)
		rw, ok := resp.Body.(io.ReadWriteCloser)
		require.True(t, ok, "the body of a 101 is a %T", resp.Body)
		_, err = io.WriteString(rw, "hello")
		require.NoError(t, err)
		got, err := io.ReadAll(rw)
		require.NoError(t, err)
		require.Equal(t, "hi", string(got))
		require.NoError(t, rw.Close())
		require.Equal(t, "hello", written.String())
	})
}

// readWriter is a 101 response's writable body.
type readWriter struct {
	io.Reader
	w io.Writer
}

func (rw readWriter) Write(p []byte) (int, error) { return rw.w.Write(p) }
func (readWriter) Close() error                   { return nil }

func TestNew_panics_on_a_Classification_no_classifier_may_return(t *testing.T) {
	for _, c := range []clientchain.Classification{
		{Retry: 0, Breaker: circuit.Success},
		{Retry: retry.Success, Breaker: 0},
		{Retry: retry.Retry, RetryAfter: -time.Second, Breaker: circuit.Failure},
	} {
		t.Run(fmt.Sprintf("%+v", c), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := &bottom{answer: func(_ int, r *http.Request) (*http.Response, error) { return respond(r, http.StatusOK, ""), nil }}
				s := settings()
				s.Classify = func(*http.Response, error) clientchain.Classification { return c }
				client := newClient(t, b, s)
				var msg string
				func() {
					defer func() { msg, _ = recover().(string) }()
					// Through the chain itself: a panic through http.Client
					// would leave its Timeout's timer behind.
					_, _ = client.Transport.RoundTrip(get(t, t.Context(), "https://payments:8443/"))
				}()
				require.True(t, strings.HasPrefix(msg, name+": Settings.Classify returned"), msg)
			})
		})
	}
}

func TestNew_injects_failures_at_the_dependencys_point(t *testing.T) {
	const point = "http:payments:8443"
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	faults, err := fault.New(fault.Settings{Decoder: codec, MaxDelay: 10 * time.Second})
	require.NoError(t, err)

	withScenario := func(t *testing.T, sc fit.Scenario) context.Context {
		t.Helper()
		tenant, err := baggage.NewMemberRaw("tenant", "acme")
		require.NoError(t, err)
		bag, err := baggage.New(tenant)
		require.NoError(t, err)
		ctx, err := fit.ContextWithScenario(baggage.ContextWithBaggage(t.Context(), bag), codec, sc)
		require.NoError(t, err)
		return ctx
	}
	newFaultyClient := func(t *testing.T, b *bottom, forward bool) *http.Client {
		t.Helper()
		s := settings()
		s.MaxAttempts = 1
		s.ForwardScenario = forward
		c, err := clientchain.New(&humus.Setup{Telemetry: telemetry.Noop(), Faults: faults}, name, "github.com/z5labs/humus/internal/clientchain", s, b)
		require.NoError(t, err)
		return c
	}
	ok := func(_ int, r *http.Request) (*http.Response, error) { return respond(r, http.StatusOK, "ok"), nil }

	t.Run("an abort sends nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: ok}
			c := newFaultyClient(t, b, false)
			ctx := withScenario(t, fit.Scenario{Rules: []fit.Rule{{Point: point, Fault: fit.Fault{Abort: true}}}})
			_, _, err := do(c, get(t, ctx, "https://payments:8443/"))
			var injected *fault.InjectedError
			require.ErrorAs(t, err, &injected)
			require.Equal(t, point, injected.Point)
			require.Zero(t, b.attempts.Load())
		})
	})

	t.Run("a delay is waited inside the attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: ok}
			c := newFaultyClient(t, b, false)
			ctx := withScenario(t, fit.Scenario{Rules: []fit.Rule{{Point: point, Fault: fit.Fault{Delay: 2 * time.Second}}}})
			start := time.Now()
			_, body, err := do(c, get(t, ctx, "https://payments:8443/"))
			require.NoError(t, err)
			require.Equal(t, "ok", body)
			require.Equal(t, 2*time.Second, time.Since(start))
		})
	})

	t.Run("a delay the attempt cuts short fails it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &bottom{answer: ok}
			c := newFaultyClient(t, b, false)
			ctx := withScenario(t, fit.Scenario{Rules: []fit.Rule{{Point: point, Fault: fit.Fault{Delay: 8 * time.Second}}}})
			_, _, err := do(c, get(t, ctx, "https://payments:8443/"))
			var injected *fault.InjectedError
			require.ErrorAs(t, err, &injected)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Zero(t, b.attempts.Load())
		})
	})

	for _, forward := range []bool{false, true} {
		t.Run("the scenario is stripped unless forwarded", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := &bottom{answer: ok}
				c := newFaultyClient(t, b, forward)
				ctx := withScenario(t, fit.Scenario{Rules: []fit.Rule{{Point: "http:ledger:8443", Fault: fit.Fault{Abort: true}}}})
				req := get(t, ctx, "https://payments:8443/")
				propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
				_, _, err := do(c, req)
				require.NoError(t, err)
				require.Len(t, b.headers, 1)
				bag := b.headers[0].Get("baggage")
				require.Contains(t, bag, "tenant=acme", "every other member is sent")
				require.Equal(t, forward, strings.Contains(bag, "acme.fault"))
			})
		})
	}
}

func TestNew_fails_on_settings_the_clients_checks_should_have_caught(t *testing.T) {
	t.Parallel()

	for field, change := range map[string]func(*clientchain.Settings){
		"retries": func(s *clientchain.Settings) { s.MaxAttempts = 0 },
		"breaker": func(s *clientchain.Settings) { s.Breaker.Window = 0 },
	} {
		s := settings()
		change(&s)
		c, err := clientchain.New(&humus.Setup{Telemetry: telemetry.Noop()}, name, "scope", s, &bottom{})
		require.Nil(t, c, field)
		require.ErrorContains(t, err, name+": NewClient: ", field)
	}
}
