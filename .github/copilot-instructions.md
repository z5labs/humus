## Quick orientation for AI code assistants

This repository is a modular Go framework (Humus) for REST, gRPC, Job and Queue apps built on top of Bedrock. Keep these facts front-and-center when making changes or generating code:

- Core design: Builder + Runner. Builders return a `bedrock.AppBuilder` and are composed with `appbuilder.OTel`, `appbuilder.Recover`, and lifecycle hooks. The runner (`humus.NewRunner`) builds then runs the app and delegates errors to a pluggable `ErrorHandler`.
- Primary package folders: `rest/`, `grpc/`, `job/`, `queue/`, `health/`, `config/`. Internal implementations live under `internal/` (e.g. `internal/otel`, `internal/httpserver`, `internal/grpcserver`). Use the public packages for examples and the `internal` packages for implementation details only.

## Important files to reference

- `README.md` — high-level quick start and examples (root).
- `CLAUDE.md` — existing AI guidance with useful commands and patterns.
- `humus.go` — logger, Runner, OnError pattern: how apps are executed and how errors are handled.
- `rest/rest.go`, `rest/rpc/` — canonical REST builder, runtime, and rpc handler patterns (ProduceJson, ConsumeJson, HandleJson, path building).
- `grpc/grpc.go` — gRPC builder and Run wrapper; auto-registers health and OTel interceptors.
- `config/otel.go` and `default_config.yaml` — canonical OTel configuration shape and environment-template functions.
- `internal/run.go` — minimal run orchestration used by other packages.
- `example/` — runnable examples (use these for end-to-end behavior and expected wiring).

## Developer workflows (commands you can use)

- Build everything: `go build ./...` (used in CI)
- Run unit tests (race + coverage): `go test -race -cover ./...` or targeted packages `go test -race -cover ./rest/rpc`
- Lint: `golangci-lint run` (CI uses same tool and settings)

CI: See `.github/workflows/*` for build, codeql and docs pipelines.

## Code patterns AI should follow (concrete, repo-specific)

- Builders: produce a `bedrock.AppBuilder` by calling `appbuilder.LifecycleContext(appbuilder.OTel(appbuilder.Recover(...)))`. See `rest.Builder` and `grpc.Builder`.
- App config: embed `humus.Config` (or `humus.Config`'s types) and implement provider interfaces when necessary (e.g., `ListenerProvider`, `HttpServerProvider`). Example: `rest.Config` embeds `humus.Config` and provides `Listener(ctx)` and `HttpServer(ctx, handler)`.
- Entry points: use package runner helpers: `rest.Run(reader, initFn)`, `grpc.Run(...)`, `job.Run(...)`, `queue.Run(...)` — those wrap builder+runner+default config.
- REST rpc handlers: prefer the typed helpers in `rest/rpc` (ProduceJson, ConsumeJson, HandleJson). Use `rest.Handle(method, rest.BasePath("/x").Param("id"), operation)` for registering.
- Error handling: surface and use `humus.OnError(...)` or implement `humus.ErrorHandler` for custom run-time behavior.
- Telemetry/config: follow `default_config.yaml` and `config/otel.go` shapes; YAML supports Go templating with `env` and `default` helpers — use these when producing config files.
- Queue semantics: respect delivery patterns. Use `queue.ProcessAtMostOnce` or `queue.ProcessAtLeastOnce` as appropriate and propagate `queue.ErrEndOfQueue` to trigger graceful shutdown.

## Naming and style constraints to obey

- Error variables must follow `ErrFoo` (ST1012 / staticcheck expectation).
- Use `testify/require` in tests (project convention). Prefer `require` over `assert` for most unit tests.
- Use `humus.Logger(name)` / `humus.LogHandler` for structured logging so logs correlate with traces.

## Quick examples to copy-paste (use these patterns exactly)

1) Minimal REST main:

```go
func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}
// Init(ctx, cfg) returns *rest.Api (build routes using rest.Handle and rpc helpers)
```

2) Builder pattern (REST): see `rest.Builder` — the builder must create listener, http.Server (with otelhttp handler) and return a bedrock app that is wrapped with `app.Recover` and `app.InterruptOn`.

## What to avoid / gotchas

- Don't bypass the Bedrock lifecycle wrappers (OTel, Recover, lifecycle hooks). Directly starting servers without the builder-wrapper breaks automatic OTel init and graceful shutdown.
- For at-least-once queue processors, ensure idempotency — retries may occur.
- When changing public-facing handler signatures, update OpenAPI generation in `rest/rpc` and confirm `example/` builds.

## Where to look for authoritative examples

- `example/rest/petstore` and `example/grpc/petstore` — real runnable apps that show proper wiring of configs, builders, and startup.

If anything here is unclear or you want more detail about a specific package or workflow, tell me which area (REST rpc, gRPC, queue runtime, OTel config, or CI) and I will expand or adjust the instructions.
