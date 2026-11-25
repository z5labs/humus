# Deploying

## The image

A Go stage builds the service static:

```dockerfile
RUN CGO_ENABLED=0 go -C example/http/polls build -trimpath -ldflags=-s -o /out/polls .
```

The image copies it to `/polls` on a distroless base, to run as the
non-root user 65532:

```dockerfile
COPY --from=build /out/polls /polls
USER 65532:65532
```

[`example/http/polls/Dockerfile`](https://github.com/z5labs/humus/blob/main/example/http/polls/Dockerfile)
names the exact images and their versions.

Build it from the repository's root, since the module replaces humus with
its directory:

```sh
docker build --file example/http/polls/Dockerfile --tag polls .
```

## The kustomization

`deploy/base` holds a ServiceAccount, the Deployment, its Service and its
disruption budget, and the service's settings:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - serviceaccount.yaml
  - deployment.yaml
  - service.yaml
  - poddisruptionbudget.yaml
configMapGenerator:
  - name: polls
    literals:
      - ADDR=:8443
      - TLS_CERT_FILE=/etc/polls/tls/tls.crt
      - TLS_KEY_FILE=/etc/polls/tls/tls.key
      - DATABASE_URL_FILE=/etc/polls/database/url
      - OTEL_SERVICE_NAME=polls
      - OTEL_TRACES_EXPORTER=otlp
      - OTEL_METRICS_EXPORTER=otlp
      - OTEL_LOGS_EXPORTER=otlp
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
```

```sh
kubectl kustomize example/http/polls/deploy/base
```

The service doesn't call the API server, so its ServiceAccount's token
isn't mounted:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: polls
automountServiceAccountToken: false
```

## The Deployment

Two replicas. A rollout starts a new replica before it stops an old one:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: polls
  labels:
    app.kubernetes.io/name: polls
spec:
  replicas: 2
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
```

Each pod runs as non-root:

```yaml
      serviceAccountName: polls
      automountServiceAccountToken: false
      enableServiceLinks: false
      terminationGracePeriodSeconds: 30
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
```

The container reads its settings from its ConfigMap, and serves the UI on
one port, over HTTPS:

```yaml
        - name: polls
          image: polls
          command: ["/polls"]
          envFrom:
            - configMapRef:
                name: polls
          ports:
            - name: https
              containerPort: 8443
```

It runs on a read-only root filesystem:

```yaml
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
```

The certificate, the database URL and the CA that PostgreSQL's certificate
is checked against are files, mounted from Secrets:

```yaml
          volumeMounts:
            - name: tls
              mountPath: /etc/polls/tls
              readOnly: true
            - name: database
              mountPath: /etc/polls/database
              readOnly: true
            - name: postgres-ca
              mountPath: /etc/polls/postgres-ca
              readOnly: true
```

```yaml
        - name: tls
          secret:
            secretName: polls-tls
            defaultMode: 0440
            items:
              - key: tls.crt
                path: tls.crt
              - key: tls.key
                path: tls.key
```

A Service in front of it takes the port by name:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: polls
  labels:
    app.kubernetes.io/name: polls
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: polls
  ports:
    - name: https
      port: 8443
      targetPort: https
```

## The probes

The probes are served on the UI's port. The startup probe gives the
service a minute to start:

```yaml
          startupProbe:
            httpGet:
              path: /health/liveness
              port: https
              scheme: HTTPS
            periodSeconds: 2
            failureThreshold: 30
          livenessProbe:
            httpGet:
              path: /health/liveness
              port: https
              scheme: HTTPS
          readinessProbe:
            httpGet:
              path: /health/readiness
              port: https
              scheme: HTTPS
            timeoutSeconds: 5
```

Readiness reaches the database until the replica is first ready, so the
readiness probe waits up to 5s for each answer.

## The disruption budget

A drain keeps one replica running:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: polls
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: polls
```

## The drain

`httpserver.Main` keeps serving for 5s after SIGTERM, since Kubernetes
takes a while to stop routing requests to a terminating pod. Then
`Shutdown` begins, and each open feed ends, so htmx connects it again to
another replica. The requests in flight get 10s, and the telemetry 5s to
flush. `terminationGracePeriodSeconds` covers the 20s and a margin, so a
rollout never ends in SIGKILL:

```yaml
      terminationGracePeriodSeconds: 30
```

## The kind overlay

`deploy/kind` adds the namespace, PostgreSQL, headless-shell and the LGTM
stack beside the service, and trusts the scenarios of a client certificate
issued by its CA:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: polls
resources:
  - namespace.yaml
  - ../base
  - postgres.yaml
  - headless-shell.yaml
  - otel-collector.yaml
  - loki.yaml
  - tempo.yaml
  - mimir.yaml
  - grafana.yaml
configMapGenerator:
  - name: polls
    behavior: merge
    literals:
      - FAULT_TRUST_CA_FILE=/etc/polls/fault-ca/ca.crt
```

A patch uses the image `up.sh` loads, and mounts that CA:

```yaml
  - target:
      kind: Deployment
      name: polls
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: polls
      spec:
        template:
          spec:
            containers:
              - name: polls
                imagePullPolicy: Never
                volumeMounts:
                  - name: fault-ca
                    mountPath: /etc/polls/fault-ca
                    readOnly: true
```

PostgreSQL serves TLS only, and checks passwords with SCRAM:

```yaml
          args:
            - -c
            - ssl=on
            - -c
            - ssl_cert_file=/etc/postgres-tls/tls.crt
            - -c
            - ssl_key_file=/etc/postgres-tls/tls.key
            - -c
            - ssl_min_protocol_version=TLSv1.2
            - -c
            - hba_file=/etc/postgres-config/pg_hba.conf
```

```text
local all postgres peer
hostssl all all all scram-sha-256
```

headless-shell is Chrome with no UI, which the browser cases drive.
[`deploy/kind/headless-shell.yaml`](https://github.com/z5labs/humus/blob/main/example/http/polls/deploy/kind/headless-shell.yaml)
pins its image. It reaches the service by its Service's name, and trusts
the overlay's CA by its public key:

```yaml
          args:
            - --disable-dev-shm-usage
            - --user-data-dir=/tmp/profile
            - --ignore-certificate-errors-spki-list=$(CA_SPKI)
          env:
            - name: HOME
              value: /tmp
            - name: CA_SPKI
              valueFrom:
                configMapKeyRef:
                  name: headless-shell
                  key: ca-spki
```

## Running it on kind

`up.sh` creates a kind cluster if there isn't one. On each run, it
generates a CA and every certificate and password, so nothing secret is
kept in the repository:

```sh
issue polls polls serverAuth "$(service_names polls), DNS:localhost, IP:127.0.0.1"
cat "$tmp/ca.crt" >>"$tmp/polls.crt"
issue postgres postgres serverAuth "$(service_names postgres)"
issue cases cases clientAuth
```

It hashes the CA's public key for headless-shell:

```sh
openssl x509 -in "$tmp/ca.crt" -noout -pubkey | openssl pkey -pubin -outform DER | openssl dgst -sha256 -binary | openssl base64 -A >"$tmp/ca-spki"
```

It loads the image, applies the overlay with the Secrets, makes the
database and its roles and applies `schema.sql`, and waits for every
Deployment:

```sh
docker build --file example/http/polls/Dockerfile --tag polls .
example/http/polls/deploy/kind/up.sh
```

Then run the shared cases and the browser cases against it:

```sh
go -C example/http/polls test -tags functional ./functional/
```

## Trying it in a browser

Read the CA from its Secret, and have your browser trust it:

```sh
kubectl --context kind-kind --namespace polls get secret polls-cases -o jsonpath='{.data.ca\.crt}' | base64 -d >ca.crt
```

Forward a port to the service:

```sh
kubectl --context kind-kind --namespace polls port-forward service/polls 8443:8443
```

Open `https://localhost:8443/`, create a poll, and open its page in a
second window. A vote in one shows in the other.

Next: [Observing](/humus/pr-preview/pr-1641/walkthroughs/http/polls/observing/)
