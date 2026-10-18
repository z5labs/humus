// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
)

// calls records when a fake dependency was called, and with what.
type calls struct {
	mu     sync.Mutex
	at     []time.Time
	bodies []string
}

// record records r, reading its whole body.
func (c *calls) record(r *http.Request) int {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = append(c.at, time.Now())
	c.bodies = append(c.bodies, string(b))
	return len(c.at)
}

func (c *calls) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.at)
}

func (c *calls) times() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.at)
}

// unavailable answers every request 503, with retryAfter as its
// Retry-After header if it is set, and records it in c.
func unavailable(c *calls, retryAfter string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.record(r)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "busy")
	})
}

// clientSpans returns the client's attempt spans in tel, in the order they
// started.
func clientSpans(tel *humustest.Telemetry) []sdktrace.ReadOnlySpan {
	var spans []sdktrace.ReadOnlySpan
	for _, s := range otelhttpSpans(tel) {
		if s.SpanKind() == trace.SpanKindClient {
			spans = append(spans, s)
		}
	}
	slices.SortStableFunc(spans, func(a, b sdktrace.ReadOnlySpan) int { return a.StartTime().Compare(b.StartTime()) })
	return spans
}

// timed is a call's response, and how long the call took.
type timed struct {
	response
	err  error
	took time.Duration
}

// timedGet sends a GET of rawURL with c, and times it.
func timedGet(ctx context.Context, c *http.Client, rawURL string) timed {
	start := time.Now()
	r, err := get(ctx, c, rawURL)
	return timed{response: r, err: err, took: time.Since(start)}
}

// callWith runs the end user's App with a client built from settings, and
// sends one GET of rawURL. It returns the call's result once the App has
// exited.
func callWith(t *testing.T, h *humustest.Host, settings httpclient.Settings, rawURL string) timed {
	t.Helper()
	var r timed
	st := runJob(t, h.Host(), settings, func(ctx context.Context, c *http.Client) error {
		r = timedGet(ctx, c, rawURL)
		return nil
	})
	requireExitZero(t, st)
	return r
}

func TestClient_retries_a_503_after_its_Retry_After(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.record(r) == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, "ok")
		}))
		defer stop()

		tel := humustest.NewTelemetry()
		var r response
		st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) (err error) {
			r, err = get(ctx, c, "https://payments:8443/charges/1")
			return err
		})
		requireExitZero(t, st)

		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, "ok", r.body)
		at := c.times()
		require.Len(t, at, 2)
		require.GreaterOrEqual(t, at[1].Sub(at[0]), 2*time.Second)

		spans := clientSpans(tel)
		require.Len(t, spans, 2)
		require.Equal(t, "GET", spans[0].Name())
		require.EqualValues(t, http.StatusServiceUnavailable, attrs(spans[0])["http.response.status_code"].AsInt64())
		require.NotContains(t, attrs(spans[0]), attribute.Key("http.request.resend_count"))
		require.Contains(t, spans[1].Attributes(), attribute.Int("http.request.resend_count", 1))
		require.True(t, spans[0].EndTime().Before(spans[1].StartTime()), "the first span ends before the wait")
	})
}

