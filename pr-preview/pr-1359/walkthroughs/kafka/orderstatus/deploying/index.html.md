# Deploying

## The image

One image holds `orderstatus` and `orderevents`. A Go stage builds each of
them static, and the image copies them to `/orderstatus` and `/orderevents`
on a distroless base, to run as the non-root user 65532 with `/orderstatus`
as its entrypoint.
[`example/kafka/orderstatus/Dockerfile`](https://github.com/z5labs/humus/blob/main/example/kafka/orderstatus/Dockerfile)
names the exact images and their versions.

Build it from the repository's root, since the module replaces humus with
its directory:

```sh
docker build --file example/kafka/orderstatus/Dockerfile --tag orderstatus .
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
  - name: orderstatus
    literals:
      - KAFKA_BROKERS=kafka:9093
      - KAFKA_GROUP_ID=orderstatus
      - KAFKA_EVENTS_TOPIC=orders.events
      - KAFKA_STATUS_TOPIC=orders.status
      - KAFKA_RETRY_TOPIC=orders.retry
      - KAFKA_DEAD_LETTER_TOPIC=orders.dlq
      - KAFKA_TLS_CA_FILE=/etc/orderstatus/kafka/ca.crt
      - KAFKA_TLS_CERT_FILE=/etc/orderstatus/kafka/tls.pem
      - RETRY_ATTEMPTS=10
      - RETRY_MIN_DELAY=1s
      - RETRY_MAX_DELAY=5m
      - POSTGRES_URL=postgres://orderstatus@postgres:5432/orders
      - POSTGRES_PASSWORD_FILE=/etc/orderstatus/postgres/password
      - POSTGRES_TLS_CA_FILE=/etc/orderstatus/postgres/ca.crt
      - PROBES_ADDR=:8443
      - PROBES_TLS_PEM_FILE=/etc/orderstatus/probes/tls.pem
      - OTEL_SERVICE_NAME=orderstatus
      - OTEL_TRACES_EXPORTER=otlp
      - OTEL_METRICS_EXPORTER=otlp
      - OTEL_LOGS_EXPORTER=otlp
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
```

The ServiceAccount mounts no token, since the processor calls no Kubernetes
API:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: orderstatus
automountServiceAccountToken: false
```

## The Deployment

Three replicas, each reading its settings from the ConfigMap, with probes
over HTTPS. A rollout starts a new replica before it stops an old one:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: orderstatus
  labels:
    app.kubernetes.io/name: orderstatus
spec:
  replicas: 3
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  selector:
    matchLabels:
      app.kubernetes.io/name: orderstatus
  template:
    metadata:
      labels:
        app.kubernetes.io/name: orderstatus
    spec:
      serviceAccountName: orderstatus
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 30
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: orderstatus
          image: orderstatus
          envFrom:
            - configMapRef:
                name: orderstatus
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

The client certificate, the password and the probes' certificate are files,
mounted from Secrets with the CA they're checked against:

```yaml
          volumeMounts:
            - name: kafka
              mountPath: /etc/orderstatus/kafka
              readOnly: true
            - name: postgres
              mountPath: /etc/orderstatus/postgres
              readOnly: true
            - name: probes
              mountPath: /etc/orderstatus/probes
              readOnly: true
      volumes:
        - name: kafka
          secret:
            secretName: orderstatus-kafka
            defaultMode: 0440
            items:
              - key: ca.crt
                path: ca.crt
              - key: tls.pem
                path: tls.pem
        - name: postgres
          secret:
            secretName: orderstatus-postgres
            defaultMode: 0440
            items:
              - key: password
                path: password
              - key: ca.crt
                path: ca.crt
        - name: probes
          secret:
            secretName: orderstatus-probes
            defaultMode: 0440
            items:
              - key: tls.pem
                path: tls.pem
```

## The disruption budget

A drain stops one replica at a time:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: orderstatus
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: orderstatus
```

## The kind overlay

`deploy/kind` adds the processor's namespace and what it needs to run:
Kafka, the Job that creates the topics and their ACLs, as
[The topics](/humus/pr-preview/pr-1359/walkthroughs/kafka/orderstatus/topics/) shows, PostgreSQL over TLS, and the
LGTM stack. It shortens the retries' backoff, and trusts the cases'
scenarios:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: orderstatus
resources:
  - namespace.yaml
  - ../base
  - kafka.yaml
  - topics.yaml
  - postgres.yaml
  - otel-collector.yaml
  - loki.yaml
  - tempo.yaml
  - mimir.yaml
  - grafana.yaml
configMapGenerator:
  - name: orderstatus
    behavior: merge
    literals:
      - RETRY_MIN_DELAY=100ms
      - RETRY_MAX_DELAY=400ms
      - FAULT_TRUSTED_TOPICS=orders.events,orders.retry
```

Kafka listens over mTLS only, and enforces its ACLs:

```yaml
            - name: KAFKA_SSL_CLIENT_AUTH
              value: required
            - name: KAFKA_AUTHORIZER_CLASS_NAME
              value: org.apache.kafka.metadata.authorizer.StandardAuthorizer
            - name: KAFKA_SUPER_USERS
              value: User:CN=kafka;User:CN=admin
            - name: KAFKA_ALLOW_EVERYONE_IF_NO_ACL_FOUND
              value: "false"
            - name: KAFKA_AUTO_CREATE_TOPICS_ENABLE
              value: "false"
```

PostgreSQL applies `schema.sql` when it first starts:

```yaml
  - name: postgres-schema
    files:
      - ../../store/schema.sql
```

A patch uses the image `up.sh` loads:

```yaml
patches:
  - target:
      kind: Deployment
      name: orderstatus
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: orderstatus
      spec:
        template:
          spec:
            containers:
              - name: orderstatus
                imagePullPolicy: Never
```

## Running it on kind

`up.sh` creates a kind cluster if there isn't one. On each run, it
generates a CA, and every certificate, key and password, so nothing secret
is kept in the repository. It loads the image, applies the overlay with the
Secrets, creates the processor's PostgreSQL role, and waits for the topics:

```sh
docker build --file example/kafka/orderstatus/Dockerfile --tag orderstatus .
example/kafka/orderstatus/deploy/kind/up.sh
```

Then run the shared cases against it:

```sh
go -C example/kafka/orderstatus test -tags functional ./functional/
```

## Producing an order's events

`orderevents` produces an order's events to `orders.events`, keyed by the
order's ID, and prints the ID:

```go
	return []humus.Service{job.NewService(s, func(ctx context.Context) error {
		records := make([]kafka.Record, 0, len(settings.Types))
		for _, typ := range settings.Types {
			value, err := json.Marshal(event{Type: typ, At: time.Now().UTC()})
			if err != nil {
				return err
			}
			records = append(records, kafka.Record{
				Topic: settings.Topic,
				Key:   []byte(settings.OrderID),
				Value: value,
			})
		}
		if err := producer.Produce(ctx, records...); err != nil {
			return err
		}
		_, err := fmt.Fprintln(s.Process.Stdout, settings.OrderID)
		return err
	})}, nil
```

It takes the events' types as arguments, in order, and the order's ID as a
flag:

```go
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage: %s [-order-id ID] TYPE...\n", fs.Name())
		fs.PrintDefaults()
	}
	fs.Func("order-id", "the order's ID, random if not given", func(v string) error {
		a.orderID, a.hasOrderID = v, true
		return nil
	})
