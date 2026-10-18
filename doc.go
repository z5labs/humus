// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus is an opinionated framework for building services on
// bedrock's process model: HTTP and REST APIs, gRPC services, queue consumers
// and jobs, with telemetry, health and graceful shutdown built in.
//
// Every humus application is an [App], which is a bedrock.App, and a
// service's main is
//
//	func main() { humus.Main(app.Init) }
//
// where app.Init is the end user's Init. [Main] runs the App on the OS
// process with defaults that fit inside Kubernetes' termination budget, and
// [Option] values, such as [WithTelemetry], change them.
//
// Nothing in humus reads os.Args, the environment, the standard streams or
// signals; all of that comes through the bedrock.Process an App is run with.
// humus.Main is the one place that hands an App the real process, through
// bedrock.Main, and so the one place that exits it. It never runs in a
// test, which runs the same App on a test Process. humus sets the
// conventions bedrock leaves open, such as environment variable names, exit
// codes, default ports, telemetry exporters and health endpoints, and every
// one of them is a default that code can override.
//
// # The App lifecycle
//
// [App] is the lifecycle every humus app type is built on. Its Telemetry, a
// telemetry.Builder, builds the App's OpenTelemetry providers before Init.
// Its Init builds the App's dependencies from a [Setup], which carries that
// telemetry, registers their release with [Setup.OnStop], and returns the
// [Service] values to run. Run calls Serve on each of them, and when a signal
// arrives or the first Service returns, it cancels Serve's context, waits for
// the drain, a [Drain] that ends once every Serve has returned, calls
// Shutdown on every Service and runs the OnStop functions, all within one
// grace period, and then shuts the telemetry down. The outcome is the exit
// status: 0, [ExitFailure] or [ExitConfig].
//
// The lifecycle logs through the App's telemetry: its own records at INFO,
// and every failure after the telemetry is built at ERROR. Standard error
// gets only what telemetry cannot report itself: a failure to build or shut
// down telemetry, a malformed setting, and OpenTelemetry's export errors,
// through the process-wide error handler Run sets.
//
// Run reads the grace period from HUMUS_SHUTDOWN_GRACE_PERIOD, through the
// Process's environment, with the App's ShutdownGracePeriod field as its
// default, and a bad default fails at startup with ExitConfig, as a bad
// variable does. The drain is the App's Drain field, unless
// HUMUS_SHUTDOWN_DRAIN_DELAY is set: then it is [DrainDelay] of the
// variable's value. An operator tunes either without a rebuild. These are
// the only environment variables the root package reads.
//
// An App reaches the network and the filesystem only through its [Host]:
// [OSHost] in main, and a Host the test controls in a test, so a whole App
// runs inside a test under testing/synctest with bedrocktest's fake process.
// Health is a Service that the health package provides: the end user's Init
// returns it next to the App's other Services.
//
// The lifecycle, its settings, its exit codes and the dependencies that leak
// around it are documented at https://z5labs.github.io/humus/lifecycle/.
//
// # Being rebuilt
//
// humus is being rebuilt, package by package, on bedrock's process model. The
// previous Builder/Runner code, its YAML configuration, its global telemetry
// and the rest, grpc, job, queue and health packages have been removed, and
// each package comes back through its own accepted proposal. The App
// lifecycle has landed: App builds its telemetry, runs Services, shuts them
// down within a grace period, releases what Init registered with
// Setup.OnStop, shuts its telemetry down and turns the outcome into an exit
// status. The health package's core is back: monitors, probes and a health
// Service that owns an App's readiness, and Main runs an App on the OS
// process. The job package has landed: it runs a job once as a Service on
// Main, and its run's error is the exit status. The httpserver package has
// landed: it serves an http.Handler over TLS as a Service, with HTTP/2
// preferred, and httpserver.Main runs it with a drain and a grace period for
// a server behind a load balancer, documented at
// https://z5labs.github.io/humus/http/. The grpc module has landed: its
// grpcserver package serves gRPC over TLS as a Service, ending every stream
// at shutdown with Unavailable, and grpcserver.Main runs it with a drain and
// a grace period for a server behind a Kubernetes Service, documented at
// https://z5labs.github.io/humus/grpc/. The queue and queue/kafka packages
// have landed: queue is the contract between a Processor and any broker, and
// queue/kafka, with franz-go, consumes Kafka topics as a Service on Main's
// defaults, at least or at most once, and produces records with a Producer
// whose calls return nil only once the cluster has stored their records,
// documented at
// https://z5labs.github.io/humus/kafka/. The other runtime packages come back
// through their own proposals.
//
// The plan is in the charter, Proposal: humus on the bedrock process model:
// https://github.com/z5labs/humus/discussions/629
//
// Earlier releases remain available at https://pkg.go.dev/github.com/z5labs/humus.
package humus
