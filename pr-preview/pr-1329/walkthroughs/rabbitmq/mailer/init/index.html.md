# Init

## The settings

Everything `Init` reads, in one struct:

```go
type settings struct {
	Consumer          rabbitmq.Settings
	Publisher         rabbitmq.PublisherSettings
	Login             string
	TrustedPublishers []string
	MaxAttempts       int
	Provider          provider.Settings
	Probes            httphealth.Settings
}
```

`config.Struct` sources and checks each one. A bad one makes `Init` fail,
and the mailer exits 78, naming its variable:

```go
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := readFile(s.Host.FS)

	read, err := config.Struct(func(r *config.Resolver) settings {
		consumer := rabbitmq.DefaultSettings()
		conn := consumer.Connection
		conn.Addr = r.Get(env("RABBITMQ_ADDR").Validate(rabbitmq.CheckAddr))
		conn.Vhost = r.Get(env("RABBITMQ_VHOST").Default(conn.Vhost).Validate(rabbitmq.CheckVhost))
		conn.TLS = r.Get(env("RABBITMQ_CA_FILE").Parse(file).Parse(certPool).Map(trusting).Default(nil))
		login := r.Get(env("RABBITMQ_USERNAME").Validate(notEmpty))
		conn.SASL = &amqp.PlainAuth{
			Username: login,
			Password: r.Get(env("RABBITMQ_PASSWORD_FILE").Parse(file).Parse(secret)),
		}
		consumer.Connection = conn
		consumer.Queue = mail.SendQueue
		consumer.Delivery = queue.AtLeastOnce
		consumer.Prefetch = r.Get(env("RABBITMQ_PREFETCH").Parse(strconv.Atoi).Default(defaultPrefetch).Validate(rabbitmq.CheckPrefetch))
		consumer.Concurrency = r.Get(env("RABBITMQ_CONCURRENCY").Parse(strconv.Atoi).Default(defaultConcurrency).Validate(rabbitmq.CheckConcurrency))

		publisher := rabbitmq.DefaultPublisherSettings()
		publisher.Connection = conn

		client := httpclient.DefaultSettings()
		return settings{
			Consumer:          consumer,
			Publisher:         publisher,
			Login:             login,
			TrustedPublishers: r.Get(env("MAIL_TRUSTED_PUBLISHERS").Parse(logins).Default(nil)),
			MaxAttempts:       r.Get(env("MAIL_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(defaultMaxAttempts).Validate(positive)),
			Provider: provider.Settings{
				URL:            r.Get(env("PROVIDER_URL").Parse(url.Parse).Validate(httpclient.CheckBaseURL)),
				CA:             r.Get(env("PROVIDER_CA_FILE").Parse(file).Parse(certPool).Default(nil)),
				Timeout:        r.Get(env("PROVIDER_TIMEOUT").Parse(time.ParseDuration).Default(client.Timeout).Validate(httpclient.CheckTimeout)),
				AttemptTimeout: r.Get(env("PROVIDER_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(client.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout)),
				MaxAttempts:    r.Get(env("PROVIDER_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(client.MaxAttempts).Validate(httpclient.CheckMaxAttempts)),
				From:           r.Get(env("MAIL_FROM").Validate(notEmpty)),
				APIKey:         r.Get(env("PROVIDER_API_KEY_FILE").Parse(file).Parse(secret)),
			},
			Probes: httphealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
				TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(pemTLS(s.Host.FS)).Validate(httphealth.CheckTLS)),
			},
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}
```

The consumer and every Publisher share one `Connection`, so they log in as
one user.

```go
const (
	defaultPrefetch    = 16
	defaultConcurrency = 8
	defaultMaxAttempts = 5
)
```

