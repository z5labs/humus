# Observing

## Opening Grafana

`up.sh` generates Grafana's admin password into the `grafana-admin` Secret.
Read it, then forward Grafana's port:

```sh
kubectl --context kind-kind --namespace mailer get secret grafana-admin \
  --output jsonpath='{.data.password}' | base64 --decode
kubectl --context kind-kind --namespace mailer port-forward service/grafana 3000:3000
```

Sign in at <http://localhost:3000> as `admin`.

## A request's trace

Publish a request with `mailrequest`, as
[Deploying](/humus/pr-preview/pr-1179/walkthroughs/mailer/deploying/#publishing-a-request)
shows, and note the `MessageId` it prints. In **Explore**, choose **Tempo** and find the
request's trace by it:

```text
{resource.service.name="mailer" && span.messaging.message.id="<MessageId>"}
```

| Span | What it is |
|---|---|
| `process mail.send` | the delivery, a child of the span that published the request |
| `POST` | each attempt at the provider, under `process mail.send` |
| `send mail.events` | the `mail.sent` or `mail.refused` event |
| `send mail.retry` | the request, parked for another attempt |
| `send mail.dead` | the dead letter |

The worker puts the attempt on the process span:

```go
span := trace.SpanFromContext(ctx)
attempt, err = attemptOf(m)
if err != nil {
	span.SetAttributes(attemptKey.Int(1))
	return outcomeMalformed, 1, w.deadLetter(ctx, m, malformed, 1, err)
}
span.SetAttributes(attemptKey.Int(attempt))
```

And the outcome, once it is decided:

```go
trace.SpanFromContext(ctx).SetAttributes(outcomeKey.String(outcome))
```

A parked request's next `process mail.send` is a child of the `send
mail.retry` that parked it, so every attempt at a request is in one trace.

## Its log records

Tempo links each span to its records in Loki, by trace ID:

```yaml
  - name: Tempo
    uid: tempo
    type: tempo
    access: proxy
    url: http://tempo:3200
    jsonData:
      tracesToLogsV2:
        datasourceUid: loki
        filterByTraceID: true
        spanStartTimeShift: -5m
        spanEndTimeShift: 5m
      tracesToMetrics:
        datasourceUid: mimir
```

On `process mail.send`, **Logs for this span** opens them. In Explore,
with **Loki**, the same query is:

```text
{service_name="mailer"} | trace_id="<trace ID>"
```

The worker writes one record per processing: `mail request <outcome>`, with
`messaging.message.id`, `mail.attempt` and `mail.outcome`. A request it
gave up on, or a malformed one, is a warning:

```go
severity, text := log.SeverityInfo, "INFO"
if outcome == outcomeGaveUp || outcome == outcomeMalformed {
	severity, text = log.SeverityWarn, "WARN"
}
```

## Its metrics

The mailer's own instrument counts requests by `mail.outcome`: `sent`,
`refused`, `retried`, `gave_up` or `malformed`:

```go
requests, err := settings.Telemetry.MeterProvider.Meter(scope).Int64Counter("mail.requests",
	metric.WithDescription("Mail requests processed, by outcome."),
	metric.WithUnit("{request}"))
```

In Mimir, a name's dots are underscores, and `service.name` is `job`:

```text
sum by (mail_outcome) (rate(mail_requests{job="mailer"}[5m]))
```

humus records its own:

| Instrument | Recorded by |
|---|---|
| `messaging.client.consumed.messages`, `messaging.process.duration` | the Service, for each delivery |
| `messaging.client.sent.messages`, `messaging.client.operation.duration` | the three Publishers |
| `humus.circuit.state`, `humus.circuit.calls`, `humus.circuit.transitions`, `humus.retry.decisions` | the provider's client, scope `github.com/z5labs/humus/httpclient`, and each Publisher, scope `github.com/z5labs/humus/queue/rabbitmq` |

The Collector reads each queue's depth and consumers from RabbitMQ's
management API, and the containers' restarts from Kubernetes:

```yaml
  rabbitmq:
    endpoint: http://rabbitmq:15672
    username: collector
    password: ${env:RABBITMQ_PASSWORD}
    collection_interval: 10s
  k8s_cluster:
    auth_type: serviceAccount
    collection_interval: 10s
```

Its login may read and write nothing, and is tagged `monitoring`, which the
management API needs:

```sh
login collector '^$' '^$' '^$'
rabbitmqctl set_user_tags collector monitoring
```

```text
rabbitmq_message_current{rabbitmq_queue_name="mail.send", state="ready"}
rabbitmq_consumer_count{rabbitmq_queue_name="mail.send"}
k8s_container_restarts{k8s_container_name="mailer"}
```

## The alerts

`deploy/alerts.yaml` holds Grafana-managed alert rules, in the folder
**Mailer**. The kind overlay provisions them:

```yaml
  - name: grafana-alerting
    files:
      - ../alerts.yaml
```

| Alert | Severity | When |
|---|---|---|
| `MailBacklog` | page | `mail.send` has held more than 1000 ready messages for 10 minutes |
| `MailNotConsuming` | page | nothing has consumed `mail.send` for 5 minutes |
| `MailParked` | page | `mail.send.dead` isn't empty |
| `MailGivingUp` | page | the mailer gave up on a request in the last 15 minutes |
| `MailRetrying` | ticket | the mailer has been parking requests for 10 minutes |
| `MailProviderBreakerOpen` | page | the provider client's breaker has been open for 5 minutes |
| `MailRetriesThrottled` | page | a client's retries were throttled in the last 5 minutes |
| `MailCrashLooping` | page | a mailer container restarted more than 3 times in 15 minutes |

`MailBacklog` reads `mail.send` only, since a request in `mail.send.retry`
waits there on purpose.

Each rule is a query of Mimir and a threshold:

```yaml
      - uid: mail-parked
        title: MailParked
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
              expr: sum(rabbitmq_message_current{rabbitmq_queue_name="mail.send.dead"}) or vector(0)
          - refId: B
            datasourceUid: __expr__
            model:
              refId: B
              type: threshold
              expression: A
              conditions:
                - evaluator:
                    type: gt
                    params: [0]
        noDataState: OK
        execErrState: Error
        for: 0s
        labels:
          severity: page
        annotations:
          summary: mail.send.dead holds {{ $values.A.Value }} requests.
          description: Each one is a request the mailer couldn't send, with why in its x-mail-dead-reason header.
```

A counter's rule adds the series that first appeared in its window, which
`increase` alone reads as no change:

```yaml
              expr: (sum(increase(mail_requests{job="mailer", mail_outcome="gave_up"}[15m])) or vector(0)) + (sum(mail_requests{job="mailer", mail_outcome="gave_up"} unless mail_requests{job="mailer", mail_outcome="gave_up"} offset 15m) or vector(0))
```

## The dashboard

`deploy/dashboard.json` is the **Mailer** dashboard, at
<http://localhost:3000/d/mailer>. The kind overlay provisions it from a
file, so it can't be changed in Grafana:

```yaml
apiVersion: 1
providers:
  - name: mailer
    orgId: 1
    folder: Mailer
    type: file
    disableDeletion: true
    allowUiUpdates: false
    options:
      path: /etc/grafana/dashboards
```

| Panel | What it shows |
|---|---|
| Queue depth | `mail.send`, `mail.send.retry` and `mail.send.dead`, by state |
| Consumers of `mail.send` | how many mailers are consuming |
| Requests by outcome | `mail.requests`, as a rate |
| Provider breaker | the provider client's breaker, by state |
| Provider retry decisions | the provider client's retries, by decision |
| Restarts | each mailer container's restarts |

Each panel is a query of Mimir:

```json
      "targets": [
        {
          "refId": "A",
          "datasource": {
            "type": "prometheus",
            "uid": "mimir"
          },
          "expr": "sum by (mail_outcome) (rate(mail_requests{job=\"mailer\"}[$__rate_interval]))",
          "legendFormat": "{{mail_outcome}}"
        }
      ]
```
