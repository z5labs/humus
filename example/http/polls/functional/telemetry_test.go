//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package functional

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/example/http/polls/cases"
	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	telemetryTimeout = 4 * time.Minute
	pollInterval     = 2 * time.Second
	voteRoute        = "POST /polls/{pollId}/votes"
	serverKind       = "SPAN_KIND_SERVER"
)

var optionButton = regexp.MustCompile(`<button type="submit" name="option" value="(\d+)">`)

func TestPolls_sends_its_telemetry_to_the_LGTM_stack_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", polls, "otel-collector", "loki", "tempo", "mimir", "grafana"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	target := sharedTarget(t)
	tempo := "http://" + forward(t, "service/tempo", 3200)
	loki := "http://" + forward(t, "service/loki", 3100)
	mimir := "http://" + forward(t, "service/mimir", 9009)
	grafana := "http://" + forward(t, "service/grafana", 3000)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	admin := &http.Client{Timeout: 10 * time.Second, Transport: basicAuth{
		username: string(secret(t, "grafana-admin", "username")),
		password: string(secret(t, "grafana-admin", "password")),
		next:     &http.Transport{},
	}}

	start := time.Now()
	traceID := voteWithTheDatabaseDown(t, target)
	t.Logf("voted in trace %s", traceID)

	ctx, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	t.Cleanup(cancel)

	eventually(ctx, t, "the vote's trace, with its server span named "+voteRoute+", in Tempo", func(ctx context.Context) (bool, error) {
		return hasServerSpan(ctx, client, tempo+"/api/v2/traces/"+traceID, polls, voteRoute)
	})

	eventually(ctx, t, "the vote's log records, with its trace ID, in Loki", func(ctx context.Context) (bool, error) {
		q := url.Values{
			"query": {fmt.Sprintf(`{service_name=%q} | trace_id=%q`, polls, traceID)},
			"start": {strconv.FormatInt(start.Add(-time.Minute).UnixNano(), 10)},
			"limit": {"100"},
		}
		var res struct {
			Data struct {
				Result []struct {
					Stream map[string]string `json:"stream"`
					Values [][2]string       `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		ok, err := getJSON(ctx, client, loki+"/loki/api/v1/query_range?"+q.Encode(), &res)
		if !ok || err != nil {
			return false, err
		}
		for _, r := range res.Data.Result {
			if r.Stream["trace_id"] == traceID && len(r.Values) > 0 {
				return true, nil
			}
		}
		return false, nil
	})

	for _, pod := range readyPods(t) {
		eventually(ctx, t, fmt.Sprintf("the service's metrics from pod %s in Mimir", pod), func(ctx context.Context) (bool, error) {
			return hasSeries(ctx, client, mimir, fmt.Sprintf(`group({job=%q, k8s_pod_name=%q})`, polls, pod))
		})
	}
	for _, query := range []string{
		fmt.Sprintf(`k8s_deployment_available{k8s_namespace_name=%q, k8s_deployment_name=%q} > 0`, namespace, polls),
		fmt.Sprintf(`k8s_container_restarts{k8s_namespace_name=%q, k8s_container_name=%q}`, namespace, polls),
	} {
		eventually(ctx, t, query+" in Mimir", func(ctx context.Context) (bool, error) {
			return hasSeries(ctx, client, mimir, query)
		})
	}

	eventually(ctx, t, "Mimir, Loki and Tempo as Grafana's data sources", func(ctx context.Context) (bool, error) {
		var sources []dataSource
		ok, err := getJSON(ctx, admin, grafana+"/api/datasources", &sources)
		if !ok || err != nil {
			return false, err
		}
		for _, want := range []dataSource{{UID: "mimir", Type: "prometheus"}, {UID: "loki", Type: "loki"}, {UID: "tempo", Type: "tempo"}} {
			if !slices.Contains(sources, want) {
				return false, fmt.Errorf("Grafana has no %s data source %s, only %v", want.Type, want.UID, sources)
			}
			if err := healthy(ctx, admin, grafana, want.UID); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

func voteWithTheDatabaseDown(t *testing.T, target cases.Target) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	defer cancel()
	c := target.TrustedBrowser()
	replica := target.Replicas[0]
	origin := replica.Scheme + "://" + replica.Host

	form := url.Values{"question": {"Tabs or spaces? " + rand.Text()}, "option1": {"Tabs"}, "option2": {"Spaces"}}
	resp, body := request(ctx, t, c, http.MethodPost, replica.JoinPath("/polls"), form, htmx(replica, origin, "/", "new-poll"))
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", body)
	pollPath := resp.Header.Get("HX-Redirect")
	require.NotEmpty(t, pollPath, "creating a poll redirected nowhere")

	resp, body = request(ctx, t, c, http.MethodGet, replica.JoinPath(pollPath), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)
	option := optionButton.FindStringSubmatch(body)
	require.NotNil(t, option, "the poll's page has no option to vote for: %s", body)

	codec, err := jsonscenario.New(target.ScenarioKey)
	require.NoError(t, err)
	bag, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Point: store.Point, Fault: fit.Fault{Abort: true}}}})
	require.NoError(t, err)

	traceID := randomHex(t, 16)
	header := htmx(replica, origin, pollPath, "results")
	header["Baggage"] = bag.String()
	header["Traceparent"] = "00-" + traceID + "-" + randomHex(t, 8) + "-01"
	resp, body = request(ctx, t, c, http.MethodPost, replica.JoinPath(pollPath, "votes"), url.Values{"option": {option[1]}}, header)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "%s", body)
	return traceID
}

func htmx(replica *url.URL, origin, from, target string) map[string]string {
	h := map[string]string{
		"HX-Request":     "true",
		"HX-Current-URL": replica.JoinPath(from).String(),
		"HX-Target":      target,
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
		"Origin":         origin,
	}
	if target == "new-poll" {
		h["HX-Trigger"] = target
	}
	return h
}

func request(ctx context.Context, t *testing.T, c *http.Client, method string, u *url.URL, form url.Values, header map[string]string) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	require.NoError(t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	require.NoError(t, err, "%s %s", method, u)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "%s %s", method, u)
	return resp, string(b)
}

func hasServerSpan(ctx context.Context, client *http.Client, u, service, name string) (bool, error) {
	var res struct {
		Trace struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []attribute `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []struct {
						Name string          `json:"name"`
						Kind json.RawMessage `json:"kind"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		} `json:"trace"`
	}
	ok, err := getJSON(ctx, client, u, &res)
	if !ok || err != nil {
		return false, err
	}
	for _, rs := range res.Trace.ResourceSpans {
		if !slices.ContainsFunc(rs.Resource.Attributes, func(a attribute) bool {
			return a.Key == "service.name" && a.Value.StringValue == service
		}) {
			continue
		}
		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				if span.Name == name && isServer(span.Kind) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

type attribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}

func isServer(kind json.RawMessage) bool {
	var s string
	if json.Unmarshal(kind, &s) == nil {
		return s == serverKind
	}
	var n int
	return json.Unmarshal(kind, &n) == nil && n == 2
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

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}
