// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the ledger's Init: its settings, its store, its client certificates, its server, its probes and its telemetry.
package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/example/grpc/wallet/internal/certfile"
	"github.com/z5labs/humus/example/grpc/wallet/ledger/server"
	"github.com/z5labs/humus/example/grpc/wallet/ledger/store"
	"github.com/z5labs/humus/example/grpc/wallet/ledgerpb"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/grpc/grpcauth"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/telemetry"
)

const (
	defaultWatchInterval = time.Second
	maxConnectionAge     = time.Minute
)

const scenarioKey = "acme.fault"

func Faults(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
	codec, err := jsonscenario.New(scenarioKey)
	if err != nil {
		return fault.Settings{}, err
	}
	d := fault.DefaultSettings()
	return config.Struct(func(r *config.Resolver) fault.Settings {
		f := d
		f.Decoder = codec
		f.MaxDelay = r.Get(config.Env(s.Process.Environment, "FAULT_MAX_DELAY").Parse(time.ParseDuration).Default(d.MaxDelay).Validate(fault.CheckMaxDelay))
		return f
	}).Read(ctx)
}

func BuildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	d := telemetry.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(p.Environment, name) }
	exporter := func(name string, def telemetry.Exporter) config.Value[telemetry.Exporter] {
		return env(name).
			Map(func(s string) telemetry.Exporter { return telemetry.Exporter(s) }).
			Default(def).
			Validate(telemetry.CheckExporter)
	}

	s, err := config.Struct(func(r *config.Resolver) telemetry.Settings {
		s := d
		s.Disabled = r.Get(env("OTEL_SDK_DISABLED").Parse(strconv.ParseBool).Default(d.Disabled))
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("ledger").Validate(telemetry.CheckServiceName))
		s.Traces = r.Get(exporter("OTEL_TRACES_EXPORTER", d.Traces))
		s.Metrics = r.Get(exporter("OTEL_METRICS_EXPORTER", d.Metrics))
		s.Logs = r.Get(exporter("OTEL_LOGS_EXPORTER", d.Logs))
		s.OTLP.Endpoint = r.Get(env("OTEL_EXPORTER_OTLP_ENDPOINT").Default(d.OTLP.Endpoint).Validate(telemetry.CheckOTLPEndpoint))
		return s
	}).Read(ctx)
	if err != nil {
		return telemetry.SDK{}, err
	}
	return telemetry.New(ctx, s, dial, p.Stdout)
}

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return InitWith(ctx, s, openPostgres)
}

type OpenStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (st server.Store, ping func(context.Context) error, err error)

func InitWith(ctx context.Context, s *humus.Setup, open OpenStore) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	st, ping, err := open(ctx, s, set.Database)
	if err != nil {
		return nil, err
	}
	ledger, err := server.New(st, set.WatchInterval, s.Telemetry)
	if err != nil {
		return nil, err
	}

	cert := auth.MutualTLS("mutualTLS", subject)
	unary, stream, err := grpcauth.Interceptors(s, grpcauth.Default(auth.Require(cert)))
	if err != nil {
		return nil, err
	}
	svc, err := grpcserver.NewService(s, set.GRPC,
		unary,
		stream,
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: maxConnectionAge}),
	)
	if err != nil {
		return nil, err
	}
	ledgerpb.RegisterLedgerServer(svc, ledger)

	probes, err := grpchealth.NewService(s, set.Probes, health.Readiness(newReached(ping)))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, probes}, nil
}

func subject(_ context.Context, chains [][]*x509.Certificate) (pkix.Name, error) {
	if len(chains) == 0 || len(chains[0]) == 0 {
		return pkix.Name{}, fmt.Errorf("no verified chain: %w", auth.ErrUnauthenticated)
	}
	return chains[0][0].Subject, nil
}

func issuedByTheCA(ctx context.Context, _ string) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	return ok && len(info.State.VerifiedChains) > 0
}

type reached struct {
	check func(context.Context) error
	turn  chan struct{}
	ok    atomic.Bool
}

func newReached(check func(context.Context) error) *reached {
	return &reached{check: check, turn: make(chan struct{}, 1)}
}

func (r *reached) Healthy(ctx context.Context) (bool, error) {
	if r.ok.Load() {
		return true, nil
	}
	select {
	case r.turn <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-r.turn }()
	if r.ok.Load() {
		return true, nil
	}
	if err := r.check(ctx); err != nil {
		return false, err
	}
	r.ok.Store(true)
	return true, nil
}

