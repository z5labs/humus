// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package billing_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/z5labs/humus/humustest"
)

const scope = "github.com/z5labs/humus/example/billing"

func outcomes(r *http.Request, account string, w http.ResponseWriter) {
	switch {
	case strings.HasPrefix(account, "fail-"):
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"bad currency"}`)
	case strings.HasPrefix(account, "down-"):
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	default:
		charging(r, account, w)
	}
}

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var named []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			named = append(named, s)
		}
	}
	return named
}

func spanAttr(s sdktrace.ReadOnlySpan, key attribute.Key) (string, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value.String(), true
		}
	}
	return "", false
}

func records(tel *humustest.Telemetry) []sdklog.Record {
	var own []sdklog.Record
	for _, r := range tel.Logs() {
		if r.InstrumentationScope().Name == scope {
			own = append(own, r)
		}
	}
	return own
}

func recordAttrs(r sdklog.Record) map[string]attribute.Value {
	attrs := map[string]attribute.Value{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		attrs[string(kv.Key)] = kv.Value
		return true
	})
	return attrs
}

func instruments(t *testing.T, tel *humustest.Telemetry) map[string]metricdata.Metrics {
	t.Helper()
	rm, err := tel.Metrics(context.Background())
	require.NoError(t, err)
	byName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != scope {
			continue
		}
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}
	return byName
}

func counter(t *testing.T, m metricdata.Metrics) map[string]int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	require.True(t, ok, "%s is a %T", m.Name, m.Data)
	require.True(t, sum.IsMonotonic, m.Name)
	byState := map[string]int64{}
	for _, dp := range sum.DataPoints {
		state, _ := dp.Attributes.Value("billing.invoice.state")
		byState[state.AsString()] += dp.Value
	}
	return byState
}

func gauge(t *testing.T, m metricdata.Metrics) []int64 {
	t.Helper()
	g, ok := m.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "%s is a %T", m.Name, m.Data)
	var values []int64
	for _, dp := range g.DataPoints {
		require.Zero(t, dp.Attributes.Len(), m.Name)
		values = append(values, dp.Value)
	}
	return values
}

func ended(t *testing.T, tel *humustest.Telemetry) map[string]int64 {
	t.Helper()
	var infos []sdklog.Record
	for _, r := range records(tel) {
		if r.Severity() == log.SeverityInfo {
			infos = append(infos, r)
		}
	}
	require.Len(t, infos, 1)
	counts := map[string]int64{}
	for k, v := range recordAttrs(infos[0]) {
		require.Equal(t, attribute.INT64, v.Type(), k)
		counts[k] = v.AsInt64()
	}
	return counts
}

func TestRun_spans_an_issue_and_a_charge_per_invoice_with_each_attempt_under_its_charge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		failedOnce := map[string]bool{}
		e := newEnv(t, func(r *http.Request, account string, w http.ResponseWriter) {
			mu.Lock()
			first := !failedOnce[account]
			failedOnce[account] = true
			mu.Unlock()
			if account == "acct-flaky" && first {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			charging(r, account, w)
		})
		e.subscribe("acct-1", "acct-flaky")
		run := e.run(t, month(1999, time.December), 1)
		ctx, parent := e.sdk(t).TracerProvider.Tracer("test").Start(t.Context(), "job")

		err := run.Run(ctx)
		parent.End()

		require.NoError(t, err)
		spans := e.tel.Spans()
		issue := spansNamed(spans, "issue")
		require.Len(t, issue, 1)
		require.Equal(t, scope, issue[0].InstrumentationScope().Name)
		require.Equal(t, parent.SpanContext().SpanID(), issue[0].Parent().SpanID())
		require.Equal(t, codes.Unset, issue[0].Status().Code)

		charges := spansNamed(spans, "charge")
		require.Len(t, charges, 2)
		byInvoice := map[string]sdktrace.ReadOnlySpan{}
		for _, c := range charges {
			require.Equal(t, scope, c.InstrumentationScope().Name)
			require.Equal(t, parent.SpanContext().SpanID(), c.Parent().SpanID())
			require.Equal(t, codes.Unset, c.Status().Code)
			id, ok := spanAttr(c, "billing.invoice.id")
			require.True(t, ok)
			byInvoice[id] = c
		}

		attempts := spansNamed(spans, http.MethodPost)
		require.Len(t, attempts, 3)
		for _, inv := range e.store.all() {
			c, ok := byInvoice[inv.ID.String()]
			require.True(t, ok, "no charge span for %s", inv.AccountID)
			var under int
			for _, a := range attempts {
				if a.Parent().SpanID() == c.SpanContext().SpanID() {
					require.Equal(t, c.SpanContext().TraceID(), a.SpanContext().TraceID())
					under++
				}
			}
			want := 1
			if inv.AccountID == "acct-flaky" {
				want = 2
			}
			require.Equal(t, want, under, inv.AccountID)
		}
	})
}

func TestRun_a_failed_issue_ends_its_span_in_error(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, charging)
		e.subscribe("acct-1")
		e.store.issueErr = errors.New("connection refused")

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.ErrorIs(t, err, e.store.issueErr)
		issue := spansNamed(e.tel.Spans(), "issue")
		require.Len(t, issue, 1)
		require.Equal(t, codes.Error, issue[0].Status().Code)
		require.Equal(t, "connection refused", issue[0].Status().Description)
		require.Empty(t, spansNamed(e.tel.Spans(), "charge"))
	})
}

func TestRun_records_a_warning_per_failed_invoice_and_the_counts_as_it_ends(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, outcomes)
		e.subscribe("acct-1", "declined-1", "fail-1", "down-1")

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.ErrorContains(t, err, "left 1 of 4 invoices pending")
		var failed, down string
		for _, inv := range e.store.all() {
			switch inv.AccountID {
			case "fail-1":
				failed = inv.ID.String()
			case "down-1":
				down = inv.ID.String()
			}
		}
		charges := map[string]sdktrace.ReadOnlySpan{}
		for _, c := range spansNamed(e.tel.Spans(), "charge") {
			id, _ := spanAttr(c, "billing.invoice.id")
			charges[id] = c
		}
		require.Equal(t, codes.Error, charges[down].Status().Code)
		require.Contains(t, charges[down].Status().Description, `500 Internal Server Error: {"error":"boom"}`)
		require.Equal(t, codes.Unset, charges[failed].Status().Code)

		var warnings []sdklog.Record
		for _, r := range records(e.tel) {
			if r.Severity() == log.SeverityWarn {
				warnings = append(warnings, r)
			}
		}
		require.Len(t, warnings, 1)
		require.Equal(t, "invoice failed", warnings[0].Body().AsString())
		attrs := recordAttrs(warnings[0])
		require.Equal(t, failed, attrs["billing.invoice.id"].AsString())
		require.Equal(t, `422 Unprocessable Entity: {"error":"bad currency"}`, attrs["billing.invoice.answer"].AsString())
		require.Equal(t, charges[failed].SpanContext().TraceID(), warnings[0].TraceID())
		require.Equal(t, charges[failed].SpanContext().SpanID(), warnings[0].SpanID())

		require.Equal(t, map[string]int64{
			"billing.invoices.issued":   4,
			"billing.invoices.charged":  1,
			"billing.invoices.declined": 1,
			"billing.invoices.failed":   1,
			"billing.invoices.pending":  1,
		}, ended(t, e.tel))
		require.Len(t, records(e.tel), 2)
	})
}

func TestRun_counts_what_it_issued_and_settled_and_gauges_what_it_left_pending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, outcomes)
		e.subscribe("acct-1", "acct-2", "declined-1", "fail-1", "down-1")

		err := e.run(t, month(1999, time.December), 2).Run(t.Context())

		require.ErrorContains(t, err, "left 1 of 5 invoices pending")
		got := instruments(t, e.tel)
		require.Len(t, got, 3)
		require.Equal(t, "{invoice}", got["billing.invoices.issued"].Unit)
		require.Equal(t, map[string]int64{"": 5}, counter(t, got["billing.invoices.issued"]))
		require.Equal(t, map[string]int64{"charged": 2, "declined": 1, "failed": 1}, counter(t, got["billing.invoices.settled"]))
		require.Equal(t, []int64{1}, gauge(t, got["billing.invoices.pending"]))
	})
}

func TestRun_a_run_with_nothing_left_gauges_zero_pending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, charging)
		e.subscribe("acct-1", "acct-2")
		run := e.run(t, month(1999, time.December), 2)
		require.NoError(t, run.Run(t.Context()))

		err := run.Run(t.Context())

		require.NoError(t, err)
		got := instruments(t, e.tel)
		require.Equal(t, map[string]int64{"": 2}, counter(t, got["billing.invoices.issued"]), "the second run issued none")
		require.Equal(t, map[string]int64{"charged": 2}, counter(t, got["billing.invoices.settled"]))
		require.Equal(t, []int64{0}, gauge(t, got["billing.invoices.pending"]))
	})
}

func TestRun_a_run_stopped_early_still_gauges_and_records_what_it_left_pending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, func(_ *http.Request, _ string, w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		e.subscribe("acct-00", "acct-01", "acct-02", "acct-03", "acct-04", "acct-05", "acct-06", "acct-07", "acct-08", "acct-09")

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.ErrorContains(t, err, "leaving 10 of 10 invoices pending")
		require.Equal(t, []int64{10}, gauge(t, instruments(t, e.tel)["billing.invoices.pending"]))
		require.Equal(t, int64(10), ended(t, e.tel)["billing.invoices.pending"])
	})
}

func TestRun_a_run_stopped_by_a_signal_still_gauges_and_records_what_it_left_pending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signal := errors.New("interrupt")
		ctx, cancel := context.WithCancelCause(t.Context())
		e := newEnv(t, func(r *http.Request, _ string, _ http.ResponseWriter) {
			cancel(signal)
			select {
			case <-r.Context().Done():
			case <-time.After(time.Minute):
			}
		})
		e.subscribe("acct-1", "acct-2", "acct-3")

		err := e.run(t, month(1999, time.December), 1).Run(ctx)

		require.Equal(t, signal, err)
		require.Equal(t, []int64{3}, gauge(t, instruments(t, e.tel)["billing.invoices.pending"]))
		counts := ended(t, e.tel)
		require.Equal(t, int64(3), counts["billing.invoices.pending"])
		require.Equal(t, int64(3), counts["billing.invoices.issued"])
	})
}

func TestRun_never_writes_the_api_key_to_a_span_or_a_record(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, func(r *http.Request, account string, w http.ResponseWriter) {
			if account == "reject-1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"bad key"}`)
				return
			}
			outcomes(r, account, w)
		})
		e.subscribe("acct-1", "declined-1", "fail-1", "down-1", "reject-1")

		err := e.run(t, month(1999, time.December), 1).Run(t.Context())

		require.Error(t, err)
		require.NotEmpty(t, spansNamed(e.tel.Spans(), http.MethodPost))
		requireNoSecret(t, e.tel, e.key)
	})
}

func requireNoSecret(t *testing.T, tel *humustest.Telemetry, secrets ...string) {
	t.Helper()
	var written []string
	attrs := func(kvs []attribute.KeyValue) {
		for _, kv := range kvs {
			written = append(written, string(kv.Key), kv.Value.String())
		}
	}
	for _, s := range tel.Spans() {
		written = append(written, s.Name(), s.Status().Description)
		attrs(s.Attributes())
		for _, ev := range s.Events() {
			written = append(written, ev.Name)
			attrs(ev.Attributes)
		}
		for _, l := range s.Links() {
			attrs(l.Attributes)
		}
	}
	for _, r := range tel.Logs() {
		written = append(written, r.Body().String())
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			written = append(written, string(kv.Key), kv.Value.String())
			return true
		})
	}
	require.NotEmpty(t, written)
	for _, w := range written {
		for _, secret := range secrets {
			require.NotContains(t, w, secret)
		}
	}
}
