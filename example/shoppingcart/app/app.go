// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the shoppingcart's Init and its telemetry: a REST API on
// Postgres, served over TLS by httpserver.
//
// [Init] does four things, in order:
//   - It sources its settings from the Process with config.Struct: the
//     httpserver.Settings, with the certificate a PKCS#12 bundle read
//     through the Host's filesystem, DATABASE_URL, and its identity
//     provider's key set and the jwt.Settings its tokens are checked
//     against. A setting that is missing or bad exits 78, naming every one.
//   - It opens its Postgres pool with pgxpool.NewWithConfig, dialing through
//     the Host, and registers the pool's close with OnStop, so the pool is
//     closed once the server has drained, or once Init fails after it.
//   - It builds the Verifier of its bearer tokens: an httpclient client for
//     the key set's origin, a jwt.RemoteKeySet on it, and a jwt.Verifier on
//     that. Nothing is fetched until the first token.
//   - It builds its API with openapi.NewAPI, from typed operations whose
//     codecs, error answers and security are all named, and serves it with
//     httpserver.NewService.
//
// # Authentication
//
// Every operation requires a JWT bearer token from the shoppingcart's
// identity provider, and every operation that changes a cart, creating one
// included, requires the scope carts:write too. A token's scopes are its
// scope claim, separated by spaces, which endpoint.Claims reads:
//
//	Authorization: Bearer <a JWT whose scope claim holds "carts:write">
//
// auth/jwt's Verifier checks each token's signature against the provider's
// JSON Web Key Set, which it fetches from JWKS_URL with an httpclient client
// that trusts the CA in JWKS_CA_FILE, or the system's roots without it. It
// checks the token's iss against JWT_ISSUER and its aud against
// JWT_AUDIENCE, and its exp, nbf and iat against the clock. The API answers:
//
//   - a request with no token, or one the Verifier rejects, with a 401 and a
//     Bearer challenge in WWW-Authenticate, as a problem detail;
//   - a token without carts:write, on an operation that changes a cart, with
//     a 403 and Bearer's insufficient_scope;
//   - a token the service couldn't get keys for, because the key set is
//     unreachable and no keys are held, with a 503 that says to try again.
//     The token may be valid, so it is never a 401.
//
// The OpenAPI document, at /openapi.json, is public: it is served without a
// token, and its security tells a client which token each operation needs.
//
// # Tests and tokens
//
// The tests are the service's identity provider too. Each one generates a
// signing key with crypto/rand, serves its public half as a JSON Web Key
// Set from a handler of the test's own, with httpserver, over TLS with a
// certificate from the test's humustest CA, and signs the tokens it sends
// with the key, with the scopes the test needs. The service reaches the key
// set through its own settings, JWKS_URL and JWKS_CA_FILE, as it does in
// production: on the humustest Host in the unit tests, and on the OS Host in
// the container test.
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
//	JWKS_URL              the identity provider's JSON Web Key Set, its
//	                      jwks_uri: an https URL; required
//	JWKS_CA_FILE          a PEM file of the CA certificates the key set's
//	                      server is trusted through, a path on the Host's
//	                      filesystem; the system's roots without it
//	JWKS_TIMEOUT, JWKS_ATTEMPT_TIMEOUT, JWKS_MAX_ATTEMPTS
//	                      the key set's httpclient.Settings Timeout,
//	                      AttemptTimeout and MaxAttempts; each defaults to
//	                      httpclient.DefaultSettings' value
//	JWT_ISSUER            the iss every token must have; required
//	JWT_AUDIENCE          what every token's aud must hold; required
//	JWT_ALGORITHMS        the algorithms a token may be signed with,
//	                      separated by commas, such as RS256,ES256
//	JWT_LEEWAY, JWT_MIN_REFRESH_INTERVAL, JWT_MAX_KEY_AGE
//	                      jwt.Settings' other fields; each, and
//	                      JWT_ALGORITHMS, defaults to jwt.DefaultSettings'
//	                      value
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
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/httpclient"
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

	// The key set's client is for its origin alone, and fetches nothing
	// until the Verifier's first token.
	idp, err := httpclient.NewClient(s, settings.KeySet.Client)
	if err != nil {
		return nil, err
	}
	keys, err := jwt.NewRemoteKeySet(settings.KeySet.URL, idp)
	if err != nil {
		return nil, err
	}
	tokens, err := jwt.NewVerifier[endpoint.Claims](s, settings.JWT, keys)
	if err != nil {
		return nil, err
	}
	bearer := auth.Bearer("bearer", tokens.Verify).
		Format("JWT").
		Description("A JWT from the shoppingcart's identity provider. Changing a cart needs the scope carts:write.")

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
		append(endpoint.Carts{Store: store, Bearer: bearer}.Operations(),
			// The document is public, so a client can read which token
			// each operation needs before it has one.
			openapi.Document(openapi.Root().Lit("openapi.json"), openapi.Security(auth.Disabled())),
		),
		// Every operation but createCart addresses a cart that may not
		// exist.
		openapi.Error[endpoint.NotFound](http.StatusNotFound, jsoncodec.Problem),
		// Every operation requires a bearer token, and those that change a
		// cart require carts:write as well. A 401 and a 403 are answered by
		// RequestErrors.
		openapi.Security(auth.Require(bearer)),
		// A token the Verifier had no keys for, because the key set is
		// unreachable, is the server's outage, which clients retry.
		openapi.Error[*jwt.KeySetError](http.StatusServiceUnavailable, endpoint.KeysUnavailable),
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
	KeySet   keySetSettings
	JWT      jwt.Settings
}

