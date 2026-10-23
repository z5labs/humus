// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package health describes a service's health as monitors that the service
// combines.
//
// A [Monitor] is anything that can report whether it is healthy right now:
// a database ping, a flag the service sets once it has started, a check of
// a queue's depth. Its Healthy method returns false, or an error, when it is
// not. [Binary] is a monitor the service marks healthy or unhealthy itself,
// and [MonitorFunc] turns any func(context.Context) (bool, error) into one.
//
// Monitors combine. [And] is healthy when every one of its monitors is: it
// checks them in order and stops at the first that is unhealthy or returns
// an error, returning that result. [Or] is healthy when any one of its
// monitors is: it stops at the first that is healthy, and otherwise is
// unhealthy and returns every monitor's error, joined with [errors.Join].
// Both pass the context they are given to every monitor they check, so a
// caller's deadline reaches each check. And and Or are monitors themselves,
// so they nest.
//
// # Liveness and readiness
//
// An orchestrator such as Kubernetes asks a service two questions, and
// [Probes] holds a monitor for each:
//
//   - Liveness: is the process still working? When a liveness probe fails,
//     the orchestrator restarts the process, so it should fail only when a
//     restart would fix it, a deadlock for example.
//   - Readiness: can the process serve traffic right now? When a readiness
//     probe fails, the orchestrator stops sending the process traffic until
//     it passes again, without restarting it. This is where a service checks
//     that it has started and that the dependencies it needs, a database for
//     example, are reachable.
//
// Liveness checks no dependencies by default, which is [And] with no
// monitors: restarting a pod doesn't fix a database that is down, and a
// liveness probe that checks one restarts every pod of the service while the
// database is down.
//
//	var started health.Binary
//	probes := health.Probes{
//		Liveness: health.And(),
//		Readiness: health.And(&started, health.MonitorFunc(func(ctx context.Context) (bool, error) {
//			return pool.Ping(ctx) == nil, nil
//		})),
//	}
//
// # The health Service
//
// A [Service] owns an App's readiness. It is built for probes attached to
// a server the App already runs: a probes server, such as
// httphealth.NewService's, owns readiness itself, building a Service from
// the same Options, so an Init that returns one returns no health Service.
// The end user's Init builds one with [NewService], adding monitors to its
// probes with [Liveness] and [Readiness], and returns it next to the App's
// other Services. Its
// readiness turns true once its Serve starts, and false as soon as its
// Serve context is done, which is when the App starts shutting down, before
// the drain. A load balancer then stops sending the App traffic while it
// drains. [Service.Probes] returns its probes:
//
//	hs := health.NewService(health.Readiness(health.MonitorFunc(func(ctx context.Context) (bool, error) {
//		return pool.Ping(ctx) == nil, nil
//	})))
//	return []humus.Service{api, hs}, nil
//
// # Exposing the probes
//
// health serves no protocol: nothing in it opens a port or answers an
// orchestrator's probes, and it depends on nothing but the standard library.
// Two packages expose the probes, one per protocol:
//
//   - github.com/z5labs/humus/httphealth answers HTTP probes, such as
//     Kubernetes' httpGet probes, at /health/liveness and /health/readiness,
//     with a handler attached to the end user's HTTPS server or a probes
//     server on a port of its own;
//   - github.com/z5labs/humus/grpc/grpchealth answers gRPC health v1, with
//     a server registered on the end user's gRPC Service or a probes server
//     on a plaintext port of its own.
//
// Exposing the probes is documented at
// https://z5labs.github.io/humus/fundamentals/health/#exposing-the-probes.
package health
