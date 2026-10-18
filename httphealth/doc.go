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
// is the error, and its attribute humus.health.probe is liveness or
// readiness. A monitor that is unhealthy without an error, such as a health
// Service's own readiness once the App starts shutting down, logs nothing.
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
//
// # The probes server
//
// [NewService] serves the handler, and nothing else, on a port of its own.
// Attach the handler wherever the pod already runs a server the kubelet can
// reach, rather than opening another port. The probes server is for the
// pods that can't: a pod with no server, such as a Kafka consumer's or a
// long-running job's, and a pod whose server requires client certificates,
// which the kubelet never presents.
//
// The probes server owns the App's readiness, as a health Service does. It
// builds a health Service from the health.Options it is given, and runs it
// for as long as it serves, so the end user's Init returns no health
// Service next to it:
//
//	probes, err := httphealth.NewService(s, probeSettings, health.Readiness(consumer.Joined()))
//	if err != nil {
//		return nil, err
//	}
//	return []humus.Service{consumer, probes}, nil
//
// Readiness is false until the Service's Serve starts, and false for good
// once Serve's context is done, which is when the App starts shutting down,
// or once Shutdown is called. Otherwise it checks the monitors
// health.Readiness adds. Liveness checks the monitors health.Liveness adds,
// and is healthy when there are none.
//
// It serves HTTPS, as every HTTP port humus serves does, so its probes set
// scheme: HTTPS:
//
//	readinessProbe:
//	  httpGet:
//	    path: /health/readiness
//	    port: probes
//	    scheme: HTTPS
//
// The kubelet doesn't verify the certificate of such a probe, and presents
// none of its own: see Settings, below.
//
// It speaks HTTP/1.1 alone, as the kubelet does, and offers only http/1.1
// in ALPN. A probe in plain HTTP, one that leaves out scheme: HTTPS, fails
// its TLS handshake, and the failure is logged at ERROR, so the log says why
// the probe failed.
//
// It serves through the drain. Once the App starts shutting down, a
// readiness probe is answered 503, not refused, and a liveness probe is
// still answered 200, until the App shuts the Service down.
//
// A probes port isn't for the public. The server answers anyone who can
// reach the port, and every request checks a monitor. The port is the
// container's, for the kubelet, and isn't routed by a Kubernetes Service
// or an ingress.
//
// The probes server records no span and no metric, and extracts no trace
// context: a probe every few seconds would be most of a quiet service's
// traces. It logs under this package's import path, through the Setup's
// LoggerProvider: "listening" at INFO, with the address as server.address,
// when it starts serving, and each line of net/http's error log, such as a
// TLS handshake error, as one ERROR record.
//
// # Settings
//
// A probes server of its own, for a pod with no server to attach the
// handler to, such as a consumer's or a long-running job's, is configured
// with [Settings]: its address and its TLS config, as plain Go values. Each
// field has a check, [CheckAddr] and [CheckTLS], that reports what is wrong
// with a value without naming the field. The package reads no environment
// variable and defines no variable name: the end user's Init sources each
// field, with whatever variable names the service uses, and passes the
// field's check to Validate, so an operator's bad value fails startup with
// exit 78, naming the variable.
//
// Neither field has a default, so the package has no DefaultSettings, and
// the zero Settings fails both checks. The end user always sources both,
// with no .Default in their chains. A variable that isn't set then fails as
// not found, with exit 78:
//
//	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
//
//	probeSettings, err := config.Struct(func(r *config.Resolver) httphealth.Settings {
//		return httphealth.Settings{
//			// No .Default: both fields are required.
//			Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
//			TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httphealth.CheckTLS)),
//		}
//	}).Read(ctx)
//	if err != nil {
//		return nil, err // exit 78, naming every bad variable
//	}
//
// readFile and serverTLS are the end user's helpers: how a certificate is
// stored and encoded, PEM or PKCS#12, is the end user's decision.
//
// The probes port serves HTTPS, as every HTTP port humus serves does, so a
// probe sets scheme: HTTPS. The kubelet doesn't verify the certificate of
// such a probe, so the end user's own certificate serves, the one their
// other ports serve or any other: the probes need none of their own. The
// kubelet presents no client certificate either, so CheckTLS rejects a
// config whose ClientAuth is RequireAnyClientCert or
// RequireAndVerifyClientCert, under which every probe would fail and the
// pod would never turn ready. VerifyClientCertIfGiven, and every weaker
// ClientAuth, is accepted.
package httphealth
