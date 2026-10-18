// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
)

// The bodies of the client's log records for the breaker's transitions.
const (
	breakerOpen     = "circuit breaker open"
	breakerHalfOpen = "circuit breaker half-open"
	breakerClosed   = "circuit breaker closed"
)

// withoutBreaker returns settings with a BreakerPolicy that never trips,
// for a test of something the breaker would otherwise cut short.
func withoutBreaker(settings httpclient.Settings) httpclient.Settings {
	settings.BreakerPolicy.Trip = func(circuit.Counts) bool { return false }
	return settings
}

// breakerRecords returns the client's log records in tel, in the order they
// were emitted.
func breakerRecords(tel *humustest.Telemetry) []sdklog.Record {
	var out []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == "github.com/z5labs/humus/httpclient" {
			out = append(out, r)
		}
	}
	return out
}

// bodies returns the bodies of records.
func bodies(records []sdklog.Record) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Body().AsString())
	}
	return out
}

// recordAttrs returns r's attributes by key.
func recordAttrs(r sdklog.Record) map[string]attribute.Value {
	out := make(map[string]attribute.Value)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value
		return true
	})
	return out
}

// failing answers 500, a breaker Failure that isn't retried, while
// healthy is false, and 200 once it is true. It counts every request.
type failing struct {
	calls   atomic.Int32
	healthy atomic.Bool
}

func (h *failing) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls.Add(1)
	if !h.healthy.Load() {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// getN sends n GETs of rawURL with c, reading and closing each response,
// and fails t if any of them fails.
func getN(t *testing.T, ctx context.Context, c *http.Client, n int, rawURL string) {
	t.Helper()
	for range n {
		_, err := get(ctx, c, rawURL)
		require.NoError(t, err)
	}
}

func TestClient_breaker_opens_rejects_and_closes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		dep := &failing{}
		stop := startPayments(t, h, h.Host(), ca, dep)
		defer stop()

		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			getN(t, ctx, c, 5, "https://payments:8443/")
			_, err := get(ctx, c, "https://payments:8443/")
			require.ErrorIs(t, err, circuit.ErrRejected)
			var rejected *circuit.RejectedError
			require.ErrorAs(t, err, &rejected)
			require.Equal(t, 10*time.Second, rejected.RetryAfter)
			require.EqualValues(t, 5, dep.calls.Load(), "the sixth call was never sent")

			dep.healthy.Store(true)
			time.Sleep(10 * time.Second) // OpenFor
			getN(t, ctx, c, 3, "https://payments:8443/")
			return nil
		})
		requireExitZero(t, st)
		require.EqualValues(t, 8, dep.calls.Load())

		records := breakerRecords(tel)
		require.Equal(t, []string{breakerOpen, breakerHalfOpen, breakerClosed}, bodies(records))
		require.Equal(t, log.SeverityWarn, records[0].Severity())
		require.Equal(t, log.SeverityInfo, records[1].Severity())
		require.Equal(t, log.SeverityInfo, records[2].Severity())

		open := recordAttrs(records[0])
		require.Equal(t, "closed", open["humus.circuit.previous_state"].AsString())
		require.Equal(t, "payments", open["server.address"].AsString())
		require.EqualValues(t, 8443, open["server.port"].AsInt64())
		require.EqualValues(t, 5, open["humus.circuit.consecutive_failures"].AsInt64())
		require.EqualValues(t, 5, open["humus.circuit.failures"].AsInt64())
		require.Zero(t, open["humus.circuit.successes"].AsInt64())
		require.Zero(t, open["humus.circuit.consecutive_successes"].AsInt64())
		require.Zero(t, open["humus.circuit.timed_out"].AsInt64())

		halfOpen := recordAttrs(records[1])
		require.Equal(t, "open", halfOpen["humus.circuit.previous_state"].AsString())
		closed := recordAttrs(records[2])
		require.Equal(t, "half-open", closed["humus.circuit.previous_state"].AsString())
		require.EqualValues(t, 3, closed["humus.circuit.consecutive_successes"].AsInt64())
		require.Equal(t, "payments", closed["server.address"].AsString())
		require.EqualValues(t, 8443, closed["server.port"].AsInt64())
	})
}

