# Deploying

## The image

One image holds the cart, the catalog and `migrate`, each built static,
onto a distroless, non-root base:

```dockerfile
FROM docker.io/library/golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY example/shoppingcart/go.mod example/shoppingcart/go.sum example/shoppingcart/
RUN go -C example/shoppingcart mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/shoppingcart build -trimpath -ldflags=-s -o /out/cart . && \
    CGO_ENABLED=0 go -C example/shoppingcart build -trimpath -ldflags=-s -o /out/catalog ./catalog && \
    CGO_ENABLED=0 go -C example/shoppingcart build -trimpath -ldflags=-s -o /out/migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/cart /cart
COPY --from=build /out/catalog /catalog
COPY --from=build /out/migrate /migrate
USER 65532:65532
```

Build it from the repository's root, since the module replaces humus with
its directory:

```sh
docker build --file example/shoppingcart/Dockerfile --tag shoppingcart .
```

## The kustomization

`deploy/base` holds the namespace, a ServiceAccount, a Deployment, a
Service and a disruption budget for each service, and each one's settings:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: shoppingcart
resources:
  - namespace.yaml
  - serviceaccount.yaml
  - cart.yaml
  - catalog.yaml
  - service.yaml
  - poddisruptionbudget.yaml
configMapGenerator:
  - name: cart
    literals:
      - ADDR=:8443
      - TLS_CERT_FILE=/etc/cart/tls/tls.crt
      - TLS_KEY_FILE=/etc/cart/tls/tls.key
      - DATABASE_URL_FILE=/etc/cart/database/url
      - CATALOG_URL=https://catalog:8443
      - CATALOG_CA_FILE=/etc/cart/ca/ca.crt
      - CATALOG_CLIENT_CERT_FILE=/etc/cart/catalog-client/tls.crt
      - CATALOG_CLIENT_KEY_FILE=/etc/cart/catalog-client/tls.key
      - JWKS_URL=https://idp.example.com/.well-known/jwks.json
      - JWKS_CA_FILE=/etc/cart/ca/ca.crt
      - JWT_ISSUER=https://idp.example.com
      - JWT_AUDIENCE=shoppingcart
      - OTEL_SERVICE_NAME=cart
      - OTEL_TRACES_EXPORTER=otlp
      - OTEL_METRICS_EXPORTER=otlp
      - OTEL_LOGS_EXPORTER=otlp
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
```

The catalog's ConfigMap is the same, with its own paths, `CLIENT_CA_FILE`
in place of the `CATALOG_` settings, and `JWT_AUDIENCE=catalog`.

```sh
kubectl kustomize example/shoppingcart/deploy/base
```

Neither service calls the API server, so neither ServiceAccount's token is
mounted:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cart
automountServiceAccountToken: false
```

## The Deployments

Two replicas of each. A rollout starts a new replica before it stops an
old one:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cart
  labels:
    app.kubernetes.io/name: cart
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
      serviceAccountName: cart
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

`migrate` runs first, as an init container, with the database owner's URL.
It grants the service's own role what it needs, so the service connects
with a URL of its own:

```yaml
      initContainers:
        - name: migrate
          image: shoppingcart
          command: ["/migrate"]
          env:
            - name: MIGRATIONS
              value: carts
            - name: APP_ROLE
              value: cart
            - name: DATABASE_URL_FILE
              value: /etc/cart/database-owner/url
```

The service reads its settings from its ConfigMap, and serves its API and
its probes on one port, over HTTPS. The startup probe gives it a minute to
start:

```yaml
        - name: cart
          image: shoppingcart
          command: ["/cart"]
          envFrom:
            - configMapRef:
                name: cart
          ports:
            - name: https
              containerPort: 8443
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

Each service's readiness calls a dependency, the cart's the catalog and
the catalog's its database, so the readiness probe waits 5s.

Every container runs on a read-only root filesystem:

```yaml
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
```

The certificates, the database URLs and the CA are files, mounted from
Secrets:

```yaml
          volumeMounts:
            - name: tls
              mountPath: /etc/cart/tls
              readOnly: true
            - name: catalog-client
              mountPath: /etc/cart/catalog-client
              readOnly: true
            - name: database
              mountPath: /etc/cart/database
              readOnly: true
            - name: ca
              mountPath: /etc/cart/ca
              readOnly: true
```

```yaml
        - name: tls
          secret:
            secretName: cart-tls
            defaultMode: 0440
            items:
              - key: tls.crt
                path: tls.crt
              - key: tls.key
                path: tls.key
