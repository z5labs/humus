# Init

## The wallet's settings

Everything the wallet's `Init` reads, in one struct:

```go
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
```

`config.Struct` sources and checks each one. A bad one makes `Init` fail,
and the wallet exits 78, naming its variable:

```go
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }
	pemFile := func(name string) config.Value[*tls.Config] { return env(name).Parse(pemTLS(s.Host.FS)) }
```

The ledger's Client comes first, as
[Calling the ledger](/humus/pr-preview/pr-1289/walkthroughs/wallet/calling-the-ledger/#the-clients-settings)
shows, then the key set's, as
[Serving the wallet](/humus/pr-preview/pr-1289/walkthroughs/wallet/serving-the-wallet/#the-fetched-key-set)
shows, then the tokens' settings and the server's:

```go
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
```

The key set's URL is an absolute `https` URL:

```go
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
```

## Every variable the wallet reads

| Variable | What it is | Default |
|---|---|---|
| `GRPC_ADDR` | the address it serves on | required |
| `GRPC_TLS_PEM_FILE` | a file holding its certificate chain and its key, PEM | required |
| `PROBES_ADDR` | the gRPC health port, in plain text | required |
| `LEDGER_ADDR` | the ledger, `host:port` | required |
| `LEDGER_CA_FILE` | the CA certificates the ledger is trusted through, PEM | required |
| `LEDGER_TLS_PEM_FILE` | a file holding its client certificate for the ledger and its key, PEM | required |
| `LEDGER_TIMEOUT`, `LEDGER_MAX_ATTEMPTS` | `grpcclient.Settings`' fields | `grpcclient.DefaultSettings()`' |
| `TOKEN_KEYS_URL` | the JSON Web Key Set that tokens are verified with, an absolute `https` URL | required |
| `TOKEN_KEYS_CA_FILE` | the CA certificates the key set's server is trusted through, PEM | required |
| `TOKEN_KEYS_TIMEOUT`, `TOKEN_KEYS_MAX_ATTEMPTS` | `httpclient.Settings`' fields | `httpclient.DefaultSettings()`' |
| `TOKEN_ISSUER`, `TOKEN_AUDIENCE` | the issuer and the audience a token must have | required |
| `FAULT_TRUST_SCOPE` | the scope a token needs for its scenario to be trusted | none |
| `FAULT_MAX_DELAY` | the longest delay a scenario injects | 10s |
| `OTEL_SERVICE_NAME` | the service's name | `wallet` |
| `OTEL_SDK_DISABLED`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT` | `telemetry.Settings`' fields | `telemetry.DefaultSettings()`' |

## The ledger's settings

The ledger's settings, and what it sources to build them:

```go
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
```

```go
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
```

Then it builds the server's TLS, as
[Serving the ledger](/humus/pr-preview/pr-1289/walkthroughs/wallet/serving-the-ledger/#mtls)
shows, and the pool's config.

## Every variable the ledger reads

| Variable | What it is | Default |
|---|---|---|
| `GRPC_ADDR`, `GRPC_TLS_PEM_FILE`, `PROBES_ADDR` | as the wallet's | required |
| `GRPC_CLIENT_CA_FILE` | the CA certificates its callers' certificates chain to, PEM | required |
| `POSTGRES_URL` | PostgreSQL's URL, without a password or TLS parameters | required |
| `POSTGRES_PASSWORD_FILE` | a file holding PostgreSQL's password | required |
| `POSTGRES_TLS_CA_FILE` | the CA certificates PostgreSQL's certificate chains to, PEM | required |
| `WATCH_INTERVAL` | how often a watch reads its account | 1s |
| `FAULT_MAX_DELAY` | as the wallet's | 10s |
| `OTEL_SERVICE_NAME` | the service's name | `ledger` |
| `OTEL_SDK_DISABLED`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT` | as the wallet's | as the wallet's |

## PostgreSQL's URL

The URL may hold no password and no TLS parameter, since `Init` sets them:

```go
var postgresParams = []string{"password", "passfile", "service", "servicefile", "sslmode", "sslrootcert", "sslcert", "sslkey", "sslpassword"}
```

```go
q := u.Query()
for _, p := range postgresParams {
	if q.Has(p) {
		errs = append(errs, fmt.Errorf("must not set %s: Init sets it", p))
	}
}
return errors.Join(errs...)
```

`poolConfig` adds the password from its file, verifies PostgreSQL's
certificate against the CA, and sets every TLS parameter, so pgx reads
nothing from the real environment or `~/.pgpass`:

```go
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
```

Its error quotes nothing of the URL, so no password reaches a log.

## The pool, through the Host

The pool dials through the Host, and leaves the host unresolved so the
Host's `Dial` resolves it too. `OnStop` closes it:

```go
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
```

## The secrets, as files

PostgreSQL's password, the private keys and the CAs are files mounted from
Secrets, each read through the Host's `FS`, so the tests can give the
service files of their own:

```go
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
```

```go
func secret(b []byte) (string, error) {
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("the file is empty")
	}
	return s, nil
}
```

```go
func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}
```

## The certificates, read again when renewed

Each certificate and its key are one PEM file, read by `internal/certfile`:

```go
func pemTLS(fsys humus.FS) func(name string) (*tls.Config, error) {
	return func(name string) (*tls.Config, error) {
		return certfile.Config(fsys, name)
	}
}
```

`Config` reads the file once, so a bad one fails `Init`. Its `tls.Config`
serves the certificate, and presents it as a client certificate:

```go
func Config(fsys humus.FS, name string) (*tls.Config, error) {
	f := &file{fs: fsys, name: name}
	if _, err := f.certificate(); err != nil {
		return nil, err
	}
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return f.certificate()
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return f.certificate()
		},
	}, nil
}
```

A handshake reads the file again, and parses it only when it changed, so a
renewal needs no restart:

```go
func (f *file) certificate() (*tls.Certificate, error) {
	data, err := readFile(f.fs, f.name)
	if err != nil {
		return nil, err
	}
	if cert := f.cached(data); cert != nil {
		return cert, nil
	}

	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, fmt.Errorf("certificate and key %s: %w", f.name, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.pem, f.cert = data, &cert
	return &cert, nil
}
```

## `MaxConnectionAge`

Both servers close every connection with a GOAWAY once it is about a minute
old, so each caller dials again through the Kubernetes Service and reaches
whichever pod it picks:

```go
svc, err := grpcserver.NewService(s, set.GRPC,
	unary,
	stream,
	grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: maxConnectionAge}),
)
if err != nil {
	return nil, err
}
```

```go
const maxConnectionAge = time.Minute
```

No `MaxConnectionAgeGrace` is set, because a grace would end every open
watch each time it ran out. A watch keeps its old connection until it ends,
so watches move only at a rollout.

## Readiness

Each service's probes are `grpchealth.NewService`, on `PROBES_ADDR`. The
ledger is ready once it has reached PostgreSQL, as
[Serving the ledger](/humus/pr-preview/pr-1289/walkthroughs/wallet/serving-the-ledger/#the-probes)
shows:

```go
probes, err := grpchealth.NewService(s, set.Probes, health.Readiness(newReached(ping)))
```

The wallet is ready once its Client has connected to the ledger, as
[Calling the ledger](/humus/pr-preview/pr-1289/walkthroughs/wallet/calling-the-ledger/#readiness-connected)
shows:

```go
probes, err := grpchealth.NewService(s, set.Probes, health.Readiness(ledger.Connected()))
```

Both latch.

## `TrustScenario`

The wallet trusts a call's failure-injection scenario when its bearer
token verifies and its scopes hold `FAULT_TRUST_SCOPE`. With no scope set,
production trusts no one:

```go
if set.FaultTrustScope != "" {
	set.GRPC.TrustScenario = trustTokens(s.Faults, tokens.Verify, set.FaultTrustScope)
}
```

```go
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
```

The token is verified with the scenario stripped, since the scenario isn't
trusted yet.

The ledger trusts a caller whose client certificate the internal CA issued:
the wallet, which forwards its scenarios.

```go
func issuedByTheCA(ctx context.Context, _ string) bool {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	return ok && len(info.State.VerifiedChains) > 0
}
```

```go
TrustScenario: issuedByTheCA,
```

## `BuildTelemetry`

The telemetry `Builder` names the service `wallet`, and sources the OTLP
endpoint and the exporters from the standard `OTEL_*` variables. The
ledger's names it `ledger`:

```go
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
```

## `Faults`

Both services decode scenarios with `jsonscenario`, under the member
`acme.fault`, and cap a delay at `FAULT_MAX_DELAY`:

```go
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
```

## `Init`

The ledger's `Init` passes the function that opens the Store to `InitWith`,
which the tests call with one of their own:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return InitWith(ctx, s, openPostgres)
}

type OpenStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (st server.Store, ping func(context.Context) error, err error)
```

```go
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
```

Then it builds `grpcauth`'s interceptors, the Service and the probes, and
returns the Service and the probes. The wallet's `Init` reads its settings,
builds the ledger's Client, the key set and the Verifier, then the same.

## `main`

Each `main` passes its `Init`, `BuildTelemetry` and `Faults` to
`grpcserver.Main`. The wallet's:

```go
func main() {
	grpcserver.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults))
}
```

The ledger's is the same line, with its own `app`,
`example/wallet/ledger/app`.
