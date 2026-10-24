# Observing

## Opening Grafana

`up.sh` generates Grafana's admin password into the `grafana-admin` Secret.
Read it, then forward Grafana's port:

```sh
kubectl --context kind-kind --namespace shoppingcart get secret grafana-admin \
  --output jsonpath='{.data.password}' | base64 --decode
kubectl --context kind-kind --namespace shoppingcart port-forward service/grafana 3000:3000
```

Sign in at <http://localhost:3000> as `admin`.

## An add's trace

Add an item to a cart, as
[Deploying](/humus/pr-preview/pr-1329/walkthroughs/http/shoppingcart/deploying/#calling-the-cart)
shows. Each server span is named after its method and the operation's route,
so in **Explore**, with **Tempo**, find the adds by their name:

```text
{resource.service.name="cart" && kind=server && name="POST /carts/{cartId}/items"}
```

| Span | Service | What it is |
|---|---|---|
| `POST /carts/{cartId}/items` | cart | the add |
| `GET`, with `server.address` `wiremock` | cart | a fetch of the key set, when the Verifier needs keys |
| `GET`, with `server.address` `catalog` | cart | each attempt at the catalog, under the add |
| `GET /products/{productId}` | catalog | the catalog's answer, a child of the cart's attempt |

So one trace holds both services. A test of the App checks it:

```go
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
```

A rehearsed failure is in the trace too. Each point a scenario reaches adds
a `humus.fit.evaluation` event to its span, the catalog's included:

```text
{resource.service.name="catalog" && event:name="humus.fit.evaluation"}
```

## Their log records

A record written in a request's context carries its trace ID, and links to
its trace in Tempo:

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

In Explore, with **Loki**, the cart's records are:

```text
{service_name="cart"}
```

| Record | Written by |
|---|---|
| `serving`, `shutting down`, `stopped` | the App, as it starts and stops |
| `listening`, and each line of `net/http`'s error log, at ERROR | `httpserver` |
| `request failed`, `operation panicked`, `response failed`, at ERROR | the API, `openapi/v3` |
| `keys refresh failed`, at WARN | `auth/jwt`, when the key set can't be fetched again |
| `circuit breaker open`, `circuit breaker half-open`, `circuit breaker closed` | each `httpclient` Client's breaker, with the dependency's `server.address` |

The catalog writes the same, with `service_name="catalog"`. Its only client
is the key set's.

A breaker's record is written in the context of the call that changed its
state, so the add that opened the catalog's breaker is linked from:

```text
{service_name="cart"} |= "circuit breaker open"
```

No bearer token is on a span or a record, whole or in part. A test of the
App checks both services:

```go
parts := strings.Split(token, ".")
secrets := map[string]string{"token": token, "forged token": forged, "claims": parts[1], "signature": parts[2]}
for _, v := range recorded {
	for name, secret := range secrets {
		require.NotContains(t, v, secret, "the %s was recorded", name)
	}
}
```

## Their metrics

Neither service records an instrument of its own. humus records these:

| Instrument | Recorded by |
|---|---|
| `http.server.request.duration` | `httpserver`, for each request to either service |
| `http.client.request.duration` | each `httpclient` Client, for each attempt: the cart's two, and the catalog's for the key set |
| `humus.circuit.state`, `humus.circuit.calls`, `humus.circuit.transitions`, `humus.retry.decisions` | the same Clients, scope `github.com/z5labs/humus/httpclient` |

In Mimir, a name's dots are underscores, and `service.name` is `job`:

```text
sum by (http_response_status_code) (rate(http_server_request_duration_count{job="cart"}[5m]))
```

The cart has two clients, and each data point of theirs carries its
dependency's `server.address`. A test of the App checks they're told apart:

```go
for _, name := range []string{"humus.circuit.state", "humus.circuit.calls", "humus.retry.decisions"} {
	require.Equal(t, []string{"catalog.internal", "idp.internal"}, serverAddresses(rm, name), name)
}
```

```text
sum by (server_address, humus_circuit_state) (humus_circuit_state{job="cart", otel_scope_name="github.com/z5labs/humus/httpclient"})
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
k8s_deployment_available{k8s_namespace_name="shoppingcart", k8s_deployment_name="cart"}
k8s_container_restarts{k8s_namespace_name="shoppingcart", k8s_container_name="catalog"}
```

## The alerts

`deploy/alerts.yaml` holds Grafana-managed alert rules, in the folder
**Shoppingcart**. The kind overlay provisions them:

```yaml
  - name: grafana-alerting
    files:
      - ../alerts.yaml
```

| Alert | Severity | When |
|---|---|---|
| `CartErrors` | page | the cart has answered 5xx to over 1% of its requests for 10 minutes |
| `CartSlow` | page | the cart's p99 latency has been over 1s for 10 minutes |
| `CatalogErrors` | page | the catalog has answered 5xx to over 1% of its requests for 10 minutes |
| `CartBreakerOpen` | page | a breaker of the cart's is open: the catalog's, or the key set's |
| `CartBreakerOpened` | ticket | a breaker of the cart's opened in the last 5 minutes, however briefly |
| `CartRetriesThrottled` | page | a client's retries were throttled in the last 5 minutes |
| `CartUnavailable`, `CatalogUnavailable` | page | the Deployment has had no available replica for 5 minutes |
| `CartCrashLooping`, `CatalogCrashLooping` | page | a container restarted more than 3 times in 15 minutes |

Each rule is a query of Mimir and a threshold:

```yaml
      - uid: cart-errors
        title: CartErrors
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
              expr: ((sum(rate(http_server_request_duration_count{job="cart", http_response_status_code=~"5.."}[5m])) or vector(0)) / sum(rate(http_server_request_duration_count{job="cart"}[5m])) >= 0) or vector(0)
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

The breakers' rules keep `server_address`, so an alert says which
dependency:

```yaml
              expr: max by (server_address) (humus_circuit_state{job="cart", otel_scope_name="github.com/z5labs/humus/httpclient", humus_circuit_state="open"}) or on () vector(0)
```

A counter's rule adds the series that first appeared in its window, which
`increase` alone reads as no change:

```yaml
              expr: (sum by (server_address) (increase(humus_circuit_transitions{job="cart", otel_scope_name="github.com/z5labs/humus/httpclient", humus_circuit_state="open"}[5m]) or (humus_circuit_transitions{job="cart", otel_scope_name="github.com/z5labs/humus/httpclient", humus_circuit_state="open"} unless humus_circuit_transitions{job="cart", otel_scope_name="github.com/z5labs/humus/httpclient", humus_circuit_state="open"} offset 5m))) or on () vector(0)
```

## The dashboard

`deploy/dashboard.json` is the **Shoppingcart** dashboard, at
<http://localhost:3000/d/shoppingcart>. The kind overlay provisions it from
a file, so it can't be changed in Grafana:

```yaml
apiVersion: 1
providers:
  - name: shoppingcart
    orgId: 1
    folder: Shoppingcart
    type: file
    disableDeletion: true
    allowUiUpdates: false
    options:
      path: /etc/grafana/dashboards
```

| Panel | What it shows |
|---|---|
| Cart requests by status | the cart's requests, by status code |
| Cart latency | the cart's p50 and p99 |
| Catalog requests by status | the catalog's requests, by status code |
| Cart breakers | each of the cart's breakers, by state |
| Cart breaker transitions | each of the cart's breakers' changes of state |
| Cart retry decisions | each of the cart's clients' retries, by decision |
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
          "expr": "sum by (http_response_status_code) (rate(http_server_request_duration_count{job=\"cart\"}[$__rate_interval]))",
          "legendFormat": "{{http_response_status_code}}"
        }
      ]
```

Next: [Production-ready](/humus/pr-preview/pr-1329/walkthroughs/http/shoppingcart/production-ready/)
