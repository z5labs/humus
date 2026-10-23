//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package functional

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/example/http/shoppingcart/cases"
	catalogdatabase "github.com/z5labs/humus/example/http/shoppingcart/catalog/database"
	catalogendpoint "github.com/z5labs/humus/example/http/shoppingcart/catalog/endpoint"
	"github.com/z5labs/humus/example/http/shoppingcart/endpoint"
	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	telemetryTimeout = 4 * time.Minute
	pollInterval     = 2 * time.Second
	addEvery         = 500 * time.Millisecond
	shed             = "isn't calling the catalog"
	breakerOpened    = "circuit breaker open"
)

func TestShoppingcart_sends_its_telemetry_to_the_LGTM_stack_on_kind(t *testing.T) {
	for _, d := range []string{catalog, cart, "otel-collector", "loki", "tempo", "mimir", "grafana"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	target := sharedTarget(t)
	tempo := "http://" + forward(t, "tempo", 3200)
	loki := "http://" + forward(t, "loki", 3100)
	mimir := "http://" + forward(t, "mimir", 9009)
	grafana := "http://" + forward(t, "grafana", 3000)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	admin := &http.Client{Timeout: 10 * time.Second, Transport: basicAuth{
		username: string(secret(t, "grafana-admin", "username")),
		password: string(secret(t, "grafana-admin", "password")),
		next:     &http.Transport{},
	}}

	start := time.Now()
	traces := openTheCatalogsBreaker(t, target)
	t.Logf("opened the catalog's breaker in %d adds", len(traces))

	ctx, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	t.Cleanup(cancel)

	var traceID string
	eventually(ctx, t, "the cart's "+breakerOpened+" record, in one of the adds' traces, in Loki", func(ctx context.Context) (bool, error) {
		q := url.Values{
			"query": {fmt.Sprintf(`{service_name=%q} |= %q`, cart, breakerOpened)},
			"start": {strconv.FormatInt(start.Add(-time.Minute).UnixNano(), 10)},
			"limit": {"100"},
		}
		var res struct {
			Data struct {
				Result []struct {
					Stream map[string]string `json:"stream"`
				} `json:"result"`
			} `json:"data"`
		}
		ok, err := getJSON(ctx, client, loki+"/loki/api/v1/query_range?"+q.Encode(), &res)
		if !ok || err != nil {
			return false, err
		}
		for _, r := range res.Data.Result {
			if id := r.Stream["trace_id"]; slices.Contains(traces, id) {
				traceID = id
				return true, nil
			}
		}
		return false, nil
	})
	t.Logf("the breaker opened in trace %s", traceID)

	eventually(ctx, t, "the add's trace, with spans of both services, in Tempo", func(ctx context.Context) (bool, error) {
		services, err := traceServices(ctx, client, tempo+"/api/v2/traces/"+traceID)
		return slices.Contains(services, cart) && slices.Contains(services, catalog), err
	})

	for _, service := range []string{cart, catalog} {
		for _, pod := range pods(t, service) {
			eventually(ctx, t, fmt.Sprintf("the %s's metrics from pod %s in Mimir", service, pod), func(ctx context.Context) (bool, error) {
				return hasSeries(ctx, client, mimir, fmt.Sprintf(`group({job=%q, k8s_pod_name=%q})`, service, pod))
			})
		}
		for _, query := range []string{
			fmt.Sprintf(`k8s_deployment_available{k8s_namespace_name=%q, k8s_deployment_name=%q} > 0`, namespace, service),
			fmt.Sprintf(`k8s_container_restarts{k8s_namespace_name=%q, k8s_container_name=%q}`, namespace, service),
		} {
			eventually(ctx, t, query+" in Mimir", func(ctx context.Context) (bool, error) {
				return hasSeries(ctx, client, mimir, query)
			})
		}
	}

	eventually(ctx, t, "Mimir, Loki and Tempo as Grafana's data sources", func(ctx context.Context) (bool, error) {
		var sources []dataSource
		ok, err := getJSON(ctx, admin, grafana+"/api/datasources", &sources)
		if !ok || err != nil {
			return false, err
		}
		for _, want := range []struct{ uid, typ string }{{"mimir", "prometheus"}, {"loki", "loki"}, {"tempo", "tempo"}} {
			if !slices.Contains(sources, dataSource{UID: want.uid, Type: want.typ}) {
				return false, fmt.Errorf("Grafana has no %s data source %s, only %v", want.typ, want.uid, sources)
			}
			if err := healthy(ctx, admin, grafana, want.uid); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

type dataSource struct {
	UID  string `json:"uid"`
	Type string `json:"type"`
}

func healthy(ctx context.Context, admin *http.Client, grafana, uid string) error {
	var health struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if _, err := getJSON(ctx, admin, grafana+"/api/datasources/uid/"+uid+"/health", &health); err != nil {
		return fmt.Errorf("the data source %s: %w", uid, err)
	}
	if health.Status != "OK" {
		return fmt.Errorf("the data source %s is %s: %s", uid, health.Status, health.Message)
	}
	return nil
}

func openTheCatalogsBreaker(t *testing.T, target cases.Target) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	defer cancel()
	productID := "sku-" + rand.Text()
	writer, err := target.Token(rand.Text(), catalogendpoint.ScopeWrite)
	require.NoError(t, err)
	code, body := send(ctx, t, target.CatalogClient, target.Catalog, http.MethodPut, "/products/"+productID, writer, "", "", map[string]any{"unitPrice": 2.5})
	require.Equal(t, http.StatusOK, code, "%s", body)

	shopper, err := target.Token(rand.Text(), endpoint.ScopeWrite, target.TrustScope)
	require.NoError(t, err)
	code, body = send(ctx, t, target.CartClient, target.Cart, http.MethodPost, "/carts", shopper, "", "", nil)
	require.Equal(t, http.StatusCreated, code, "%s", body)
	var c struct {
		CartID string `json:"cartId"`
	}
	require.NoError(t, json.Unmarshal(body, &c))
	items := "/carts/" + c.CartID + "/items"
	item := map[string]any{"productId": productID, "quantity": 1}

	codec, err := jsonscenario.New(target.ScenarioKey)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Point: catalogdatabase.Point, Fault: fit.Fault{Abort: true}}}})
	require.NoError(t, err)
	scenario := b.String()

	var traces []string
	for {
		traceID := randomHex(t, 16)
		traces = append(traces, traceID)
		code, body := send(ctx, t, target.CartClient, target.Cart, http.MethodPost, items, shopper, scenario, traceID, item)
		require.Equal(t, http.StatusServiceUnavailable, code, "%s", body)
		if strings.Contains(string(body), shed) {
			break
		}
		pause(ctx, t, addEvery)
	}

	for {
		code, body := send(ctx, t, target.CartClient, target.Cart, http.MethodPost, items, shopper, "", "", item)
		if code == http.StatusCreated {
			return traces
		}
		require.Equal(t, http.StatusServiceUnavailable, code, "%s", body)
		pause(ctx, t, addEvery)
	}
}