func TestClient_breaker_counts_a_body(t *testing.T) {
	t.Run("a body that fails mid-read counts as a Failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			var calls atomic.Int32
			stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				// A 200, which the default Classify counts as a Success,
				// whose body is cut off after its first bytes.
				_, _ = io.WriteString(w, "partial")
				_ = http.NewResponseController(w).Flush()
				panic(http.ErrAbortHandler)
			}))
			defer stop()

			st := runJob(t, h.Host(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
				for range 5 {
					r, err := get(ctx, c, "https://payments:8443/charges/1")
					require.Error(t, err, "the body's read fails")
					require.NotErrorIs(t, err, circuit.ErrRejected)
					require.Equal(t, http.StatusOK, r.code)
				}
				_, err := get(ctx, c, "https://payments:8443/charges/1")
				require.ErrorIs(t, err, circuit.ErrRejected)
				return nil
			})
			requireExitZero(t, st)
			require.EqualValues(t, 5, calls.Load())
		})
	})

	t.Run("a caller that cancels while reading counts as Ignored", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			var calls atomic.Int32
			stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/fail" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = io.WriteString(w, "partial")
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
			}))
			defer stop()

			// readCancelled sends a GET answered 200, and cancels it once
			// its body's first bytes are read.
			readCancelled := func(ctx context.Context, c *http.Client) {
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://payments:8443/slow", nil)
				require.NoError(t, err)
				resp, err := c.Do(req)
				require.NoError(t, err)
				defer func() { _ = resp.Body.Close() }()
				require.Equal(t, http.StatusOK, resp.StatusCode)
				time.AfterFunc(time.Second, cancel)
				_, err = io.ReadAll(resp.Body)
				require.ErrorIs(t, err, context.Canceled)
			}

			tel := humustest.NewTelemetry()
			st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
				// Four failures, then three bodies whose caller cancels them,
				// then a fifth failure. Ignored neither extends nor ends the
				// run of failures, so the fifth opens the breaker: a Success
				// would have ended the run, and a Failure opened it sooner.
				getN(t, ctx, c, 4, "https://payments:8443/fail")
				for range 3 {
					readCancelled(ctx, c)
				}
				getN(t, ctx, c, 1, "https://payments:8443/fail")
				_, err := get(ctx, c, "https://payments:8443/fail")
				require.ErrorIs(t, err, circuit.ErrRejected)
				return nil
			})
			requireExitZero(t, st)
			require.EqualValues(t, 8, calls.Load())

			records := breakerRecords(tel)
			require.Equal(t, []string{breakerOpen}, bodies(records))
			open := recordAttrs(records[0])
			require.EqualValues(t, 5, open["humus.circuit.consecutive_failures"].AsInt64())
			require.EqualValues(t, 5, open["humus.circuit.failures"].AsInt64())
			require.Zero(t, open["humus.circuit.successes"].AsInt64())
		})
	})
}

func TestClient_a_rejected_attempts_span_and_metric(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		dep := &failing{}
		stop := startPayments(t, h, h.Host(), ca, dep)
		defer stop()

		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
			getN(t, ctx, c, 5, "https://payments:8443/charges/1")
			_, err := get(ctx, c, "https://payments:8443/charges/1")
			require.ErrorIs(t, err, circuit.ErrRejected)
			return nil
		})
		requireExitZero(t, st)

		spans := clientSpans(tel)
		require.Len(t, spans, 6, "the rejected attempt has a span")
		rejected := spans[5]
		require.Equal(t, "GET", rejected.Name())
		require.Equal(t, codes.Error, rejected.Status().Code)
		a := attrs(rejected)
		require.Equal(t, "*circuit.RejectedError", a["error.type"].AsString())
		require.Equal(t, "payments", a["server.address"].AsString())
		require.EqualValues(t, 8443, a["server.port"].AsInt64())
		require.Equal(t, "https://payments:8443/charges/1", a["url.full"].AsString())
		_, found := a["http.response.status_code"]
		require.False(t, found)
		require.Equal(t, rejected.StartTime(), rejected.EndTime(), "it ends at once")

		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		duration := histogram[float64](t, rm, "http.client.request.duration")
		require.Len(t, duration.DataPoints, 2, "the 500s and the rejection")
		var rejections []metricdata.HistogramDataPoint[float64]
		for _, dp := range duration.DataPoints {
			if _, ok := dp.Attributes.Value("error.type"); ok {
				rejections = append(rejections, dp)
			}
		}
		require.Len(t, rejections, 1, "data points: %+v", duration.DataPoints)
		require.EqualValues(t, 1, rejections[0].Count)
		// otelhttp names the type in a data point's error.type with its
		// package path, and on a span as Go prints it.
		errType, _ := rejections[0].Attributes.Value("error.type")
		require.Equal(t, "github.com/z5labs/bedrock/circuit.RejectedError", errType.AsString())
		_, found = rejections[0].Attributes.Value("http.response.status_code")
		require.False(t, found)
	})
}

