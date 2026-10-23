# Deploying

## The image

The controller is built static onto a distroless, non-root base:

```dockerfile
FROM docker.io/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY kube/go.mod kube/go.sum kube/
COPY example/kube/databases/go.mod example/kube/databases/go.sum example/kube/databases/
RUN go -C example/kube/databases mod download
COPY . .
RUN CGO_ENABLED=0 go -C example/kube/databases build -trimpath -ldflags=-s -o /out/databases .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:d62067879de890a1def9da0e7766b7d20810a5e5555cef8506b752437d7a3e88
COPY --from=build /out/databases /databases
USER 65532:65532
ENTRYPOINT ["/databases"]
```

Build it from the repository's root, since the module replaces humus and
`kube` with their directories:

```sh
docker build --file example/kube/databases/Dockerfile --tag databases .
```

## The kustomization

`deploy` holds the CRD, the namespace, the ServiceAccount, RBAC, the
Deployment, its disruption budget and the settings:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: databases
resources:
  - crd.yaml
  - namespace.yaml
  - serviceaccount.yaml
  - clusterrole.yaml
  - clusterrolebinding.yaml
  - role.yaml
  - rolebinding.yaml
  - deployment.yaml
  - poddisruptionbudget.yaml
configMapGenerator:
  - name: databases
    envs:
      - databases.env
```

The settings, in `databases.env`:

```text
CONTROLLER_NAME=databases-controller
POSTGRES_ADMIN_DSN_FILE=/etc/databases/postgres/dsn
POSTGRES_HOST=postgres.databases.svc
POSTGRES_PORT=5432
PROBES_ADDR=:8443
PROBES_TLS_PEM_FILE=/etc/databases/probes/tls.pem
OTEL_SERVICE_NAME=databases
```

```sh
kubectl kustomize example/kube/databases/deploy
```

## RBAC

The controller runs as the `databases` ServiceAccount:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: databases
  namespace: databases
```

A ClusterRole lets it watch and patch `Database`s and their status, and
read and apply Secrets, in every namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: databases-controller
rules:
  - apiGroups: [postgres.example.com]
    resources: [databases]
    verbs: [get, list, watch, patch]
  - apiGroups: [postgres.example.com]
    resources: [databases/status]
    verbs: [patch]
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, list, watch, create, patch]
```

A ClusterRoleBinding grants it to the ServiceAccount:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: databases-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: databases-controller
subjects:
  - kind: ServiceAccount
    name: databases
    namespace: databases
```

A Role, in its own namespace, lets it hold its Lease and no other. `create`
can't be limited to a name, so it's a rule of its own:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: databases-controller
  namespace: databases
rules:
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [create]
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    resourceNames: [databases-controller]
    verbs: [get, update]
```

A RoleBinding grants it to the ServiceAccount, in the same way.

## The Deployment

Two replicas, which share the Lease, each reading its settings from the
ConfigMap, with probes over HTTPS. A rollout starts a new replica before it
stops an old one:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: databases
  namespace: databases
  labels:
    app.kubernetes.io/name: databases
spec:
  replicas: 2
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  selector:
    matchLabels:
      app.kubernetes.io/name: databases
  template:
    metadata:
      labels:
        app.kubernetes.io/name: databases
    spec:
      serviceAccountName: databases
      automountServiceAccountToken: true
      terminationGracePeriodSeconds: 30
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: databases
          image: databases
          envFrom:
            - configMapRef:
                name: databases
          env:
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
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

The ServiceAccount's token is mounted, since the controller calls the API
server with it.

The container runs as non-root, on a read-only root filesystem:

```yaml
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
```

The admin DSN and the probes' certificate are files, mounted from Secrets:

```yaml
          volumeMounts:
            - name: postgres
              mountPath: /etc/databases/postgres
              readOnly: true
            - name: probes
              mountPath: /etc/databases/probes
              readOnly: true
      volumes:
        - name: postgres
          secret:
            secretName: databases-postgres-admin
            defaultMode: 0440
            items:
              - key: dsn
                path: dsn
        - name: probes
          secret:
            secretName: databases-probes
            defaultMode: 0440
            items:
              - key: tls.pem
                path: tls.pem
