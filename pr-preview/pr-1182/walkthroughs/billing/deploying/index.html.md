# Deploying

## The image

The image builds a static binary and runs it on distroless, as a user that
isn't root. It is built from the repository's root, because the module
replaces humus with `../..`:

```dockerfile
FROM docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY example/billing/go.mod example/billing/go.sum example/billing/
RUN go -C example/billing mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/billing build -trimpath -ldflags=-s -o /out/billing .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/billing /billing
USER 65532:65532
ENTRYPOINT ["/billing"]
```

```sh
docker build --file example/billing/Dockerfile --tag billing .
```

## The CronJob

It runs daily at 03:00 UTC, one run at a time:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: billing
spec:
  schedule: "0 3 * * *"
  timeZone: Etc/UTC
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 3600
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
```

A failed run is retried up to six times, within an hour. A bad setting exits
78 and fails the Job at once, because every retry would fail the same way. A
pod evicted by a node's drain doesn't count against `backoffLimit`:

```yaml
backoffLimit: 6
activeDeadlineSeconds: 3600
podReplacementPolicy: Failed
podFailurePolicy:
  rules:
    - action: FailJob
      onExitCodes:
        containerName: billing
        operator: In
        values: [78]
    - action: Ignore
      onPodConditions:
        - type: DisruptionTarget
          status: "True"
```

The pod runs as the image's user, with no service account token:

```yaml
restartPolicy: Never
serviceAccountName: billing
automountServiceAccountToken: false
terminationGracePeriodSeconds: 30
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
```

The container's settings point at files mounted from two Secrets and a
ConfigMap, and its telemetry at an OpenTelemetry Collector:

```yaml
containers:
  - name: billing
    image: billing
    env:
      - name: DATABASE_URL_FILE
        value: /etc/billing/database/url
      - name: PAYMENTS_URL
        value: https://payments.example.com
      - name: PAYMENTS_API_KEY_FILE
        value: /etc/billing/payments/api-key
      - name: PAYMENTS_CA_FILE
        value: /etc/billing/payments-ca/ca.crt
      - name: OTEL_SERVICE_NAME
        value: billing
      - name: OTEL_TRACES_EXPORTER
        value: otlp
      - name: OTEL_METRICS_EXPORTER
        value: otlp
      - name: OTEL_LOGS_EXPORTER
        value: otlp
      - name: OTEL_EXPORTER_OTLP_ENDPOINT
        value: otel-collector:4317
```

```yaml
resources:
  requests:
    cpu: 100m
    memory: 64Mi
  limits:
    memory: 256Mi
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
volumeMounts:
  - name: database
    mountPath: /etc/billing/database
    readOnly: true
  - name: payments
    mountPath: /etc/billing/payments
    readOnly: true
  - name: payments-ca
    mountPath: /etc/billing/payments-ca
    readOnly: true
```

```yaml
volumes:
  - name: database
    secret:
      secretName: billing-database
      defaultMode: 0440
      items:
        - key: url
          path: url
  - name: payments
    secret:
      secretName: billing-payments
      defaultMode: 0440
      items:
        - key: api-key
          path: api-key
  - name: payments-ca
    configMap:
      name: billing-payments-ca
      items:
        - key: ca.crt
          path: ca.crt
```

## The kustomization

The namespace, the service account and the CronJob:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: billing
resources:
  - namespace.yaml
  - serviceaccount.yaml
  - cronjob.yaml
```

The Secrets `billing-database` and `billing-payments`, and the ConfigMap
`billing-payments-ca`, are yours to create in each cluster.

```sh
kubectl kustomize example/billing/deploy
```

## The kind overlay

The overlay takes the base's files and adds PostgreSQL, WireMock and the
LGTM stack:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: billing
resources:
  - ../namespace.yaml
  - ../serviceaccount.yaml
  - ../cronjob.yaml
  - postgres.yaml
  - wiremock.yaml
  - otel-collector.yaml
  - loki.yaml
  - tempo.yaml
  - mimir.yaml
  - grafana.yaml
```

It suspends the CronJob, so only the Jobs you create run. It uses the image
loaded into kind, and points the payments client at WireMock:

```yaml
patches:
  - target:
      kind: CronJob
      name: billing
    patch: |-
      apiVersion: batch/v1
      kind: CronJob
      metadata:
        name: billing
      spec:
        suspend: true
        jobTemplate:
          spec:
            template:
              spec:
                containers:
                  - name: billing
                    imagePullPolicy: Never
                    env:
                      - name: PAYMENTS_URL
                        value: https://wiremock
```

## PostgreSQL

The schema is a ConfigMap:

```yaml
configMapGenerator:
  - name: postgres-schema
    files:
      - ../../postgres/schema.sql
```

PostgreSQL applies it when it creates the database:

```yaml
- name: schema
  mountPath: /docker-entrypoint-initdb.d
  readOnly: true
