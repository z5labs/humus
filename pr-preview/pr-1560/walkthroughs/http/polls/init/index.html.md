# Init

## The settings

Everything `Init` reads, in one struct:

```go
type settings struct {
	HTTP     httpserver.Settings
	Database *pgxpool.Config
	TrustCAs *x509.CertPool
}

type serverCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error)
```

`config.Struct` sources and checks each one. A bad one makes `Init` fail,
and the service exits 78, naming its variable:

```go
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	d := httpserver.DefaultSettings()
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }
	timeout := func(name string, def time.Duration, check func(time.Duration) error) config.Value[time.Duration] {
		return env(name).Parse(time.ParseDuration).Default(def).Validate(check)
	}

	read, err := config.Struct(func(r *config.Resolver) settings {
		server := d
		server.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		server.TLS = &tls.Config{
			GetCertificate: r.Get(env("TLS_CERT_FILE").Validate(holdsCertificate(s.Host.FS)).FlatMap(func(certFile string) config.Value[serverCertificate] {
				return env("TLS_KEY_FILE").Parse(func(keyFile string) (serverCertificate, error) {
					cfg, err := certfile.Config(s.Host.FS, certFile, keyFile)
					if err != nil {
						return nil, err
					}
					return cfg.GetCertificate, nil
				})
			})),
		}
		server.ReadHeaderTimeout = r.Get(timeout("READ_HEADER_TIMEOUT", d.ReadHeaderTimeout, httpserver.CheckReadHeaderTimeout))
		server.ReadTimeout = r.Get(timeout("READ_TIMEOUT", d.ReadTimeout, httpserver.CheckReadTimeout))
		server.WriteTimeout = r.Get(timeout("WRITE_TIMEOUT", d.WriteTimeout, httpserver.CheckWriteTimeout))
		server.IdleTimeout = r.Get(timeout("IDLE_TIMEOUT", d.IdleTimeout, httpserver.CheckIdleTimeout))
		return settings{
			HTTP:     server,
			Database: r.Get(file("DATABASE_URL_FILE").Parse(databaseURL)),
			TrustCAs: r.Get(env("FAULT_TRUST_CA_FILE").Parse(trustCAs(s.Host.FS)).Default(nil)),
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}
	if read.TrustCAs != nil {
		read.HTTP.TLS.ClientAuth = tls.VerifyClientCertIfGiven
		read.HTTP.TLS.ClientCAs = read.TrustCAs
		read.HTTP.TrustScenario = issuedByATrustedCA
	}
	return read, nil
}
```

The certificate's file is checked on its own before the pair is read, so a
file that holds no certificate is an error that names `TLS_CERT_FILE`, not
`TLS_KEY_FILE`.

## Every variable it reads

| Variable | What it is | Default |
|---|---|---|
| `ADDR` | the address it serves on | required |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | files holding its certificate and its key, PEM | required |
| `READ_HEADER_TIMEOUT`, `READ_TIMEOUT`, `WRITE_TIMEOUT`, `IDLE_TIMEOUT` | `httpserver.Settings`' fields | `httpserver.DefaultSettings()`' |
| `DATABASE_URL_FILE` | a file holding its PostgreSQL URL | required |
| `FAULT_TRUST_CA_FILE` | the CA certificates a client certificate is verified against, for a trusted scenario, PEM | none |
| `FAULT_MAX_DELAY` | the longest delay a scenario injects | 10s |
| `OTEL_SERVICE_NAME` | the service's name | `polls` |
| `OTEL_SDK_DISABLED`, `OTEL_TRACES_EXPORTER`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_EXPORTER_OTLP_ENDPOINT` | `telemetry.Settings`' fields | `telemetry.DefaultSettings()`' |

The base's ConfigMap sets them:

```yaml
configMapGenerator:
  - name: polls
    literals:
      - ADDR=:8443
      - TLS_CERT_FILE=/etc/polls/tls/tls.crt
      - TLS_KEY_FILE=/etc/polls/tls/tls.key
      - DATABASE_URL_FILE=/etc/polls/database/url
      - OTEL_SERVICE_NAME=polls
      - OTEL_TRACES_EXPORTER=otlp
      - OTEL_METRICS_EXPORTER=otlp
      - OTEL_LOGS_EXPORTER=otlp
      - OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317
```

## The secrets and certificates, as files

The database's URL, with its password, is a file mounted from a Secret, and
so are the certificate and its key:

```yaml
volumeMounts:
  - name: tls
    mountPath: /etc/polls/tls
    readOnly: true
  - name: database
    mountPath: /etc/polls/database
    readOnly: true
```

Each is read through the Host's `FS`, so the tests can give the service
files of their own:

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
func trustCAs(fsys humus.FS) func(name string) (*x509.CertPool, error) {
	return func(name string) (*x509.CertPool, error) {
		if name == "" {
			return nil, nil
		}
		b, err := readFile(fsys)(name)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(b) {
			return nil, errors.New("no PEM certificate in the file")
		}
		return roots, nil
	}
}
```

## The certificate, read again when renewed

`internal/certfile` serves the certificate from its files:

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
half-written renewal breaks nothing. A read that finishes after a later one
began doesn't replace it:

