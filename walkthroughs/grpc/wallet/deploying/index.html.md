# Deploying

## The image

One image holds the wallet and the ledger, each built static, onto a
distroless, non-root base:

```dockerfile
FROM docker.io/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY grpc/go.mod grpc/go.sum grpc/
COPY example/grpc/wallet/go.mod example/grpc/wallet/go.sum example/grpc/wallet/
RUN go -C example/grpc/wallet mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/grpc/wallet build -trimpath -ldflags=-s -o /out/wallet . && \
    CGO_ENABLED=0 go -C example/grpc/wallet build -trimpath -ldflags=-s -o /out/ledger ./ledger

FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
COPY --from=build /out/wallet /wallet
COPY --from=build /out/ledger /ledger
USER 65532:65532
```

Build it from the repository's root, since the module replaces humus and
`humus/grpc` with their directories:

```sh
docker build --file example/grpc/wallet/Dockerfile --tag wallet .
```

## The kustomization

`deploy/base` holds the namespace, a ServiceAccount, a Deployment, a
Service and a disruption budget for each service, and each one's settings:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: wallet
resources:
  - namespace.yaml
  - serviceaccount.yaml
  - wallet.yaml
  - ledger.yaml
  - service.yaml
  - poddisruptionbudget.yaml
configMapGenerator:
  - name: wallet
    literals:
      - GRPC_ADDR=:8443
      - GRPC_TLS_PEM_FILE=/etc/wallet/tls/tls.pem
      - PROBES_ADDR=:8081
      - LEDGER_ADDR=ledger:8443
      - LEDGER_CA_FILE=/etc/wallet/ca/ledger.crt
      - LEDGER_TLS_PEM_FILE=/etc/wallet/ledger-client/tls.pem
      - TOKEN_KEYS_URL=https://idp.example.com/.well-known/jwks.json
      - TOKEN_KEYS_CA_FILE=/etc/wallet/ca/token-keys.crt
      - TOKEN_ISSUER=https://idp.example.com
      - TOKEN_AUDIENCE=wallet
      - OTEL_SERVICE_NAME=wallet
      - OTEL_TRACES_EXPORTER=otlp
      - OTEL_METRICS_EXPORTER=otlp
      - OTEL_LOGS_EXPORTER=otlp
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
  - name: ledger
    literals:
      - GRPC_ADDR=:8443
      - GRPC_TLS_PEM_FILE=/etc/ledger/tls/tls.pem
      - GRPC_CLIENT_CA_FILE=/etc/ledger/ca/client-ca.crt
      - PROBES_ADDR=:8081
      - POSTGRES_URL=postgres://ledger@postgres:5432/ledger
      - POSTGRES_PASSWORD_FILE=/etc/ledger/postgres/password
      - POSTGRES_TLS_CA_FILE=/etc/ledger/ca/postgres.crt
      - OTEL_SERVICE_NAME=ledger
      - OTEL_TRACES_EXPORTER=otlp
      - OTEL_METRICS_EXPORTER=otlp
      - OTEL_LOGS_EXPORTER=otlp
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
```

The wallet reaches the ledger through the ledger's Service, `ledger:8443`.

```sh
kubectl kustomize example/grpc/wallet/deploy/base
```

Neither service calls the API server, so neither ServiceAccount's token is
mounted:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: wallet
automountServiceAccountToken: false
```

## The Deployments

Two replicas of each. A rollout starts a new replica before it stops an
old one:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: wallet
  labels:
    app.kubernetes.io/name: wallet
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
      serviceAccountName: wallet
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

The service reads its settings from its ConfigMap. It serves gRPC on one
port, over TLS, and its probes on another, in plaintext:

```yaml
        - name: wallet
          image: wallet
          command: ["/wallet"]
          envFrom:
            - configMapRef:
                name: wallet
          ports:
            - name: grpc
              containerPort: 8443
            - name: probes
              containerPort: 8081
```

Every container runs on a read-only root filesystem:

```yaml
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
```

The certificates and the CAs are files, mounted from Secrets. Each
certificate and its key are one PEM file:

```yaml
          volumeMounts:
            - name: tls
              mountPath: /etc/wallet/tls
              readOnly: true
            - name: ledger-client
              mountPath: /etc/wallet/ledger-client
              readOnly: true
            - name: ca
              mountPath: /etc/wallet/ca
              readOnly: true
```

```yaml
        - name: ca
          secret:
            secretName: wallet-ca
            defaultMode: 0440
            items:
              - key: ledger.crt
                path: ledger.crt
              - key: token-keys.crt
                path: token-keys.crt
```

