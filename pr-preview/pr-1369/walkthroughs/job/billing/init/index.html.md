# Init

## The settings

Everything `Init` reads, in one struct:

```go
type settings struct {
	Database    *pgxpool.Config
	Payments    httpclient.Settings
	APIKey      string
	Period      time.Time
	Concurrency int
}
```

`config.Struct` sources and checks each one. A bad one makes `Init` fail, and
the job exits 78, naming its variable:

```go
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }

	return config.Struct(func(r *config.Resolver) settings {
		client := httpclient.DefaultSettings()
		client.BaseURL = r.Get(env("PAYMENTS_URL").Parse(url.Parse).Validate(checkPaymentsURL))
		client.TLS = r.Get(file("PAYMENTS_CA_FILE").Parse(certPool).Default(nil).Map(func(roots *x509.CertPool) *tls.Config {
			return &tls.Config{RootCAs: roots}
		}).Validate(httpclient.CheckTLS))
		client.Timeout = r.Get(env("PAYMENTS_TIMEOUT").Parse(time.ParseDuration).Default(client.Timeout).Validate(httpclient.CheckTimeout))
		client.AttemptTimeout = r.Get(env("PAYMENTS_ATTEMPT_TIMEOUT").Parse(time.ParseDuration).Default(client.AttemptTimeout).Validate(httpclient.CheckAttemptTimeout))
		client.MaxAttempts = r.Get(env("PAYMENTS_MAX_ATTEMPTS").Parse(strconv.Atoi).Default(client.MaxAttempts).Validate(httpclient.CheckMaxAttempts))

		return settings{
			Database:    r.Get(file("DATABASE_URL_FILE").Parse(databaseURL)),
			Payments:    client,
			APIKey:      r.Get(file("PAYMENTS_API_KEY_FILE").Parse(apiKey)),
			Period:      r.Get(env("BILLING_PERIOD").Parse(parseMonth).Default(lastMonth(time.Now())).Validate(checkEnded)),
			Concurrency: r.Get(env("BILLING_CONCURRENCY").Parse(strconv.Atoi).Default(4).Validate(checkConcurrency)),
		}
	}).Read(ctx)
}
```

| Variable | What it is | Default |
|---|---|---|
| `DATABASE_URL_FILE` | a file holding the PostgreSQL URL, password included | required |
| `PAYMENTS_URL` | the payments API's base URL, `https` | required |
| `PAYMENTS_API_KEY_FILE` | a file holding the API key | required |
| `PAYMENTS_CA_FILE` | the CA certificates the API is trusted through, PEM | the system's roots |
| `PAYMENTS_TIMEOUT`, `PAYMENTS_ATTEMPT_TIMEOUT`, `PAYMENTS_MAX_ATTEMPTS` | `httpclient.Settings`' fields | `httpclient.DefaultSettings()`' |
| `BILLING_PERIOD` | the month to issue, `YYYY-MM`, which has ended | last month, in UTC |
| `BILLING_CONCURRENCY` | charges in flight, 1 to 32 | 4 |

## The secrets, as files

The database URL and the API key are files mounted from Secrets, read
through the Host's `FS`, so the tests can give the job files of their own:

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

func apiKey(b []byte) (string, error) {
	key := string(bytes.TrimSpace(b))
	if key == "" {
		return "", errors.New("must hold the API key")
	}
	return key, nil
}
```

Neither error quotes the file, so neither secret reaches a log.

## The payments API's checks

The CA file becomes the client's roots, and the URL must be `https`:

```go
func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}

func checkPaymentsURL(u *url.URL) error {
	if err := httpclient.CheckBaseURL(u); err != nil {
		return err
	}
	if u.Scheme != "https" {
		return errors.New("must be an https URL")
	}
	return nil
}
```

## The period and the concurrency

`BILLING_PERIOD` is a month that has ended, and the concurrency is from 1 to
32:

```go
func parseMonth(s string) (time.Time, error) {
	m, err := time.Parse("2006-01", s)
	if err != nil {
		return time.Time{}, errors.New("must be a month, YYYY-MM")
	}
	return m, nil
}

func lastMonth(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC)
}

func checkEnded(month time.Time) error {
	if month.AddDate(0, 1, 0).After(time.Now()) {
		return fmt.Errorf("%s hasn't ended", month.Format("2006-01"))
	}
	return nil
}

func checkConcurrency(n int) error {
	if n < 1 || n > 32 {
		return errors.New("must be from 1 to 32")
	}
	return nil
}
```

## The pool

The pgx pool dials through the Host, and leaves resolving the name to it.
Its close runs when the App stops:

```go
func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (billing.Invoices, error) {
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
	return postgres.New(pool, s.Faults), nil
}
```

## The client, and `Init`

`Init` reads the settings, opens the store, builds the payments client and
returns the run as its only Service. A job serves no probes:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres)
}

type openStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (billing.Invoices, error)

func initWith(ctx context.Context, s *humus.Setup, open openStore) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	invoices, err := open(ctx, s, set.Database)
	if err != nil {
		return nil, err
	}

	client, err := payments.NewClient(s, set.Payments, set.APIKey)
	if err != nil {
		return nil, err
	}

	run := billing.Run{
		Invoices:    invoices,
		Payments:    client,
		Period:      set.Period,
		Concurrency: set.Concurrency,
		Telemetry:   s.Telemetry,
	}
	return []humus.Service{job.NewService(s, run.Run)}, nil
}
```

`initWith` takes the store's constructor, so the unit tests can give the run
a store in memory.

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

## The telemetry

The telemetry `Builder` names the service `billing`, and sources the OTLP
endpoint and the exporters from the standard `OTEL_*` variables:

```go
func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("billing").Validate(telemetry.CheckServiceName))
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

## `Options` and `main`

`Options` is what `main` passes to `humus.Main`, so the tests run the App
that `main` runs:

```go
func Options() []humus.Option {
	return []humus.Option{
		humus.WithTelemetry(buildTelemetry),
		humus.WithFaults(Faults),
	}
}
```

```go
package main

import (
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/job/billing/app"
)

func main() {
	humus.Main(app.Init, app.Options()...)
}
```

Next: [Testing](/humus/pr-preview/pr-1369/walkthroughs/job/billing/testing/)
