# Init

## The settings

Everything `Init` reads, in one struct:

```go
type settings struct {
	Consumer  kafka.Settings
	Producer  kafka.ProducerSettings
	Projector projector.Settings
	Database  *pgxpool.Config
	Probes    httphealth.Settings
}
```

`config.Struct` sources and checks each one, with what it needs to build the
rest. A bad one makes `Init` fail, and the processor exits 78, naming its
variable:

```go
type postgres struct {
	URL      *url.URL
	Password string
	CA       *x509.CertPool
}

type sourced struct {
	settings
	KafkaCA       *x509.CertPool
	KafkaTLS      *tls.Config
	Postgres      postgres
	TrustedTopics []string
}
```

```go
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }
	list := func(v string) []string { return strings.Split(v, ",") }

	got, err := config.Struct(func(r *config.Resolver) sourced {
		consumer := kafka.DefaultSettings()
		consumer.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		consumer.GroupID = r.Get(env("KAFKA_GROUP_ID").Validate(kafka.CheckGroupID))
		consumer.Delivery = queue.AtLeastOnce
		consumer.SessionTimeout = r.Get(env("KAFKA_SESSION_TIMEOUT").Parse(time.ParseDuration).Default(consumer.SessionTimeout).Validate(kafka.CheckSessionTimeout))

		producer := kafka.DefaultProducerSettings()
		producer.DeliveryTimeout = r.Get(env("KAFKA_DELIVERY_TIMEOUT").Parse(time.ParseDuration).Default(producer.DeliveryTimeout).Validate(kafka.CheckDeliveryTimeout))
		producer.Breaker.OpenFor = r.Get(env("KAFKA_BREAKER_OPEN_FOR").Parse(time.ParseDuration).Default(producer.Breaker.OpenFor).Validate(kafka.CheckBreakerOpenFor))

		return sourced{
			settings: settings{
				Consumer: consumer,
				Producer: producer,
				Projector: projector.Settings{
					EventsTopic:     r.Get(env("KAFKA_EVENTS_TOPIC").Validate(notEmpty)),
					StatusTopic:     r.Get(env("KAFKA_STATUS_TOPIC").Validate(notEmpty)),
					RetryTopic:      r.Get(env("KAFKA_RETRY_TOPIC").Validate(notEmpty)),
					DeadLetterTopic: r.Get(env("KAFKA_DEAD_LETTER_TOPIC").Validate(notEmpty)),
					Attempts:        r.Get(env("RETRY_ATTEMPTS").Parse(strconv.Atoi).Default(defaultRetryAttempts).Validate(positive[int])),
					MinDelay:        r.Get(env("RETRY_MIN_DELAY").Parse(time.ParseDuration).Default(defaultRetryMinDelay).Validate(positive[time.Duration])),
					MaxDelay:        r.Get(env("RETRY_MAX_DELAY").Parse(time.ParseDuration).Default(defaultRetryMaxDelay).Validate(positive[time.Duration])),
				},
				Probes: httphealth.Settings{
					Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
					TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(pemTLS(s.Host.FS)).Validate(httphealth.CheckTLS)),
				},
			},
			KafkaCA:  r.Get(file("KAFKA_TLS_CA_FILE").Parse(certPool)),
			KafkaTLS: r.Get(env("KAFKA_TLS_CERT_FILE").Parse(pemTLS(s.Host.FS))),
			Postgres: postgres{
				URL:      r.Get(env("POSTGRES_URL").Parse(parsePostgresURL).Validate(checkPostgresURL)),
				Password: r.Get(file("POSTGRES_PASSWORD_FILE").Parse(secret)),
				CA:       r.Get(file("POSTGRES_TLS_CA_FILE").Parse(certPool)),
			},
			TrustedTopics: r.Get(env("FAULT_TRUSTED_TOPICS").Parse(topicList).Default(nil)),
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}
```

```go
const (
	defaultRetryAttempts = 10
	defaultRetryMinDelay = time.Second
	defaultRetryMaxDelay = 5 * time.Minute
)
```

