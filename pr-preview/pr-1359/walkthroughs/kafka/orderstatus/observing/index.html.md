# Observing

## Opening Grafana

`up.sh` generates Grafana's admin password into the `grafana-admin` Secret.
Read it, then forward Grafana's port:

```sh
kubectl --context kind-kind --namespace orderstatus get secret grafana-admin \
  --output jsonpath='{.data.password}' | base64 --decode
kubectl --context kind-kind --namespace orderstatus port-forward service/grafana 3000:3000
```

Sign in at <http://localhost:3000> as `admin`.

## A record's trace

Produce an order's events with `orderevents`, as
[Deploying](/humus/pr-preview/pr-1359/walkthroughs/kafka/orderstatus/deploying/#producing-an-orders-events)
shows. In **Explore**, choose **Tempo** and find the latest records' traces:

```text
{resource.service.name="orderstatus" && name="process orders.events"}
```

| Span | What it is |
|---|---|
| `process orders.events` | the record's processing, with its partition and offset |
| `send orders.status` | each change of status it produced |
| `send orders.retry` | the record, produced for another attempt |
| `process orders.retry` | that attempt, a child of the `send orders.retry` that produced it |
| `send orders.dlq` | the dead letter |

So every attempt at a record is in one trace. A test of the App checks it,
for a record retried once, then dead-lettered:

```go
refused := processSpan(t, spans, eventsTopic, 1)
require.Equal(t, []string{"send " + retryTopic}, childNames(spans, refused))
retry := childNamed(t, spans, refused, "send "+retryTopic)
retried := processSpan(t, spans, retryTopic, 0)
require.Equal(t, retry.SpanContext().SpanID(), retried.Parent().SpanID())
require.Equal(t, refused.SpanContext().TraceID(), retried.SpanContext().TraceID())
require.Equal(t, []string{"send " + deadLetterTopic}, childNames(spans, retried))
```

A record's key, the order's ID, is on no span, so search by the topic, the
partition and the offset:

```text
{resource.service.name="orderstatus" && span.messaging.destination.partition.id="3" && span.messaging.kafka.offset=42}
```

## Its log records

In Explore, with **Loki**, the processor's records are:

```text
{service_name="orderstatus"}
```

| Record | Written by |
|---|---|
| `serving`, `shutting down` | the App, as it starts and stops |
| `partitions assigned`, `partitions revoked`, `partitions lost` | the Service, on each rebalance, with the topic and the partitions |
| `circuit breaker open`, `circuit breaker half-open`, `circuit breaker closed` | each `Producer`'s breaker, as it changes state |
| franz-go's own, at INFO and above | the Service's and the `Producer`s' clients |
| the cause, at ERROR | the App, when it stops on a record, which it names by topic, partition and offset |

The records of a crashed processor are its errors:

```text
{service_name="orderstatus"} | severity_text="ERROR"
```

A record with a trace ID links to its trace in Tempo:

```yaml
    jsonData:
      derivedFields:
        - name: trace_id
          matcherType: label
          matcherRegex: trace_id
          datasourceUid: tempo
          url: "$${__value.raw}"
          urlDisplayLabel: Trace
```

## Its metrics

The processor counts the records it retried and dead-lettered, by
`orderstatus.reason`, as
[What it counts](/humus/pr-preview/pr-1359/walkthroughs/kafka/orderstatus/retries/#what-it-counts)
shows:

```go
p.dropped.Add(ctx, 1, metric.WithAttributes(reasonKey.String(reason)))
```

In Mimir, a name's dots are underscores, and `service.name` is `job`:

```text
sum by (orderstatus_reason) (rate(orderstatus_dead_letters{job="orderstatus"}[5m]))
```

humus records its own:

| Instrument | Recorded by |
|---|---|
| `messaging.client.consumed.messages`, `messaging.process.duration` | the Service, for each record, by topic and partition |
| `messaging.client.operation.duration` | the Service, for each commit, and the `Producer`s, for each record |
| `messaging.client.sent.messages` | the `Producer`s, by topic |
| `humus.circuit.state`, `humus.circuit.calls`, `humus.circuit.transitions` | each `Producer`'s breaker, scope `github.com/z5labs/humus/queue/kafka` |
| `messaging.kafka.*` | franz-go's clients, scope `github.com/twmb/franz-go/plugin/kotel` |

The Collector reads the group's lag from Kafka, and the containers'
restarts from Kubernetes. A processor that has crashed reports neither:

```yaml
  kafkametrics:
    brokers: [kafka:9093]
    tls:
      ca_file: /etc/otel-collector-kafka/ca.crt
      cert_file: /etc/otel-collector-kafka/tls.crt
      key_file: /etc/otel-collector-kafka/tls.key
    scrapers: [consumers]
    group_match: ^orderstatus$
    topic_match: ^orders\.(events|retry)$
    collection_interval: 10s
  k8s_cluster:
    auth_type: serviceAccount
    collection_interval: 10s
```

Its login, `CN=otel-collector`, may describe the group and the two topics
it reads, and nothing else:

```sh
acl otel-collector --operation Describe --group orderstatus
acl otel-collector --operation Describe --operation DescribeConfigs --topic orders.events --topic orders.retry
```

```text
kafka_consumer_group_lag{group="orderstatus", topic="orders.events"}
k8s_container_restarts{k8s_container_name="orderstatus"}
```

## The alerts

`deploy/alerts.yaml` holds Grafana-managed alert rules, in the folder
**Order status**. The kind overlay provisions them:

```yaml
  - name: grafana-alerting
    files:
      - ../alerts.yaml
```

| Alert | Severity | When |
|---|---|---|
| `OrderStatusLagHigh` | page | the group has been more than 1000 records behind on `orders.events` for 10 minutes |
| `OrderStatusStalled` | page | a partition of `orders.events` has records waiting, and no commit in 5 minutes |
| `OrderStatusCrashLooping` | page | an `orderstatus` container restarted more than 3 times in 15 minutes |
| `OrderStatusDeadLetters` | ticket | a record was dead-lettered in the last 15 minutes |
| `OrderStatusRetrying` | ticket | records have been sent to `orders.retry` for 10 minutes |
| `OrderStatusBreakerOpen` | page | a `Producer`'s breaker is open |
| `OrderStatusSlowProcessing` | page | the p99 of processing a record of `orders.events` has been over 5s for 10 minutes |

The lag rules read `orders.events` only, since a record in `orders.retry`
waits there on purpose.

Each rule is a query of Mimir and a threshold:

```yaml
      - uid: order-status-lag-high
        title: OrderStatusLagHigh
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
              expr: sum(clamp_min(kafka_consumer_group_lag{group="orderstatus", topic="orders.events"}, 0)) or vector(0)
          - refId: B
            datasourceUid: __expr__
            model:
              refId: B
              type: threshold
              expression: A
              conditions:
                - evaluator:
                    type: gt
                    params: [1000]
        noDataState: OK
        execErrState: Error
        for: 10m
        labels:
          severity: page
```

A counter's rule adds the series that first appeared in its window, which
`increase` alone reads as no change:

```yaml
              expr: sum by (orderstatus_reason) ((increase(orderstatus_dead_letters{job="orderstatus"}[15m]) > 0) or (orderstatus_dead_letters{job="orderstatus"} unless orderstatus_dead_letters{job="orderstatus"} offset 15m)) or on () vector(0)
```

## The dashboard

`deploy/dashboard.json` is the **Order status** dashboard, at
<http://localhost:3000/d/orderstatus>. The kind overlay provisions it from
a file, so it can't be changed in Grafana:

```yaml
apiVersion: 1
providers:
  - name: orderstatus
    orgId: 1
    folder: Order status
    type: file
    disableDeletion: true
    allowUiUpdates: false
    options:
      path: /etc/grafana/dashboards
```

| Panel | What it shows |
|---|---|
| Lag of the group orderstatus | the lag on `orders.events` and `orders.retry` |
| Records processed | the records consumed, by topic |
| Records retried by reason | `orderstatus.retries`, as a rate |
| Records dead-lettered by reason | `orderstatus.dead_letters`, as a rate |
| Processing time of orders.events | its mean and p99 |
| Producers' breakers | the `Producer`s' breakers, by state |
| Restarts | each `orderstatus` container's restarts |

Each panel is a query of Mimir:

```json
      "targets": [
        {
          "refId": "A",
          "datasource": {
            "type": "prometheus",
            "uid": "mimir"
          },
          "expr": "sum by (orderstatus_reason) (rate(orderstatus_dead_letters{job=\"orderstatus\"}[$__rate_interval]))",
          "legendFormat": "{{orderstatus_reason}}"
        }
      ]
```

Next: [Production-ready](/humus/pr-preview/pr-1359/walkthroughs/kafka/orderstatus/production-ready/)
