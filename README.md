# Humus

[![Go Reference](https://pkg.go.dev/badge/github.com/z5labs/humus.svg)](https://pkg.go.dev/github.com/z5labs/humus)
[![build](https://github.com/z5labs/humus/actions/workflows/build.yaml/badge.svg)](https://github.com/z5labs/humus/actions/workflows/build.yaml)

Humus is an opinionated Go framework for building services on
[bedrock](https://github.com/z5labs/bedrock): HTTP and REST APIs, gRPC
services, queue consumers and jobs, with telemetry, health and graceful
shutdown built in.

## Humus is being rebuilt

bedrock has been rebuilt around one idea: an application is an OS process. A
`bedrock.App` is `Run(ctx context.Context, p bedrock.Process) bedrock.ExitStatus`,
and `func main() { bedrock.Main(app) }` is the only place that touches the real
process. Humus is being rebuilt on that process model, package by package.

The previous code has been removed from `main`: the Builder/Runner code, the
YAML configuration, the global telemetry, and the `rest`, `grpc`, `job`,
`queue` and `health` packages. Each package comes back through its own accepted
proposal. The App lifecycle has landed in the root package: a service's `main`
is `func main() { humus.Main(app.Init) }`, and `humus.Main` runs the
`humus.App` it builds from that `Init` on the OS process, with defaults that
fit inside Kubernetes' termination budget, while `humustest.Start` runs the
same App in a test, on a test Host. The `health` package's core is back
too: monitors, probes and a health Service that owns an App's readiness. The
first runtime package, which exports Services for an `Init` to return, has
landed: `job` runs a job once as a Service, and its run's error is the exit
status. See [Jobs](https://z5labs.github.io/humus/services/jobs/). The
`httpserver` package has landed too: it serves an `http.Handler` over TLS as a
Service, with HTTP/2 preferred, and `httpserver.Main` runs it with a drain and
a grace period for a server behind a load balancer, while `httpservertest`
tests it on the same defaults. See
[Serving HTTP](https://z5labs.github.io/humus/services/http/serving/). The `openapi/v3`
package has landed too: it serves an API of typed operations on typed paths,
whose every body names its codec and every error its answer, and the API's
OpenAPI 3.2 document, written from the same declarations, as the handler
`httpserver` serves, with `jsoncodec` for JSON bodies and `problem` for RFC
9457 problem details beside it. See
[Serving an API with OpenAPI](https://z5labs.github.io/humus/services/http/openapi/). The
`grpc` module has landed as well: `grpcserver` serves gRPC over TLS as a Service, ending every
stream at shutdown with `Unavailable`, and `grpcserver.Main` runs it with a
drain and a grace period for a server behind a Kubernetes Service, while
`grpcservertest` tests it on the same defaults. See
[Serving gRPC](https://z5labs.github.io/humus/services/grpc/serving/). The `queue` and
`queue/kafka` packages have landed too: `queue` is the contract between a
`Processor` and any broker, and `queue/kafka`, with franz-go, consumes Kafka
topics as a Service on `humus.Main`'s defaults, at least or at most once, and
produces records with a `Producer` whose calls return nil only once the
cluster has stored their records, and a consumer turns ready once it has
joined its group, with `Joined`. See
[Consuming and producing Kafka](https://z5labs.github.io/humus/services/kafka/). The
`queue/rabbitmq` package has landed too: with amqp091-go, it consumes a
RabbitMQ queue as a Service on `humus.Main`'s defaults, at least or at most
once, riding out a broker's rolling upgrade, and publishes with a
`Publisher` whose calls return nil only once the broker has confirmed the
message, and a consumer turns ready once it consumes, with `Consuming`. See
[Consuming and publishing with RabbitMQ](https://z5labs.github.io/humus/services/rabbitmq/). The
`kube` module has landed too: it writes Kubernetes controllers as Services on
client-go, without controller-runtime, declaring custom resources as Go
values whose CRDs it reflects from Go types, and reconciling their objects,
or those of Kubernetes' own kinds, with leader election on a Lease. See
[Writing a Kubernetes controller](https://z5labs.github.io/humus/services/kubernetes/). The
`httphealth` package and the `grpc` module's `grpchealth` package have landed
too: they expose an App's probes, `httphealth` at `/health/liveness` and
`/health/readiness` over HTTPS, and `grpchealth` as gRPC health v1, each
attached to a server the App already runs or on a probes port of its own.
See [Health](https://z5labs.github.io/humus/fundamentals/health/). The
`httpclient` package has landed too: `httpclient.NewClient` builds a plain
`*http.Client` for one dependency in the end user's `Init`, which dials
through the Setup's Host, retries what is safe to send twice as the end
user's `Classify` decides, and fails calls at once with a circuit breaker
while the dependency keeps failing. See
[Calling HTTP services](https://z5labs.github.io/humus/dependencies/http/). The
`grpc` module's `grpcclient` package has landed too: `grpcclient.NewClient`
builds a `Client` for one gRPC dependency in the end user's `Init`, which
generated code takes, dials through the Setup's Host, retries as the end
user's `Classify` decides, fails calls at once with a circuit breaker while
the dependency keeps failing, and turns readiness on once it has connected,
with `Connected`. See
[Calling gRPC services](https://z5labs.github.io/humus/dependencies/grpc/). The
`auth` and `auth/jwt` packages and the `grpc` module's `grpcauth` package
have landed too: `auth` authenticates and authorizes the requests a service
serves, with typed schemes whose principals a handler reads with `From`, and
requirements that `openapi.Security`, `auth.NewHandler` and
`grpcauth.Interceptors` enforce, while `auth/jwt` verifies JWT bearer tokens
against keys from an identity provider's JSON Web Key Set or a `KeySet` of
the end user's. See
[Authentication and authorization](https://z5labs.github.io/humus/hardening/auth/). The
`fault` package and its `fault/jsonscenario` package have landed too: a
service turns failure injection on with `humus.WithFaults`, so a request
whose baggage carries a failure scenario fails or slows down at the points
the scenario names, every humus client being one, while every humus server
strips scenarios from callers it doesn't trust, and `jsonscenario` carries a
scenario as JSON in one baggage member. See
[Failure injection](https://z5labs.github.io/humus/hardening/failure-injection/). Earlier
releases remain available on [pkg.go.dev](https://pkg.go.dev/github.com/z5labs/humus?tab=versions).

The plan is in the charter,
[Proposal: humus on the bedrock process model](https://github.com/z5labs/humus/discussions/629).
The first wave of proposals lays the foundations:

- [Proposal: the humus App lifecycle](https://github.com/z5labs/humus/discussions/630)
- [Proposal: configuration for humus apps](https://github.com/z5labs/humus/discussions/631)
- [Proposal: OpenTelemetry without globals](https://github.com/z5labs/humus/discussions/632)
- [Proposal: humustest — whole-service testing on top of bedrocktest](https://github.com/z5labs/humus/discussions/633)
- [Proposal: health monitors and probes](https://github.com/z5labs/humus/discussions/634)

Review happens in the comments on each proposal, and stories are written only
from accepted ones.

## Documentation

The [documentation site](https://z5labs.github.io/humus/) is rewritten as each
package lands.

## Testing

A whole humus App runs inside a test, under `testing/synctest`:
`humustest.Start` hands a service's `Init` to the App `humus.Main` would run,
on a [bedrocktest](https://pkg.go.dev/github.com/z5labs/bedrock/bedrocktest)
process and a [humustest](https://pkg.go.dev/github.com/z5labs/humus/humustest)
Host, whose network and filesystem the test controls. See
[Testing a humus App](https://z5labs.github.io/humus/fundamentals/testing/).

## Releases

humus is several Go modules: the root `github.com/z5labs/humus`, and the
nested `grpc`, `queue/kafka`, `queue/rabbitmq` and `kube` modules. Every
humus module is released with the same version on every release, whether or
not it changed, so one version names a set of modules that work together.
Each example, at `example/<type>/<name>` under its service type's folder,
is a module of its own too, and is never released.
Require the same version of every humus module you use:

```sh
go get github.com/z5labs/humus@vX.Y.Z github.com/z5labs/humus/grpc@vX.Y.Z
```

The release workflow and how to run it are described in
[`.github/workflows/release.yaml`](.github/workflows/release.yaml).

## License

Released under the [MIT License](LICENSE).
