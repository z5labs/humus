---
title: Deploying
linkTitle: Deploying
description: >
  How the mailer is deployed: its image, the kustomization, the Deployment
  and its disruption budget, the grace period, and the kind overlay. Then
  running it on kind with up.sh, and publishing requests to it with
  mailrequest.
type: docs
weight: 80
---

## The image

One image holds `mailer` and `mailrequest`, built static onto a distroless,
non-root base:

```dockerfile
FROM docker.io/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY queue/rabbitmq/go.mod queue/rabbitmq/go.sum queue/rabbitmq/
COPY example/mailer/go.mod example/mailer/go.sum example/mailer/
RUN go -C example/mailer mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/mailer build -trimpath -ldflags=-s -o /out/mailer . && \
    CGO_ENABLED=0 go -C example/mailer build -trimpath -ldflags=-s -o /out/mailrequest ./cmd/mailrequest

FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
COPY --from=build /out/mailer /mailer
COPY --from=build /out/mailrequest /mailrequest
USER 65532:65532
ENTRYPOINT ["/mailer"]
```

Build it from the repository's root, since the module replaces humus with
its directory:

```sh
docker build --file example/mailer/Dockerfile --tag mailer .
```

## The kustomization

`deploy/base` holds a ServiceAccount, the Deployment, its disruption budget
and the settings:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - serviceaccount.yaml
  - deployment.yaml
  - poddisruptionbudget.yaml
configMapGenerator:
  - name: mailer
    literals:
      - RABBITMQ_ADDR=rabbitmq:5671
      - RABBITMQ_USERNAME=mailer
      - RABBITMQ_PASSWORD_FILE=/etc/mailer/rabbitmq/password
      - RABBITMQ_CA_FILE=/etc/mailer/rabbitmq-ca/ca.crt
      - MAIL_FROM=mailer@example.com
      - PROVIDER_URL=https://mail-provider.example.com
      - PROVIDER_API_KEY_FILE=/etc/mailer/provider/api-key
      - PROBES_ADDR=:8443
      - PROBES_TLS_PEM_FILE=/etc/mailer/probes/tls.pem
      - OTEL_SERVICE_NAME=mailer
      - OTEL_TRACES_EXPORTER=otlp
      - OTEL_METRICS_EXPORTER=otlp
      - OTEL_LOGS_EXPORTER=otlp
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
```

The ServiceAccount mounts no token, since the mailer calls no Kubernetes
API:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: mailer
automountServiceAccountToken: false
```

## The Deployment

Two replicas, each reading its settings from the ConfigMap, with probes
over HTTPS:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mailer
  labels:
    app.kubernetes.io/name: mailer
spec:
  replicas: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: mailer
  template:
    metadata:
      labels:
        app.kubernetes.io/name: mailer
    spec:
      serviceAccountName: mailer
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 35
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: mailer
          image: mailer
          envFrom:
            - configMapRef:
                name: mailer
          ports:
            - name: probes
              containerPort: 8443
          livenessProbe:
            httpGet:
              path: /health/liveness
              port: probes
              scheme: HTTPS
          readinessProbe:
            httpGet:
              path: /health/readiness
              port: probes
              scheme: HTTPS
          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              memory: 256Mi
```

The container runs as non-root, on a read-only root filesystem:

```yaml
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: [ALL]
```

The password, the API key and the probes' certificate are files, mounted
from Secrets, and so is the broker's CA from a ConfigMap:

```yaml
    volumeMounts:
      - name: rabbitmq
        mountPath: /etc/mailer/rabbitmq
        readOnly: true
      - name: rabbitmq-ca
        mountPath: /etc/mailer/rabbitmq-ca
        readOnly: true
      - name: provider
        mountPath: /etc/mailer/provider
        readOnly: true
      - name: probes
        mountPath: /etc/mailer/probes
        readOnly: true
volumes:
  - name: rabbitmq
    secret:
      secretName: mailer-rabbitmq
      defaultMode: 0440
      items:
        - key: password
          path: password
  - name: rabbitmq-ca
    configMap:
      name: mailer-rabbitmq-ca
      items:
        - key: ca.crt
          path: ca.crt
  - name: provider
    secret:
      secretName: mailer-provider
      defaultMode: 0440
      items:
        - key: api-key
          path: api-key
  - name: probes
    secret:
      secretName: mailer-probes
      defaultMode: 0440
      items:
        - key: tls.pem
          path: tls.pem
```

## The disruption budget

A drain keeps one replica consuming:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: mailer
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: mailer
```

## The grace period