The ledger's Deployment is the same, with its own names, the CA it checks
client certificates against in place of the wallet's client certificate,
and PostgreSQL's password:

```yaml
        - name: postgres
          secret:
            secretName: ledger-postgres
            defaultMode: 0440
            items:
              - key: password
                path: password
```

A Service in front of each takes the gRPC port by name:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: ledger
  labels:
    app.kubernetes.io/name: ledger
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: ledger
  ports:
    - name: grpc
      port: 8443
      targetPort: grpc
```

## The gRPC probes

The kubelet calls gRPC health on the probes port, with the service each
probe is for. The startup probe gives the service a minute to start:

```yaml
          startupProbe:
            grpc:
              port: 8081
              service: liveness
            periodSeconds: 2
            failureThreshold: 30
          livenessProbe:
            grpc:
              port: 8081
              service: liveness
          readinessProbe:
            grpc:
              port: 8081
              service: readiness
            timeoutSeconds: 5
```

The ledger's readiness pings PostgreSQL until it is first reached, so the
readiness probe waits 5s.

## The disruption budgets

A drain keeps one replica of each service running:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: wallet
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: wallet
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: ledger
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: ledger
```

## The drain

`grpcserver.Main` keeps serving for 5s after SIGTERM, since Kubernetes
takes a while to stop routing calls to a terminating pod. `Shutdown` then
ends every open watch with `Unavailable`, `server is shutting down`, and
gives the calls in flight 15s. The telemetry gets 5s to flush.
`terminationGracePeriodSeconds` covers the 25s, so a rollout never ends in
SIGKILL:

```yaml
terminationGracePeriodSeconds: 30
```