func TestClient_retries_only_a_request_that_is_safe_to_send_twice(t *testing.T) {
	send := func(t *testing.T, newRequest func(ctx context.Context) (*http.Request, error)) *calls {
		t.Helper()
		var c calls
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, unavailable(&c, ""))
			defer stop()

			var code int
			st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, cl *http.Client) error {
				req, err := newRequest(ctx)
				if err != nil {
					return err
				}
				resp, err := cl.Do(req)
				if err != nil {
					return err
				}
				code = resp.StatusCode
				return resp.Body.Close()
			})
			requireExitZero(t, st)
			require.Equal(t, http.StatusServiceUnavailable, code)
		})
		return &c
	}
	post := func(header string) func(ctx context.Context) (*http.Request, error) {
		return func(ctx context.Context) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://payments:8443/charges", strings.NewReader(`{"amount":100}`))
			if err != nil {
				return nil, err
			}
			if header != "" {
				req.Header.Set(header, rand.Text())
			}
			return req, nil
		}
	}

	t.Run("a POST without an idempotency key is sent once", func(t *testing.T) {
		c := send(t, post(""))
		require.Equal(t, 1, c.count())
	})

	for _, header := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		t.Run("a POST with "+header+" is sent three times, with its body each time", func(t *testing.T) {
			c := send(t, post(header))
			require.Equal(t, []string{`{"amount":100}`, `{"amount":100}`, `{"amount":100}`}, c.bodies)
		})
	}

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete} {
		t.Run("a "+method+" is sent three times", func(t *testing.T) {
			c := send(t, func(ctx context.Context) (*http.Request, error) {
				return http.NewRequestWithContext(ctx, method, "https://payments:8443/charges/1", nil)
			})
			require.Equal(t, 3, c.count())
		})
	}

	t.Run("a PATCH is sent once", func(t *testing.T) {
		c := send(t, func(ctx context.Context) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodPatch, "https://payments:8443/charges/1", strings.NewReader("{}"))
		})
		require.Equal(t, 1, c.count())
	})

	t.Run("a PUT whose body can't be rewound is sent once", func(t *testing.T) {
		c := send(t, func(ctx context.Context) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodPut, "https://payments:8443/charges/1", io.NopCloser(strings.NewReader("{}")))
		})
		require.Equal(t, []string{"{}"}, c.bodies)
	})

	t.Run("a PUT whose body fails to rewind makes no further attempt", func(t *testing.T) {
		var callErr error
		var c calls
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, unavailable(&c, ""))
			defer stop()

			errRewind := errors.New("the body is gone")
			st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, cl *http.Client) error {
				req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://payments:8443/charges/1", strings.NewReader("{}"))
				if err != nil {
					return err
				}
				req.GetBody = func() (io.ReadCloser, error) { return nil, errRewind }
				_, callErr = cl.Do(req)
				return nil
			})
			requireExitZero(t, st)
			require.ErrorIs(t, callErr, errRewind)
		})
		require.Equal(t, 1, c.count())
	})
}

func TestClient_retries_a_hung_attempt_after_AttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		var cancelled atomic.Bool
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.record(r) == 1 {
				<-r.Context().Done()
				cancelled.Store(true)
				return
			}
			_, _ = io.WriteString(w, "ok")
		}))
		defer stop()

		settings := paymentsSettings(t, ca)
		classify := settings.Classify
		var mu sync.Mutex
		var classified []error
		settings.Classify = func(resp *http.Response, err error) httpclient.Classification {
			mu.Lock()
			classified = append(classified, err)
			mu.Unlock()
			return classify(resp, err)
		}
		r := callWith(t, h, settings, "https://payments:8443/charges/1")
		synctest.Wait()

		require.NoError(t, r.err)
		require.Equal(t, "ok", r.body)
		require.Equal(t, 2, c.count())
		at := c.times()
		require.GreaterOrEqual(t, at[1].Sub(at[0]), 5*time.Second, "the first attempt failed after AttemptTimeout")
		require.Less(t, r.took, 5*time.Second+100*time.Millisecond+time.Nanosecond, "the second attempt followed after one backoff")
		require.True(t, cancelled.Load(), "the hung attempt's request was cancelled")

		require.Len(t, classified, 2)
		require.ErrorIs(t, classified[0], context.DeadlineExceeded, "the attempt's error reached Classify")
		require.NoError(t, classified[1])
	})
}

func TestClient_doesnt_wait_for_a_Retry_After_beyond_BackoffMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.record(r)
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "slow down")
		}))
		defer stop()

		r := callWith(t, h, paymentsSettings(t, ca), "https://payments:8443/charges/1")

		require.NoError(t, r.err)
		require.Equal(t, http.StatusTooManyRequests, r.code)
		require.Equal(t, "slow down", r.body, "the response is returned as it came")
		require.Zero(t, r.took, "the call returns at once")
		require.Equal(t, 1, c.count())
	})
}

