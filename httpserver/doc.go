// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package httpserver serves HTTP for a humus service.
//
// [NewService] builds a [Service] that serves an http.Handler, in the end
// user's Init, from [Settings] the end user sources:
//
//	func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
//		settings, err := readSettings(ctx, s) // httpserver.Settings, sourced with config.Struct
//		if err != nil {
//			return nil, err
//		}
//		svc, err := httpserver.NewService(s, settings, mux)
//		if err != nil {
//			return nil, err
//		}
//		return []humus.Service{svc}, nil
//	}
//
// NewService listens on the address through the Setup's Host, so a port
// that is already taken fails Init, and a connection that arrives before
// Serve waits in the backlog.
//
// # Main and its defaults
//
// [Main] runs the end user's Init with the defaults an HTTP server behind a
// load balancer needs. It is humus.Main with [Defaults] applied before the
// end user's options, and does nothing else:
//
//	func main() {
//		httpserver.Main(Init, humus.WithTelemetry(buildTelemetry))
//	}
//
// The package exports no App. The end user's Init builds the Service with
// NewService and returns it, along with any other Service the process
// runs, such as a job or a queue consumer next to the API. Main runs no
// health Service and serves no probes.
//
// Defaults sets two of humus.Main's defaults, and leaves the Host and the
// telemetry Builder as humus.Main's:
//
//   - The drain is humus.DrainDelay(5 * time.Second). Kubernetes removes a
//     terminating pod from a Service's endpoints asynchronously, so for a
//     while after SIGTERM, load balancers still route requests to the pod.
//     The Service keeps serving through the drain, so those requests are
//     answered rather than refused.
//   - The grace period is 10s. With telemetry's 5s shutdown after it, the
//     drain and the grace period add up to 20s, inside the 25s that
//     humus.Main budgets within Kubernetes' default
//     terminationGracePeriodSeconds of 30s.
//
// The end user's options come after Defaults, so they win:
// httpserver.Main(Init, humus.WithShutdownGracePeriod(15*time.Second)) has a
// 15s grace period and still the 5s drain, and humus.WithDrain(nil) removes
// the drain. An operator overrides both without a rebuild, with
// HUMUS_SHUTDOWN_GRACE_PERIOD and HUMUS_SHUTDOWN_DRAIN_DELAY, in the same
// manifest that sets terminationGracePeriodSeconds. A test runs the App on
// the same defaults by passing Defaults to humustest.Start with
// humustest.WithApp.
//
// Whether a process calls httpserver.Main or humus.Main depends on what a
// load balancer routes to. A process uses httpserver.Main when a load
// balancer routes to its HTTP server. A job's pod whose only HTTP server
// serves probes or metrics isn't routed to, so it calls humus.Main, with no
// drain.
//
// # TLS
//
// TLS is required. The Service serves HTTPS with Settings.TLS and nothing in
// plain text: there is no plain-HTTP setting and no self-signed fallback. A
// plain-HTTP request fails its TLS handshake, which the Service logs as an
// ERROR record.
//
// # Protocols
//
// HTTP/2 is always on and preferred. The Service offers h2 first in ALPN,
// whatever the config's NextProtos lists, so every client that offers
// HTTP/2 gets it, and a client that doesn't falls back to HTTP/1.1. Any
// other protocol the config lists, such as acme-tls/1, is offered after
// them. The Service serves a clone of the config, so the end user's is never
// changed. Unencrypted HTTP/2 (h2c) is never served.
//
// With Settings.HTTP2Only, the Service serves HTTP/2 alone. A client that
// offers only HTTP/1.1, or no ALPN at all, completes the TLS handshake and
// is then disconnected before a request is read: it sees its connection
// closed with no response, and nothing is logged.
//
// # Shutdown
//
// The Service follows the two shutdown signals of a humus App:
//
//   - Serve's context being done changes nothing. The Service keeps serving
//     through the App's drain, while a load balancer stops sending it
//     traffic.
//   - Shutdown stops accepting connections, closes idle ones and waits for
//     in-flight requests, within the grace period. When the grace period
//     runs out first, it closes every connection, which cancels the context
//     of each request still in flight, and the App exits with a
//     *humus.GracePeriodError.
//
// A request's context doesn't derive from Serve's context, which is done as
// soon as shutdown starts, so a request that arrives during the drain is
// still served. It is done when the client goes away, or when Shutdown runs
// out of time and closes its connection.
//
// A handler should return once r.Context() is done. One that ignores it
// outlives the App after Shutdown has closed its connection, and in a test
// that runs the App inside testing/synctest, synctest reports its goroutine
// as a leak, still blocked when the test ends.
//
// A connection a handler hijacks, such as a WebSocket, isn't tracked by
// Shutdown or by the close that follows it: the handler closes it itself.
//
// # Telemetry
//
// The Service takes everything from the Setup's telemetry and never reads
// or sets OpenTelemetry's globals. h is served wrapped in OpenTelemetry's
// net/http instrumentation, otelhttp, given the TracerProvider, the
// MeterProvider and the Propagator of s.Telemetry.
//
// Each request gets one server span. Its parent is the trace context the
// request brought in, extracted with the Setup's propagator, so a request
// joins its caller's trace. The propagator comes from the telemetry
// Builder, and it decides which headers carry the trace context: the
// W3C traceparent and tracestate headers, and baggage, with the default
// one. The span is named after the method and the route pattern, such as
// "GET /hello/{name}", when the handler sets Request.Pattern, as an
// http.ServeMux does. Otherwise it is named after the method alone, such as
// "GET".
//
// Each request also records OpenTelemetry's HTTP server metrics, such as
// http.server.request.duration, on the Setup's MeterProvider.
//
// The Service logs through the LoggerProvider of the Setup's telemetry,
// with the OpenTelemetry Logs API and the logger name
// "github.com/z5labs/humus/httpserver". It emits one INFO record,
// "listening", when Serve starts, with the attribute server.address set to
// [Service.Addr], and one ERROR record for each line net/http writes to the
// server's error log, such as a TLS handshake error. Nothing reaches the
// real standard error.
//
// # Panics
//
// A handler that panics aborts its request, which is recorded but isn't
// answered. The Service recovers the panic, records it on the request's
// span as an exception event with its stack trace, sets the span's status
// to Error, and emits one ERROR record, "handler panicked", in the
// request's context, so the record carries the span's trace and span IDs.
// Then it panics again with [http.ErrAbortHandler], and net/http aborts the
// response without logging it a second time: no 500 is written, and no
// header the handler hadn't written. Over HTTP/2 the client sees its stream
// reset, and over HTTP/1.1 its connection closed, even when the handler had
// already written part of its response. The Service keeps serving every
// other request.
//
// A protocol that defines an error response, such as REST, recovers in its
// own handler and writes that response itself.
//
// A handler aborts a response without it being logged or recorded by
// panicking with http.ErrAbortHandler itself, which the Service passes
// through untouched.
//
// A panic in Settings.TrustScenario is handled the same way: it is
// recorded, and fails that request.
//
// # Failure injection
//
// The Service strips the failure scenario a request carries from callers
// the end user hasn't chosen to trust, before any of the end user's code
// runs, so a caller on the internet can't make the service fail on
// purpose. It strips with the fault.Injector in the Setup's Faults, which
// the App builds from humus.WithFaults, right after otelhttp has extracted
// the request's baggage. A request it doesn't trust reaches the handler
// with the scenario stripped from its context, and every other baggage
// member kept, and the request's server span gets a humus.fit.strip event,
// with no attributes.
//
// Settings.TrustScenario decides which callers are trusted. It is given the
// *http.Request as it arrived, scenario and all, before the handler sees
// it, and reports whether the scenario reaches the handler. It is asked
// only about a request that carries a scenario, and only when the App has
// failure injection, so it costs nothing on any other request. When it is
// nil, the default, no caller is trusted, and every scenario is stripped:
// a server the end user hasn't decided about is a trust boundary. Without
// humus.WithFaults, nothing is stripped and TrustScenario is never asked.
//
// Trust needs nothing from authentication, which runs later, in the end
// user's handler. A request already carries what is needed to trust it,
// such as:
//
//   - a verified client certificate, in r.TLS.VerifiedChains, such as one
//     the service mesh issued;
//   - a listener only internal callers reach, such as a second Service on
//     an address the internet can't reach, whose TrustScenario returns
//     true;
//   - a header that a gateway sets, and strips from outside callers.
//
// For example, a Service behind a mesh trusts a caller whose client
// certificate the mesh's CA verified:
//
//	settings.TLS.ClientAuth = tls.VerifyClientCertIfGiven
//	settings.TLS.ClientCAs = meshCAs
//	settings.TrustScenario = func(r *http.Request) bool {
//		return r.TLS != nil && len(r.TLS.VerifiedChains) > 0
//	}
//
// A handler that httphealth attaches to the Service is behind the strip,
// like every other request. httphealth's own Service is unchanged.
//
// # Settings
//
// The server is configured with [Settings]: the address, the TLS config and
// the net/http limits, as plain Go values. [DefaultSettings] returns the
// package's defaults, and each field with a valid range has a check, such as
// [CheckReadTimeout], that reports what is wrong with a value without naming
// the field. TrustScenario is the one field that is set in code rather than
// sourced. The package reads no environment variable and defines no
// variable name: the end user's Init sources each other field, with whatever
// variable names the service uses, and passes the field's check to
// Validate, so an operator's bad value fails startup with exit 78, naming
// the variable.
//
// Addr and TLS have no default. DefaultSettings leaves Addr "" and TLS nil,
// which [CheckAddr] and [CheckTLS] reject, so the end user always sources
// them, with no .Default in their chains. A variable that isn't set then
// fails as not found, with exit 78:
//
//	d := httpserver.DefaultSettings()
//	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
//
//	settings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
//		return httpserver.Settings{
//			// No .Default: Addr and TLS are required.
//			Addr:              r.Get(env("ADDR").Validate(httpserver.CheckAddr)),
//			TLS:               r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS)),
//			HTTP2Only:         r.Get(env("HTTP2_ONLY").Parse(strconv.ParseBool).Default(d.HTTP2Only)),
//			ReadHeaderTimeout: r.Get(env("READ_HEADER_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadHeaderTimeout).Validate(httpserver.CheckReadHeaderTimeout)),
//			ReadTimeout:       r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(httpserver.CheckReadTimeout)),
//			WriteTimeout:      r.Get(env("WRITE_TIMEOUT").Parse(time.ParseDuration).Default(d.WriteTimeout).Validate(httpserver.CheckWriteTimeout)),
//			IdleTimeout:       r.Get(env("IDLE_TIMEOUT").Parse(time.ParseDuration).Default(d.IdleTimeout).Validate(httpserver.CheckIdleTimeout)),
//			MaxHeaderBytes:    r.Get(env("MAX_HEADER_BYTES").Parse(strconv.Atoi).Default(d.MaxHeaderBytes).Validate(httpserver.CheckMaxHeaderBytes)),
//		}
//	}).Read(ctx)
//	if err != nil {
//		return nil, err // exit 78, naming every bad variable
//	}
//
// readFile and serverTLS are the end user's helpers: how a certificate is
// stored and encoded, PEM or PKCS#12, is the end user's decision.
//
// The timeouts keep net/http's meaning of zero, which is "no timeout" or
// "use ReadTimeout", so their checks accept zero. MaxHeaderBytes must be
// positive: in net/http a zero means 1 MiB, a default applied to a zero
// value, so CheckMaxHeaderBytes rejects it instead. HTTP2Only has no check,
// since both values are valid.
package httpserver