func TestClient_a_rejection_isnt_retried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		var unavailableNow atomic.Bool
		dep := &failing{}
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if unavailableNow.Load() {
				unavailable(&c, "").ServeHTTP(w, r)
				return
			}
			dep.ServeHTTP(w, r)
		}))
		defer stop()

		var classified atomic.Int32
		settings := paymentsSettings(t, ca)
		classify := settings.Classify
		settings.Classify = func(resp *http.Response, err error) httpclient.Classification {
			classified.Add(1)
			return classify(resp, err)
		}
		tel := humustest.NewTelemetry()
		var r timed
		st := runJobWith(t, h.Host(), tel.Builder(), settings, func(ctx context.Context, cl *http.Client) error {
			getN(t, ctx, cl, 5, "https://payments:8443/charges/1")
			// A 503 is retried, so a call sent now would be, if the breaker
			// let it through.
			unavailableNow.Store(true)
			r = timedGet(ctx, cl, "https://payments:8443/charges/1")
			return nil
		})
		requireExitZero(t, st)

		require.ErrorIs(t, r.err, circuit.ErrRejected)
		require.Zero(t, r.took, "it returns at once")
		require.Zero(t, c.count(), "it makes no attempt")
		require.EqualValues(t, 5, classified.Load(), "Classify isn't asked about a rejection")
		require.Len(t, clientSpans(tel), 6, "one span for the rejected call, which isn't retried")
	})
}

func TestClient_the_caller_giving_up_isnt_counted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			c.record(r)
			<-r.Context().Done()
		}))
		defer stop()

		settings := paymentsSettings(t, ca)
		settings.Classify = func(*http.Response, error) httpclient.Classification {
			return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}
		}
		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), settings, func(ctx context.Context, cl *http.Client) error {
			for range 7 {
				ctx, cancel := context.WithCancel(ctx)
				time.AfterFunc(time.Second, cancel)
				r := timedGet(ctx, cl, "https://payments:8443/charges/1")
				cancel()
				require.ErrorIs(t, r.err, context.Canceled)
				require.NotErrorIs(t, r.err, circuit.ErrRejected)
			}
			return nil
		})
		requireExitZero(t, st)

		require.Equal(t, 7, c.count(), "the seventh call is sent: six cancelled calls leave the breaker closed")
		require.Empty(t, bodies(breakerRecords(tel)))
	})
}

func TestClient_deadlines_count_against_the_dependency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		var c calls
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			c.record(r)
			<-r.Context().Done()
		}))
		defer stop()

		settings := paymentsSettings(t, ca)
		settings.MaxAttempts = 1
		settings.Timeout = 2 * time.Second
		settings.AttemptTimeout = 5 * time.Second
		var results []timed
		st := runJob(t, h.Host(), settings, func(ctx context.Context, cl *http.Client) error {
			for range 6 {
				results = append(results, timedGet(ctx, cl, "https://payments:8443/charges/1"))
			}
			return nil
		})
		requireExitZero(t, st)

		require.Len(t, results, 6)
		for i, r := range results[:5] {
			require.Error(t, r.err, "call %d", i)
			require.NotErrorIs(t, r.err, circuit.ErrRejected, "call %d", i)
			require.Equal(t, 2*time.Second, r.took, "call %d ends at Timeout", i)
		}
		require.ErrorIs(t, results[5].err, circuit.ErrRejected)
		require.Zero(t, results[5].took)
		require.Equal(t, 5, c.count())
	})
}

