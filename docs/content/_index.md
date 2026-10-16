---
title: Humus
type: docs
---

**An opinionated Go framework for building services on [bedrock](https://github.com/z5labs/bedrock)'s process model.**

Humus is for HTTP and REST APIs, gRPC services, queue consumers and jobs, with
telemetry, health and graceful shutdown built in. Every humus application is a
`bedrock.App`, and its `main` is `func main() { bedrock.Main(app) }`.

## Humus is being rebuilt

Humus is being rebuilt on bedrock's process model, package by package. The
previous code, and the pages that documented it, have been removed: the
Builder/Runner code, the YAML configuration, the global telemetry, and the
`rest`, `grpc`, `job`, `queue` and `health` packages. Each package comes back
through its own accepted proposal, and its pages are written here as it lands.

The App lifecycle has landed in the root package:
[The App lifecycle]({{< relref "lifecycle" >}}) explains `humus.App`, which every
humus app type is built on. The runtime packages built on it have not landed
yet.

The plan is in the charter,
[Proposal: humus on the bedrock process model](https://github.com/z5labs/humus/discussions/629).

Earlier releases, and their package documentation, remain available on
[pkg.go.dev](https://pkg.go.dev/github.com/z5labs/humus?tab=versions).

## Resources

- [GitHub Repository](https://github.com/z5labs/humus)
- [GitHub Discussions](https://github.com/z5labs/humus/discussions)
