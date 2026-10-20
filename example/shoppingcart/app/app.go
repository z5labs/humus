// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the shoppingcart's Init: its settings, its store, its bearer tokens, its API and its telemetry.
package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres)
}

type openStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (endpoint.Store, error)

func initWith(ctx context.Context, s *humus.Setup, open openStore) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	store, err := open(ctx, s, settings.Database)
	if err != nil {
		return nil, err
	}

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
	bearer := auth.Bearer("bearer", withSubject(tokens.Verify)).
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
			openapi.Document(openapi.Root().Lit("openapi.json"), openapi.Security(auth.Disabled())),
		),
		openapi.Error[endpoint.NotFound](http.StatusNotFound, jsoncodec.Problem),
		openapi.Security(auth.Require(bearer)),
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

func withSubject(verify func(context.Context, string) (endpoint.Claims, error)) func(context.Context, string) (endpoint.Claims, error) {
	return func(ctx context.Context, token string) (endpoint.Claims, error) {
		claims, err := verify(ctx, token)
		if err != nil {
			return endpoint.Claims{}, err
		}
		if claims.Subject == "" {
			return endpoint.Claims{}, fmt.Errorf("the token has no sub: %w", auth.ErrUnauthenticated)
		}
		return claims, nil
	}
}

type settings struct {
	HTTP     httpserver.Settings
	Database *pgxpool.Config
	KeySet   keySetSettings
	JWT      jwt.Settings
}

type keySetSettings struct {
	URL    *url.URL
	Client httpclient.Settings
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	read, err := config.Struct(func(r *config.Resolver) settings {
		client := httpclient.DefaultSettings()
		client.TLS = r.Get(env("JWKS_CA_FILE").Parse(readFile(s.Host.FS)).Parse(certPool).Default(nil).Map(func(roots *x509.CertPool) *tls.Config {
			return &tls.Config{RootCAs: roots}
		}))
		client.Timeout = r.Get(env("JWKS_TIMEOUT").Parse(time.ParseDuration).Default(client.Timeout).Validate(httpclient.CheckTimeout))
		client.AttemptTimeout = r.Get(env("JWKS_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(client.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout))
		client.MaxAttempts = r.Get(env("JWKS_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(client.MaxAttempts).Validate(httpclient.CheckMaxAttempts))

		tokens := jwt.DefaultSettings()
		tokens.Issuer = r.Get(env("JWT_ISSUER").Validate(jwt.CheckIssuer))
		tokens.Audience = r.Get(env("JWT_AUDIENCE").Validate(jwt.CheckAudience))
		tokens.Algorithms = r.Get(env("JWT_ALGORITHMS").Parse(parseAlgorithms).Default(tokens.Algorithms).Validate(jwt.CheckAlgorithms))
		tokens.Leeway = r.Get(env("JWT_LEEWAY").Parse(time.ParseDuration).Default(tokens.Leeway).Validate(jwt.CheckLeeway))
		tokens.MinRefreshInterval = r.Get(env("JWT_MIN_REFRESH_INTERVAL").Parse(time.ParseDuration).Default(tokens.MinRefreshInterval).Validate(jwt.CheckMinRefreshInterval))
		tokens.MaxKeyAge = r.Get(env("JWT_MAX_KEY_AGE").Parse(time.ParseDuration).Default(tokens.MaxKeyAge).Validate(jwt.CheckMaxKeyAge))

		return settings{
			HTTP: httpserver.Settings{
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
	read.KeySet.Client.BaseURL = &url.URL{Scheme: read.KeySet.URL.Scheme, Host: read.KeySet.URL.Host}
	return read, nil
}

func checkKeySetURL(u *url.URL) error {
	switch {
	case u.Scheme != "https" || u.Host == "":
		return errors.New("must be an absolute https URL")
	case u.User != nil:
		return errors.New("must not hold user info")
	case u.Fragment != "" || u.RawFragment != "":
		return errors.New("must not have a fragment")
	}
	return nil
}

func parseAlgorithms(s string) ([]jwt.Algorithm, error) {
	var algs []jwt.Algorithm
	for name := range strings.SplitSeq(s, ",") {
		algs = append(algs, jwt.Algorithm(strings.TrimSpace(name)))
	}
	return algs, nil
}

func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}

func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (endpoint.Store, error) {
	cfg.ConnConfig.DialFunc = s.Host.Dial
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

func BuildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "shoppingcart"
	return telemetry.New(ctx, s, dial, p.Stdout)
}
