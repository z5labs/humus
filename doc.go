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
// # Being rebuilt
//
// humus is being rebuilt, package by package, on bedrock's process model. The
// previous Builder/Runner code, its YAML configuration, its global telemetry
// and the rest, grpc, job, queue and health packages have been removed, and
// each package comes back through its own accepted proposal. Until the App
// lifecycle lands, this package has no app type to run.
//
// The plan is in the charter, Proposal: humus on the bedrock process model:
// https://github.com/z5labs/humus/discussions/629
//
// Earlier releases remain available at https://pkg.go.dev/github.com/z5labs/humus.
package humus
