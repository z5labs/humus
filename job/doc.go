// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package job runs a job once, as a humus Service.
//
// # The run
//
// A job's work is its run: a plain func(ctx context.Context) error. A type
// with a Run method passes its method value, such as e.Run, so the package
// has no interface for it and no adapter. The end user's Init builds the
// run's dependencies, releases them with s.OnStop, and returns
// [NewService] of the run to humus.Main:
//
//	func main() {
//		humus.Main(Init, humus.WithTelemetry(buildTelemetry))
//	}
//
//	func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
//		pool, err := openPool(ctx, s)
//		if err != nil {
//			return nil, err
//		}
//		s.OnStop(func(context.Context) error { pool.Close(); return nil })
//
//		e := &exporter{pool: pool, fs: s.Host.FS}
//		return []humus.Service{job.NewService(s, e.Run)}, nil
//	}
//
// The Service's Serve calls the run once, on Serve's own goroutine, and
// returns when it does. A Service returning on its own ends the App, so the
// job finishing ends the process. A process that runs a job next to other
// Services returns them all from its Init, with the job's among them.
//
// # The defaults
//
// A job runs on humus.Main's defaults, which are a job's: a grace period of
// 20s, which fits inside Kubernetes' default terminationGracePeriodSeconds
// of 30s with room for the telemetry to shut down, and no drain, since
// nothing routes traffic to a job. So the package has no Main and no
// defaults of its own. An operator changes them with
// HUMUS_SHUTDOWN_GRACE_PERIOD and HUMUS_SHUTDOWN_DRAIN_DELAY, as for any App,
// and a main that wants a different default in code passes
// humus.WithShutdownGracePeriod to humus.Main.
//
// # Exit statuses
//
// The package adds no exit codes: the App's apply, and the run decides
// which. A run that returns nil exits 0. A run that returns an error exits 1,
// with "serve: " and the run's error as the cause; Serve returns the error
// unchanged.
//
// A run that a signal interrupts exits 1 unless it returns nil, so that
// Kubernetes counts the pod as failed and retries unfinished work, rather
// than completing the Job. A run that stops early because its context is
// done returns ctx.Err(), as almost all Go code already does, and the cause
// is "serve: context canceled". A run that wants the signal in its cause
// returns context.Cause(ctx) instead, and the cause is then, for example,
// "serve: terminated signal received". When a run returns nil after a
// signal, the run has either finished its work or chosen to treat stopping
// early as done, and the App exits 0, with the signal as the cause.
//
// # The two signals
//
// The App gives a Service two signals to stop on, and the run stops on
// either. The run's context is made from Serve's, which is done as soon as
// the App starts shutting down, so a signal cancels the run at once and the
// run has the grace period to stop. Shutdown cancels the run's context too,
// and waits for Serve to return. In an App, Serve's context is always done
// first; when a caller calls Shutdown first, as humustest.TestService does,
// Serve returns nil for a run that returns a cancellation, because the
// Service contract says a Service that Shutdown stopped returns nil.
//
// # Telemetry
//
// Each run has one span, named "job", of kind internal, from the
// TracerProvider of the Setup's telemetry. The run's context carries it, so
// every span the run starts from that context is its child, and every
// record it logs with that context carries its trace ID. When the run
// returns an error, the span's status is Error with the error's text,
// including when Serve reports a run that Shutdown stopped as nil. The
// package writes no log records and records no metrics: the App already
// logs the lifecycle, and the orchestrator sees every run.
//
// # Panics
//
// The package recovers no panic, and doesn't retry the run. A run that
// panics crashes the process with Go's exit status 2, which Kubernetes
// counts as a failure and retries under the Job's backoffLimit.
package job