| Variable | What it is | Default |
|---|---|---|
| `KAFKA_BROKERS` | the brokers' `host:port`s, separated by commas | required |
| `KAFKA_GROUP_ID` | the consumer group | required |
| `KAFKA_EVENTS_TOPIC`, `KAFKA_STATUS_TOPIC`, `KAFKA_RETRY_TOPIC`, `KAFKA_DEAD_LETTER_TOPIC` | the four topics, each a different one | required |
| `KAFKA_TLS_CA_FILE`, `KAFKA_TLS_CERT_FILE` | the brokers' CA, and a file holding the processor's certificate and key, PEM | required |
| `KAFKA_SESSION_TIMEOUT` | `kafka.Settings`' `SessionTimeout` | `kafka.DefaultSettings()`' |
| `KAFKA_DELIVERY_TIMEOUT`, `KAFKA_BREAKER_OPEN_FOR` | `kafka.ProducerSettings`' `DeliveryTimeout` and `Breaker.OpenFor` | `kafka.DefaultProducerSettings()`' |
| `RETRY_ATTEMPTS` | attempts before a record is dead-lettered | 10 |
| `RETRY_MIN_DELAY`, `RETRY_MAX_DELAY` | the backoff of a record's first retry, and the longest | 1s and 5m |
| `POSTGRES_URL` | PostgreSQL's URL, with its user and database, and no password | required |
| `POSTGRES_PASSWORD_FILE`, `POSTGRES_TLS_CA_FILE` | a file holding the password, and PostgreSQL's CA | required |
| `PROBES_ADDR`, `PROBES_TLS_PEM_FILE` | the probes' address, and a file holding their certificate and key, PEM | required |
| `FAULT_TRUSTED_TOPICS` | the topics whose records' scenarios are trusted, separated by commas | none |
| `FAULT_MAX_DELAY` | the longest delay a scenario injects | 10s |

The four topics must differ, and the backoff can't start above its longest:

```go
if err := checkTopics(got.Projector); err != nil {
	return settings{}, err
}
if got.Projector.MinDelay > got.Projector.MaxDelay {
	return settings{}, &config.Error{
		Name: "RETRY_MIN_DELAY",
		Err:  fmt.Errorf("must be at most RETRY_MAX_DELAY, %s", got.Projector.MaxDelay),
	}
}
```

```go
func checkTopics(p projector.Settings) error {
	topics := []struct{ name, topic string }{
		{"KAFKA_EVENTS_TOPIC", p.EventsTopic},
		{"KAFKA_STATUS_TOPIC", p.StatusTopic},
		{"KAFKA_RETRY_TOPIC", p.RetryTopic},
		{"KAFKA_DEAD_LETTER_TOPIC", p.DeadLetterTopic},
	}
	var errs []error
	for i, t := range topics {
		for _, earlier := range topics[:i] {
			if t.topic == earlier.topic {
				errs = append(errs, &config.Error{Name: t.name, Err: fmt.Errorf("must not be %s's topic, %s", earlier.name, earlier.topic)})
				break
			}
		}
	}
	return errors.Join(errs...)
}
```

```go
func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func positive[T int | time.Duration](v T) error {
	if v <= 0 {
		return errors.New("must be positive")
	}
	return nil
}
```

## The secrets, as files

PostgreSQL's password, and every CA, certificate and key, are files mounted
from Secrets, read through the Host's `FS`, so the tests can give the
processor files of their own:

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

func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}
```

Its error doesn't quote the file, so no secret reaches a log.

The Deployment mounts each Secret read-only:

```yaml
volumeMounts:
  - name: kafka
    mountPath: /etc/orderstatus/kafka
    readOnly: true
  - name: postgres
    mountPath: /etc/orderstatus/postgres
    readOnly: true
  - name: probes
    mountPath: /etc/orderstatus/probes
    readOnly: true
```

## mTLS to Kafka

The processor's certificate and key are one PEM file, read again when it
changes, so a renewal needs no restart. `certfile.Config` serves it to the
brokers as a client certificate, and on the probes as a server's:

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
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return f.certificate()
		},
	}, nil
}
```

```go
func (f *files) certificate() (*tls.Certificate, error) {
	certPEM, err := readFile(f.fs, f.certFile)
	if err != nil {
		return nil, err
	}
	keyPEM := certPEM
	if f.keyFile != f.certFile {
		keyPEM, err = readFile(f.fs, f.keyFile)
		if err != nil {
			return nil, err
		}
	}
	if cert := f.cached(certPEM, keyPEM); cert != nil {
		return cert, nil
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate %s and key %s: %w", f.certFile, f.keyFile, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.certPEM, f.keyPEM, f.cert = certPEM, keyPEM, &cert
	return &cert, nil
}
```

`readSettings` then trusts the brokers' CA, and gives the consumer and the
`Producer`s the same brokers and TLS, so they log in as one principal. The
consumer reads both `orders.events` and `orders.retry`:

```go
set := got.settings
set.Consumer.Topics = []string{set.Projector.EventsTopic, set.Projector.RetryTopic}
set.Consumer.TrustScenario = trustTopics(got.TrustedTopics)
kafkaTLS := got.KafkaTLS
kafkaTLS.RootCAs = got.KafkaCA
kafkaTLS.MinVersion = tls.VersionTLS12
set.Consumer.TLS = kafkaTLS
set.Producer.Brokers = set.Consumer.Brokers
set.Producer.TLS = kafkaTLS
set.Database, err = poolConfig(got.Postgres)
if err != nil {
	return settings{}, err
}
return set, nil
```

