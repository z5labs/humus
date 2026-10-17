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
[The App lifecycle](/humus/lifecycle/) explains `humus.Main`, which
runs a service's `Init` on the OS process with defaults that fit inside
Kubernetes' termination budget, the `humus.App` it builds, and the drain that
ends once nothing is serving. The first of the runtime packages, which export
Services for an `Init` to return, has landed: [Jobs](/humus/jobs/)
explains the `job` package, which runs a job once as a Service, with the exit
status Kubernetes retries on and a span that joins the trace of the process
that started it. The `httpserver` package has landed:
[Serving HTTP](/humus/http/) explains how a service serves an
`http.Handler` over TLS, with HTTP/2 preferred, on `httpserver.Main`'s
drain and grace period, and how to test it with `httpservertest`. The
`grpc` module has landed: [gRPC services](/humus/grpc/) explains how
a service serves gRPC over TLS with `grpcserver`, ends its streams and shuts
down on `grpcserver.Main`'s drain and grace period, and how to test it with
`grpcservertest`. The core of the `health` package has landed too:
[Health](/humus/health/) explains monitors, liveness and readiness,
and the health Service that owns an App's readiness.
[Configuring a humus service](/humus/configuration/) explains how
a service's settings are sourced, [Telemetry](/humus/telemetry/)
explains how a service gets its OpenTelemetry providers, and
[Testing a humus App](/humus/testing/) explains how to run a whole
App inside a test with `humustest.Start`, which hands a service's `Init` to
the App `humus.Main` would run, on a test Host.

The plan is in the charter,
[Proposal: humus on the bedrock process model](https://github.com/z5labs/humus/discussions/629).

Earlier releases, and their package documentation, remain available on
[pkg.go.dev](https://pkg.go.dev/github.com/z5labs/humus?tab=versions).

## Resources

- [GitHub Repository](https://github.com/z5labs/humus)
- [GitHub Discussions](https://github.com/z5labs/humus/discussions)
