<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/assets/icons/logo.svg">
  <img src="docs/assets/icons/logo-on-light.svg" alt="" width="96" height="96">
</picture>

# Humus

[![Go Reference](https://pkg.go.dev/badge/github.com/z5labs/humus.svg)](https://pkg.go.dev/github.com/z5labs/humus)
[![build](https://github.com/z5labs/humus/actions/workflows/build.yaml/badge.svg)](https://github.com/z5labs/humus/actions/workflows/build.yaml)

Humus is an opinionated Go framework for building services on
[bedrock](https://github.com/z5labs/bedrock): HTTP and REST APIs, gRPC
services, jobs, Kafka and RabbitMQ consumers, and Kubernetes controllers. A
service is an `Init` that returns its Services, and `humus.Main` runs them
with telemetry, health and graceful shutdown built in, on defaults that fit
inside Kubernetes. `humustest.Start` runs the same service in a test.

## Install

```sh
go get github.com/z5labs/humus@v0.16.0
```

## A service

`Init` builds the service: here, a job that greets the name its environment
gives it, once.

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	name, err := config.Env(s.Process.Environment, "HELLO_NAME").Read(ctx)
	if err != nil {
		return nil, err
	}

	out := s.Process.Stdout
	greet := func(context.Context) error {
		_, err := fmt.Fprintf(out, "Hello, %s!\n", name)
		return err
	}
	return []humus.Service{job.NewService(s, greet)}, nil
}
```

`main` hands it to `humus.Main`, which runs its Services until they return,
then exits with their status.

```go
func main() {
	humus.Main(Init)
}
```

The whole service, with its imports and its tests, is
[`example/job/hello`](example/job/hello).

## Service types

Each service type has a docs section, a walkthrough of a production-ready
service, and that service's code.

| Service type | Docs | Walkthrough | Example |
| --- | --- | --- | --- |
| HTTP | [HTTP](https://z5labs.github.io/humus/services/http/) | [Shopping cart](https://z5labs.github.io/humus/walkthroughs/http/shoppingcart/) | [`example/http/shoppingcart`](example/http/shoppingcart) |
| gRPC | [gRPC](https://z5labs.github.io/humus/services/grpc/) | [Wallet](https://z5labs.github.io/humus/walkthroughs/grpc/wallet/) | [`example/grpc/wallet`](example/grpc/wallet) |
| Jobs | [Jobs](https://z5labs.github.io/humus/services/jobs/) | [Billing job](https://z5labs.github.io/humus/walkthroughs/job/billing/) | [`example/job/billing`](example/job/billing) |
| Kafka | [Kafka](https://z5labs.github.io/humus/services/kafka/) | [Order status processor](https://z5labs.github.io/humus/walkthroughs/kafka/orderstatus/) | [`example/kafka/orderstatus`](example/kafka/orderstatus) |
| RabbitMQ | [RabbitMQ](https://z5labs.github.io/humus/services/rabbitmq/) | [Mailer](https://z5labs.github.io/humus/walkthroughs/rabbitmq/mailer/) | [`example/rabbitmq/mailer`](example/rabbitmq/mailer) |
| Kubernetes | [Kubernetes controllers](https://z5labs.github.io/humus/services/kubernetes/) | [Databases controller](https://z5labs.github.io/humus/walkthroughs/kube/databases/) | [`example/kube/databases`](example/kube/databases) |

## Documentation

[Getting started](https://z5labs.github.io/humus/getting-started/) takes you
from an empty folder to a running, tested service. The
[documentation site](https://z5labs.github.io/humus/) covers the rest, from
the App lifecycle to every service type.

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

A release updates every humus version the docs name, here and on the
documentation site, so they install the latest release. A pre-release leaves
them as they are.
The release workflow and how to run it are described in
[`.github/workflows/release.yaml`](.github/workflows/release.yaml).

## Questions and proposals

Use [GitHub Discussions](https://github.com/z5labs/humus/discussions): ask
questions in [Q&A](https://github.com/z5labs/humus/discussions/categories/q-a),
report bugs in [Bugs](https://github.com/z5labs/humus/discussions/categories/bugs),
and propose changes in
[General](https://github.com/z5labs/humus/discussions/categories/general).

## License

Released under the [MIT License](LICENSE).