func send(ctx context.Context, t *testing.T, client *http.Client, base *url.URL, method, path, token, scenario, traceID string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(b)
	}
	u := base.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if scenario != "" {
		req.Header.Set("Baggage", scenario)
	}
	if traceID != "" {
		req.Header.Set("Traceparent", "00-"+traceID+"-"+randomHex(t, 8)+"-01")
	}
	resp, err := client.Do(req)
	require.NoError(t, err, "%s %s", method, u)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "%s %s", method, u)
	return resp.StatusCode, b
}

func pods(t *testing.T, service string) []string {
	t.Helper()
	names := strings.Fields(string(kubectl(t, "get", "pods",
		"--selector", "app.kubernetes.io/name="+service,
		"--field-selector", "status.phase=Running",
		"--output", "jsonpath={.items[*].metadata.name}",
	)))
	require.NotEmpty(t, names, "no %s pod is running", service)
	return names
}

func traceServices(ctx context.Context, client *http.Client, u string) ([]string, error) {
	var res struct {
		Trace struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"resource"`
			} `json:"resourceSpans"`
		} `json:"trace"`
	}
	ok, err := getJSON(ctx, client, u, &res)
	if !ok || err != nil {
		return nil, err
	}
	var services []string
	for _, rs := range res.Trace.ResourceSpans {
		for _, a := range rs.Resource.Attributes {
			if a.Key == "service.name" && !slices.Contains(services, a.Value.StringValue) {
				services = append(services, a.Value.StringValue)
			}
		}
	}
	return services, nil
}

func hasSeries(ctx context.Context, client *http.Client, mimir, query string) (bool, error) {
	var res struct {
		Data struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	ok, err := getJSON(ctx, client, mimir+"/prometheus/api/v1/query?"+url.Values{"query": {query}}.Encode(), &res)
	return ok && len(res.Data.Result) > 0, err
}

type basicAuth struct {
	username, password string
	next               http.RoundTripper
}

func (b basicAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.SetBasicAuth(b.username, b.password)
	return b.next.RoundTrip(req)
}

func getJSON(ctx context.Context, client *http.Client, u string, v any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GET %s: %s: %s", u, resp.Status, body)
	}
	return true, json.Unmarshal(body, v)
}

func eventually(ctx context.Context, t *testing.T, what string, found func(ctx context.Context) (bool, error)) {
	t.Helper()
	var last error
	for {
		ok, err := found(ctx)
		if ok && err == nil {
			return
		}
		if err != nil {
			last = err
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			require.FailNowf(t, "not found", "%s: %v (last error: %v)", what, context.Cause(ctx), last)
		}
	}
}

func pause(ctx context.Context, t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-time.After(d):
	case <-ctx.Done():
		require.FailNowf(t, "timed out", "pausing for %s: %v", d, context.Cause(ctx))
	}
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}