```

## WireMock

WireMock stands in for the payments API with three mappings. The lowest
`priority` wins, so a charge without a `Bearer` API key is refused before it
can be charged:

```json
{
  "priority": 1,
  "request": {
    "method": "POST",
    "url": "/v1/charges",
    "headers": {
      "Authorization": {
        "or": [
          { "absent": true },
          { "doesNotMatch": "^Bearer \\S+$" }
        ]
      }
    }
  },
  "response": {
    "status": 401,
    "headers": {
      "Content-Type": "application/json"
    },
    "jsonBody": {
      "code": "unauthorized"
    }
  }
}
```

```json
{
  "priority": 2,
  "request": {
    "method": "POST",
    "url": "/v1/charges",
    "bodyPatterns": [
      {
        "matchesJsonPath": {
          "expression": "$.account",
          "matches": "^declined-.*"
        }
      }
    ]
  },
  "response": {
    "status": 402,
    "headers": {
      "Content-Type": "application/json"
    },
    "jsonBody": {
      "code": "card_declined"
    }
  }
}
```

The charge's ID is made from its `Idempotency-Key`, so a repeated key gets
the same answer back:

```json
{
  "priority": 3,
  "request": {
    "method": "POST",
    "url": "/v1/charges"
  },
  "response": {
    "status": 201,
    "headers": {
      "Content-Type": "application/json"
    },
    "body": "{\"id\":\"ch_{{request.headers.Idempotency-Key}}\"}",
    "transformers": ["response-template"]
  }
}
```

It serves HTTPS only, from a keystore in a Secret:

```yaml
- name: wiremock
  image: docker.io/wiremock/wiremock:3.13.2
  args:
    - --disable-http
    - --https-port
    - "8443"
    - --https-keystore
    - /etc/wiremock/keystore.p12
    - --keystore-type
    - PKCS12
    - --keystore-password
    - $(KEYSTORE_PASSWORD)
    - --key-manager-password
    - $(KEYSTORE_PASSWORD)
  env:
    - name: KEYSTORE_PASSWORD
      valueFrom:
        secretKeyRef:
          name: wiremock
          key: password
```

## The LGTM stack

The Collector takes the job's OTLP and forwards logs to Loki, traces to Tempo
and metrics to Mimir:

```yaml
extensions:
  health_check:
    endpoint: 0.0.0.0:13133
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
processors:
  batch: {}
exporters:
  otlp_http/loki:
    endpoint: http://loki:3100/otlp
  otlp_grpc/tempo:
    endpoint: tempo:4317
    tls:
      insecure: true
  otlp_http/mimir:
    endpoint: http://mimir:9009/otlp
service:
  extensions: [health_check]
  pipelines:
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp_http/loki]
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp_grpc/tempo]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp_http/mimir]
```

Grafana has all three as data sources, with a trace's records a click away:

```yaml
apiVersion: 1
datasources:
  - name: Mimir
    uid: mimir
    type: prometheus
    access: proxy
    url: http://mimir:9009/prometheus
    isDefault: true
    jsonData:
      prometheusType: Mimir
  - name: Loki
    uid: loki
    type: loki
    access: proxy
    url: http://loki:3100
    jsonData:
      derivedFields:
        - name: TraceID
          matcherType: label
          matcherRegex: trace_id
          datasourceUid: tempo
          url: "$${__value.raw}"
  - name: Tempo
    uid: tempo
    type: tempo
    access: proxy
    url: http://tempo:3200
    jsonData:
      tracesToLogsV2:
        datasourceUid: loki
        filterByTraceID: true
```

## Running it on kind

`up.sh` creates the cluster if there isn't one, and generates every secret
on each run, so none is kept in the repository:

```sh
password=$(openssl rand -hex 24)
api_key=$(openssl rand -hex 32)
grafana_password=$(openssl rand -hex 24)
```

It then makes WireMock's CA and keystore, loads the image into kind, and
applies the overlay with the Secrets:

```sh
docker build --file example/billing/Dockerfile --tag billing .
example/billing/deploy/kind/up.sh
```

Run the CronJob's template once:

```sh
kubectl --namespace billing create job billing-kind --from=cronjob/billing
kubectl --namespace billing wait --for=condition=Complete --timeout=5m job/billing-kind
```

Then run the [shared cases](/humus/pr-preview/pr-1182/walkthroughs/billing/testing/)
against it:

```sh
example/billing/deploy/kind/test.sh
```

## A backfill

A month the CronJob didn't issue is a Job from its template, with
`BILLING_PERIOD` set:

```sh
kubectl --namespace billing create job billing-2025-11 --from=cronjob/billing --dry-run=client --output yaml \
  | kubectl set env --local --filename - --containers billing BILLING_PERIOD=2025-11 --output yaml \
  | kubectl create --filename -
```

It charges whatever else is pending too. A month issued already issues
nothing again.

## A rehearsal

A rehearsal is a Job from the template with a scenario in `BAGGAGE`. This
one aborts every call to the payments API:

```sh
kubectl --namespace billing create job billing-rehearsal --from=cronjob/billing --dry-run=client --output yaml \
  | kubectl set env --local --filename - --containers billing \
    'BAGGAGE=acme.fault={%22rules%22:[{%22point%22:%22http:*%22%2C%22abort%22:true}]}' --output yaml \
  | kubectl create --filename -
```

An abort sends nothing, and a delay only waits, so the worst a rehearsal
does, even in production, is leave invoices pending for the next run. Anyone
who may create Jobs in the namespace may rehearse, so the RBAC on `jobs` is
the boundary.
