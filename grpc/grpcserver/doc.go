// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package grpcserver serves gRPC with humus's lifecycle.
//
// It is in the nested module github.com/z5labs/humus/grpc, which is
// always built against the core module in the same commit.
//
// # Settings
//
// [Settings] holds what an operator changes from one deployment to the
// next: the address to listen on, and the TLS config to serve with. Every
// field is a plain value, and each has a check, [CheckAddr] and [CheckTLS],
// that reports what is wrong with a value without naming the field. The
// package reads no environment variable and no file: the end user sources
// the settings in their Init, with whatever variable names they choose,
// and runs each field's check as they do:
//
//	settings, err := config.Struct(func(r *config.Resolver) grpcserver.Settings {
//		return grpcserver.Settings{
//			// No .Default: both fields are required, so an unset variable fails as not found.
//			Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
//			TLS:  r.Get(env("GRPC_TLS_P12_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(grpcserver.CheckTLS)),
//		}
//	}).Read(ctx)
//
// Here s is the Setup the end user's Init is given, and env, readFile and
// serverTLS are the end user's own helpers: env names a variable, readFile
// reads the file it names, and serverTLS builds a *tls.Config from what it
// read.
//
// Neither field has a default. humus imposes no port, and serves nothing in
// plain text, so the end user always sources both, with no .Default in
// their chains. The package has no DefaultSettings: it would only return
// the zero Settings, whose Addr of "" and nil TLS both checks reject. A
// variable that isn't set fails as not found, and a value that fails its
// check fails Init, naming the variable, so the process exits 78.
package grpcserver
