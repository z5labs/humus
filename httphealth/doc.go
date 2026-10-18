// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package httphealth answers an orchestrator's HTTP probes, such as
// Kubernetes' liveness and readiness probes, from an App's health.Probes.
//
// # The paths and their answers
//
// [NewHandler] answers two paths, each from one of the probes' monitors:
//
//   - GET /health/liveness checks Probes.Liveness;
//   - GET /health/readiness checks Probes.Readiness.
//
// A probe answers 200 with the body "OK\n" when its monitor is healthy, and
// 503 with the body "Service Unavailable\n" when its monitor is unhealthy or
// returns an error. Every answer is text/plain; charset=utf-8, with
// Cache-Control: no-store. A HEAD is answered as a GET is, with the same
// status and headers and no body. Any other method on either path gets 405,
// with an Allow header, and any other path 404, /health/ itself included.
//
// Each request checks its monitor once, with the request's context. The
// handler adds no timeout and caches nothing: the probe's own timeout bounds
// the check, which closes the connection and so cancels the request's
// context, and the probe's period sets how often the monitor is checked. A
// monitor that is too expensive to check that often caches its own result.
//
// The kubelet decides on the status code alone, so the body says no more
// than which code it is, for a person with curl. A monitor's error is never
// in the body, since it can name a host or a credential's user. It is logged
// instead, as one ERROR record through the Setup's LoggerProvider, under
// this package's import path, with the request's context. The record's body
// is the error, and its attribute health.probe is liveness or readiness. A
// monitor that is unhealthy without an error, such as a health Service's own
// readiness once the App starts shutting down, logs nothing.
//
// # Attaching the handler
//
// The handler routes on the full path, so it is mounted at "/health/" of the
// end user's mux, without http.StripPrefix, next to the API it serves. It
// answers from the Probes of a health Service, which owns the App's
// readiness and is returned from Init next to the server the handler is
// attached to:
//
//	hs := health.NewService(health.Readiness(health.MonitorFunc(func(ctx context.Context) (bool, error) {
//		return pool.Ping(ctx) == nil, nil
//	})))
//	mux := http.NewServeMux()
//	mux.Handle("/", api)
//	mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))
//	svc, err := httpserver.NewService(s, settings, mux)
//	if err != nil {
//		return nil, err
//	}
//	return []humus.Service{svc, hs}, nil
//
// The pod then needs no second port: the kubelet probes the API's own port,
// with scheme: HTTPS, and doesn't verify its certificate. The handler
// recovers no panic of a monitor: the server it is attached to does.
//
// A probe through an attached handler is a request like any other, so the
// server's instrumentation traces it, and a monitor's ERROR record carries
// the trace of the probe's span. A team that doesn't want the probes' spans
// drops them in its sampler.
package httphealth