```

Run it from the processor's image, as the `cases` principal, which may
write `orders.events`:

```sh
kubectl --context kind-kind --namespace orderstatus run orderevents --rm --stdin --restart=Never --quiet \
  --image=orderstatus --image-pull-policy=Never --override-type=strategic --overrides='
{"spec": {
  "containers": [{
    "name": "orderevents",
    "command": ["/orderevents"],
    "volumeMounts": [
      {"name": "kafka", "mountPath": "/etc/orderevents/kafka", "readOnly": true}
    ]
  }],
  "volumes": [
    {"name": "kafka", "secret": {"secretName": "cases-kafka"}}
  ]
}}' \
  --env KAFKA_BROKERS=kafka:9093 \
  --env KAFKA_EVENTS_TOPIC=orders.events \
  --env KAFKA_TLS_CA_FILE=/etc/orderevents/kafka/ca.crt \
  --env KAFKA_TLS_CERT_FILE=/etc/orderevents/kafka/tls.pem \
  -- placed paid shipped
```

Then read the order's status from PostgreSQL:

```sh
kubectl --context kind-kind --namespace orderstatus exec deployment/postgres -- \
  psql -U postgres -d orders -c "SELECT * FROM order_status WHERE order_id = '<the printed ID>'"
```

Next: [Observing](/humus/pr-preview/pr-1359/walkthroughs/kafka/orderstatus/observing/)
