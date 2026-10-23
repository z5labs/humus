// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the wallet's Init: its settings, the ledger's client, its key set, its bearer tokens, its server, its probes and its telemetry.
package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/example/grpc/wallet/internal/certfile"
	"github.com/z5labs/humus/example/grpc/wallet/ledgerpb"
	"github.com/z5labs/humus/example/grpc/wallet/server"
	"github.com/z5labs/humus/example/grpc/wallet/walletpb"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/grpc/grpcauth"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/grpc/grpchealth"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/telemetry"
)

const maxConnectionAge = time.Minute

const reasonKeysUnavailable = "KEYS_UNAVAILABLE"

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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("wallet").Validate(telemetry.CheckServiceName))
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
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	ledger, err := grpcclient.NewClient(s, set.Ledger)
	if err != nil {
		return nil, err
	}

	keysClient, err := httpclient.NewClient(s, set.KeySet.Client)
	if err != nil {
		return nil, err
	}
	keys, err := jwt.NewRemoteKeySet(set.KeySet.URL, keysClient)
	if err != nil {
		return nil, err
	}
	tokens, err := jwt.NewVerifier[server.Claims](s, set.JWT, keys)
	if err != nil {
		return nil, err
	}

	if set.FaultTrustScope != "" {
		set.GRPC.TrustScenario = trustTokens(s.Faults, tokens.Verify, set.FaultTrustScope)
	}
	bearer := auth.Bearer("bearer", keysUnavailable(tokens.Verify))
	write := auth.RequireScopes(bearer, server.ScopeWrite)
	unary, stream, err := grpcauth.Interceptors(s,
		grpcauth.Default(auth.RequireScopes(bearer, server.ScopeRead)),
		grpcauth.Method(walletpb.Wallet_OpenAccount_FullMethodName, write),
		grpcauth.Method(walletpb.Wallet_Send_FullMethodName, write),
	)
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
	wallet, err := server.New(ledgerpb.NewLedgerClient(ledger), bearer, s.Telemetry)
	if err != nil {
		return nil, err
	}
	walletpb.RegisterWalletServer(svc, wallet)

	probes, err := grpchealth.NewService(s, set.Probes, health.Readiness(ledger.Connected()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, probes}, nil
}

func trustTokens(faults fault.Injector, verify func(context.Context, string) (server.Claims, error), scope string) func(context.Context, string) bool {
	return func(ctx context.Context, _ string) bool {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("authorization")
		if len(values) != 1 {
			return false
		}
		scheme, token, ok := strings.Cut(values[0], " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return false
		}
		stripped, _ := faults.Strip(ctx)
		claims, err := verify(stripped, strings.TrimSpace(token))
		return err == nil && claims.HasScope(scope)
	}
}

func keysUnavailable(verify func(context.Context, string) (server.Claims, error)) func(context.Context, string) (server.Claims, error) {
	return func(ctx context.Context, token string) (server.Claims, error) {
		claims, err := verify(ctx, token)
		if _, ok := errors.AsType[*jwt.KeySetError](err); ok {
			return server.Claims{}, unavailableKeys()
		}
		return claims, err
	}
}

func unavailableKeys() error {
	st := status.New(codes.Unavailable, "the wallet can't verify tokens right now; try again later")
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: reasonKeysUnavailable,
		Domain: walletpb.Wallet_ServiceDesc.ServiceName,
	})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}

type settings struct {
	GRPC            grpcserver.Settings
	Probes          grpchealth.Settings
	Ledger          grpcclient.Settings
	KeySet          keySetSettings
	JWT             jwt.Settings
	FaultTrustScope string
}

type keySetSettings struct {
	URL    *url.URL
	Client httpclient.Settings
}

type (
	serverCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	clientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
)

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }
	pemFile := func(name string) config.Value[*tls.Config] { return env(name).Parse(pemTLS(s.Host.FS)) }

	got, err := config.Struct(func(r *config.Resolver) settings {
		ledger := grpcclient.DefaultSettings()
		ledger.Addr = r.Get(env("LEDGER_ADDR").Validate(grpcclient.CheckAddr))
		ledger.TLS = &tls.Config{
			RootCAs: r.Get(file("LEDGER_CA_FILE").Parse(certPool)),
			GetClientCertificate: r.Get(pemFile("LEDGER_TLS_PEM_FILE").Map(func(c *tls.Config) clientCertificate {
				return c.GetClientCertificate
			})),
			MinVersion: tls.VersionTLS12,
		}
		ledger.Timeout = r.Get(env("LEDGER_TIMEOUT").Parse(time.ParseDuration).Default(ledger.Timeout).Validate(grpcclient.CheckTimeout))
		ledger.MaxAttempts = r.Get(env("LEDGER_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(ledger.MaxAttempts).Validate(grpcclient.CheckMaxAttempts))
		ledger.ForwardScenario = true

		keys := httpclient.DefaultSettings()
		keys.TLS = &tls.Config{
			RootCAs:    r.Get(file("TOKEN_KEYS_CA_FILE").Parse(certPool)),
			MinVersion: tls.VersionTLS12,
		}
		keys.Timeout = r.Get(env("TOKEN_KEYS_TIMEOUT").Parse(time.ParseDuration).Default(keys.Timeout).Validate(httpclient.CheckTimeout))
		keys.MaxAttempts = r.Get(env("TOKEN_KEYS_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(keys.MaxAttempts).Validate(httpclient.CheckMaxAttempts))

		tokens := jwt.DefaultSettings()
		tokens.Issuer = r.Get(env("TOKEN_ISSUER").Validate(jwt.CheckIssuer))
		tokens.Audience = r.Get(env("TOKEN_AUDIENCE").Validate(jwt.CheckAudience))

		return settings{
			GRPC: grpcserver.Settings{
				Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
				TLS: &tls.Config{
					GetCertificate: r.Get(pemFile("GRPC_TLS_PEM_FILE").Map(func(c *tls.Config) serverCertificate {
						return c.GetCertificate
					})),
					MinVersion: tls.VersionTLS12,
				},
			},
			Probes: grpchealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(grpchealth.CheckAddr)),
			},
			Ledger: ledger,
			KeySet: keySetSettings{
				URL:    r.Get(env("TOKEN_KEYS_URL").Parse(parseURL).Validate(checkKeySetURL)),
				Client: keys,
			},
			JWT:             tokens,
			FaultTrustScope: r.Get(env("FAULT_TRUST_SCOPE").Default("").Validate(checkScope)),
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}
	got.KeySet.Client.BaseURL = &url.URL{Scheme: got.KeySet.URL.Scheme, Host: got.KeySet.URL.Host}
	return got, nil
}

func parseURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, errors.New("must be a URL")
	}
	return u, nil
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

func checkScope(scope string) error {
	if strings.ContainsFunc(scope, unicode.IsSpace) {
		return errors.New("must be one scope, with no white space")
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
