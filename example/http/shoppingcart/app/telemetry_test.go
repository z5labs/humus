// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	catalogdatabase "github.com/z5labs/humus/example/http/shoppingcart/catalog/database"
	"github.com/z5labs/humus/example/http/shoppingcart/endpoint"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

type observed struct {
	*shoppingcart
	cart    *humustest.Telemetry
	catalog *humustest.Telemetry
}

func startObserved(t *testing.T) observed {
	t.Helper()
	cart, catalog := humustest.NewTelemetry(), humustest.NewTelemetry()
	s := startShoppingcartWith(t, map[string]string{"FAULT_TRUST_SCOPE": trustScope}, cart.Builder(), catalog.Builder())
	return observed{shoppingcart: s, cart: cart, catalog: catalog}
}

func attr(kvs []attribute.KeyValue, key attribute.Key) attribute.Value {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}

func serverSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, method, pathSuffix string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.SpanKind() != trace.SpanKindServer {
			continue
		}
		if attr(span.Attributes(), "http.request.method").AsString() == method && strings.HasSuffix(attr(span.Attributes(), "url.path").AsString(), pathSuffix) {
			return span
		}
	}
	t.Fatalf("no %s server span for a path ending %s among %d spans", method, pathSuffix, len(spans))
	return nil
}

func descends(span, ancestor sdktrace.ReadOnlySpan, spans []sdktrace.ReadOnlySpan) bool {
	byID := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byID[s.SpanContext().SpanID()] = s
	}
	for parent := span.Parent(); parent.IsValid(); {
		if parent.SpanID() == ancestor.SpanContext().SpanID() {
			return true
		}
		next, ok := byID[parent.SpanID()]
		if !ok {
			return false
		}
		parent = next.Parent()
	}
	return false
}

func catalogAttempts(spans []sdktrace.ReadOnlySpan, traceID trace.TraceID) []sdktrace.ReadOnlySpan {
	var attempts []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.SpanKind() != trace.SpanKindClient || span.SpanContext().TraceID() != traceID {
			continue
		}
		if attr(span.Attributes(), "server.address").AsString() == "catalog.internal" {
			attempts = append(attempts, span)
		}
	}
	return attempts
}

func TestInit_traces_an_add_across_the_cart_and_the_catalog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startObserved(t)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite)
		cartID := s.createCart(t, token)
		a := s.addItem(t, cartID, token)
		require.Equal(t, http.StatusCreated, a.code, "%s", a.body)
		s.stop(t)

		cartSpans := s.cart.Spans()
		add := serverSpan(t, cartSpans, http.MethodPost, "/items")
		attempts := catalogAttempts(cartSpans, add.SpanContext().TraceID())
		require.NotEmpty(t, attempts, "the cart has no attempt span in the add's trace")
		for _, attempt := range attempts {
			require.True(t, descends(attempt, add, cartSpans), "the attempt %s is not under the add", attempt.Name())
		}

		product := serverSpan(t, s.catalog.Spans(), http.MethodGet, "/products/"+sku)
		require.Equal(t, add.SpanContext().TraceID(), product.SpanContext().TraceID())
		require.True(t, slices.ContainsFunc(attempts, func(attempt sdktrace.ReadOnlySpan) bool {
			return attempt.SpanContext().SpanID() == product.Parent().SpanID()
		}), "the catalog's server span is under none of the cart's attempts")
	})
}

func TestInit_names_an_adds_server_spans_after_their_routes(t *testing.T) {
	var addName, productName string
	synctest.Test(t, func(t *testing.T) {
		s := startObserved(t)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite)
		cartID := s.createCart(t, token)
		a := s.addItem(t, cartID, token)
		require.Equal(t, http.StatusCreated, a.code, "%s", a.body)
		s.stop(t)

		addName = serverSpan(t, s.cart.Spans(), http.MethodPost, "/items").Name()
		productName = serverSpan(t, s.catalog.Spans(), http.MethodGet, "/products/"+sku).Name()
	})
	t.Logf("the add's span %q, the catalog's span %q", addName, productName)
	require.Equal(t, "POST /carts/{cartId}/items", addName)
	require.Equal(t, "GET /products/{productId}", productName)
}

func TestInit_records_a_forwarded_scenarios_evaluations_on_the_catalogs_spans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startObserved(t)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite, trustScope)
		cartID := s.createCart(t, token)
		unavailable(t, s.addItem(t, cartID, token, abort(catalogdatabase.Point)))
		s.stop(t)

		add := serverSpan(t, s.cart.Spans(), http.MethodPost, "/items")
		var aborted int
		for _, span := range s.catalog.Spans() {
			if span.SpanContext().TraceID() != add.SpanContext().TraceID() {
				continue
			}
			for _, ev := range span.Events() {
				if ev.Name != "humus.fit.evaluation" {
					continue
				}
				if attr(ev.Attributes, "humus.fit.point").AsString() == catalogdatabase.Point && attr(ev.Attributes, "humus.fit.abort").AsBool() {
					aborted++
				}
			}
		}
		require.Equal(t, s.products.queried(), aborted)
	})
}