func TestClient_Timeout_ends_the_retries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, unavailable(&c, "4"))
		defer stop()

		settings := paymentsSettings(t, ca)
		settings.Timeout = 6 * time.Second
		r := callWith(t, h, settings, "https://payments:8443/charges/1")

		require.NoError(t, r.err)
		require.Equal(t, http.StatusServiceUnavailable, r.code)
		require.Equal(t, "busy", r.body)
		require.Equal(t, 2, c.count())
		require.Equal(t, 4*time.Second, r.took, "the call returns the second 503 rather than start a wait that ends after Timeout")
	})
}

func TestClient_a_wait_cut_short_returns_the_buffered_response(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, unavailable(&c, "4"))
		defer stop()

		var r timed
		st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, cl *http.Client) error {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			time.AfterFunc(2*time.Second, cancel)
			r = timedGet(ctx, cl, "https://payments:8443/charges/1")
			return nil
		})
		requireExitZero(t, st)

		require.NoError(t, r.err)
		require.Equal(t, http.StatusServiceUnavailable, r.code)
		require.Equal(t, "busy", r.body, "the first response's buffered body")
		require.Equal(t, 2*time.Second, r.took)
		require.Equal(t, 1, c.count())
	})
}

func TestClient_doesnt_retry_a_body_larger_than_64KiB(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		large := []byte(strings.Repeat(rand.Text(), (1<<20)/26+1))[:1<<20]
		var c calls
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.record(r)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(large)
		}))
		defer stop()

		r := callWith(t, h, paymentsSettings(t, ca), "https://payments:8443/charges/1")

		require.NoError(t, r.err)
		require.Equal(t, http.StatusServiceUnavailable, r.code)
		require.True(t, bytes.Equal(large, []byte(r.body)), "the body is returned whole: %d bytes of %d", len(r.body), len(large))
		require.Equal(t, 1, c.count())
	})
}

func TestClient_retries_within_the_budget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, unavailable(&c, ""))
		defer stop()

		var perCall []int
		st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, cl *http.Client) error {
			for range 5 {
				before := c.count()
				r, err := get(ctx, cl, "https://payments:8443/charges/1")
				if err != nil {
					return err
				}
				if r.code != http.StatusServiceUnavailable {
					return fmt.Errorf("the call returned %d", r.code)
				}
				perCall = append(perCall, c.count()-before)
			}
			return nil
		})
		requireExitZero(t, st)

		// The budget's 10 tokens lose one for each attempt classified Retry,
		// and allow a retry only while more than 5 are left: the first call
		// makes its 3 attempts and leaves 7, the second makes 2 and leaves
		// 5, and every call after makes one.
		require.Equal(t, []int{3, 2, 1, 1, 1}, perCall)
	})
}