func TestClient_a_policy_that_never_trips_turns_the_breaker_off(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		dep := &failing{}
		stop := startPayments(t, h, h.Host(), ca, dep)
		defer stop()

		tel := humustest.NewTelemetry()
		st := runJobWith(t, h.Host(), tel.Builder(), withoutBreaker(paymentsSettings(t, ca)), func(ctx context.Context, c *http.Client) error {
			getN(t, ctx, c, 30, "https://payments:8443/charges/1")
			return nil
		})
		requireExitZero(t, st)

		require.EqualValues(t, 30, dep.calls.Load())
		require.Empty(t, bodies(breakerRecords(tel)))
	})
}

func TestClient_a_panic_in_BreakerPolicy_reaches_the_caller(t *testing.T) {
	t.Run("from a body's read", testPolicyPanicFromABody)

	t.Run("from an attempt that fails, inside RoundTrip", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t) // nothing listens at payments:8443
			host, dials := recordDials(h)

			settings := paymentsSettings(t, humustest.NewCA(t))
			settings.MaxAttempts = 1
			trip := settings.BreakerPolicy.Trip
			var panicked atomic.Bool
			settings.BreakerPolicy.Trip = func(c circuit.Counts) bool {
				if panicked.CompareAndSwap(false, true) {
					panic("the end user's bug")
				}
				return trip(c)
			}
			var recovered any
			st := runJob(t, host, settings, func(ctx context.Context, c *http.Client) error {
				func() {
					defer func() { recovered = recover() }()
					_, _ = get(ctx, c, "https://payments:8443/charges/1")
				}()
				for range 4 {
					_, err := get(ctx, c, "https://payments:8443/charges/1")
					require.Error(t, err)
					require.NotErrorIs(t, err, circuit.ErrRejected)
				}
				_, err := get(ctx, c, "https://payments:8443/charges/1")
				require.ErrorIs(t, err, circuit.ErrRejected)
				return nil
			})
			requireExitZero(t, st)
			// A panic skips http.Client's stopping of the timer it set for
			// Timeout, whose goroutine then waits for it to fire.
			time.Sleep(settings.Timeout)

			require.Equal(t, "the end user's bug", recovered)
			require.Len(t, dials.all(), 5, "the failure Trip panicked on stays counted")
		})
	})
}

// testPolicyPanicFromABody shows a panic in BreakerPolicy, raised as a
// body's read settles its ticket, reaching the caller, with the breaker
// left consistent.
func testPolicyPanicFromABody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		dep := &failing{}
		stop := startPayments(t, h, h.Host(), ca, dep)
		defer stop()

		settings := paymentsSettings(t, ca)
		trip := settings.BreakerPolicy.Trip
		var panicked atomic.Bool
		settings.BreakerPolicy.Trip = func(c circuit.Counts) bool {
			if panicked.CompareAndSwap(false, true) {
				panic("the end user's bug")
			}
			return trip(c)
		}
		var recovered any
		st := runJob(t, h.Host(), settings, func(ctx context.Context, c *http.Client) error {
			func() {
				defer func() { recovered = recover() }()
				_, _ = get(ctx, c, "https://payments:8443/charges/1")
			}()
			// The failure that reached the panicking Trip stays counted,
			// so four more open the breaker.
			getN(t, ctx, c, 4, "https://payments:8443/charges/1")
			_, err := get(ctx, c, "https://payments:8443/charges/1")
			require.ErrorIs(t, err, circuit.ErrRejected)
			return nil
		})
		requireExitZero(t, st)

		require.Equal(t, "the end user's bug", recovered)
		require.EqualValues(t, 5, dep.calls.Load())
	})
}
