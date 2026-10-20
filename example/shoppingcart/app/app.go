// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the shoppingcart's Init: its settings, its store, its catalog, its bearer tokens, its API, its probes and its telemetry.
package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
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

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/example/shoppingcart/catalogclient"
	"github.com/z5labs/humus/example/shoppingcart/database"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/example/shoppingcart/internal/certfile"
	"github.com/z5labs/humus/example/shoppingcart/internal/latch"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
	"github.com/z5labs/humus/telemetry"
)

const maxBodyBytes = 64 << 10

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres, openCatalog)
}

type openStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (store endpoint.Store, ping func(context.Context) error, err error)

type newCatalog func(s *humus.Setup, settings httpclient.Settings) (catalog endpoint.Catalog, ready func(context.Context) error, err error)

func initWith(ctx context.Context, s *humus.Setup, open openStore, newCatalog newCatalog) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	store, ping, err := open(ctx, s, settings.Database)
	if err != nil {
		return nil, err
	}

	catalog, catalogReady, err := newCatalog(s, settings.Catalog)
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
	bearer := auth.Bearer("bearer", endpoint.KeysUnavailable(withSubject(tokens.Verify))).
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
		append(endpoint.Carts{Store: store, Catalog: catalog, Bearer: bearer}.Operations(),
			openapi.Document(openapi.Root().Lit("openapi.json"), openapi.Security(auth.Disabled())),
		),
		openapi.Error[endpoint.NotFound](http.StatusNotFound, jsoncodec.Problem),
		openapi.Security(auth.Require(bearer)),
		openapi.Error[endpoint.Unavailable](http.StatusServiceUnavailable, jsoncodec.Problem),
	)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/health/", httphealth.NewHandler(s, health.Probes{
		Liveness:  health.And(),
		Readiness: health.And(latch.New(ping), latch.New(catalogReady)),
	}))
	mux.Handle("/", http.MaxBytesHandler(api, maxBodyBytes))

	svc, err := httpserver.NewService(s, settings.HTTP, mux)
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

func openCatalog(s *humus.Setup, settings httpclient.Settings) (endpoint.Catalog, func(context.Context) error, error) {
	client, err := httpclient.NewClient(s, settings)
	if err != nil {
		return nil, nil, err
	}
	return catalogclient.New(client, settings.BaseURL), readiness(client, settings.BaseURL.JoinPath("health", "readiness")), nil
}

func readiness(client *http.Client, u *url.URL) func(context.Context) error {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
			_ = resp.Body.Close()
		}()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the catalog's readiness answered %d", resp.StatusCode)
		}
		return nil
	}
}

type settings struct {
	HTTP     httpserver.Settings
	Database *pgxpool.Config
	Catalog  httpclient.Settings
	KeySet   keySetSettings
	JWT      jwt.Settings
}

type keySetSettings struct {
	URL    *url.URL
	Client httpclient.Settings
}

type serverCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error)

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }

	read, err := config.Struct(func(r *config.Resolver) settings {
		client := httpclient.DefaultSettings()
		client.TLS = r.Get(file("JWKS_CA_FILE").Parse(certPool).Default(nil).Map(func(roots *x509.CertPool) *tls.Config {
			return &tls.Config{RootCAs: roots}
		}))
		client.Timeout = r.Get(env("JWKS_TIMEOUT").Parse(time.ParseDuration).Default(client.Timeout).Validate(httpclient.CheckTimeout))
		client.AttemptTimeout = r.Get(env("JWKS_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(client.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout))
		client.MaxAttempts = r.Get(env("JWKS_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(client.MaxAttempts).Validate(httpclient.CheckMaxAttempts))

		catalog := httpclient.DefaultSettings()
		catalog.BaseURL = r.Get(env("CATALOG_URL").Parse(url.Parse).Validate(checkCatalogURL))
		catalog.TLS = &tls.Config{
			RootCAs: r.Get(file("CATALOG_CA_FILE").Parse(certPool)),
			GetClientCertificate: r.Get(env("CATALOG_CLIENT_CERT_FILE").Validate(holdsCertificate(s.Host.FS)).FlatMap(func(certFile string) config.Value[clientCertificate] {
				return env("CATALOG_CLIENT_KEY_FILE").Parse(func(keyFile string) (clientCertificate, error) {
					cfg, err := certfile.Config(s.Host.FS, certFile, keyFile)
					if err != nil {
						return nil, err
					}
					return cfg.GetClientCertificate, nil
				})
			})),
		}
		catalog.Timeout = r.Get(env("CATALOG_TIMEOUT").Parse(time.ParseDuration).Default(catalog.Timeout).Validate(httpclient.CheckTimeout))
		catalog.AttemptTimeout = r.Get(env("CATALOG_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(catalog.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout))
		catalog.MaxAttempts = r.Get(env("CATALOG_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(catalog.MaxAttempts).Validate(httpclient.CheckMaxAttempts))
		catalog.ForwardScenario = true

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
				TLS: &tls.Config{
					GetCertificate: r.Get(env("TLS_CERT_FILE").Validate(holdsCertificate(s.Host.FS)).FlatMap(func(certFile string) config.Value[serverCertificate] {
						return env("TLS_KEY_FILE").Parse(func(keyFile string) (serverCertificate, error) {
							cfg, err := certfile.Config(s.Host.FS, certFile, keyFile)
							if err != nil {
								return nil, err
							}
							return cfg.GetCertificate, nil
						})
					})),
				},
				HTTP2Only:         r.Get(env("HTTP2_ONLY").Parse(strconv.ParseBool).Default(d.HTTP2Only)),
				ReadHeaderTimeout: r.Get(env("READ_HEADER_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadHeaderTimeout).Validate(httpserver.CheckReadHeaderTimeout)),
				ReadTimeout:       r.Get(env("READ_TIMEOUT").Parse(time.ParseDuration).Default(d.ReadTimeout).Validate(httpserver.CheckReadTimeout)),
				WriteTimeout:      r.Get(env("WRITE_TIMEOUT").Parse(time.ParseDuration).Default(d.WriteTimeout).Validate(httpserver.CheckWriteTimeout)),
				IdleTimeout:       r.Get(env("IDLE_TIMEOUT").Parse(time.ParseDuration).Default(d.IdleTimeout).Validate(httpserver.CheckIdleTimeout)),
				MaxHeaderBytes:    r.Get(env("MAX_HEADER_BYTES").Parse(strconv.Atoi).Default(d.MaxHeaderBytes).Validate(httpserver.CheckMaxHeaderBytes)),
			},
			Database: r.Get(file("DATABASE_URL_FILE").Parse(databaseURL)),
			Catalog:  catalog,
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

type clientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error)

func holdsCertificate(fsys humus.FS) func(name string) error {
	return func(name string) error {
		b, err := readFile(fsys)(name)
		if err != nil {
			return err
		}
		for {
			var block *pem.Block
			block, b = pem.Decode(b)
			if block == nil {
				return errors.New("no PEM certificate in the file")
			}
			if block.Type == "CERTIFICATE" {
				return nil
			}
		}
	}
}

func databaseURL(b []byte) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(string(bytes.TrimSpace(b)))
	if err != nil {
		return nil, errors.New("must hold a PostgreSQL URL")
	}
	return cfg, nil
}

func checkCatalogURL(u *url.URL) error {
	if err := httpclient.CheckBaseURL(u); err != nil {
		return err
	}
	if u.Scheme != "https" {
		return errors.New("must be an https URL")
	}
	return nil
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

func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (endpoint.Store, func(context.Context) error, error) {
	cfg.ConnConfig.DialFunc = s.Host.Dial
	cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	s.OnStop(func(context.Context) error {
		pool.Close()
		return nil
	})
	return database.New(pool), pool.Ping, nil
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

func BuildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "shoppingcart"
	return telemetry.New(ctx, s, dial, p.Stdout)
}
