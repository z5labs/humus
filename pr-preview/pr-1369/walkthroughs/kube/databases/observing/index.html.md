# Observing

## Opening Grafana

`up.sh` generates Grafana's admin password into the `grafana-admin` Secret.
Read it, then forward Grafana's port:

```sh
kubectl --context kind-kind --namespace databases get secret grafana-admin \
  --output jsonpath='{.data.password}' | base64 --decode
kubectl --context kind-kind --namespace databases port-forward service/grafana 3000:3000
```

Sign in at <http://localhost:3000> as `admin`.

## A reconcile's trace

Apply the `Database` in `deploy/kind/database.yaml`, as
[Deploying](/humus/pr-preview/pr-1369/walkthroughs/kube/databases/deploying/#applying-a-database)
shows. In **Explore**, choose **Tempo** and find its reconciles by its
namespace and name:

```text
{resource.service.name="databases" && span.k8s.namespace.name="default" && span.humus.kube.object.name="orders"}
```

| Span | What it is |
|---|---|
| `reconcile Database` | the reconcile, a root span, with `k8s.namespace.name` and `humus.kube.object.name` |
| `SELECT pg_roles`, `CREATE ROLE`, `ALTER ROLE`, `SELECT pg_database`, `CREATE DATABASE`, `ALTER DATABASE` | the `Server`'s statements as it provisions, with `db.query.summary` and no SQL text |
| `DROP DATABASE`, `DROP ROLE` | the statements of a `Finalize` whose policy is `Delete` |
| `GET`, `PATCH` | each request to the API server, from `otelhttp`, such as reading the Secret and applying it |

So a reconcile's trace holds everything it did. A test of the App checks
it:

```go
		require.True(t, applied, "no successful reconcile has both admin operations and the Secret's apply under it")
		require.Contains(t, summaries, "CREATE ROLE")
		require.Contains(t, summaries, "CREATE DATABASE")
```

A failed reconcile's span is an error:

```text
{resource.service.name="databases" && name="reconcile Database" && status=error}
```

## Its log records

In Explore, with **Loki**, the controller's records are:

```text
{service_name="databases"}
```

| Record | Written by |
|---|---|
| `serving`, `shutting down`, `stopped` | the App, at INFO, as it starts and stops |
| `reconcile failed` | the Controller, at WARN, with the object and the error |
| `reconcile panicked` | the Controller, at ERROR, with the object and the panic's stack |
| client-go's own | the reflector, the work queue and leader election: an Info at INFO, and an Error at ERROR |

A `Database`'s failures are:

```text
{service_name="databases"} | severity_text="WARN" | humus_kube_object_name="orders"
```

A test of the App checks that a failed reconcile is logged, with its
object:

```go
		require.Positive(t, logged, "no reconcile failed record for the Database")
```

A record with a trace ID links to its trace in Tempo, and a span links to
its records in Loki:

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
```

## Its metrics

The controller records no metric of its own. humus's Controller records
six, in the scope `github.com/z5labs/humus/kube`, each with
`humus.kube.controller.name`, which is `databases-controller`:

| Instrument | Is |
|---|---|
| `humus.kube.controller.queue.depth` | the `Database`s waiting in the work queue |
| `humus.kube.controller.queue.adds` | the `Database`s added to the work queue |
| `humus.kube.controller.queue.duration` | how long a `Database` waited for a worker |
| `humus.kube.controller.reconcile.duration` | how long each reconcile took, by `humus.kube.controller.reconcile.outcome`: `success`, `error` or `panic` |
| `humus.kube.controller.reconcile.unfinished_duration` | how long the reconciles in flight have run, added together |
| `humus.kube.controller.reconcile.longest_unfinished_duration` | how long the longest reconcile in flight has run |

In Mimir, a name's dots are underscores, and `service.name` is `job`:

```text
sum by (humus_kube_controller_reconcile_outcome) (rate(humus_kube_controller_reconcile_duration_count{job="databases", humus_kube_controller_name="databases-controller"}[5m]))
```

A standby's work queue holds every `Database`, so read the queue per
replica. The Collector adds the pod's name to each of them:

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

```text
max by (k8s_pod_name) (humus_kube_controller_queue_depth{job="databases", humus_kube_controller_name="databases-controller"})
```

The Collector reads the containers' restarts from Kubernetes, since a
controller that crashes can't report them:

```yaml
  k8s_cluster:
    auth_type: serviceAccount
    collection_interval: 10s
```

```yaml
    metrics/cluster:
      receivers: [k8s_cluster]
      processors: [batch]
      exporters: [otlp_http/mimir]
```

```text
k8s_container_restarts{k8s_namespace_name="databases", k8s_container_name="databases"}
```

## The alerts

`deploy/alerts.yaml` holds Grafana-managed alert rules, in the folder
**Databases**. The kind overlay provisions them:

```yaml
  - name: grafana-alerting
    files:
      - ../alerts.yaml
```

| Alert | Severity | When |
|---|---|---|
| `DatabasesReconcileFailing` | page | reconciles have kept ending with `error` for 15 minutes |
| `DatabasesReconcilePanicked` | ticket | a reconcile panicked in the last 15 minutes |
| `DatabasesReconcileStuck` | page | a reconcile has been running for more than 5 minutes |
| `DatabasesBacklogged` | page | more than 50 `Database`s have waited in the work queue for 10 minutes |
| `DatabasesRestarting` | ticket | a `databases` container restarted in the last 15 minutes |

Each rule is a query of Mimir and a threshold:

```yaml
      - uid: databases-reconcile-stuck
        title: DatabasesReconcileStuck
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
              expr: max(humus_kube_controller_reconcile_longest_unfinished_duration{job="databases", humus_kube_controller_name="databases-controller"}) or vector(0)
          - refId: B
            datasourceUid: __expr__
            model:
              refId: B
              type: threshold
              expression: A
              conditions:
                - evaluator:
                    type: gt
                    params: [300]
        noDataState: OK
        execErrState: Error
        for: 0s
        labels:
          severity: page
```

`DatabasesBacklogged` reads only the replica that reconciles, since a
standby's queue holds every `Database`:

```yaml
              expr: max(humus_kube_controller_queue_depth{job="databases", humus_kube_controller_name="databases-controller"} and on (k8s_pod_name) humus_kube_controller_reconcile_duration_count{job="databases", humus_kube_controller_name="databases-controller"}) or vector(0)
```

Each rule's annotations say where to look:

```yaml
        annotations:
          summary: The controller's reconciles have kept ending with error for 15 minutes.
          description: A failing Database's Ready condition says why, as PostgresUnavailable or SecretApplyFailed. Look at its reconciles' traces in Tempo and the controller's logs in Loki, for whether PostgreSQL or the API server is failing them.
```

## The dashboard

`deploy/dashboard.json` is the **Databases** dashboard, at
<http://localhost:3000/d/databases>. The kind overlay provisions it from a
file, so it can't be changed in Grafana:

```yaml
apiVersion: 1
providers:
  - name: databases
    orgId: 1
    folder: Databases
    type: file
    disableDeletion: true
    allowUiUpdates: false
    options:
      path: /etc/grafana/dashboards
```

| Panel | What it shows |
|---|---|
| Reconciles by outcome | the reconciles a second, by outcome |
| Reconcile duration | a reconcile's mean and p99 |
| Work queue depth by replica | each replica's `queue.depth` |
| Longest unfinished reconcile by replica | each replica's `longest_unfinished_duration` |
| Restarts | each `databases` container's restarts |

Each panel is a query of Mimir:

```json
      "targets": [
        {
          "refId": "A",
          "datasource": {
            "type": "prometheus",
            "uid": "mimir"
          },
          "expr": "sum by (humus_kube_controller_reconcile_outcome) (rate(humus_kube_controller_reconcile_duration_count{job=\"databases\", humus_kube_controller_name=\"databases-controller\"}[$__rate_interval]))",
          "legendFormat": "{{humus_kube_controller_reconcile_outcome}}"
        }
      ]
```

## Checking it on kind

Behind the `functional` tag, a test checks that Grafana loaded every rule
and the dashboard, that Mimir holds every series the rules read, and that
each rule evaluates without error:

```go
var alertRules = []struct {
	title  string
	series []string
}{
	{title: "DatabasesReconcileFailing", series: []string{reconciles + `, humus_kube_controller_reconcile_outcome="error"}`}},
	{title: "DatabasesReconcilePanicked", series: []string{reconciles + `, humus_kube_controller_reconcile_outcome=~".+"}`}},
	{title: "DatabasesReconcileStuck", series: []string{`humus_kube_controller_reconcile_longest_unfinished_duration{` + controller + `}`}},
	{title: "DatabasesBacklogged", series: []string{
		`humus_kube_controller_queue_depth{` + controller + `, k8s_pod_name!=""}`,
		reconciles + `, k8s_pod_name!=""}`,
	}},
	{title: "DatabasesRestarting", series: []string{`k8s_container_restarts{k8s_namespace_name="databases", k8s_container_name="databases"}`}},
}
```

It runs `PostgreSQLDown` first, so the series of a failed reconcile exist:

```go
	started := time.Now()
	t.Run("PostgreSQLDown", func(t *testing.T) {
		cases.PostgreSQLDown(t, target)
	})
```

Another checks that a `Database`'s trace reaches Tempo, the controller's
records reach Loki, and every instrument reaches Mimir:

```sh
go -C example/kube/databases test -tags functional ./functional/
```

Next: [Production-ready](/humus/pr-preview/pr-1369/walkthroughs/kube/databases/production-ready/)
