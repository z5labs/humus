// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the shoppingcart's Init and its telemetry: a REST API on
// Postgres, served over TLS by httpserver.
//
// [Init] does three things, in order:
//   - It sources its settings from the Process with config.Struct: the
//     httpserver.Settings, with the certificate a PKCS#12 bundle read
//     through the Host's filesystem, and DATABASE_URL. A setting that is
//     missing or bad exits 78, naming every one.
//   - It opens its Postgres pool with pgxpool.NewWithConfig, dialing through
//     the Host, and registers the pool's close with OnStop, so the pool is
//     closed once the server has drained, or once Init fails after it.
//   - It builds its API with openapi.NewAPI, from typed operations whose
//     codecs and error answers are all named, and serves it with
//     httpserver.NewService.
//
// # The settings
//
// Every variable the service reads:
//
//	ADDR                  the address to listen on, host:port; required
//	TLS_PKCS12_FILE       the PKCS#12 bundle of the certificate and its key, a
//	                      path on the Host's filesystem; required
//	TLS_PKCS12_PASSWORD   the bundle's password; required
//	DATABASE_URL          the Postgres connection URL; required
//	HTTP2_ONLY, READ_HEADER_TIMEOUT, READ_TIMEOUT, WRITE_TIMEOUT,
//	IDLE_TIMEOUT, MAX_HEADER_BYTES
//	                      httpserver.Settings' other fields; each defaults to
//	                      httpserver.DefaultSettings' value
//
// # What pgx reads from outside the Host
//
// pgx dials through the Host: Init sets the pool's DialFunc to the Host's
// Dial, and its LookupFunc to one that returns the host unresolved, so the
// Host's Dial resolves the name too. Everything else pgx reads, it reads
// from the real process, whatever Host the App runs on:
//
//   - pgconn.ParseConfig, which parses DATABASE_URL, fills in whatever the URL
//     leaves out from the real PG* variables, such as PGHOST, PGUSER,
//     PGPASSWORD, PGSSLMODE and PGSERVICE, not from the Process's
//     environment.
//   - With no password in the URL or in PGPASSWORD, it reads the password
//     from the real ~/.pgpass, or the file PGPASSFILE names. A PGSERVICE, or
//     a service in the URL, is read from the real pg_service.conf.
//   - The files sslcert, sslkey and sslrootcert name, and ~/.postgresql's
//     defaults for them, are read from the real filesystem, not the Host's.
//   - A URL with no host connects to a Unix socket, in /tmp or the like,
//     through the Host's Dial, which may not reach it.
//
// So the URL sets every field the service uses, host, port, user, password,
// database and sslmode, and nothing is left for pgx to find:
//
//	DATABASE_URL=postgres://carts:secret@db.internal:5432/carts?sslmode=verify-full
package app

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
	"github.com/z5labs/humus/telemetry"
)

// Init is the shoppingcart's Init: it sources its settings, opens its
// Postgres pool through the Host, and serves its API.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres)
}

// openStore opens the Store the API serves, from the pool's config. Init's
// is openPostgres. The tests swap in an in-memory Store, since Postgres
// can't run on humustest's Host.
type openStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (endpoint.Store, error)

// initWith is Init, with the Store open returns.
func initWith(ctx context.Context, s *humus.Setup, open openStore) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	store, err := open(ctx, s, settings.Database)
	if err != nil {
		return nil, err
	}

	api, err := openapi.NewAPI(s,
		openapi.Info{
			Title:       "Shopping Cart API",
			Version:     "1.0.0",
			Description: "API for managing shopping carts on an e-commerce platform.",
		},
		openapi.Unmatched{
			NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
			MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
		},
		openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
		openapi.CatchAll(http.StatusInternalServerError, endpoint.InternalError, jsoncodec.Problem),
		append(endpoint.Carts{Store: store}.Operations(),
			openapi.Document(openapi.Root().Lit("openapi.json")),
		),
		// Every operation but createCart addresses a cart that may not
		// exist.
		openapi.Error[endpoint.NotFound](http.StatusNotFound, jsoncodec.Problem),
	)
	if err != nil {
		return nil, err
	}

	svc, err := httpserver.NewService(s, settings.HTTP, api)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// settings are everything Init sources.
type settings struct {
	HTTP     httpserver.Settings
	Database *pgxpool.Config
}

// readSettings sources the settings from s.Process's environment, and the
// certificate from s.Host's filesystem.
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	return config.Struct(func(r *config.Resolver) settings {
		return settings{
			HTTP: httpserver.Settings{
				// No .Default: Addr and TLS are required.
				Addr: r.Get(env("ADDR").Validate(httpserver.CheckAddr)),
				TLS: r.Get(env("TLS_PKCS12_FILE").Parse(readFile(s.Host.FS)).FlatMap(func(bundle []byte) config.Value[*tls.Config] {
					return env("TLS_PKCS12_PASSWORD").Parse(pkcs12TLS(bundle))
				}).Validate(httpserver.CheckTLS)),
				HTTP2Only:         r.Get(env("HTTP2_ONLY").Parse(strconv.ParseBool).Default(d.HTTP2Only)),
				ReadHeaderTimeout: r.Get(env("READ_HEADER_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadHeaderTimeout).Validate(httpserver.CheckReadHeaderTimeout)),
				ReadTimeout:       r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(httpserver.CheckReadTimeout)),
				WriteTimeout:      r.Get(env("WRITE_TIMEOUT").Parse(time.ParseDuration).Default(d.WriteTimeout).Validate(httpserver.CheckWriteTimeout)),
				IdleTimeout:       r.Get(env("IDLE_TIMEOUT").Parse(time.ParseDuration).Default(d.IdleTimeout).Validate(httpserver.CheckIdleTimeout)),
				MaxHeaderBytes:    r.Get(env("MAX_HEADER_BYTES").Parse(strconv.Atoi).Default(d.MaxHeaderBytes).Validate(httpserver.CheckMaxHeaderBytes)),
			},
			// pgxpool.ParseConfig reads the real PG* variables and
			// ~/.pgpass for what the URL leaves out: see the package doc.
			Database: r.Get(env("DATABASE_URL").Parse(pgxpool.ParseConfig)),
		}
	}).Read(ctx)
}

// openPostgres opens a pool on cfg that dials through s.Host, registers its
// close with s.OnStop, and returns the Store on it. The pool connects
// lazily, on the first request that needs a connection.
func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (endpoint.Store, error) {
	cfg.ConnConfig.DialFunc = s.Host.Dial
	// pgconn resolves the host itself, with the real resolver, unless its
	// LookupFunc says otherwise. This one hands the name to the Host's Dial
	// unresolved.
	cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s.OnStop(func(context.Context) error {
		pool.Close()
		return nil
	})
	return database.New(pool), nil
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

// BuildTelemetry is the service's telemetry.Builder. It names the service,
// and keeps telemetry.DefaultSettings' every other field.
func BuildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "shoppingcart"
	return telemetry.New(ctx, s, dial, p.Stdout)
}
