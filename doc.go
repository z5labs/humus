// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humus is an opinionated framework for building services on
// bedrock's process model: HTTP and REST APIs, gRPC services, queue consumers
// and jobs, with telemetry, health and graceful shutdown built in.
//
// Every humus application is a bedrock.App, and its main is
//
//	func main() { bedrock.Main(app) }
//
// Nothing in humus reads os.Args, the environment, the standard streams or
// signals, and nothing in it exits the process; all of that comes through the
// bedrock.Process an App is run with. humus sets the conventions bedrock
// leaves open, such as environment variable names, exit codes, default ports,
// telemetry exporters and health endpoints, and every one of them is a default
// that code can override.
//
// # The App lifecycle
//
// [App] is the lifecycle every humus app type is built on. Its Telemetry, a
// telemetry.Builder, builds the App's OpenTelemetry providers before Init.
// Its Init builds the App's dependencies from a [Setup], which carries that
// telemetry, registers their release with [Setup.OnStop], and returns the
// [Service] values to run. Run calls Serve on each of them, and when a signal
// arrives or the first Service returns, it cancels Serve's context, waits the
// drain delay, calls Shutdown on every Service and runs the OnStop functions,
// all within one grace period, and then shuts the telemetry down. The outcome
// is the exit status: 0, [ExitFailure] or [ExitConfig].
//
// The lifecycle logs through the App's telemetry: its own records at INFO,
// and every failure after the telemetry is built at ERROR. Standard error
// gets only what telemetry cannot report itself: a failure to build or shut
// down telemetry, a malformed setting, and OpenTelemetry's export errors,
// through the process-wide error handler Run sets.
//
// Run reads the grace period from HUMUS_SHUTDOWN_GRACE_PERIOD and the drain
// delay from HUMUS_SHUTDOWN_DRAIN_DELAY, through the Process's environment,
// with the App's ShutdownGracePeriod and DrainDelay fields as their defaults.
// An operator tunes either without a rebuild, and a bad default fails at
// startup with ExitConfig, as a bad variable does. These are the only
// environment variables the root package reads.
//
// An App reaches the network and the filesystem only through its [Host]:
// [OSHost] in main, and a Host the test controls in a test, so a whole App
// runs inside a test under testing/synctest with bedrocktest's fake process.
// Health is not part of the lifecycle yet; it is added by its own package.
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
// status. The runtime packages that build their Apps
// on it have not landed yet.
//
// The plan is in the charter, Proposal: humus on the bedrock process model:
// https://github.com/z5labs/humus/discussions/629
//
// Earlier releases remain available at https://pkg.go.dev/github.com/z5labs/humus.
package humus