```go
func (f *files) certificate() *tls.Certificate {
	f.mu.Lock()
	last := f.last
	if time.Since(f.checked) < interval {
		f.mu.Unlock()
		return last.cert
	}
	checked := time.Now()
	f.checked = checked
	f.mu.Unlock()

	p, err := f.read(last)
	if err != nil {
		return last.cert
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checked.Equal(checked) {
		f.last = p
	}
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

## The store and the listener, through the Host

`openPostgres` opens the pool and the listener. Both dial through the
Host, and leave the host unresolved so the Host's `Dial` resolves it too.
The listener gets a copy of the pool's connection settings, and `OnStop`
closes the pool:

```go
type backend struct {
	store     ui.Store
	results   ui.Results
	listen    func(context.Context)
	reached   func(context.Context) error
	listening func(context.Context) error
}

type openBackend func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (backend, error)
```

```go
func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (backend, error) {
	cfg.ConnConfig.DialFunc = s.Host.Dial
	cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return backend{}, err
	}
	s.OnStop(func(context.Context) error {
		pool.Close()
		return nil
	})
	polls := store.New(pool, s.Faults)
	l := store.NewListener(cfg.ConnConfig.Copy(), polls, s.Telemetry.LoggerProvider.Logger(loggerName))
	return backend{
		store:     polls,
		results:   l,
		listen:    l.Run,
		reached:   pool.Ping,
		listening: l.Listening,
	}, nil
}
```

```go
const loggerName = "github.com/z5labs/humus/example/http/polls"
```

## The listener's Service

The listener runs as a `humus.Service` of its own, beside the server.
`Serve` runs it until `Shutdown` cancels it, and `Shutdown` waits for it
to return. A `Serve` after `Shutdown` returns at once:

```go
type listener struct {
	run  func(context.Context)
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}

	mu      sync.Mutex
	serving bool
}

func newListener(run func(context.Context)) *listener {
	ctx, stop := context.WithCancel(context.Background())
	return &listener{run: run, ctx: ctx, stop: stop, done: make(chan struct{})}
}

func (l *listener) Serve(context.Context) error {
	l.mu.Lock()
	if l.serving || l.ctx.Err() != nil {
		l.mu.Unlock()
		return nil
	}
	l.serving = true
	l.mu.Unlock()

	defer close(l.done)
	l.run(l.ctx)
	return nil
}

func (l *listener) Shutdown(ctx context.Context) error {
	l.mu.Lock()
	l.stop()
	serving := l.serving
	l.mu.Unlock()
	if !serving {
		return nil
	}
	select {
	case <-l.done:
		return nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("stopping the listener: %w", err)
	}
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stopping the listener: %w", ctx.Err())
	}
}
```

## `Init`

`Init` passes `openPostgres` to `initWith`, which the tests call with a
backend of their own:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres)
}
```

`initWith` reads the settings, opens the backend, builds the
[Site](/humus/pr-preview/pr-1560/walkthroughs/http/polls/site/) with the server's
`WriteTimeout` for its feeds, and serves it beside the probes:

```go
func initWith(ctx context.Context, s *humus.Setup, open openBackend) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	b, err := open(ctx, s, settings.Database)
	if err != nil {
		return nil, err
	}

	u, err := ui.New(b.store, b.results, settings.HTTP.WriteTimeout)
	if err != nil {
		return nil, err
	}
	site, err := u.Site(s)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/health/", httphealth.NewHandler(s, health.Probes{
		Liveness:  health.And(),
		Readiness: health.And(latch.New(b.reached), latch.New(b.listening)),
	}))
	mux.Handle("/", site)

	svc, err := httpserver.NewService(s, settings.HTTP, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{newListener(b.listen), svc}, nil
}
```

## Readiness

The probes are on the service's own port, at `/health/`, beside the Site.
A pod is ready once it has reached PostgreSQL and its listener has listened,
each once:

```go
func New(check func(ctx context.Context) error) *Monitor {
	return &Monitor{check: check, turn: make(chan struct{}, 1)}
}

func (m *Monitor) Healthy(ctx context.Context) (bool, error) {
	if m.reached.Load() {
		return true, nil
	}
	select {
	case m.turn <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-m.turn }()
	if m.reached.Load() {
		return true, nil
	}
	if err := m.check(ctx); err != nil {
		return false, err
	}
	m.reached.Store(true)
	return true, nil
}
```

Both latch:

- a release that can't reach PostgreSQL, or can't listen, never turns
  ready, so its rollout stops at its first pod while the old pods keep
  serving;
- an outage after that keeps every pod ready, so a browser gets the 503
  page, and the listener catches its feeds up once it listens again.

## `TrustScenario`

With `FAULT_TRUST_CA_FILE` set, the server asks for a client certificate,
and a request's failure-injection scenario is trusted when it presented one
issued by those CAs. With none set, production trusts no one:

```go
func issuedByATrustedCA(r *http.Request) bool {
	return r.TLS != nil && len(r.TLS.VerifiedChains) > 0
}
```

## `BuildTelemetry`

The telemetry `Builder` names the service `polls`, and sources the OTLP
endpoint and the exporters from the standard `OTEL_*` variables:

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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("polls").Validate(telemetry.CheckServiceName))
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

It decodes scenarios with `jsonscenario`, under the member `acme.fault`,
and caps a delay at `FAULT_MAX_DELAY`:

```go
const scenarioKey = "acme.fault"
```

```go
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

`main` passes `Init`, `BuildTelemetry` and `Faults` to `httpserver.Main`:

```go
func main() {
	httpserver.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults))
}
```

Next: [Wallet](/humus/pr-preview/pr-1560/walkthroughs/grpc/wallet/)