type settings struct {
	GRPC          grpcserver.Settings
	Probes        grpchealth.Settings
	Database      *pgxpool.Config
	WatchInterval time.Duration
}

type postgres struct {
	URL      *url.URL
	Password string
	CA       *x509.CertPool
}

type sourced struct {
	settings
	ServerTLS *tls.Config
	ClientCAs *x509.CertPool
	Postgres  postgres
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }

	got, err := config.Struct(func(r *config.Resolver) sourced {
		return sourced{
			settings: settings{
				GRPC: grpcserver.Settings{
					Addr:          r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
					TrustScenario: issuedByTheCA,
				},
				Probes: grpchealth.Settings{
					Addr: r.Get(env("PROBES_ADDR").Validate(grpchealth.CheckAddr)),
				},
				WatchInterval: r.Get(env("WATCH_INTERVAL").Parse(time.ParseDuration).Default(defaultWatchInterval).Validate(positive)),
			},
			ServerTLS: r.Get(env("GRPC_TLS_PEM_FILE").Parse(pemTLS(s.Host.FS))),
			ClientCAs: r.Get(file("GRPC_CLIENT_CA_FILE").Parse(certPool)),
			Postgres: postgres{
				URL:      r.Get(env("POSTGRES_URL").Parse(parsePostgresURL).Validate(checkPostgresURL)),
				Password: r.Get(file("POSTGRES_PASSWORD_FILE").Parse(secret)),
				CA:       r.Get(file("POSTGRES_TLS_CA_FILE").Parse(certPool)),
			},
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}

	set := got.settings
	set.GRPC.TLS = &tls.Config{
		GetCertificate: got.ServerTLS.GetCertificate,
		ClientAuth:     tls.VerifyClientCertIfGiven,
		ClientCAs:      got.ClientCAs,
		MinVersion:     tls.VersionTLS12,
	}
	set.Database, err = poolConfig(got.Postgres)
	if err != nil {
		return settings{}, err
	}
	return set, nil
}

var postgresParams = []string{"password", "passfile", "service", "servicefile", "sslmode", "sslrootcert", "sslcert", "sslkey", "sslpassword"}

func parsePostgresURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.New("must be a URL")
	}
	return u, nil
}

func checkPostgresURL(u *url.URL) error {
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("must be a postgres URL")
	}
	if _, ok := u.User.Password(); ok {
		return errors.New("must not hold a password: POSTGRES_PASSWORD_FILE holds it")
	}
	var errs []error
	if u.Hostname() == "" || u.Port() == "" {
		errs = append(errs, errors.New("must have a host and a port"))
	}
	if u.User.Username() == "" {
		errs = append(errs, errors.New("must have a user"))
	}
	if strings.TrimPrefix(u.Path, "/") == "" {
		errs = append(errs, errors.New("must name a database"))
	}
	q := u.Query()
	for _, p := range postgresParams {
		if q.Has(p) {
			errs = append(errs, fmt.Errorf("must not set %s: Init sets it", p))
		}
	}
	return errors.Join(errs...)
}

func poolConfig(p postgres) (*pgxpool.Config, error) {
	u := *p.URL
	u.User = url.UserPassword(p.URL.User.Username(), p.Password)
	q := u.Query()
	q.Set("sslmode", "verify-full")
	for _, name := range []string{"sslrootcert", "sslcert", "sslkey"} {
		q.Set(name, "")
	}
	u.RawQuery = q.Encode()

	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, &config.Error{Name: "POSTGRES_URL", Err: errors.New("must be a PostgreSQL URL that pgx accepts")}
	}
	cfg.ConnConfig.TLSConfig = &tls.Config{
		RootCAs:    p.CA,
		ServerName: u.Hostname(),
		MinVersion: tls.VersionTLS12,
	}
	cfg.ConnConfig.Fallbacks = nil
	return cfg, nil
}

func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (server.Store, func(context.Context) error, error) {
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
	return store.New(pool, s.Faults), pool.Ping, nil
}

func positive(d time.Duration) error {
	if d <= 0 {
		return errors.New("must be positive")
	}
	return nil
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

func secret(b []byte) (string, error) {
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("the file is empty")
	}
	return s, nil
}

func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}

func pemTLS(fsys humus.FS) func(name string) (*tls.Config, error) {
	return func(name string) (*tls.Config, error) {
		return certfile.Config(fsys, name)
	}
}
