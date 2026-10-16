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
proposal. The App lifecycle, `humus.App`, has landed in the root package, and
the `health` package's core is back: monitors, probes and a health Service that
owns an App's readiness. The runtime packages built on the lifecycle have not
landed yet. Earlier
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

A whole humus App runs inside a test, under `testing/synctest`, with
[bedrocktest](https://pkg.go.dev/github.com/z5labs/bedrock/bedrocktest) for the
process and [humustest](https://pkg.go.dev/github.com/z5labs/humus/humustest)
for the network, the filesystem and TLS. See
[Testing a humus App](https://z5labs.github.io/humus/testing/).

## License

Released under the [MIT License](LICENSE).