| Variable | What it is | Default |
|---|---|---|
| `RABBITMQ_ADDR` | the broker's `host:port` | required |
| `RABBITMQ_USERNAME`, `RABBITMQ_PASSWORD_FILE` | the mailer's login, and a file holding its password | required |
| `RABBITMQ_VHOST` | the vhost | `/` |
| `RABBITMQ_CA_FILE` | the CA certificates the broker is trusted through, PEM | plain text |
| `RABBITMQ_PREFETCH`, `RABBITMQ_CONCURRENCY` | deliveries unacked, and processed at once | 16 and 8 |
| `MAIL_FROM` | the sender's address | required |
| `MAIL_MAX_ATTEMPTS` | attempts before a request is dead-lettered | 5 |
| `MAIL_TRUSTED_PUBLISHERS` | the logins whose scenarios are trusted, separated by commas | none |
| `PROVIDER_URL`, `PROVIDER_API_KEY_FILE` | the provider's base URL, and a file holding its API key | required |
| `PROVIDER_CA_FILE` | the CA certificates the provider is trusted through, PEM | the system's roots |
| `PROVIDER_TIMEOUT`, `PROVIDER_ATTEMPT_TIMEOUT`, `PROVIDER_MAX_ATTEMPTS` | `httpclient.Settings`' fields | `httpclient.DefaultSettings()`' |
| `PROBES_ADDR`, `PROBES_TLS_PEM_FILE` | the probes' address, and a file holding their certificate and key, PEM | required |
| `FAULT_MAX_DELAY` | the longest delay a scenario injects | 10s |

The concurrency can't be more than the prefetch, since a worker without an
unacked delivery has nothing to process:

```go
if read.Consumer.Concurrency > read.Consumer.Prefetch {
	return settings{}, &config.Error{
		Name: "RABBITMQ_CONCURRENCY",
		Err:  fmt.Errorf("must be at most RABBITMQ_PREFETCH, %d", read.Consumer.Prefetch),
	}
}
```

## The secrets, as files

The password and the API key are files mounted from Secrets, read through
the Host's `FS`, so the tests can give the mailer files of their own:

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

func secret(b []byte) (string, error) {
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("the file is empty")
	}
	return s, nil
}
```

Its error doesn't quote the file, so no secret reaches a log.

The CA files become roots, and the broker's a `tls.Config`:

```go
func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}

func trusting(roots *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: roots}
}
```

## The other checks

```go
func logins(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var users []string
	for user := range strings.SplitSeq(s, ",") {
		user = strings.TrimSpace(user)
		if user == "" {
			return nil, errors.New("must be RabbitMQ users separated by commas, with none empty")
		}
		users = append(users, user)
	}
	return users, nil
}
```

```go
func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func positive(n int) error {
	if n < 1 {
		return errors.New("must be positive")
	}
	return nil
}
```

## `TrustScenario`

A delivery's failure-injection scenario is trusted when its `UserID` is the
mailer's own login, or in `MAIL_TRUSTED_PUBLISHERS`. RabbitMQ validates a
`UserID`, so no publisher can claim another's:

```go
read.Consumer.TrustScenario = trustedBy(append([]string{read.Login}, read.TrustedPublishers...))
return read, nil
```

```go
func trustedBy(users []string) func(m rabbitmq.Message) bool {
	trusted := make(map[string]bool, len(users))
	for _, user := range users {
		trusted[user] = true
	}
	return func(m rabbitmq.Message) bool {
		return trusted[m.UserID]
	}
}
```

With the default, production trusts no publisher but the mailer.

## The Publishers and `UserID`

`Init` reads the settings, then builds three Publishers. Only the retries'
forwards the scenario, since a parked request comes back to the mailer, and
the events' and the dead letters' consumers are other teams':

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	retrySettings := settings.Publisher
	retrySettings.ForwardScenario = true
	retries, err := rabbitmq.NewPublisher(s, retrySettings)
	if err != nil {
		return nil, err
	}
	results, err := rabbitmq.NewPublisher(s, settings.Publisher)
	if err != nil {
		return nil, err
	}
	deadLetters, err := rabbitmq.NewPublisher(s, settings.Publisher)
	if err != nil {
		return nil, err
	}
```