```

## The disruption budget

A drain keeps one replica running:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: databases
  namespace: databases
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: databases
```

## The grace period

On SIGTERM, each replica finishes the reconciles in flight and releases the
Lease, which the other replica takes. `main` gives that 20s:

```go
func main() {
	humus.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults), humus.WithShutdownGracePeriod(20*time.Second))
}
```

`terminationGracePeriodSeconds` covers the grace period, the telemetry's
shutdown, and a margin, so a rollout never ends in SIGKILL:

```yaml
terminationGracePeriodSeconds: 30
```

## The kind overlay

`deploy/kind` adds PostgreSQL and the LGTM stack beside the controller,
exports its telemetry to the Collector, and trusts the cases' scenarios:

```yaml
resources:
  - ../crd.yaml
  - ../namespace.yaml
  - ../serviceaccount.yaml
  - ../clusterrole.yaml
  - ../clusterrolebinding.yaml
  - ../role.yaml
  - ../rolebinding.yaml
  - ../deployment.yaml
  - ../poddisruptionbudget.yaml
  - postgres.yaml
  - otel-collector.yaml
  - loki.yaml
  - tempo.yaml
  - mimir.yaml
  - grafana.yaml
configMapGenerator:
  - name: databases
    envs:
      - ../databases.env
    literals:
      - FAULT_TRUSTED_NAMESPACE_PREFIX=databases-cases-
      - OTEL_TRACES_EXPORTER=otlp
      - OTEL_METRICS_EXPORTER=otlp
      - OTEL_LOGS_EXPORTER=otlp
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
```

PostgreSQL checks passwords with SCRAM, and takes its admin password from a
Secret:

```yaml
            - name: POSTGRES_INITDB_ARGS
              value: --auth-host=scram-sha-256
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: postgres
                  key: password
```

A patch uses the image `up.sh` loads:

```yaml
patches:
  - target:
      kind: Deployment
      name: databases
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: databases
      spec:
        template:
          spec:
            containers:
              - name: databases
                imagePullPolicy: Never
```

## Running it on kind

`up.sh` creates a kind cluster if there isn't one. On each run, it
generates PostgreSQL's admin password, Grafana's, and a CA and the probes'
certificate, so nothing secret is kept in the repository. It loads the
image, applies the overlay with the Secrets, and waits for every
Deployment:

```sh
docker build --file example/kube/databases/Dockerfile --tag databases .
example/kube/databases/deploy/kind/up.sh
```

Then run the shared cases against it:

```sh
go -C example/kube/databases test -tags functional ./functional/
```

## Applying a `Database`

`deploy/kind/database.yaml` is a `Database`:

```yaml
apiVersion: postgres.example.com/v1alpha1
kind: Database
metadata:
  name: orders
spec:
  name: orders
  deletionPolicy: Delete
```

Apply it, and wait for it to be `Ready`:

```sh
kubectl --context kind-kind --namespace default apply --filename example/kube/databases/deploy/kind/database.yaml
kubectl --context kind-kind --namespace default wait database/orders --for=condition=Ready --timeout 2m
```

Then log in with the credentials in its Secret:

```sh
password=$(kubectl --context kind-kind --namespace default get secret orders-postgres -o jsonpath='{.data.password}' | base64 -d)
kubectl --context kind-kind --namespace databases exec deployment/postgres -- \
  env PGPASSWORD="$password" psql -h 127.0.0.1 -U orders -d orders -c 'SELECT current_user'
```

Deleting it drops the database and the role, as its `deletionPolicy` says:

```sh
kubectl --context kind-kind --namespace default delete database orders
```

Next: [Observing](/humus/walkthroughs/kube/databases/observing/)