// keySetSettings are where the identity provider's key set is, and the
// client it is fetched with.
type keySetSettings struct {
	URL *url.URL

	// Client's BaseURL is URL's origin.
	Client httpclient.Settings
}

// readSettings sources the settings from s.Process's environment, and the
// certificate from s.Host's filesystem.
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	read, err := config.Struct(func(r *config.Resolver) settings {
		// The key set's client keeps every default but the fields below.
		// Its BaseURL is set from JWKS_URL once that has been read.
		client := httpclient.DefaultSettings()
		// Without JWKS_CA_FILE the pool is nil, which trusts the system's
		// roots.
		client.TLS = r.Get(env("JWKS_CA_FILE").Parse(readFile(s.Host.FS)).Parse(certPool).Default(nil).Map(func(roots *x509.CertPool) *tls.Config {
			return &tls.Config{RootCAs: roots}
		}))
		client.Timeout = r.Get(env("JWKS_TIMEOUT").Parse(time.ParseDuration).Default(client.Timeout).Validate(httpclient.CheckTimeout))
		client.AttemptTimeout = r.Get(env("JWKS_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(client.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout))
		client.MaxAttempts = r.Get(env("JWKS_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(client.MaxAttempts).Validate(httpclient.CheckMaxAttempts))

		tokens := jwt.DefaultSettings()
		// No .Default: Issuer and Audience are required.
		tokens.Issuer = r.Get(env("JWT_ISSUER").Validate(jwt.CheckIssuer))
		tokens.Audience = r.Get(env("JWT_AUDIENCE").Validate(jwt.CheckAudience))
		tokens.Algorithms = r.Get(env("JWT_ALGORITHMS").Parse(parseAlgorithms).Default(tokens.Algorithms).Validate(jwt.CheckAlgorithms))
		tokens.Leeway = r.Get(env("JWT_LEEWAY").Parse(time.ParseDuration).Default(tokens.Leeway).Validate(jwt.CheckLeeway))
		tokens.MinRefreshInterval = r.Get(env("JWT_MIN_REFRESH_INTERVAL").Parse(time.ParseDuration).Default(tokens.MinRefreshInterval).Validate(jwt.CheckMinRefreshInterval))
		tokens.MaxKeyAge = r.Get(env("JWT_MAX_KEY_AGE").Parse(time.ParseDuration).Default(tokens.MaxKeyAge).Validate(jwt.CheckMaxKeyAge))

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
			KeySet: keySetSettings{
				URL:    r.Get(env("JWKS_URL").Parse(url.Parse).Validate(checkKeySetURL)),
				Client: client,
			},
			JWT: tokens,
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}
	// The key set's client is for its origin: the URL's scheme, host and
	// port.
	read.KeySet.Client.BaseURL = &url.URL{Scheme: read.KeySet.URL.Scheme, Host: read.KeySet.URL.Host}
	return read, nil
}

// checkKeySetURL rejects a key set URL that isn't an absolute https URL
// with a host. A key set fetched in plain text would let anyone on the path
// add a key, and sign tokens the service accepts.
func checkKeySetURL(u *url.URL) error {
	if u.Scheme != "https" || u.Host == "" {
		return errors.New("must be an absolute https URL")
	}
	return nil
}

// parseAlgorithms parses a list of JWS algorithms, separated by commas,
// such as "RS256,ES256". jwt.CheckAlgorithms checks each one.
func parseAlgorithms(s string) ([]jwt.Algorithm, error) {
	var algs []jwt.Algorithm
	for name := range strings.SplitSeq(s, ",") {
		algs = append(algs, jwt.Algorithm(strings.TrimSpace(name)))
	}
	return algs, nil
}

// certPool parses pem, one or more PEM CERTIFICATE blocks, into a pool of
// roots.
func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
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
