# Init

## The cart's settings

Everything the cart's `Init` reads, in one struct:

```go
type settings struct {
	HTTP            httpserver.Settings
	Database        *pgxpool.Config
	Catalog         httpclient.Settings
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
and the cart exits 78, naming its variable:

```go
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
```

The catalog's client comes next, as
[Calling the catalog](/humus/pr-preview/pr-1272/walkthroughs/shoppingcart/calling-the-catalog/#the-clients-settings)
shows, then the tokens' settings and the server's:

```go
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
	JWT:             tokens,
	FaultTrustScope: r.Get(env("FAULT_TRUST_SCOPE").Default("").Validate(checkScope)),
}
```

```go
type serverCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error)
```

The certificate's file is checked on its own before the pair is read, so
a file that holds no certificate is an error that names `TLS_CERT_FILE`,
not `TLS_KEY_FILE`.

## Every variable the cart reads

| Variable | What it is | Default |
|---|---|---|
| `ADDR` | the address it serves on | required |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | files holding its certificate and its key, PEM | required |
| `HTTP2_ONLY` | whether it serves HTTP/2 only | false |
| `READ_HEADER_TIMEOUT`, `READ_TIMEOUT`, `WRITE_TIMEOUT`, `IDLE_TIMEOUT`, `MAX_HEADER_BYTES` | `httpserver.Settings`' fields | `httpserver.DefaultSettings()`' |
| `DATABASE_URL_FILE` | a file holding its PostgreSQL URL | required |
| `CATALOG_URL` | the catalog's base URL, `https` | required |
| `CATALOG_CA_FILE` | the CA certificates the catalog is trusted through, PEM | required |
| `CATALOG_CLIENT_CERT_FILE`, `CATALOG_CLIENT_KEY_FILE` | files holding the client certificate it presents to the catalog and its key, PEM | required |
| `CATALOG_TIMEOUT`, `CATALOG_ATTEMPT_TIMEOUT`, `CATALOG_MAX_ATTEMPTS` | `httpclient.Settings`' fields, for the catalog | `httpclient.DefaultSettings()`' |
| `JWKS_URL` | the key set's URL, absolute and `https` | required |
| `JWKS_CA_FILE` | the CA certificates the key set is trusted through, PEM | the system's roots |
| `JWKS_TIMEOUT`, `JWKS_ATTEMPT_TIMEOUT`, `JWKS_MAX_ATTEMPTS` | `httpclient.Settings`' fields, for the key set | `httpclient.DefaultSettings()`' |
| `JWT_ISSUER`, `JWT_AUDIENCE` | the issuer and the audience a token must have | required |
| `JWT_ALGORITHMS` | the algorithms a token may be signed with, separated by commas | `RS256,PS256,ES256,EdDSA` |
| `JWT_LEEWAY`, `JWT_MIN_REFRESH_INTERVAL`, `JWT_MAX_KEY_AGE` | `jwt.Settings`' fields | 1m, 1m and 1h |
| `FAULT_TRUST_SCOPE` | the scope a token needs for its scenario to be trusted | none |
| `FAULT_MAX_DELAY` | the longest delay a scenario injects | 10s |
| `OTEL_SERVICE_NAME` | the service's name | `cart` |
| `OTEL_SDK_DISABLED`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT` | `telemetry.Settings`' fields | `telemetry.DefaultSettings()`' |

## Every variable the catalog reads

The catalog's `readSettings` is the cart's without the catalog's client
and `FAULT_TRUST_SCOPE`, and with the CA it verifies client certificates
against:

| Variable | What it is | Default |
|---|---|---|
| `ADDR`, `TLS_CERT_FILE`, `TLS_KEY_FILE`, `DATABASE_URL_FILE` | as the cart's | required |
| `CLIENT_CA_FILE` | the CA certificates a client certificate is verified against, PEM | required |
| `HTTP2_ONLY`, `READ_HEADER_TIMEOUT`, `READ_TIMEOUT`, `WRITE_TIMEOUT`, `IDLE_TIMEOUT`, `MAX_HEADER_BYTES` | as the cart's | as the cart's |
| `JWKS_URL`, `JWT_ISSUER`, `JWT_AUDIENCE` | as the cart's | required |
| `JWKS_CA_FILE`, `JWKS_TIMEOUT`, `JWKS_ATTEMPT_TIMEOUT`, `JWKS_MAX_ATTEMPTS`, `JWT_ALGORITHMS`, `JWT_LEEWAY`, `JWT_MIN_REFRESH_INTERVAL`, `JWT_MAX_KEY_AGE` | as the cart's | as the cart's |
| `FAULT_MAX_DELAY` | as the cart's | 10s |
| `OTEL_SERVICE_NAME` | the service's name | `catalog` |
| `OTEL_SDK_DISABLED`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT` | as the cart's | as the cart's |

## The other checks

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

func checkScope(scope string) error {
	if strings.ContainsFunc(scope, unicode.IsSpace) {
		return errors.New("must be one scope, with no white space")
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
```