func TestClient_with_the_end_users_Classify(t *testing.T) {
	t.Run("one that retries a 409 makes a GET answered 409 and then 200 succeed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			var c calls
			stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.record(r) == 1 {
					w.WriteHeader(http.StatusConflict)
					return
				}
				_, _ = io.WriteString(w, "paid")
			}))
			defer stop()

			settings := paymentsSettings(t, ca)
			d := httpclient.DefaultSettings()
			classify := d.Classify
			settings.Classify = func(resp *http.Response, err error) httpclient.Classification {
				if resp != nil && resp.StatusCode == http.StatusConflict {
					return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
				}
				return classify(resp, err)
			}
			r := callWith(t, h, settings, "https://payments:8443/charges/1")

			require.NoError(t, r.err)
			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, "paid", r.body)
			require.Equal(t, 2, c.count())
		})
	})

	t.Run("one that reads a 200's body and finds an error in it makes a second attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			var c calls
			stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.record(r) == 1 {
					_, _ = io.WriteString(w, `{"error":"the ledger is busy"}`)
					return
				}
				_, _ = io.WriteString(w, `{"charge":"paid"}`)
			}))
			defer stop()

			settings := paymentsSettings(t, ca)
			classify := settings.Classify
			settings.Classify = func(resp *http.Response, err error) httpclient.Classification {
				if err != nil {
					return classify(resp, err)
				}
				b, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(b))
				if readErr != nil {
					return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}
				}
				if bytes.Contains(b, []byte(`"error"`)) {
					return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
				}
				return classify(resp, err)
			}
			r := callWith(t, h, settings, "https://payments:8443/charges/1")

			require.NoError(t, r.err)
			require.Equal(t, http.StatusOK, r.code)
			require.Equal(t, `{"charge":"paid"}`, r.body, "service code reads the second response's body whole")
			require.Equal(t, 2, c.count())
		})
	})

	t.Run("a RetryAfter beyond BackoffMax ends the call after one request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			var c calls
			stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.record(r)
				w.Header().Set("RateLimit-Reset", "6")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer stop()

			settings := paymentsSettings(t, ca)
			settings.Classify = func(*http.Response, error) httpclient.Classification {
				return httpclient.Classification{Retry: retry.Retry, RetryAfter: settings.BackoffMax + time.Second, Breaker: circuit.Success}
			}
			r := callWith(t, h, settings, "https://payments:8443/charges/1")

			require.NoError(t, r.err)
			require.Equal(t, http.StatusTooManyRequests, r.code)
			require.Zero(t, r.took)
			require.Equal(t, 1, c.count())
		})
	})
}

func TestClient_decides_what_is_about_the_call(t *testing.T) {
	retryEverything := func(classified *atomic.Int32) func(*http.Response, error) httpclient.Classification {
		return func(*http.Response, error) httpclient.Classification {
			classified.Add(1)
			return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}
		}
	}

	t.Run("a POST without a key is sent once, whatever Classify returns", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			var c calls
			stop := startPayments(t, h, h.Host(), ca, unavailable(&c, ""))
			defer stop()

			var classified atomic.Int32
			settings := paymentsSettings(t, ca)
			settings.Classify = retryEverything(&classified)
			st := runJob(t, h.Host(), settings, func(ctx context.Context, cl *http.Client) error {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://payments:8443/charges", strings.NewReader("{}"))
				if err != nil {
					return err
				}
				resp, err := cl.Do(req)
				if err != nil {
					return err
				}
				return resp.Body.Close()
			})
			requireExitZero(t, st)

			require.Equal(t, 1, c.count())
			require.EqualValues(t, 1, classified.Load())
		})
	})

	t.Run("a call whose caller cancels it makes no further attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			var c calls
			stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				c.record(r)
				<-r.Context().Done()
			}))
			defer stop()

			var classified atomic.Int32
			settings := paymentsSettings(t, ca)
			settings.Classify = retryEverything(&classified)
			var r timed
			st := runJob(t, h.Host(), settings, func(ctx context.Context, cl *http.Client) error {
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				time.AfterFunc(time.Second, cancel)
				r = timedGet(ctx, cl, "https://payments:8443/charges/1")
				return nil
			})
			requireExitZero(t, st)

			require.ErrorIs(t, r.err, context.Canceled)
			require.Equal(t, time.Second, r.took)
			require.Equal(t, 1, c.count())
			require.Zero(t, classified.Load(), "Classify isn't asked about an error once the call's context is done")
		})
	})

	t.Run("a call whose Timeout passes makes no further attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			var c calls
			stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				c.record(r)
				<-r.Context().Done()
			}))
			defer stop()

			var classified atomic.Int32
			settings := paymentsSettings(t, ca)
			settings.Classify = retryEverything(&classified)
			settings.Timeout = 2 * time.Second
			r := callWith(t, h, settings, "https://payments:8443/charges/1")

			require.Error(t, r.err)
			require.Equal(t, 2*time.Second, r.took)
			require.Equal(t, 1, c.count())
			require.Zero(t, classified.Load())
		})
	})
}

