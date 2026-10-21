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
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/example/wallet/cases"
	"github.com/z5labs/humus/example/wallet/ledgerpb"
	"github.com/z5labs/humus/example/wallet/server"
	"github.com/z5labs/humus/example/wallet/walletpb"
	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	telemetryTimeout    = 4 * time.Minute
	pollInterval        = 2 * time.Second
	ledgerDatabasePoint = "postgresql:ledger"
)

func TestWallet_sends_its_telemetry_to_the_LGTM_stack_on_kind(t *testing.T) {
	for _, d := range []string{ledger, wallet, "otel-collector", "loki", "tempo", "mimir", "grafana"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	target := sharedTarget(t)
	tempo := "http://" + forwardHTTP(t, "tempo", 3200)
	loki := "http://" + forwardHTTP(t, "loki", 3100)
	mimir := "http://" + forwardHTTP(t, "mimir", 9009)
	grafana := "http://" + forwardHTTP(t, "grafana", 3000)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	admin := &http.Client{Timeout: 10 * time.Second, Transport: basicAuth{
		username: string(secret(t, "grafana-admin", "username")),
		password: string(secret(t, "grafana-admin", "password")),
		next:     &http.Transport{},
	}}

	start := time.Now()
	traceID := sendWhileTheLedgersDatabaseIsDown(t, target)
	t.Logf("the send failed in trace %s", traceID)

	ctx, cancel := context.WithTimeout(t.Context(), telemetryTimeout)
	t.Cleanup(cancel)

	var servedBy map[string][]string
	eventually(ctx, t, "the send's trace, with spans of both services' pods, in Tempo", func(ctx context.Context) (bool, error) {
		pods, err := tracePods(ctx, client, tempo+"/api/v2/traces/"+traceID)
		servedBy = pods
		return len(pods[wallet]) > 0 && len(pods[ledger]) > 0, err
	})
	t.Logf("the send was served by %v", servedBy)

	eventually(ctx, t, "both services' records of the send, with its trace ID, in Loki", func(ctx context.Context) (bool, error) {
		q := url.Values{
			"query": {fmt.Sprintf(`{service_name=~%q} | trace_id=%q`, wallet+"|"+ledger, traceID)},
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
		var services []string
		for _, r := range res.Data.Result {
			if r.Stream["trace_id"] == traceID {
				services = append(services, r.Stream["service_name"])
			}
		}
		return slices.Contains(services, wallet) && slices.Contains(services, ledger), nil
	})

	for _, service := range []string{wallet, ledger} {
		for _, pod := range servedBy[service] {
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
				return false, nil
			}
			var health struct {
				Status string `json:"status"`
			}
			ok, err := getJSON(ctx, admin, grafana+"/api/datasources/uid/"+want.uid+"/health", &health)
			if !ok || err != nil || health.Status != "OK" {
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

func sendWhileTheLedgersDatabaseIsDown(t *testing.T, target cases.Target) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	defer cancel()
	wallets := walletpb.NewWalletClient(target.Wallet)

	alice, bob := rand.Text(), rand.Text()
	aliceToken, err := target.Token(alice, server.ScopeRead, server.ScopeWrite, target.TrustScope)
	require.NoError(t, err)
	bobToken, err := target.Token(bob, server.ScopeRead, server.ScopeWrite)
	require.NoError(t, err)
	for _, token := range []string{aliceToken, bobToken} {
		_, err := wallets.OpenAccount(withToken(ctx, token), &walletpb.OpenAccountRequest{})
		require.NoError(t, err, "opening an account")
	}
	_, err = ledgerpb.NewLedgerClient(target.Ledger).Deposit(ctx, &ledgerpb.DepositRequest{
		TransferId:  "deposit-" + rand.Text(),
		AccountId:   alice,
		AmountCents: 1000,
	})
	require.NoError(t, err, "depositing into %s", alice)

	codec, err := jsonscenario.New(target.ScenarioKey)
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{{Point: ledgerDatabasePoint, Fault: fit.Fault{Abort: true}}}})
	require.NoError(t, err)

	sendOnceAnswered(ctx, t, wallets, aliceToken, &walletpb.SendRequest{RequestId: rand.Text(), ToAccountId: bob, AmountCents: 100})

	traceID := randomHex(t, 16)
	send := &walletpb.SendRequest{RequestId: rand.Text(), ToAccountId: bob, AmountCents: 300}
	failing := metadata.AppendToOutgoingContext(withToken(ctx, aliceToken),
		"baggage", b.String(),
		"traceparent", "00-"+traceID+"-"+randomHex(t, 8)+"-01",
	)
	_, err = wallets.Send(failing, send)
	require.Equal(t, codes.Unavailable, status.Code(err), "sending while the ledger's database is down: %v", err)

	sendOnceAnswered(ctx, t, wallets, aliceToken, send)
	return traceID
}

func sendOnceAnswered(ctx context.Context, t *testing.T, wallets walletpb.WalletClient, token string, send *walletpb.SendRequest) {
	t.Helper()
	for {
		_, err := wallets.Send(withToken(ctx, token), send)
		if status.Code(err) != codes.Unavailable {
			require.NoError(t, err, "sending %d to %s", send.GetAmountCents(), send.GetToAccountId())
			return
		}
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "sending %d to %s: %v", send.GetAmountCents(), send.GetToAccountId(), context.Cause(ctx))
		}
	}
}

func withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

func forwardHTTP(t *testing.T, service string, remotePort int) string {
	t.Helper()
	p, err := startPortForward(t.Context(), service, remotePort)
	if p != nil {
		t.Cleanup(func() {
			p.stop()
			if t.Failed() {
				t.Logf("kubectl port-forward service/%s: %v\n%s", service, p.waitErr, p.stderr)
			}
		})
	}
	require.NoError(t, err)
	return p.addr
}

func tracePods(ctx context.Context, client *http.Client, u string) (map[string][]string, error) {
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
	pods := make(map[string][]string)
	for _, rs := range res.Trace.ResourceSpans {
		var service, pod string
		for _, a := range rs.Resource.Attributes {
			switch a.Key {
			case "service.name":
				service = a.Value.StringValue
			case "k8s.pod.name":
				pod = a.Value.StringValue
			}
		}
		if service != "" && pod != "" && !slices.Contains(pods[service], pod) {
			pods[service] = append(pods[service], pod)
		}
	}
	return pods, nil
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