On SIGTERM, each replica stops taking deliveries, finishes and acks the ones
in flight, and nacks the rest with requeue, which the other replica takes.
`main` gives that 25s:

```go
func main() {
	humus.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults), humus.WithShutdownGracePeriod(25*time.Second))
}
```

`terminationGracePeriodSeconds` covers the grace period, the telemetry's 5s
shutdown, and a margin, so a rollout never ends in SIGKILL:

```yaml
terminationGracePeriodSeconds: 35
```

## The kind overlay

`deploy/kind` adds the mailer's namespace and what it needs to run:
RabbitMQ over TLS, WireMock over HTTPS with the walkthrough's mappings, and
the LGTM stack. It points the mailer at WireMock, and trusts the cases'
scenarios:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: mailer
resources:
  - namespace.yaml
  - ../base
  - rabbitmq.yaml
  - wiremock.yaml
  - otel-collector.yaml
  - loki.yaml
  - tempo.yaml
  - mimir.yaml
  - grafana.yaml
configMapGenerator:
  - name: mailer
    behavior: merge
    literals:
      - PROVIDER_URL=https://wiremock
      - PROVIDER_CA_FILE=/etc/mailer/provider-ca/ca.crt
      - MAIL_TRUSTED_PUBLISHERS=cases
```

RabbitMQ imports `definitions.json`, then the overlay's policies, which take
precedence, so a request is parked for 1s and dead-lettered after 2
deliveries:

```json
{
  "name": "kind.mail.send",
  "vhost": "/",
  "pattern": "^mail\\.send$",
  "apply-to": "queues",
  "priority": 1,
  "definition": {
    "delivery-limit": 2,
    "dead-letter-exchange": "mail.dead",
    "dead-letter-strategy": "at-least-once",
    "overflow": "reject-publish"
  }
},
```

A patch uses the image `up.sh` loads, and mounts WireMock's CA:

```yaml
patches:
  - target:
      kind: Deployment
      name: mailer
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: mailer
      spec:
        template:
          spec:
            containers:
              - name: mailer
                imagePullPolicy: Never
                volumeMounts:
                  - name: provider-ca
                    mountPath: /etc/mailer/provider-ca
                    readOnly: true
            volumes:
              - name: provider-ca
                configMap:
                  name: mailer-provider-ca
                  items:
                    - key: ca.crt
                      path: ca.crt
```

## Running it on kind

`up.sh` creates a kind cluster if there isn't one. On each run, it
generates a CA, every certificate, key and password, and the provider's API
key, so nothing secret is kept in the repository. It loads the image,
applies the overlay with the Secrets, imports the definitions, and creates
the logins with `rabbitmqctl`:

```sh
docker build --file example/mailer/Dockerfile --tag mailer .
example/mailer/deploy/kind/up.sh
```

Then run the shared cases against it:

```sh
go -C example/mailer test -tags functional ./functional/
```

## Publishing a request

`mailrequest` publishes one request to `mail`, as the `mailrequest` login,
which may write `mail` and nothing else. It takes the request as flags:

```go
for _, f := range []struct{ name, usage string }{
	{"to", "the address to send the email to"},
	{"subject", "the email's subject"},
	{"text", "the email's text"},
	{"message-id", "the request's MessageId, random if not given"},
} {
```

Run it from the mailer's image, with the login's Secret and the broker's CA
mounted:

```sh
kubectl --context kind-kind --namespace mailer run mailrequest --rm --stdin --restart=Never --quiet \
  --image=mailer --image-pull-policy=Never --override-type=strategic --overrides='
{"spec": {
  "containers": [{
    "name": "mailrequest",
    "command": ["/mailrequest"],
    "volumeMounts": [
      {"name": "rabbitmq", "mountPath": "/etc/mailrequest/rabbitmq", "readOnly": true},
      {"name": "rabbitmq-ca", "mountPath": "/etc/mailrequest/rabbitmq-ca", "readOnly": true}
    ]
  }],
  "volumes": [
    {"name": "rabbitmq", "secret": {"secretName": "mailrequest-rabbitmq"}},
    {"name": "rabbitmq-ca", "configMap": {"name": "mailer-rabbitmq-ca"}}
  ]
}}' \
  --env RABBITMQ_ADDR=rabbitmq:5671 \
  --env RABBITMQ_USERNAME=mailrequest \
  --env RABBITMQ_PASSWORD_FILE=/etc/mailrequest/rabbitmq/password \
  --env RABBITMQ_CA_FILE=/etc/mailrequest/rabbitmq-ca/ca.crt \
  -- -to ada@example.com -subject "Your order" -text "It shipped."
```

It prints the request's `MessageId` once the broker has confirmed it.
