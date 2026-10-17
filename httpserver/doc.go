// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package httpserver serves HTTP for a humus service.
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