func TestClient_doesnt_retry_a_server_certificate_it_doesnt_trust(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		handler := &ok{}
		stop := startPayments(t, h, h.Host(), humustest.NewCA(t), handler)
		defer stop()

		host, dials := recordDials(h)
		var r timed
		st := runJob(t, host, paymentsSettings(t, humustest.NewCA(t)), func(ctx context.Context, c *http.Client) error {
			r = timedGet(ctx, c, "https://payments:8443/charges/1")
			return nil
		})
		requireExitZero(t, st)

		require.Error(t, r.err)
		require.Equal(t, []string{"payments:8443"}, dials.all(), "one attempt")
		require.Zero(t, handler.calls.Load())
	})
}

func TestClient_panics_in_Classify_reach_the_caller(t *testing.T) {
	// call runs a GET with classify as the client's Classify, and returns
	// what the job's run recovered.
	call := func(t *testing.T, classify func(*http.Response, error) httpclient.Classification) (recovered any, c *calls) {
		t.Helper()
		c = &calls{}
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			stop := startPayments(t, h, h.Host(), ca, unavailable(c, ""))
			defer stop()

			settings := paymentsSettings(t, ca)
			settings.Classify = classify
			st := runJob(t, h.Host(), settings, func(ctx context.Context, cl *http.Client) error {
				defer func() { recovered = recover() }()
				_, _ = get(ctx, cl, "https://payments:8443/charges/1")
				return nil
			})
			requireExitZero(t, st)
			// A panic skips http.Client's stopping of the timer it set for
			// Timeout, whose goroutine then waits for it to fire.
			time.Sleep(settings.Timeout)
		})
		return recovered, c
	}

	t.Run("a panic in Classify, once the response is closed", func(t *testing.T) {
		var body *closeRecorder
		recovered, c := call(t, func(resp *http.Response, _ error) httpclient.Classification {
			body = &closeRecorder{ReadCloser: resp.Body}
			resp.Body = body
			panic("the end user's bug")
		})
		require.Equal(t, "the end user's bug", recovered)
		require.True(t, body.closed.Load(), "the attempt's response is closed")
		require.Equal(t, 1, c.count())
	})

	for name, class := range map[string]httpclient.Classification{
		"a zero Classification":             {},
		"a zero Retry":                      {Breaker: circuit.Success},
		"a zero Breaker":                    {Retry: retry.Success},
		"a Retry that is not an Outcome":    {Retry: retry.Outcome(9), Breaker: circuit.Success},
		"a Breaker that is not an Outcome":  {Retry: retry.Stop, Breaker: circuit.Outcome(9)},
		"retry.Retry with a negative delay": {Retry: retry.Retry, RetryAfter: -time.Second, Breaker: circuit.Failure},
	} {
		t.Run(name+" panics", func(t *testing.T) {
			var body *closeRecorder
			recovered, c := call(t, func(resp *http.Response, _ error) httpclient.Classification {
				body = &closeRecorder{ReadCloser: resp.Body}
				resp.Body = body
				return class
			})
			msg, isString := recovered.(string)
			require.True(t, isString, "recovered %v", recovered)
			require.True(t, strings.HasPrefix(msg, "httpclient: Settings.Classify returned"), msg)
			require.True(t, body.closed.Load(), "the attempt's response is closed")
			require.Equal(t, 1, c.count())
		})
	}
}

// closeRecorder is a response body that records being closed.
type closeRecorder struct {
	io.ReadCloser
	closed atomic.Bool
}

func (b *closeRecorder) Close() error {
	b.closed.Store(true)
	return b.ReadCloser.Close()
}

