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
// # Logs
//
// The Service logs through the LoggerProvider of the Setup's telemetry,
// with the OpenTelemetry Logs API and the logger name
// "github.com/z5labs/humus/httpserver". It emits one INFO record,
// "listening", when Serve starts, with the attribute server.address set to
// [Service.Addr], and one ERROR record for each line net/http writes to the
// server's error log, such as a TLS handshake error. Nothing reaches the
// real standard error.
//
// # Settings
//
// The server is configured with [Settings]: the address, the TLS config and
// the net/http limits, as plain Go values. [DefaultSettings] returns the
// package's defaults, and each field with a valid range has a check, such as
// [CheckReadTimeout], that reports what is wrong with a value without naming
// the field. The package reads no environment variable and defines no
// variable name: the end user's Init sources each field, with whatever
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
