// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpservertest_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/z5labs/bedrock/config"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
)

// Init is the end user's Init, sourced as the proposal's revisions 1 and 2
// show: Addr and TLS have no default, and the certificate is a PEM file
// named by TLS_PEM_FILE. It serves greet on GET /hello/{name}.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		return httpserver.Settings{
			// No .Default: Addr and TLS are required, so an unset ADDR or
			// TLS_PEM_FILE fails as not found.
			Addr:              r.Get(env("ADDR").Validate(httpserver.CheckAddr)),
			TLS:               r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS)),
			HTTP2Only:         r.Get(env("HTTP2_ONLY").Parse(strconv.ParseBool).Default(d.HTTP2Only)),
			ReadHeaderTimeout: r.Get(env("READ_HEADER_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadHeaderTimeout).Validate(httpserver.CheckReadHeaderTimeout)),
			ReadTimeout:       r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(httpserver.CheckReadTimeout)),
			WriteTimeout:      r.Get(env("WRITE_TIMEOUT").Parse(time.ParseDuration).Default(d.WriteTimeout).Validate(httpserver.CheckWriteTimeout)),
			IdleTimeout:       r.Get(env("IDLE_TIMEOUT").Parse(time.ParseDuration).Default(d.IdleTimeout).Validate(httpserver.CheckIdleTimeout)),
			MaxHeaderBytes:    r.Get(env("MAX_HEADER_BYTES").Parse(strconv.Atoi).Default(d.MaxHeaderBytes).Validate(httpserver.CheckMaxHeaderBytes)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}
	return serve(s, settings)
}

// InitPKCS12 is an Init like Init, whose certificate is a PKCS#12 bundle in
// the file TLS_PKCS12_FILE names, encrypted with TLS_PKCS12_PASSWORD. Every
// other field keeps its default. It serves greet on GET /hello/{name}.
func InitPKCS12(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		settings := httpserver.DefaultSettings()
		settings.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		settings.TLS = r.Get(env("TLS_PKCS12_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(bundle []byte) config.Value[*tls.Config] {
			return env("TLS_PKCS12_PASSWORD").Parse(pkcs12TLS(bundle))
		}).Validate(httpserver.CheckTLS))
		return settings
	}).Read(ctx)
	if err != nil {
		return nil, err
	}
	return serve(s, settings)
}

// serve returns the one Service of Init and InitPKCS12: greet on
// GET /hello/{name}, with settings.
func serve(s *humus.Setup, settings httpserver.Settings) ([]humus.Service, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello/{name}", greet)

	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// greet answers GET /hello/{name} with "hello, " and the name.
func greet(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "hello, "+r.PathValue("name"))
}

// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

// serverTLS returns a server's TLS config for a PEM file that holds its
// certificate chain and its private key.
func serverTLS(pem []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// pkcs12TLS returns a parser for config.Value.Parse that decodes bundle,
// a PKCS#12 bundle, with the password it is given, into a server's TLS
// config.
func pkcs12TLS(bundle []byte) func(password string) (*tls.Config, error) {
	return func(password string) (*tls.Config, error) {
		key, leaf, chain, err := pkcs12.DecodeChain(bundle, password)
		if err != nil {
			return nil, err
		}
		cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
		for _, c := range chain {
			cert.Certificate = append(cert.Certificate, c.Raw)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
	}
}
