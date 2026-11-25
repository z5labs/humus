# Observing

## Opening Grafana

`up.sh` generates Grafana's admin password into the `grafana-admin` Secret.
Read it, then forward Grafana's port:

```sh
kubectl --context kind-kind --namespace polls get secret grafana-admin \
  --output jsonpath='{.data.password}' | base64 --decode
kubectl --context kind-kind --namespace polls port-forward service/grafana 3000:3000
```

Sign in at <http://localhost:3000> as `admin`.

## A vote's trace

Vote on a poll, as
[Trying it in a browser](/humus/pr-preview/pr-1641/walkthroughs/http/polls/deploying/#trying-it-in-a-browser)
shows. Each server span is named after its method and its route, so in
**Explore**, with **Tempo**, find the votes by their name:

```text
{resource.service.name="polls" && kind=server && name="POST /polls/{pollId}/votes"}
```

A test of the App checks the names:

```go
const (
	voteRoute = "POST /polls/{pollId}/votes"
	feedRoute = "GET /polls/{pollId}/feed"
)
```

```go
		span := only(t, tel.Spans(), voteRoute)
		require.Equal(t, codes.Unset, span.Status().Code)
```

A feed is one span, whatever it sends, and the span ends when the feed
does. So an open page's feed is in Tempo once the page closes, or
`Shutdown` ends it:

```go
		spans := tel.Spans()
		feed := only(t, spans, feedRoute)
		require.Equal(t, end.at, feed.EndTime(), "the feed's span ends as the feed does")
		require.Len(t, inTrace(spans, feed.SpanContext().TraceID()), 1, "a feed is one span, whatever it sends")
```

A vote the database can't take has an `exception` event, and its span's
status is Error. A rehearsed failure is in the trace too. Each point a
scenario reaches adds a `humus.fit.evaluation` event to its span:

```text
{resource.service.name="polls" && event:name="humus.fit.evaluation"}
```

## Its log records

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

In Explore, with **Loki**, the service's records are:

```text
{service_name="polls"}
```

| Record | Written by |
|---|---|
| `serving`, `shutting down`, `stopped` | the App, as it starts and stops |
| `listening`, and each line of `net/http`'s error log, at ERROR | `httpserver` |
| `request failed`, `response failed`, `handler panicked`, at ERROR | the Site, `hypermedia` |
| a probe's error, at ERROR, with `humus.health.probe` | `httphealth` |
| `listening for votes failed`, at ERROR | the listener, when its connection is lost |
| `a vote's notification names no poll`, at WARN | the listener, with the notification's `payload` |

The listener writes its ERROR once for each lost connection, not on each
try to listen again:

```go
		if !logged {
			l.emit(ctx, log.SeverityError, "ERROR", "listening for votes failed", err)
			logged = true
		}
```

A vote the database can't take is one ERROR record, in the vote's span. A
test of the App checks it:

```go
		require.Len(t, records, 1, "the vote's ERROR records")
		require.Equal(t, "request failed", records[0].Body().AsString())
		require.Equal(t, span.SpanContext().SpanID(), records[0].SpanID())
```

A request the browser got wrong, such as a 404 or a 422, writes none.

No voter cookie or database password is on a span or a record, whole or in
part:

```go
		requireNoneHold(t, recorded(tel), map[string]string{"voter cookie": cookie, "database password": s.f.dbPassword})
```

## The metrics

The service records no instrument of its own. `httpserver` records
`http.server.request.duration` for each request, by its route:

```go
		require.Equal(t, []string{"/health/liveness", "/polls/{pollId}", "/polls/{pollId}/feed", "/polls/{pollId}/votes"}, routes(t, rm, "http.server.request.duration"))
```

In Mimir, a name's dots are underscores, and `service.name` is `job`:

```text
sum by (http_route, http_response_status_code) (rate(http_server_request_duration_count{job="polls"}[5m]))
```

The Collector adds each pod's attributes, so the two replicas are told apart
by `k8s_pod_name`:

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

It reads the Deployment's available replicas and the containers' restarts
from Kubernetes, since a service that is down reports nothing:

```yaml
  k8s_cluster:
    auth_type: serviceAccount
    collection_interval: 10s
```

```text
k8s_deployment_available{k8s_namespace_name="polls", k8s_deployment_name="polls"}
k8s_container_restarts{k8s_namespace_name="polls", k8s_container_name="polls"}
```

## The alerts

`deploy/alerts.yaml` holds Grafana-managed alert rules, in the folder
**Polls**. The kind overlay provisions them:

```yaml
  - name: grafana-alerting
    files:
      - ../alerts.yaml
```

| Alert | Severity | When |
|---|---|---|
| `PollsErrors` | page | the service has answered 5xx to over 1% of its requests for 10 minutes |
| `PollsSlow` | page | the service's p99 latency, the feed left out, has been over 1s for 10 minutes |
| `PollsUnavailable` | page | the Deployment has had no available replica for 5 minutes |
| `PollsCrashLooping` | page | a container restarted more than 3 times in 15 minutes |

Each rule is a query of Mimir and a threshold:

```yaml
      - uid: polls-errors
        title: PollsErrors
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
              expr: ((sum(rate(http_server_request_duration_count{job="polls", http_response_status_code=~"5.."}[5m])) or vector(0)) / sum(rate(http_server_request_duration_count{job="polls"}[5m])) >= 0) or vector(0)
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

`PollsSlow` leaves the feed out, since a feed's request lasts as long as its
page is open:

```yaml
              expr: (histogram_quantile(0.99, sum by (le) (rate(http_server_request_duration_bucket{job="polls", http_route!="/polls/{pollId}/feed"}[5m]))) >= 0) or vector(0)
```

## The dashboard

`deploy/dashboard.json` is the **Polls** dashboard, at
<http://localhost:3000/d/polls>. The kind overlay provisions it from a file,
so it can't be changed in Grafana:

```yaml
apiVersion: 1
providers:
  - name: polls
    orgId: 1
    folder: Polls
    type: file
    disableDeletion: true
    allowUiUpdates: false
    options:
      path: /etc/grafana/dashboards
```

| Panel | What it shows |
|---|---|
| Requests by route and status | the service's requests, by route and status code |
| Latency by route, the feed left out | each route's p50 and p99, but the feed's |
| Available replicas | the Deployment's available replicas |
| Restarts | each pod's restarts |

Each panel is a query of Mimir:

```json
      "targets": [
        {
          "refId": "A",
          "datasource": {
            "type": "prometheus",
            "uid": "mimir"
          },
          "expr": "sum by (http_route, http_response_status_code) (rate(http_server_request_duration_count{job=\"polls\"}[$__rate_interval]))",
          "legendFormat": "{{http_route}} {{http_response_status_code}}"
        }
      ]
```

Next: [Production-ready](/humus/pr-preview/pr-1641/walkthroughs/http/polls/production-ready/)
