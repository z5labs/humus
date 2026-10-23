//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package functional

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

const (
	telemetryTimeout = 4 * time.Minute
	pollInterval     = 2 * time.Second
)

func TestMailer_sends_its_telemetry_to_the_LGTM_stack_on_kind(t *testing.T) {
	for _, d := range []string{"mailer", "otel-collector", "loki", "tempo", "mimir"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	broker := forward(t, "rabbitmq", 5671)
	tempo := forward(t, "tempo", 3200)
	loki := forward(t, "loki", 3100)
	mimir := forward(t, "mimir", 9009)

	ctx, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	t.Cleanup(cancel)

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	start := time.Now()
	traceID := publishRequest(ctx, t, broker, pool(t, "mailer-rabbitmq-ca"))
	t.Logf("published a request in trace %s", traceID)

	eventually(ctx, t, "the request's trace in Tempo", func(ctx context.Context) (bool, error) {
		return traceHasService(ctx, client, "http://"+tempo+"/api/v2/traces/"+traceID, "mailer")
	})

	eventually(ctx, t, "the request's records in Loki", func(ctx context.Context) (bool, error) {
		q := url.Values{
			"query": {fmt.Sprintf(`{service_name="mailer"} | trace_id=%q`, traceID)},
			"start": {strconv.FormatInt(start.Add(-time.Minute).UnixNano(), 10)},
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
		{"the mailer's requests", `mail_requests{job="mailer", mail_outcome="sent"} > 0`},
		{"the mail.send queue's depth", `rabbitmq_message_current{rabbitmq_queue_name="mail.send"}`},
		{"the mail.send queue's consumers", `rabbitmq_consumer_count{rabbitmq_queue_name="mail.send"} > 0`},
		{"the mailer containers' restarts", `k8s_container_restarts{k8s_namespace_name="mailer", k8s_container_name="mailer"}`},
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

func publishRequest(ctx context.Context, t *testing.T, broker string, ca *x509.CertPool) string {
	t.Helper()
	traceID := randomHex(t, 16)

	conn, err := amqp.DialConfig("amqps://"+broker, amqp.Config{
		SASL: []amqp.Authentication{&amqp.PlainAuth{
			Username: secret(t, "mailrequest-rabbitmq", "username"),
			Password: secret(t, "mailrequest-rabbitmq", "password"),
		}},
		Vhost:           "/",
		TLSClientConfig: &tls.Config{RootCAs: ca, ServerName: "rabbitmq", MinVersion: tls.VersionTLS12},
	})
	require.NoError(t, err, "connecting to the broker at %s", broker)
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	require.NoError(t, err)
	require.NoError(t, ch.Confirm(false))

	to := strings.ToLower(rand.Text()) + "@example.com"
	body, err := json.Marshal(map[string]string{"to": to, "subject": "Your order has shipped", "text": "It arrives on Tuesday."})
	require.NoError(t, err)
	confirm, err := ch.PublishWithDeferredConfirmWithContext(ctx, "mail", "mail.send", true, false, amqp.Publishing{
		Headers:      amqp.Table{"traceparent": "00-" + traceID + "-" + randomHex(t, 8) + "-01"},
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    rand.Text(),
		Body:         body,
	})
	require.NoError(t, err)
	acked, err := confirm.WaitContext(ctx)
	require.NoError(t, err)
	require.True(t, acked, "the broker nacked the request")
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
