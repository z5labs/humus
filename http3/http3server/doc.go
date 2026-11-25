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
// service needs neither.
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