## The secrets, as files

The database's URL, with its password, is a file mounted from a Secret, and
so are the certificates and their keys. Each is read through the Host's
`FS`, so the tests can give the service files of their own:

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
func databaseURL(b []byte) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(string(bytes.TrimSpace(b)))
	if err != nil {
		return nil, errors.New("must hold a PostgreSQL URL")
	}
	return cfg, nil
}
```

Its error quotes nothing of the file, so no password reaches a log.

```go
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

`internal/certfile` serves a certificate, and presents a client
certificate, from its files:

```go
const interval = 30 * time.Second

func Config(fsys humus.FS, certFile, keyFile string) (*tls.Config, error) {
	f := &files{fs: fsys, certFile: certFile, keyFile: keyFile, checked: time.Now()}
	p, err := f.read(nil)
	if err != nil {
		return nil, err
	}
	f.last = p
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return f.certificate(), nil
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return f.certificate(), nil
		},
	}, nil
}
```

A handshake reads the files again at most every 30s, and a pair that
doesn't parse keeps the last good one, so a renewal needs no restart and a
half-written renewal breaks nothing:

```go
func (f *files) certificate() *tls.Certificate {
	f.mu.Lock()
	last := f.last
	if time.Since(f.checked) < interval {
		f.mu.Unlock()
		return last.cert
	}
	f.checked = time.Now()
	f.mu.Unlock()

	p, err := f.read(last)
	if err != nil {
		return last.cert
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = p
	return p.cert
}
```

```go
func (f *files) read(last *pair) (*pair, error) {
	certPEM, err := readFile(f.fs, f.certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readFile(f.fs, f.keyFile)
	if err != nil {
		return nil, err
	}
	if last != nil && bytes.Equal(certPEM, last.certPEM) && bytes.Equal(keyPEM, last.keyPEM) {
		return last, nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate %s and key %s: %w", f.certFile, f.keyFile, err)
	}
	return &pair{certPEM: certPEM, keyPEM: keyPEM, cert: &cert}, nil
}
```

## The pool, through the Host

The pool dials through the Host, and leaves the host unresolved so the
Host's `Dial` resolves it too. `OnStop` closes it:

```go
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
	return database.New(pool, s.Faults), pool.Ping, nil
}
```

What pgx still reads from the real process is in
[pgx's ambient leaks](/humus/pr-preview/pr-1272/openapi/#pgxs-ambient-leaks). The URL
sets every field the service uses, so nothing is left for pgx to find.

## `Init`

`Init` passes the functions that open the store and the catalog to
`initWith`, which the tests call with ones of their own:

```go
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
```

Then it verifies tokens and builds the API, as
[Who may do what](/humus/pr-preview/pr-1272/walkthroughs/shoppingcart/access/#verifying-tokens)
and [The cart's API](/humus/pr-preview/pr-1272/walkthroughs/shoppingcart/api/#the-api)
show. The catalog's `Init` is the same, with `InitWith` and no catalog to
open.

## Readiness

The probes are attached to the cart's own port, at `/health/`, beside the
API rather than behind its tokens. Its readiness is both dependencies,
each reached once:

```go
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
```

Both latch, as the
[catalog's](/humus/pr-preview/pr-1272/walkthroughs/shoppingcart/catalog/#the-probes)
does:

- a release that can't reach a dependency never turns ready, so its
  rollout stops at its first pod while the old pods keep serving;
- an outage after that keeps every pod ready, so callers get the cart's own
  503, and reads that don't need the catalog still work.

## `TrustScenario`

A request's failure-injection scenario is trusted when its bearer token
verifies and its scopes hold `FAULT_TRUST_SCOPE`. With no scope set,
production trusts no one:

```go
if settings.FaultTrustScope != "" {
	settings.HTTP.TrustScenario = trustTokens(s.Faults, tokens.Verify, settings.FaultTrustScope)
}
```

```go
func trustTokens(faults fault.Injector, verify func(context.Context, string) (endpoint.Claims, error), scope string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return false
		}
		ctx, _ := faults.Strip(r.Context())
		claims, err := verify(ctx, strings.TrimSpace(token))
		return err == nil && claims.HasScope(scope)
	}
}
```

The token is verified with the scenario stripped, since the scenario isn't
trusted yet. The catalog trusts a verified client certificate instead, as
[The catalog](/humus/pr-preview/pr-1272/walkthroughs/shoppingcart/catalog/#the-scenarios-it-trusts)
shows.

## `BuildTelemetry`

The telemetry `Builder` names the service `cart`, and sources the OTLP
endpoint and the exporters from the standard `OTEL_*` variables. The
catalog's names it `catalog`:

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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("cart").Validate(telemetry.CheckServiceName))
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

## `main`

Each `main` passes its `Init`, `BuildTelemetry` and `Faults` to
`httpserver.Main`. The cart's:

```go
func main() {
	httpserver.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults))
}
```

The catalog's is the same line, with its own `app`,
`example/shoppingcart/catalog/app`.
