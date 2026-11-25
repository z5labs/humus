// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/humustest"
)

// scope is the instrumentation scope of the client's own instruments and
// log records: its import path.
const scope = "github.com/z5labs/humus/http3/http3client"

// counts returns the data points of the int64 sum named name, in the
// client's scope, keyed by their key attribute. It fails t unless every data
// point carries the dependency's server.address, payments, and server.port,
// 8443.
func counts(t *testing.T, tel *humustest.Telemetry, name, key string) map[string]int64 {
	t.Helper()
	return countsIn(t, tel, scope, name, key)
}

// countsIn is counts, in the scope named in.
func countsIn(t *testing.T, tel *humustest.Telemetry, in, name, key string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, dp := range dataPoints(t, tel, in, name) {
		v, ok := dp.Attributes.Value(attribute.Key(key))
		require.True(t, ok, "a data point of %s without %s", name, key)
		out[v.AsString()] += dp.Value
	}
	return out
}

// requireCounts requires counts to be want.
func requireCounts(t *testing.T, tel *humustest.Telemetry, name, key string, want map[string]int64) {
	t.Helper()
	require.Equal(t, want, counts(t, tel, name, key), name)
}

// dataPoints returns the data points of the int64 sum named name in the
// scope named in, once the App has exited.
func dataPoints(t *testing.T, tel *humustest.Telemetry, in, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	rm, err := tel.Metrics(context.Background())
	require.NoError(t, err)
	var out []metricdata.DataPoint[int64]
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != in {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "%s is a %T", name, m.Data)
			for _, dp := range sum.DataPoints {
				address, _ := dp.Attributes.Value("server.address")
				port, _ := dp.Attributes.Value("server.port")
				require.Equal(t, "payments", address.AsString(), "%s's server.address", name)
				require.EqualValues(t, 8443, port.AsInt64(), "%s's server.port", name)
				out = append(out, dp)
			}
		}
	}
	return out
}

// scopes returns, for each instrument whose name starts with humus., the
// scopes it was recorded under.
func scopes(t *testing.T, tel *humustest.Telemetry) map[string][]string {
	t.Helper()
	rm, err := tel.Metrics(context.Background())
	require.NoError(t, err)
	out := map[string][]string{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if strings.HasPrefix(m.Name, "humus.") {
				out[m.Name] = append(out[m.Name], sm.Scope.Name)
			}
		}
	}
	for _, s := range out {
		sort.Strings(s)
	}
	return out
}

// clientSpans returns the client spans tel recorded: otelhttp's, one for
// each attempt.
func clientSpans(tel *humustest.Telemetry) []map[string]string {
	var out []map[string]string
	for _, s := range tel.Spans() {
		if s.SpanKind() != trace.SpanKindClient {
			continue
		}
		attrs := map[string]string{}
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.String()
		}
		out = append(out, attrs)
	}
	return out
}

func TestClient_records_each_attempt_as_HTTP3(t *testing.T) {
	t.Run("when it succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			serveH3(t, h, ca.Issue(t, "payments"), hello)

			tel := humustest.NewTelemetry()
			st := runJob(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
				_, err := get(ctx, c, "https://payments:8443/")
				return err
			})
			requireExitZero(t, st)

			spans := clientSpans(tel)
			require.Len(t, spans, 1)
			require.Equal(t, "3.0", spans[0]["network.protocol.version"])
			require.Equal(t, "200", spans[0]["http.response.status_code"])
		})
	})

	t.Run("when it fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)

			tel := humustest.NewTelemetry()
			var callErr error
			st := runJob(t, h.Host(), tel.Builder(), paymentsSettings(t, ca), func(ctx context.Context, c *http.Client) error {
				_, callErr = get(ctx, c, "https://payments:8443/")
				return nil
			})
			requireExitZero(t, st)

			require.Error(t, callErr)
			spans := clientSpans(tel)
			require.Len(t, spans, 3, "one for each attempt")
			for _, s := range spans {
				require.Equal(t, "3.0", s["network.protocol.version"], "a failed attempt records HTTP/3 too")
				require.NotEmpty(t, s["error.type"])
			}
		})
	})
}

func TestClient_records_httpclients_instruments_under_its_own_scope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		serveH3(t, h, ca.Issue(t, "payments"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))

		tel := humustest.NewTelemetry()
		settings := paymentsSettings(t, ca)
		settings.MaxAttempts = 1
		var errs []error
		st := runJob(t, h.Host(), tel.Builder(), settings, func(ctx context.Context, c *http.Client) error {
			// The default policy opens the breaker after 5 failures in a
			// row, and the sixth call is rejected.
			for range 6 {
				_, err := get(ctx, c, "https://payments:8443/")
				errs = append(errs, err)
			}
			return nil
		})
		requireExitZero(t, st)

		require.ErrorIs(t, errs[5], circuit.ErrRejected)
		require.Equal(t, map[string][]string{
			"humus.circuit.state":       {scope},
			"humus.circuit.transitions": {scope},
			"humus.circuit.calls":       {scope},
			"humus.retry.decisions":     {scope},
		}, scopes(t, tel), "httpclient's instruments, under http3client's scope alone")
		requireCounts(t, tel, "humus.circuit.calls", "humus.circuit.outcome", map[string]int64{"failure": 5, "rejected": 1})
		requireCounts(t, tel, "humus.circuit.transitions", "humus.circuit.state", map[string]int64{"open": 1})
		requireCounts(t, tel, "humus.circuit.state", "humus.circuit.state", map[string]int64{"closed": 0, "open": 1, "half-open": 0})
		requireCounts(t, tel, "humus.retry.decisions", "humus.retry.decision", map[string]int64{"exhausted": 5, "stopped": 1})

		var records []string
		for _, r := range tel.Logs() {
			if r.InstrumentationScope().Name == scope {
				records = append(records, r.Body().AsString())
			}
		}
		require.Equal(t, []string{"circuit breaker open"}, records, "the breaker logs under the client's scope")
		for _, r := range tel.Logs() {
			require.NotEqual(t, "github.com/z5labs/humus/http/httpclient", r.InstrumentationScope().Name)
		}
	})
}
