# Humus

Humus is a Go framework for services that run on Kubernetes: HTTP and
OpenAPI APIs, gRPC services, jobs, Kafka and RabbitMQ consumers and
Kubernetes controllers. It runs each of them the same way, on
[bedrock](https://github.com/z5labs/bedrock)'s process model, with
OpenTelemetry, health probes and a graceful shutdown that fits inside
Kubernetes' termination budget.

## What a service looks like

A service's `main` is one call, `func main() { humus.Main(app.Init) }`, where
`app.Init` is the service's own `Init`. `Init` sources the service's
settings, builds its dependencies, registers their release, and returns the
Services the App runs until it shuts down:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	out := s.Process.Stdout
	s.OnStop(func(context.Context) error {
		_, err := fmt.Fprintln(out, "on stop: close the cache")
		return err
	})
	s.OnStop(func(context.Context) error {
		_, err := fmt.Fprintln(out, "on stop: close the queue")
		return err
	})

	if _, err := config.Env(s.Process.Environment, "DATABASE_URL").Read(ctx); err != nil {
		return nil, err // exit 78: init: config DATABASE_URL: not found
	}
	return []humus.Service{newServer(out)}, nil
}
```

A service type's package, such as `httpserver` or `queue/kafka`, builds the
Services an `Init` returns. [The App lifecycle](/humus/fundamentals/lifecycle/)
explains what the App does with them.

## The reading order

The sections of this site are meant to be read in order, and every page
links to the next one:

1. [Fundamentals](/humus/fundamentals/): the App lifecycle,
   configuration, telemetry, health and testing, which every service shares.
2. [Service types](/humus/services/): one section for each kind of
   service humus runs.
3. [Calling dependencies](/humus/dependencies/): the HTTP and gRPC
   clients a service calls its dependencies with.
4. [Hardening](/humus/hardening/): authentication and
   authorization, and failure injection.
5. [Walkthroughs](/humus/walkthroughs/): a production-ready
   application of each service type, built step by step.

To start from a service type instead, find it here:

| Service type | Guide | Walkthrough | Example |
|---|---|---|---|
| HTTP | [Serving HTTP](/humus/services/http/serving/), [Serving an API with OpenAPI](/humus/services/http/openapi/) | [Shoppingcart](/humus/walkthroughs/http/shoppingcart/) | [`example/http/shoppingcart`](https://github.com/z5labs/humus/tree/main/example/http/shoppingcart) |
| gRPC | [Serving gRPC](/humus/services/grpc/serving/) | [Wallet](/humus/walkthroughs/grpc/wallet/) | [`example/grpc/wallet`](https://github.com/z5labs/humus/tree/main/example/grpc/wallet) |
| Jobs | [Jobs](/humus/services/jobs/) | [Billing job](/humus/walkthroughs/job/billing/) | [`example/job/billing`](https://github.com/z5labs/humus/tree/main/example/job/billing) |
| Kafka | [Consuming and producing Kafka](/humus/services/kafka/) | [Order status processor](/humus/walkthroughs/kafka/orderstatus/) | [`example/kafka/orderstatus`](https://github.com/z5labs/humus/tree/main/example/kafka/orderstatus) |
| RabbitMQ | [Consuming and publishing with RabbitMQ](/humus/services/rabbitmq/) | [Mailer](/humus/walkthroughs/rabbitmq/mailer/) | [`example/rabbitmq/mailer`](https://github.com/z5labs/humus/tree/main/example/rabbitmq/mailer) |
| Kubernetes controllers | [Writing a Kubernetes controller](/humus/services/kubernetes/) | [Databases controller](/humus/walkthroughs/kube/databases/) | [`example/kube/databases`](https://github.com/z5labs/humus/tree/main/example/kube/databases) |

## Resources

- [GitHub Repository](https://github.com/z5labs/humus)
- [GitHub Discussions](https://github.com/z5labs/humus/discussions)

Next: [Fundamentals](/humus/fundamentals/)
