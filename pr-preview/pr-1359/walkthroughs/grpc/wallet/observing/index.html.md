# Observing

## Opening Grafana

`up.sh` generates Grafana's admin password into the `grafana-admin` Secret.
Read it, then forward Grafana's port:

```sh
kubectl --context kind-kind --namespace wallet get secret grafana-admin \
  --output jsonpath='{.data.password}' | base64 --decode
kubectl --context kind-kind --namespace wallet port-forward service/grafana 3000:3000
```

Sign in at <http://localhost:3000> as `admin`.

## A send's trace

Send from alice's account to bob's, as
[Deploying](/humus/pr-preview/pr-1359/walkthroughs/grpc/wallet/deploying/#trying-it-with-walletctl)
shows. Each server span is named after its method, so in **Explore**, with
**Tempo**, find the sends by name:

```text
{resource.service.name="wallet" && kind=server && name="wallet.v1.Wallet/Send"}
```

| Span | Service | What it is |
|---|---|---|
| `wallet.v1.Wallet/Send`, a server span | wallet | the send |
| `GET`, with `server.address` `wiremock` | wallet | a fetch of the key set, when the Verifier lacks the token's key |
| `ledger.v1.Ledger/Transfer`, a client span | wallet | each attempt at the ledger, under the send |
| `ledger.v1.Ledger/Transfer`, a server span | ledger | the ledger's answer, a child of the wallet's attempt |

So one trace holds both services. A test of the Apps checks it:

```go
walletSpans := o.walletTel.Spans()
call := serverSpanNamed(t, walletSpans, "wallet.v1.Wallet/"+method)
traceID := call.SpanContext().TraceID()

attempts = spansNamed(walletSpans, trace.SpanKindClient, "ledger.v1.Ledger/Transfer", traceID)
require.NotEmpty(t, attempts, "the wallet has no attempt span in the %s's trace", method)
for _, attempt := range attempts {
	require.True(t, descends(attempt, call, walletSpans), "an attempt is not under the wallet's %s", method)
}

ledgerSpans = spansNamed(o.ledgerTel.Spans(), trace.SpanKindServer, "ledger.v1.Ledger/Transfer", traceID)
require.Len(t, ledgerSpans, len(attempts), "the ledger's server spans in the %s's trace", method)
```

A watch is one span on each side, `wallet.v1.Wallet/WatchAccount` and
`ledger.v1.Ledger/WatchAccount`, from its opening to its end.

A rehearsed failure is in the trace too. A send whose scenario aborts
`postgresql:ledger`, as the case `TheLedgersDatabaseIsDown` in
[The failure injection cases](/humus/pr-preview/pr-1359/walkthroughs/grpc/wallet/testing/#the-failure-injection-cases)
sends, has one attempt for each of the Client's `MaxAttempts`:

```go
err := o.send(withScenario(t, t.Context(), abort(ledgerPoint)), alice, bob, rand.Text(), 200)
requireCode(t, err, codes.Unavailable)
o.stop(t)

attempts, _ := requireOneTrace(t, o, "Send")
require.Len(t, attempts, grpcclient.DefaultSettings().MaxAttempts)
```

Each point a scenario reaches adds a `humus.fit.evaluation` event to its
span, the ledger's included:

```text
{resource.service.name="ledger" && event:name="humus.fit.evaluation"}
```

## Their log records

A record written in a call's context carries its trace ID, and links to its
trace in Tempo:

```yaml
  - name: Loki
    uid: loki
    type: loki
    access: proxy
    url: http://loki:3100
    jsonData:
      derivedFields:
        - name: trace_id
          matcherType: label
          matcherRegex: trace_id
          datasourceUid: tempo
          url: "$${__value.raw}"
          urlDisplayLabel: Trace
```

In Explore, with **Loki**, the wallet's records are:

```text
{service_name="wallet"}
```

humus writes these:

| Record | Written by |
|---|---|
| `serving`, `shutting down`, `stopped` | the App, as it starts and stops |
| `listening`, and a probe's error at ERROR, with `humus.health.probe` | `grpchealth`, on the probes port |
| `authentication failed`, at ERROR | `grpcauth`, when a call's credentials are refused |
| `RPC panicked`, at ERROR | `grpcserver` |
| `keys refresh failed`, at WARN | `auth/jwt`, when the key set can't be fetched again |
| `circuit breaker open`, `circuit breaker half-open`, `circuit breaker closed` | each of the wallet's Clients' breakers, with the dependency's `server.address` |

The ledger writes the same, with `service_name="ledger"`, but none of the
last two: it calls nothing but PostgreSQL.

Each service writes one WARN record of its own, with `rpc.method` and
`exception.message`: the wallet when it answers `Unavailable` for the ledger,
and the ledger when a transaction fails:

```go
func (s *Server) answer(ctx context.Context, err error) error {
	answered := answer(ctx, err)
	if status.Code(answered) == codes.Unavailable {
		s.warn(ctx, err)
	}
	return answered
}

func (s *Server) warn(ctx context.Context, err error) {
	method, _ := grpc.Method(ctx)
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(log.SeverityWarn)
	r.SetSeverityText("WARN")
	r.SetBody(attribute.StringValue("the wallet answered Unavailable for the ledger"))
	r.AddAttributes(semconv.RPCMethod(strings.TrimPrefix(method, "/")), semconv.ExceptionMessage(err.Error()))
	s.logger.Emit(context.WithoutCancel(ctx), r)
}
```

So a send the ledger failed is linked from:

```text
{service_name="wallet"} |= "the wallet answered Unavailable for the ledger"
```

No bearer token is on a span or a record, whole or in part. A test of the
Apps checks both services:

```go
parts := strings.Split(alice.token, ".")
secrets := map[string]string{"token": alice.token, "forged token": forged.token, "claims": parts[1], "signature": parts[2]}
for _, v := range recorded {
	for name, secret := range secrets {
		require.NotContains(t, v, secret, "the %s was recorded", name)
	}
}
```

## Their metrics

| Instrument | Recorded by |
|---|---|
| `rpc.server.call.duration` | otelgrpc, in each service, for each call |
| `rpc.client.call.duration` | otelgrpc, in the wallet, for each attempt at the ledger |
| `http.client.request.duration` | the wallet's Client for the key set, for each attempt |
| `humus.circuit.state`, `humus.circuit.calls`, `humus.circuit.transitions`, `humus.retry.decisions` | the wallet's two Clients, scopes `github.com/z5labs/humus/grpc/grpcclient` and `github.com/z5labs/humus/httpclient` |
| `ledger.transfers`, by `ledger.transfer.outcome` | the ledger, for each transfer it answers: `posted`, `replayed` or `refused` |
| `ledger.watches` | the ledger, the watches open on it |
| `wallet.watches.reopened` | the wallet, each watch of the ledger's it opened again |

The ledger records its own two:

```go
meter := tel.MeterProvider.Meter(scope)
transfers, err := meter.Int64Counter("ledger.transfers",
	metric.WithDescription("Transfers the ledger answered, by outcome."),
	metric.WithUnit("{transfer}"))
if err != nil {
	return nil, err
}
watches, err := meter.Int64UpDownCounter("ledger.watches",
	metric.WithDescription("Watches of an account open on the ledger."),
	metric.WithUnit("{watch}"))
```

A test of the Apps checks the outcomes, a repeated send included:

```go
require.Equal(t, map[string]int64{"posted": 2, "replayed": 1, "refused": 1}, sums(t, o.ledgerTel, "ledger.transfers", "ledger.transfer.outcome"))
```

In Mimir, a name's dots are underscores, and `service.name` is `job`:

```text
sum by (rpc_method, rpc_response_status_code) (rate(rpc_server_call_duration_count{job="wallet"}[5m]))
sum by (ledger_transfer_outcome) (increase(ledger_transfers{job="ledger"}[5m]))
```

Each data point of the wallet's Clients carries its dependency's
`server.address`, `ledger` or `wiremock`:

```text
sum by (server_address, humus_circuit_state) (humus_circuit_state{job="wallet", otel_scope_name=~"github.com/z5labs/humus/(grpc/grpcclient|httpclient)"})
```

The Collector adds each pod's attributes, so the two replicas of a service
are told apart by `k8s_pod_name`:

```yaml
  k8s_attributes:
    auth_type: serviceAccount
    pod_association:
      - sources:
          - from: connection
    extract:
      metadata:
        - k8s.namespace.name
        - k8s.pod.name
        - k8s.deployment.name
        - k8s.node.name
```

It reads the Deployments' available replicas and the containers' restarts
from Kubernetes, since a service that is down reports nothing:

```yaml
  k8s_cluster:
    auth_type: serviceAccount
    collection_interval: 10s
```

```text
k8s_deployment_available{k8s_namespace_name="wallet", k8s_deployment_name="ledger"}
k8s_container_restarts{k8s_namespace_name="wallet", k8s_container_name="wallet"}
```

## The alerts

`deploy/alerts.yaml` holds Grafana-managed alert rules, in the folder
**Wallet**. The kind overlay provisions them:

```yaml
  - name: grafana-alerting
    files:
      - ../alerts.yaml
```

| Alert | Severity | When |
|---|---|---|
| `WalletErrors` | page | the wallet has ended over 1% of its unary calls with a server's code for 10 minutes |
| `WalletSlow` | page | the wallet's p99 latency of unary calls has been over 1s for 10 minutes |
| `LedgerErrors` | page | the ledger has ended over 1% of its unary calls with a server's code for 10 minutes |
| `WalletBreakerOpen` | page | a breaker of the wallet's is open: the ledger's, or the key set's |
| `WalletBreakerOpened` | ticket | a breaker of the wallet's opened in the last 5 minutes, however briefly |
| `WalletRetriesThrottled` | page | a Client's retries were throttled in the last 5 minutes |
| `WalletUnavailable`, `LedgerUnavailable` | page | the Deployment has had no available replica for 5 minutes |
| `WalletCrashLooping`, `LedgerCrashLooping` | page | a container restarted more than 3 times in 15 minutes |

Each rule is a query of Mimir and a threshold. A server's codes are those
`grpcserver` marks its spans `Error` for. `WatchAccount` is left out, since
a watch's duration is how long it was open, and every rollout ends it with
`Unavailable`:

```yaml
      - uid: wallet-errors
        title: WalletErrors
        condition: B
        data:
          - refId: A
            relativeTimeRange:
              from: 600
              to: 0
            datasourceUid: mimir
            model:
              refId: A
              instant: true
              expr: ((sum(rate(rpc_server_call_duration_count{job="wallet", rpc_method!="wallet.v1.Wallet/WatchAccount", rpc_response_status_code=~"UNKNOWN|DEADLINE_EXCEEDED|UNIMPLEMENTED|INTERNAL|UNAVAILABLE|DATA_LOSS"}[5m])) or vector(0)) / sum(rate(rpc_server_call_duration_count{job="wallet", rpc_method!="wallet.v1.Wallet/WatchAccount"}[5m])) >= 0) or vector(0)
          - refId: B
            datasourceUid: __expr__
            model:
              refId: B
              type: threshold
              expression: A
              conditions:
                - evaluator:
                    type: gt
                    params: [0.01]
        noDataState: OK
        execErrState: Error
        for: 10m
        labels:
          severity: page
```

The breakers' and retries' rules keep `server_address`, so an alert says
which dependency:

```yaml
              expr: max by (server_address) (humus_circuit_state{job="wallet", otel_scope_name=~"github.com/z5labs/humus/(grpc/grpcclient|httpclient)", humus_circuit_state="open"}) or on () vector(0)
```

A counter's rule adds the series that first appeared in its window, which
`increase` alone reads as no change:

```yaml
              expr: (sum by (server_address) (increase(humus_circuit_transitions{job="wallet", otel_scope_name=~"github.com/z5labs/humus/(grpc/grpcclient|httpclient)", humus_circuit_state="open"}[5m]) or (humus_circuit_transitions{job="wallet", otel_scope_name=~"github.com/z5labs/humus/(grpc/grpcclient|httpclient)", humus_circuit_state="open"} unless humus_circuit_transitions{job="wallet", otel_scope_name=~"github.com/z5labs/humus/(grpc/grpcclient|httpclient)", humus_circuit_state="open"} offset 5m))) or on () vector(0)
```

## The dashboard

`deploy/dashboard.json` is the **Wallet** dashboard, at
<http://localhost:3000/d/wallet>. The kind overlay provisions it from a
file, so it can't be changed in Grafana:

```yaml
apiVersion: 1
providers:
  - name: wallet
    orgId: 1
    folder: Wallet
    type: file
    disableDeletion: true
    allowUiUpdates: false
    options:
      path: /etc/grafana/dashboards
```

| Panel | What it shows |
|---|---|
| Wallet calls by method and code | the wallet's calls, by method and code |
| Ledger calls by method and code | the ledger's calls, by method and code |
| Wallet latency of unary calls | the wallet's p50 and p99, without `WatchAccount` |
| Ledger latency of unary calls | the ledger's p50 and p99, without `WatchAccount` |
| Open watches | the watches open on the ledger |
| Watches opened again | the ledger's watches the wallet opened again |
| Transfers by outcome | the ledger's transfers, `posted`, `replayed` or `refused` |
| Wallet breakers | each of the wallet's breakers, by state |
| Wallet breaker transitions | each of the wallet's breakers' changes of state |
| Wallet retry decisions | each of the wallet's Clients' retries, by decision |
| Available replicas | each Deployment's available replicas |
| Restarts | each container's restarts |

Each panel is a query of Mimir:

```json
      "targets": [
        {
          "refId": "A",
          "datasource": {
            "type": "prometheus",
            "uid": "mimir"
          },
          "expr": "sum by (rpc_method, rpc_response_status_code) (rate(rpc_server_call_duration_count{job=\"wallet\"}[$__rate_interval]))",
          "legendFormat": "{{rpc_method}} {{rpc_response_status_code}}"
        }
      ]
```

Next: [Production-ready](/humus/pr-preview/pr-1359/walkthroughs/grpc/wallet/production-ready/)