func serverAddresses(rm metricdata.ResourceMetrics, name string) []string {
	var addrs []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				addr, _ := dp.Attributes.Value("server.address")
				if !slices.Contains(addrs, addr.AsString()) {
					addrs = append(addrs, addr.AsString())
				}
			}
		}
	}
	slices.Sort(addrs)
	return addrs
}

func TestInit_measures_the_catalog_client_and_the_key_set_client_apart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startObserved(t)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite)
		cartID := s.createCart(t, token)
		a := s.addItem(t, cartID, token)
		require.Equal(t, http.StatusCreated, a.code, "%s", a.body)
		s.stop(t)

		rm, err := s.cart.Metrics(context.Background())
		require.NoError(t, err)
		for _, name := range []string{"humus.circuit.state", "humus.circuit.calls", "humus.retry.decisions"} {
			require.Equal(t, []string{"catalog.internal", "idp.internal"}, serverAddresses(rm, name), name)
		}
	})
}

func spanStrings(spans []sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, span := range spans {
		out = append(out, span.Name(), span.Status().Description)
		for _, kv := range span.Attributes() {
			out = append(out, kv.Value.String())
		}
		for _, ev := range span.Events() {
			for _, kv := range ev.Attributes {
				out = append(out, kv.Value.String())
			}
		}
	}
	return out
}

func logStrings(records []sdklog.Record) []string {
	var out []string
	for _, r := range records {
		out = append(out, r.Body().String())
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			out = append(out, kv.Value.String())
			return true
		})
	}
	return out
}

func TestInit_puts_no_bearer_token_on_a_span_or_a_log_record(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startObserved(t)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite, trustScope)
		forged := token[:strings.LastIndex(token, ".")+1] + "c2lnbmF0dXJl"
		cartID := s.createCart(t, token)
		require.Equal(t, http.StatusCreated, s.addItem(t, cartID, token).code)
		unavailable(t, s.addItem(t, cartID, token, abort(catalogdatabase.Point)))
		require.Equal(t, http.StatusUnauthorized, s.addItem(t, cartID, forged).code)
		require.Equal(t, http.StatusUnauthorized, s.send(t, http.MethodGet, "/carts/"+cartID, forged, nil).code)
		s.stop(t)

		var recorded []string
		for _, tel := range []*humustest.Telemetry{s.cart, s.catalog} {
			recorded = append(recorded, spanStrings(tel.Spans())...)
			recorded = append(recorded, logStrings(tel.Logs())...)
		}
		require.NotEmpty(t, recorded)
		parts := strings.Split(token, ".")
		secrets := map[string]string{"token": token, "forged token": forged, "claims": parts[1], "signature": parts[2]}
		for _, v := range recorded {
			for name, secret := range secrets {
				require.NotContains(t, v, secret, "the %s was recorded", name)
			}
		}
	})
}

type resourceAttr struct {
	Key   string
	Value struct{ Value any }
}

func TestBuildTelemetry_names_the_service_cart_and_nothing_else(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"by default":           {env: map[string]string{}, want: "cart"},
		"unless it is renamed": {env: map[string]string{"OTEL_SERVICE_NAME": "cart-canary"}, want: "cart-canary"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_LOGS_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none"}
			maps.Copy(env, tc.env)
			var stdout bytes.Buffer
			sdk, err := BuildTelemetry(t.Context(), bedrock.Process{Environment: bedrock.NewStaticEnvironment(env), Stdout: &stdout}, nil)
			require.NoError(t, err)

			_, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "probe")
			span.End()
			require.NoError(t, sdk.Shutdown(context.Background()))

			var exported struct{ Resource []resourceAttr }
			require.NoError(t, json.NewDecoder(&stdout).Decode(&exported))
			require.Len(t, exported.Resource, 1, "%+v", exported.Resource)
			require.Equal(t, "service.name", exported.Resource[0].Key)
			require.Equal(t, tc.want, exported.Resource[0].Value.Value)
		})
	}
}

func TestInit_with_a_bad_telemetry_setting_exits_78(t *testing.T) {
	for name, value := range map[string]string{
		"OTEL_SDK_DISABLED":           "maybe",
		"OTEL_SERVICE_NAME":           "",
		"OTEL_TRACES_EXPORTER":        "zipkin",
		"OTEL_METRICS_EXPORTER":       "prometheus",
		"OTEL_LOGS_EXPORTER":          "file",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "collector",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				f.env[name] = value
				_, status := httpservertest.Start(t, Init,
					humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(BuildTelemetry), humus.WithFaults(Faults)),
					humustest.WithEnv(f.env),
				)
				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), "config "+name+":")
			})
		})
	}
}