When a ledger pod goes, the wallet opens each of its watches again on the
other replica, as
[Serving the wallet](/humus/walkthroughs/grpc/wallet/serving-the-wallet/#relaying-the-watch)
shows. When a wallet pod goes, the app watches again.

## The kind overlay

`deploy/kind` adds PostgreSQL, WireMock serving the key set, and the LGTM
stack, beside both services. The wallet fetches its key set from WireMock,
and trusts the scenarios of a token with `faults:rehearse`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: wallet
resources:
  - ../base
  - postgres.yaml
  - wiremock.yaml
  - otel-collector.yaml
  - loki.yaml
  - tempo.yaml
  - mimir.yaml
  - grafana.yaml
configMapGenerator:
  - name: wallet
    behavior: merge
    literals:
      - TOKEN_KEYS_URL=https://wiremock/.well-known/jwks.json
      - FAULT_TRUST_SCOPE=faults:rehearse
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

WireMock serves HTTPS only:

```yaml
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
```

It answers the key set's path with the key set `up.sh` makes:

```json
{
  "request": {
    "method": "GET",
    "urlPath": "/.well-known/jwks.json"
  },
  "response": {
    "status": 200,
    "headers": {
      "Content-Type": "application/jwk-set+json"
    },
    "bodyFileName": "jwks.json"
  }
}
```

A patch on each Deployment uses the image `up.sh` loads:

```yaml
  - target:
      kind: Deployment
      name: wallet
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: wallet
      spec:
        template:
          spec:
            containers:
              - name: wallet
                imagePullPolicy: Never
```

## Running it on kind

`up.sh` creates a kind cluster if there isn't one:

```sh
if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --wait 2m
fi
```

On each run, it generates a CA and, with it, every certificate: the
wallet's, its client certificate for the ledger, the ledger's,
PostgreSQL's, WireMock's and the cases':

```sh
issue wallet wallet serverAuth "DNS:localhost, IP:127.0.0.1"
issue wallet-ledger-client wallet clientAuth
issue ledger ledger serverAuth "DNS:localhost, IP:127.0.0.1"
issue postgres postgres serverAuth
issue wiremock wiremock serverAuth
issue cases cases clientAuth
```

It generates every password, and the key tokens are signed with, so
nothing secret is kept in the repository. `walletctl jwks` makes the key
set WireMock serves from that key:

```sh
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp/signing.key" 2>/dev/null
go -C "$module" run ./cmd/walletctl jwks -key "$tmp/signing.key" >"$tmp/jwks.json"
```

It loads the image and applies the overlay with the Secrets. Then it
creates the ledger's database, applies `schema.sql` as its owner, and
grants the ledger's role what it needs:

```sh
{
  cat <<SQL
CREATE ROLE ledger_owner;
CREATE ROLE ledger LOGIN PASSWORD '$ledger_password';
CREATE DATABASE ledger OWNER ledger_owner;
REVOKE ALL ON DATABASE ledger FROM PUBLIC;
GRANT CONNECT ON DATABASE ledger TO ledger;
\connect ledger
SET ROLE ledger_owner;
SQL
  cat "$module/ledger/store/schema.sql"
  cat <<SQL
GRANT SELECT, INSERT, UPDATE ON accounts, transfers TO ledger;
SQL
} | k exec -i deployment/postgres -- psql -v ON_ERROR_STOP=1 -U postgres -d postgres
```

Last, it waits for every Deployment:

```sh
k rollout restart deployment/grafana deployment/ledger deployment/wallet
for d in loki tempo mimir otel-collector grafana ledger wallet; do
  k rollout status "deployment/$d" --timeout 5m
done
```

Build the image, and bring it up:

```sh
docker build --file example/grpc/wallet/Dockerfile --tag wallet .
example/grpc/wallet/deploy/kind/up.sh
```

Then run the shared cases against it:

```sh
go -C example/grpc/wallet test -tags functional ./functional/
```

## Trying it with walletctl

`walletctl` calls the wallet as a subject, with a token it signs, and the
ledger with a client certificate:

```go
var commands = []command{
	{jwksCommand, []flagSpec{keyFlag}},
	{tokenCommand, []flagSpec{keyFlag, subFlag, scopeFlag, issFlag, audFlag, ttlFlag}},
	{openCommand, walletFlags},
	{balanceCommand, walletFlags},
	{sendCommand, append(slices.Clone(walletFlags), toFlag, amountFlag, idFlag)},
	{watchCommand, walletFlags},
	{depositCommand, []flagSpec{addrFlag, caFlag, serverNameFlag, certFlag, accountFlag, amountFlag, idFlag}},
}
```

Each token holds the scope its command needs:

```go
var scopeFor = map[string]string{
	openCommand:    server.ScopeWrite,
	balanceCommand: server.ScopeRead,
	sendCommand:    server.ScopeWrite,
	watchCommand:   server.ScopeRead,
}
```

Read the signing key, the CA and the cases' certificate from their
Secrets:

```sh
kubectl --context kind-kind --namespace wallet get secret token-signing-key -o jsonpath='{.data.key\.pem}' | base64 -d >signing.key
kubectl --context kind-kind --namespace wallet get secret cases-tls -o jsonpath='{.data.ca\.crt}' | base64 -d >ca.crt
kubectl --context kind-kind --namespace wallet get secret cases-tls -o jsonpath='{.data.tls\.pem}' | base64 -d >cases.pem
```

Forward a port to each service:

```sh
kubectl --context kind-kind --namespace wallet port-forward service/wallet 8443:8443 &
kubectl --context kind-kind --namespace wallet port-forward service/ledger 9443:8443 &
```

Open two accounts, alice's and bob's, each with a token for its subject:

```sh
go -C example/grpc/wallet run ./cmd/walletctl open -addr localhost:8443 -ca "$PWD/ca.crt" \
  -key "$PWD/signing.key" -sub alice -iss https://idp.example.com -aud wallet
go -C example/grpc/wallet run ./cmd/walletctl open -addr localhost:8443 -ca "$PWD/ca.crt" \
  -key "$PWD/signing.key" -sub bob -iss https://idp.example.com -aud wallet
```

Deposit into alice's, through the ledger, with the cases' certificate:

```sh
go -C example/grpc/wallet run ./cmd/walletctl deposit -addr localhost:9443 -ca "$PWD/ca.crt" \
  -cert "$PWD/cases.pem" -account alice -amount 1000
```

Watch bob's account in one terminal:

```sh
go -C example/grpc/wallet run ./cmd/walletctl watch -addr localhost:8443 -ca "$PWD/ca.crt" \
  -key "$PWD/signing.key" -sub bob -iss https://idp.example.com -aud wallet
```

And send from alice's in another:

```sh
go -C example/grpc/wallet run ./cmd/walletctl send -addr localhost:8443 -ca "$PWD/ca.crt" \
  -key "$PWD/signing.key" -sub alice -iss https://idp.example.com -aud wallet -to bob -amount 250
```

The watch prints bob's balance, 0, then 250. Roll the ledger out, and the
watch stays open, while the wallet opens the ledger's watch again on the
other replica:

```sh
kubectl --context kind-kind --namespace wallet rollout restart deployment/ledger
```

Next: [Observing](/humus/walkthroughs/grpc/wallet/observing/)