```

The catalog's Deployment is the same, with its own names, and the CA it
checks client certificates against in place of the cart's client
certificate.

A Service in front of each takes the port by name:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: cart
  labels:
    app.kubernetes.io/name: cart
spec:
  selector:
    app.kubernetes.io/name: cart
  ports:
    - name: https
      port: 8443
      targetPort: https
```

## The disruption budgets

A drain keeps one replica of each service running:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: cart
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: cart
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: catalog
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: catalog
```

## The drain

`httpserver.Main` keeps serving for 5s after SIGTERM, since Kubernetes
takes a while to stop routing requests to a terminating pod. It then gives
the requests in flight 10s, and the telemetry 5s to flush.
`terminationGracePeriodSeconds` covers the 20s and a margin, so a rollout
never ends in SIGKILL:

```yaml
terminationGracePeriodSeconds: 30
```

## The kind overlay

`deploy/kind` adds PostgreSQL, WireMock as the identity provider, and the
LGTM stack, beside both services. The cart and the catalog get their key
set from WireMock, and the cart trusts the scenarios of a token with
`faults:rehearse`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: shoppingcart
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
  - name: cart
    behavior: merge
    literals:
      - JWKS_URL=https://wiremock/.well-known/jwks.json
      - FAULT_TRUST_SCOPE=faults:rehearse
  - name: catalog
    behavior: merge
    literals:
      - JWKS_URL=https://wiremock/.well-known/jwks.json
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

WireMock serves the key set over TLS, with a mapping of its own:

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
      name: cart
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: cart
      spec:
        template:
          spec:
            initContainers:
              - name: migrate
                imagePullPolicy: Never
            containers:
              - name: cart
                imagePullPolicy: Never
```

## Running it on kind

`up.sh` creates a kind cluster if there isn't one. On each run, it
generates a CA and every certificate, every password, and the key tokens
are signed with, so nothing secret is kept in the repository. `carttoken`
makes the key set WireMock serves from that key. `up.sh` loads the image,
applies the overlay with the Secrets, creates each service's database and
roles, and waits for every Deployment:

```sh
docker build --file example/shoppingcart/Dockerfile --tag shoppingcart .
example/shoppingcart/deploy/kind/up.sh
```

Then run the shared cases against it:

```sh
go -C example/shoppingcart test -tags functional ./functional/
```

## Calling the cart

`carttoken sign` signs a token with a key, for a subject, with scopes:

```go
		_, _ = fmt.Fprintf(p.Stderr, "Usage:\n  %s %s -key FILE\n  %s %s -key FILE -sub SUBJECT [-scope SCOPE]... -iss ISSUER -aud AUDIENCE [-ttl DURATION]\n",
			name, jwksCommand, name, signCommand)
```

Read the signing key and the CA from their Secrets:

```sh
kubectl --context kind-kind --namespace shoppingcart get secret token-signing-key -o jsonpath='{.data.key\.pem}' | base64 -d >signing.key
kubectl --context kind-kind --namespace shoppingcart get secret cart-ca -o jsonpath='{.data.ca\.crt}' | base64 -d >ca.crt
```

Forward a port to each service:

```sh
kubectl --context kind-kind --namespace shoppingcart port-forward service/catalog 9443:8443 &
kubectl --context kind-kind --namespace shoppingcart port-forward service/cart 8443:8443 &
```

Put a product in the catalog, with a token for the catalog's audience that
holds `catalog:write`:

```sh
token=$(go -C example/shoppingcart run ./cmd/carttoken sign -key "$PWD/signing.key" \
  -sub admin -scope catalog:write -iss https://idp.example.com -aud catalog)
curl --cacert ca.crt -X PUT https://localhost:9443/products/sku-1 \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d '{"unitPrice":9.99}'
```

Then create a cart, and add the product to it, with a token for the
cart's audience that holds `carts:write`:

```sh
token=$(go -C example/shoppingcart run ./cmd/carttoken sign -key "$PWD/signing.key" \
  -sub shopper -scope carts:write -iss https://idp.example.com -aud shoppingcart)
cart=$(curl -s --cacert ca.crt -X POST https://localhost:8443/carts -H "Authorization: Bearer $token" | jq -r .cartId)
curl --cacert ca.crt -X POST "https://localhost:8443/carts/$cart/items" \
  -H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d '{"productId":"sku-1","quantity":2}'
```

The item has the catalog's price:

```sh
curl --cacert ca.crt "https://localhost:8443/carts/$cart" -H "Authorization: Bearer $token"
```
