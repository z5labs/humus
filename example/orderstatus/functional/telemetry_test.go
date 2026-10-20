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
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	telemetryTimeout = 4 * time.Minute
	pollInterval     = 2 * time.Second
	logLookback      = time.Hour
)

func TestOrderStatus_sends_its_telemetry_to_the_LGTM_stack_on_kind(t *testing.T) {
	for _, d := range []string{"orderstatus", "otel-collector", "loki", "tempo", "mimir"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	env := settings(t)
	kafka := forward(t, "kafka", 9093)
	tempo := forward(t, "tempo", 3200)
	loki := forward(t, "loki", 3100)
	mimir := forward(t, "mimir", 9009)

	ctx, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	t.Cleanup(cancel)

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	start := time.Now()
	traceID := projectOrder(ctx, t, kafka, env.get(t, "KAFKA_EVENTS_TOPIC"))
	t.Logf("produced an order's event in trace %s", traceID)

	eventually(ctx, t, "the order's trace in Tempo", func(ctx context.Context) (bool, error) {
		return traceHasService(ctx, client, "http://"+tempo+"/api/v2/traces/"+traceID, "orderstatus")
	})

	eventually(ctx, t, "the processor's records in Loki", func(ctx context.Context) (bool, error) {
		q := url.Values{
			"query": {`{service_name="orderstatus"}`},
			"start": {strconv.FormatInt(start.Add(-logLookback).UnixNano(), 10)},
			"limit": {"100"},
		}
		var res struct {
			Data struct {
				Result []json.RawMessage `json:"result"`
			} `json:"data"`
		}
		ok, err := getJSON(ctx, client, "http://"+loki+"/loki/api/v1/query_range?"+q.Encode(), &res)
		return ok && len(res.Data.Result) > 0, err
	})

	for _, c := range []struct {
		what  string
		query string
	}{
		{"the processor's consumed records", `messaging_client_consumed_messages{job="orderstatus", messaging_destination_name="orders.events"} > 0`},
		{"the processor's Producer calls", `humus_circuit_calls{job="orderstatus"} > 0`},
		{"the group's lag", `kafka_consumer_group_lag{group="orderstatus", topic="orders.events"}`},
		{"the processor containers' restarts", `k8s_container_restarts{k8s_namespace_name="orderstatus", k8s_container_name="orderstatus"}`},
	} {
		eventually(ctx, t, c.what+" in Mimir", func(ctx context.Context) (bool, error) {
			var res struct {
				Data struct {
					Result []json.RawMessage `json:"result"`
				} `json:"data"`
			}
			ok, err := getJSON(ctx, client, "http://"+mimir+"/prometheus/api/v1/query?"+url.Values{"query": {c.query}}.Encode(), &res)
			return ok && len(res.Data.Result) > 0, err
		})
	}
}

func projectOrder(ctx context.Context, t *testing.T, forwarded, topic string) string {
	t.Helper()
	traceID := randomHex(t, 16)

	cl, err := kgo.NewClient(kgo.SeedBrokers(broker), kgo.Dialer(dialer(t, forwarded)))
	require.NoError(t, err)
	t.Cleanup(cl.Close)

	value, err := json.Marshal(map[string]any{"type": "placed", "at": time.Now().UTC()})
	require.NoError(t, err)
	err = cl.ProduceSync(ctx, &kgo.Record{
		Topic:   topic,
		Key:     []byte("order-" + rand.Text()),
		Value:   value,
		Headers: []kgo.RecordHeader{{Key: "traceparent", Value: []byte("00-" + traceID + "-" + randomHex(t, 8) + "-01")}},
	}).FirstErr()
	require.NoError(t, err, "producing an order's event to %s", topic)
	return traceID
}

func traceHasService(ctx context.Context, client *http.Client, u, service string) (bool, error) {
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
		return false, err
	}
	for _, rs := range res.Trace.ResourceSpans {
		for _, a := range rs.Resource.Attributes {
			if a.Key == "service.name" && a.Value.StringValue == service {
				return true, nil
			}
		}
	}
	return false, nil
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
	switch resp.StatusCode {
	case http.StatusOK:
		return true, json.Unmarshal(body, v)
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("GET %s: %s: %s", u, resp.Status, body)
	}
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
