# Humus

Humus is for HTTP and REST APIs, gRPC services, queue consumers and jobs, with
telemetry, health and graceful shutdown built in. Every humus application is a
`bedrock.App`, and a service's `main` is `func main() { humus.Main(app.Init) }`,
where `app.Init` is the service's own `Init`.

## Humus is being rebuilt

Humus is being rebuilt on
[bedrock](https://github.com/z5labs/bedrock)'s process model, package by package. The
previous code, and the pages that documented it, have been removed: the
Builder/Runner code, the YAML configuration, the global telemetry, and the
`rest`, `grpc`, `job`, `queue` and `health` packages. Each package comes back
through its own accepted proposal, and its pages are written here as it lands.

The App lifecycle has landed in the root package:
[The App lifecycle](/humus/pr-preview/pr-929/lifecycle/) explains `humus.Main`, which
runs a service's `Init` on the OS process with defaults that fit inside
Kubernetes' termination budget, the `humus.App` it builds, and the drain that
ends once nothing is serving. The first of the runtime packages, which export
Services for an `Init` to return, has landed: [Jobs](/humus/pr-preview/pr-929/jobs/)
explains the `job` package, which runs a job once as a Service, with the exit
status Kubernetes retries on and a span that joins the trace of the process
that started it. The `httpserver` package has landed:
[Serving HTTP](/humus/pr-preview/pr-929/http/) explains how a service serves an
`http.Handler` over TLS, with HTTP/2 preferred, on `httpserver.Main`'s
drain and grace period, and how to test it with `httpservertest`. The
`openapi/v3` package has landed:
[Serving an API with OpenAPI](/humus/pr-preview/pr-929/openapi/) explains how a
service serves an API of typed operations on typed paths with
`openapi.NewAPI`, the codecs every body names, `jsoncodec` and codecs of its
own, streaming ones included, the answers to errors and rejected requests
and RFC 9457 problem details with `problem`, the OpenAPI 3.2 document written
from the same declarations, and how to test the API with `httpservertest`.
The `httpclient` package has landed: [Calling HTTP services](/humus/pr-preview/pr-929/http-client/)
explains how a service calls each of its HTTP dependencies with a client
built in `Init`, which requests are retried and how the end user's
`Classify` decides what an answer means, the circuit breaker in front of
each dependency, and how to test the calls against a fake dependency. The
`grpc` module has landed: [gRPC services](/humus/pr-preview/pr-929/grpc/) explains how
a service serves gRPC over TLS with `grpcserver`, ends its streams and shuts
down on `grpcserver.Main`'s drain and grace period, and how to test it with
`grpcservertest`. Its `grpcclient` package has landed too:
[Calling gRPC services](/humus/pr-preview/pr-929/grpc-client/) explains how a service
calls each of its gRPC dependencies with a `Client` built in `Init`, how
names and load behave behind a Kubernetes Service, which calls are retried
and how the end user's `Classify` decides what a code means, the circuit
breaker in front of each dependency, readiness with `Connected`, and how to
test the end user's whole App next to a `grpcserver` backend. The `queue` and `queue/kafka` packages have landed:
[Consuming and producing Kafka](/humus/pr-preview/pr-929/kafka/) explains the contract
a `queue.Processor` keeps, how a service consumes Kafka topics with
`kafka.NewService` on `humus.Main`'s defaults and what each delivery commits,
how it produces records, such as dead letters, with a `kafka.Producer`, how
a consumer turns ready once it has joined its group, with `Joined`, and how
to test both on kfake. The `queue/rabbitmq` package has landed too:
[Consuming and publishing with RabbitMQ](/humus/pr-preview/pr-929/rabbitmq/) explains
how a service consumes a RabbitMQ queue with `rabbitmq.NewService`, what each
delivery acks and counts toward a quorum queue's delivery limit, how the
consumer rides out a broker's rolling upgrade and turns ready with
`Consuming`, how it publishes with confirms through a `rabbitmq.Publisher`,
and how to test an App that uses RabbitMQ, which humus ships no fake broker
for. The `health` package has landed too, with
`httphealth` and the `grpc` module's `grpchealth`, which expose its probes:
[Health](/humus/pr-preview/pr-929/health/) explains monitors, liveness and readiness,
the health Service that owns an App's readiness, and how a pod's probes reach
Kubernetes, attached to a server it already runs or on a probes port of its
own.
[Configuring a humus service](/humus/pr-preview/pr-929/configuration/) explains how
a service's settings are sourced, [Telemetry](/humus/pr-preview/pr-929/telemetry/)
explains how a service gets its OpenTelemetry providers, and
[Testing a humus App](/humus/pr-preview/pr-929/testing/) explains how to run a whole
App inside a test with `humustest.Start`, which hands a service's `Init` to
the App `humus.Main` would run, on a test Host.

The plan is in the charter,
[Proposal: humus on the bedrock process model](https://github.com/z5labs/humus/discussions/629).

Earlier releases, and their package documentation, remain available on
[pkg.go.dev](https://pkg.go.dev/github.com/z5labs/humus?tab=versions).

## Resources

- [GitHub Repository](https://github.com/z5labs/humus)
- [GitHub Discussions](https://github.com/z5labs/humus/discussions)