A parked request carries the mailer's login as its `UserID`, so its
scenario is still trusted when it comes back:

```go
p.UserID = w.user
```

## The worker and the Service

The provider's client and the worker, then the Service that consumes
`mail.send`:

```go
client, err := provider.New(s, settings.Provider)
if err != nil {
	return nil, err
}
w, err := worker.New(worker.Settings{
	Provider:    client,
	Retries:     retries,
	Results:     results,
	DeadLetters: deadLetters,
	MaxAttempts: settings.MaxAttempts,
	User:        settings.Login,
	Telemetry:   s.Telemetry,
})
if err != nil {
	return nil, err
}

svc, err := rabbitmq.NewService(s, settings.Consumer, w)
if err != nil {
	return nil, err
}
```

## The probes

An `httphealth` server, ready while the Service is consuming. It has no
liveness monitor, since the Service reconnects on its own:

```go
	probes, err := httphealth.NewService(s, settings.Probes, health.Readiness(svc.Consuming()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, probes}, nil
}
```

Its certificate and key are one PEM file, read again when it changes, so a
renewal needs no restart:

```go
func pemTLS(fsys humus.FS) func(name string) (*tls.Config, error) {
	return func(name string) (*tls.Config, error) {
		return certfile.Config(fsys, name, name)
	}
}
```

```go
func Config(fsys humus.FS, certFile, keyFile string) (*tls.Config, error) {
	f := &files{fs: fsys, certFile: certFile, keyFile: keyFile}
	if _, err := f.certificate(); err != nil {
		return nil, err
	}
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return f.certificate()
		},
	}, nil
}
```

```go
func (f *files) certificate() (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	certPEM, err := readFile(f.fs, f.certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readFile(f.fs, f.keyFile)
	if err != nil {
		return nil, err
	}
	if f.cert != nil && bytes.Equal(certPEM, f.certPEM) && bytes.Equal(keyPEM, f.keyPEM) {
		return f.cert, nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate %s and key %s: %w", f.certFile, f.keyFile, err)
	}
	f.certPEM, f.keyPEM, f.cert = certPEM, keyPEM, &cert
	return f.cert, nil
}
```

## `BuildTelemetry`

The telemetry `Builder` names the service `mailer`, and sources the OTLP
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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("mailer").Validate(telemetry.CheckServiceName))
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

The App's `fault.Injector` decodes scenarios with `jsonscenario`, under the
member `acme.fault`, and caps a delay at `FAULT_MAX_DELAY`:

```go
const scenarioKey = "acme.fault"

func Faults(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
	codec, err := jsonscenario.New(scenarioKey)
	if err != nil {
		return fault.Settings{}, err
	}
	settings := fault.DefaultSettings()
	settings.Decoder = codec
	settings.MaxDelay, err = config.Env(s.Process.Environment, "FAULT_MAX_DELAY").
		Parse(time.ParseDuration).
		Default(settings.MaxDelay).
		Validate(fault.CheckMaxDelay).
		Read(ctx)
	if err != nil {
		return fault.Settings{}, err
	}
	return settings, nil
}
```

## `main` and its grace period

`main` passes both to `humus.Main`, with a grace period of 25s. A delivery
in flight at SIGTERM may need the provider's `Timeout` and a publish's, 10s
each, which `humus.Main`'s default of 20s would cut short:

```go
func main() {
	humus.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults), humus.WithShutdownGracePeriod(25*time.Second))
}
```

The Deployment's `terminationGracePeriodSeconds` covers the grace period,
the telemetry's 5s shutdown, and a margin:

```yaml
terminationGracePeriodSeconds: 35
```

Next: [Testing](/humus/pr-preview/pr-1329/walkthroughs/rabbitmq/mailer/testing/)
