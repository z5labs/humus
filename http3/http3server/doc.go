// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package http3server serves HTTP/3 for a humus service.
//
// It is in the nested module github.com/z5labs/humus/http3, which keeps
// quic-go out of the core module and is always built against the core
// module in the same commit. Its API is its own: no exported name of the
// package names a type of httpserver's or httpclient's, so an HTTP/3
// service needs neither. quic-go serves HTTP/3, and no quic-go type is in
// the package's API.
//
// # Serving
//
// [NewService] returns a humus.Service that serves an http.Handler over
// HTTP/3, on a UDP socket it binds through the [Network] it is given.
// humus.Host is one, so the end user's Init passes its Host. The end user
// returns it beside httpserver's Service, which serves the same handler
// over HTTP/2 on the same host:port, so HTTP/3 is off unless asked for,
// and both drain in one grace period:
//
//	tcp, err := httpserver.NewService(ctx, s, h, settings.tcp, mux)
//	if err != nil {
//		return nil, err
//	}
//	udp, err := http3server.NewService(ctx, s, h, settings.udp, mux)
//	if err != nil {
//		return nil, err
//	}
//	return []humus.Service{tcp, udp}, nil
//
// TCP and UDP have separate ports, so the two bind one port number without
// conflict. A UDP port that is taken fails NewService, and so the end
// user's Init, with EADDRINUSE. The socket is closed by a function
// NewService registers with Setup.OnStop, and by nothing else: quic-go
// never closes a socket it is handed.
//
// The handler is served through the handler chain httpserver serves
// through: otelhttp's spans and metrics, with network.protocol.version
// 3.0, the span named after the request's route, a panic recorded on the
// span and as one ERROR record, "handler panicked", and the failure
// scenario a request carries stripped unless Settings.TrustScenario trusts
// the caller. A panic resets the request's stream with H3_INTERNAL_ERROR,
// where HTTP/2 resets the stream too.
//
// ReadTimeout and WriteTimeout bound each request's stream from the end of
// the request's headers, where net/http starts an HTTP/2 stream's timers.
// Once ReadTimeout has passed, a read of the body fails, so a slow upload
// ends. When WriteTimeout passes, the request's context is cancelled and
// the stream's reads and writes fail, so a handler that watches its context
// or writes returns, and a handler that never writes doesn't hold its
// goroutine. The stream is reset with H3_INTERNAL_ERROR once the handler
// returns, and not before: quic-go gives a handler no other way to reset
// its stream, so a handler that ignores its context and never writes holds
// the stream until it returns, where HTTP/2 resets it at the timer. A
// handler that streams moves either deadline with http.ResponseController's
// SetReadDeadline and SetWriteDeadline. A handler that takes its stream
// over with http3.HTTPStreamer lifts both bounds.
//
// QUIC's limits are code, not settings, as HTTP/2's stream limit is in
// httpserver. Each one is passed explicitly, so a quic-go bump changes
// none: a 30s MaxIdleTimeout, 100 incoming streams and 100 incoming
// unidirectional streams, a 6 MiB stream and 15 MiB connection receive
// window, no keep-alives and no datagrams. 0-RTT is never accepted. The
// handshake is bounded by half the smallest of ReadHeaderTimeout,
// ReadTimeout and WriteTimeout that is set, or 5s when none is: quic-go
// aborts a handshake silent that long, or twice that long in all.
//
// # The drain
//
// [Service.Shutdown] sends each connection a GOAWAY, and resets with
// H3_REQUEST_REJECTED each stream its client opens after it. It waits for
// the requests in flight, and closes each connection with H3_NO_ERROR one
// second after its last request finished, or one second after Shutdown
// began for a connection with none in flight, as net/http's HTTP/2 server
// closes one a second after its GOAWAY. So an idle client that keeps its
// connection open doesn't hold Shutdown to the end of the grace period,
// and the second lets the last response finish. A connection opened after
// Shutdown has begun gets no answer, and its client waits out its
// handshake timeout.
//
// When the grace period runs out, Shutdown closes every connection at
// once, which cancels the context of each request still in flight, and
// returns without waiting for a handler that ignores its context, as
// httpserver's does.
//
// # Logs
//
// The Service logs through the Setup's LoggerProvider, under the scope
// github.com/z5labs/humus/http3/http3server: "listening", at INFO, when it
// begins serving, and each of quic-go's Error records as one ERROR record,
// such as "Malformed Content-Length" for a response whose handler set a
// Content-Length that isn't a number. quic-go's records below Error are
// dropped, and none reaches slog.Default(). A QUIC handshake that fails is
// logged nowhere: quic-go reports it only to a qlog tracer, which the
// Service has none of.
//
// # Ambient leaks
//
// quic-go reaches around the process in two ways that humus can't stop
// without setting global state, which it never does:
//
//   - It writes a warning to the real standard error with log.Printf, once
//     per process, when it can't raise a UDP socket's receive or send
//     buffer to the 7 MiB it wants. On a real host that is
//     when the node's net.core.rmem_max or net.core.wmem_max is below it,
//     which a pod can't raise, and the process lacks CAP_NET_ADMIN, with
//     which quic-go forces the size. It also writes to the real standard
//     error when QUIC_GO_LOG_LEVEL turns its own logger on, and when that
//     level isn't one it knows. Silencing them means log.SetOutput, which
//     is global.
//   - It reads its QUIC_GO_* variables, such as
//     QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING and QUIC_GO_LOG_LEVEL, from
//     the real environment with os.Getenv, not from the Process's. An
//     operator who sets one sets it on the process itself.
//
// humustest's sockets have SetReadBuffer and SetWriteBuffer, which succeed,
// so a test on a humustest Host writes nothing to the real standard error.
//
// # Main and its defaults
//
// [Main] runs the end user's Init with the defaults an HTTP server behind a
// load balancer needs. It is humus.Main with [Defaults] applied before the
// end user's options, and does nothing else:
//
//	func main() {
//		http3server.Main(Init, humus.WithTelemetry(buildTelemetry))
//	}
//
// Defaults are httpserver's: a drain of humus.DrainDelay(5 * time.Second),
// so a pod keeps serving while load balancers stop routing to it, and a 10s
// grace period. The Host and the telemetry Builder stay humus.Main's. A
// process that serves HTTP/3 beside HTTP/1.1 and HTTP/2 may call either
// package's Main, since both give the same defaults.
//
// The end user's options come after Defaults, so they win:
// http3server.Main(Init, humus.WithShutdownGracePeriod(15*time.Second)) has
// a 15s grace period and still the 5s drain, and humus.WithDrain(nil)
// removes the drain. A test runs the App on the same defaults by passing
// Defaults to humustest.Start with humustest.WithApp.
//
// # Settings
//
// The server is configured with [Settings]: the UDP address, the TLS config
// and the request limits, as plain Go values. [DefaultSettings] returns the
// package's defaults, and each field with a valid range has a check, such
// as [CheckReadTimeout], that reports what is wrong with a value without
// naming the field. TrustScenario is the one field that is set in code
// rather than sourced. The package reads no environment variable and
// defines no variable name: the end user's Init sources each other field,
// with whatever variable names the service uses, and passes the field's
// check to Validate, so an operator's bad value fails startup with exit 78,
// naming the variable.
//
// Addr and TLS have no default. DefaultSettings leaves Addr "" and TLS nil,
// which [CheckAddr] and [CheckTLS] reject, so the end user always sources
// them, with no .Default in their chains. A variable that isn't set then
// fails as not found, with exit 78:
//
//	d := http3server.DefaultSettings()
//	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
//
//	settings, err := config.Struct(func(r *config.Resolver) http3server.Settings {
//		return http3server.Settings{
//			// No .Default: Addr and TLS are required.
//			Addr:              r.Get(env("HTTP3_ADDR").Validate(http3server.CheckAddr)),
//			TLS:               r.Get(env("TLS_PEM_FILE").Parse(readFile(h.Filesystem)).Parse(serverTLS).Validate(http3server.CheckTLS)),
//			ReadHeaderTimeout: r.Get(env("READ_HEADER_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadHeaderTimeout).Validate(http3server.CheckReadHeaderTimeout)),
//			ReadTimeout:       r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(http3server.CheckReadTimeout)),
//			WriteTimeout:      r.Get(env("WRITE_TIMEOUT").Parse(time.ParseDuration).Default(d.WriteTimeout).Validate(http3server.CheckWriteTimeout)),
//			IdleTimeout:       r.Get(env("IDLE_TIMEOUT").Parse(time.ParseDuration).Default(d.IdleTimeout).Validate(http3server.CheckIdleTimeout)),
//			MaxHeaderBytes:    r.Get(env("MAX_HEADER_BYTES").Parse(strconv.Atoi).Default(d.MaxHeaderBytes).Validate(http3server.CheckMaxHeaderBytes)),
//		}
//	}).Read(ctx)
//	if err != nil {
//		return nil, err // exit 78, naming every bad variable
//	}
//
// readFile and serverTLS are the end user's helpers: how a certificate is
// stored and encoded, PEM or PKCS#12, is the end user's decision.
//
// QUIC requires TLS 1.3, so CheckTLS rejects a config whose MaxVersion is
// below it. One config that allows both TLS 1.2 and TLS 1.3 passes the
// CheckTLS of both this package and httpserver, so a process serving both
// transports sources it once. The two packages' CheckAddr have the same
// rules, so an address checked by one passes the other.
//
// The timeouts' zero means "no timeout" or "use ReadTimeout", so their
// checks accept zero. MaxHeaderBytes must be at least 1, so
// CheckMaxHeaderBytes rejects zero rather than reading it as a default.
// There is no HTTP2Only: HTTP/3 has no older protocol on its transport to
// turn off.
package http3server