func TestClient_a_call_cancelled_during_a_wait_after_an_error_returns_why(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t) // nothing listens at payments:8443

		settings := paymentsSettings(t, humustest.NewCA(t))
		settings.Classify = func(*http.Response, error) httpclient.Classification {
			return httpclient.Classification{Retry: retry.Retry, RetryAfter: 4 * time.Second, Breaker: circuit.Failure}
		}
		var r timed
		st := runJob(t, h.Host(), settings, func(ctx context.Context, c *http.Client) error {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			time.AfterFunc(time.Second, cancel)
			r = timedGet(ctx, c, "https://payments:8443/charges/1")
			return nil
		})
		requireExitZero(t, st)

		require.ErrorIs(t, r.err, context.Canceled)
		require.ErrorContains(t, r.err, "the last attempt failed")
		require.Equal(t, time.Second, r.took)
	})
}

func TestClient_AttemptTimeout_ends_with_the_headers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c.record(r)
			_, _ = io.WriteString(w, "first, ")
			w.(http.Flusher).Flush()
			time.Sleep(7 * time.Second) // past AttemptTimeout, within Timeout
			_, _ = io.WriteString(w, "then the rest")
		}))
		defer stop()

		r := callWith(t, h, paymentsSettings(t, ca), "https://payments:8443/charges/1")

		require.NoError(t, r.err)
		require.Equal(t, "first, then the rest", r.body, "a body slower than AttemptTimeout is read whole")
		require.Equal(t, 7*time.Second, r.took)
		require.Equal(t, 1, c.count())
	})
}

func TestClient_a_request_sent_once_still_counts_in_the_budget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, unavailable(&c, ""))
		defer stop()

		var getAttempts int
		st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, cl *http.Client) error {
			// Each POST answered 503 is classified Retry, and takes a token
			// although it isn't retried: five leave 5 of the 10.
			for range 5 {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://payments:8443/charges", strings.NewReader("{}"))
				if err != nil {
					return err
				}
				resp, err := cl.Do(req)
				if err != nil {
					return err
				}
				if err := resp.Body.Close(); err != nil {
					return err
				}
			}
			before := c.count()
			if _, err := get(ctx, cl, "https://payments:8443/charges/1"); err != nil {
				return err
			}
			getAttempts = c.count() - before
			return nil
		})
		requireExitZero(t, st)

		require.Equal(t, 1, getAttempts, "the budget allows the GET no retry")
	})
}

func TestClient_Transport_keeps_a_101_responses_writable_body(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		l, err := h.Listen("tcp", "payments:8080")
		require.NoError(t, err)
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, rw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
				_ = rw.Flush()
				b := make([]byte, 4)
				if _, err := io.ReadFull(rw, b); err != nil {
					return
				}
				_, _ = rw.Write(b)
				_ = rw.Flush()
			}),
			ReadHeaderTimeout: time.Second,
		}
		served := make(chan error, 1)
		go func() { served <- srv.Serve(l) }()

		settings := httpclient.DefaultSettings()
		settings.BaseURL = mustParse(t, "http://payments:8080")
		var echoed string
		st := runJob(t, h.Host(), settings, func(ctx context.Context, c *http.Client) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://payments:8080/echo", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "echo")
			// http.Client wraps the body of a call with a Timeout in a type
			// that can't be written to, so the upgrade goes through the
			// client's Transport, as a WebSocket library's dialer does.
			resp, err := c.Transport.RoundTrip(req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			rw, ok := resp.Body.(io.ReadWriteCloser)
			if !ok || resp.StatusCode != http.StatusSwitchingProtocols {
				return fmt.Errorf("a %d whose body is a %T", resp.StatusCode, resp.Body)
			}
			if _, err := io.WriteString(rw, "ping"); err != nil {
				return err
			}
			b := make([]byte, 4)
			if _, err := io.ReadFull(rw, b); err != nil {
				return err
			}
			echoed = string(b)
			return nil
		})
		requireExitZero(t, st)
		require.Equal(t, "ping", echoed)

		require.NoError(t, srv.Close())
		require.ErrorIs(t, <-served, http.ErrServerClosed)
	})
}