## Trusting a topic's scenarios

A record's failure-injection scenario is trusted only when its topic is in
`FAULT_TRUSTED_TOPICS`. With the default, production trusts none:

```go
func topicList(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	topics := strings.Split(s, ",")
	if slices.Contains(topics, "") {
		return nil, errors.New("must not name an empty topic")
	}
	return topics, nil
}

func trustTopics(topics []string) func(m kafka.Message) bool {
	if len(topics) == 0 {
		return nil
	}
	trusted := make(map[string]bool, len(topics))
	for _, t := range topics {
		trusted[t] = true
	}
	return func(m kafka.Message) bool {
		return trusted[m.Topic]
	}
}
```

The kind overlay trusts both topics a record is consumed from:

```yaml
- FAULT_TRUSTED_TOPICS=orders.events,orders.retry
```

## The pool

`POSTGRES_URL` holds no password, and none of the parameters `Init` sets
itself:

```go
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
```

`poolConfig` adds the password and verifies PostgreSQL's certificate against
its CA. The empty file parameters stop pgx reading the real filesystem:

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

`openPostgres` dials through the Host, so the tests can give the pool their
own network, and closes the pool when the App stops:

```go
func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (projector.Store, error) {
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
	return store.New(pool), nil
}
```

## The `Producer`s

`Init` opens the store, then builds two `Producer`s. Only the retries'
forwards a record's scenario, so a retried record fails the same way again,
and no scenario reaches `orders.status` or `orders.dlq`, whose consumers are
other teams':

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres)
}

type openStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (projector.Store, error)

func initWith(ctx context.Context, s *humus.Setup, open openStore) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	opened, err := open(ctx, s, set.Database)
	if err != nil {
		return nil, err
	}
	st := faultyStore{store: opened, faults: s.Faults}

	forwarding := set.Producer
	forwarding.ForwardScenario = true
	retries, err := kafka.NewProducer(s, forwarding)
	if err != nil {
		return nil, err
	}
	results, err := kafka.NewProducer(s, set.Producer)
	if err != nil {
		return nil, err
	}
```

`initWith` takes the store's opener, so the in-process tests can give the
App a store of their own.

## The projector and the Service

The projector produces changes and dead letters with `results`, and retries
with `retries`. The Service consumes both topics with it:

```go
p, err := projector.New(st, results, retries, results, set.Projector, s.Telemetry.MeterProvider)
if err != nil {
	return nil, err
}

consumer, err := kafka.NewService(s, set.Consumer, p)
if err != nil {
	return nil, err
}
```

## The probes

An `httphealth` server, ready once the Service has joined its group:

```go
	probes, err := httphealth.NewService(s, set.Probes, health.Readiness(consumer.Joined()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{consumer, probes}, nil
}
```

## `BuildTelemetry`

The telemetry `Builder` names the service `orderstatus`, and sources the
OTLP endpoint and the exporters from the standard `OTEL_*` variables:

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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("orderstatus").Validate(telemetry.CheckServiceName))
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
func Faults(ctx context.Context, s *humus.Setup) (fault.Settings, error) {
	codec, err := jsonscenario.New("acme.fault")
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

The store's calls have a point of their own, `postgresql:orders`. It wraps
the `Store` interface, so any store `initWith` is given has it:

```go
const storePoint = "postgresql:orders"

type faultyStore struct {
	store  projector.Store
	faults fault.Injector
}

func (f faultyStore) inject(ctx context.Context) error {
	got := f.faults.Eval(ctx, storePoint)
	if err := got.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: storePoint, Err: err}
	}
	if got.Abort {
		return &fault.InjectedError{Point: storePoint}
	}
	return nil
}
```

Each of its methods injects before it calls the store:

```go
func (f faultyStore) Apply(ctx context.Context, e store.Event, status store.Status) error {
	if err := f.inject(ctx); err != nil {
		return err
	}
	return f.store.Apply(ctx, e, status)
}
```

## `main`

`main` passes both to `humus.Main`, on its default grace period of 20s,
which a record's 15s deadline fits under:

```go
func main() {
	humus.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults))
}
```

The Deployment's `terminationGracePeriodSeconds` covers the grace period,
the telemetry's shutdown, and a margin:

```yaml
terminationGracePeriodSeconds: 30
```

Next: [Testing](/humus/pr-preview/pr-1369/walkthroughs/kafka/orderstatus/testing/)
