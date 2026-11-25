# Init

## The settings

Everything `Init` reads, in one struct:

```go
type settings struct {
	Connection    kube.Connection
	Controller    kube.Settings
	Admin         *pgxpool.Config
	Host          string
	Port          int
	Probes        httphealth.Settings
	TrustedPrefix string
}
```

`config.Struct` sources and checks each one. A bad one makes `Init` fail,
and the controller exits 78, naming its variable:

```go
func readSettings(ctx context.Context, s *humus.Setup, h humus.Host) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	read := readFile(h.Filesystem)

	return config.Struct(func(r *config.Resolver) settings {
		tokenFile := r.Get(env("KUBERNETES_TOKEN_FILE").Default(serviceAccount + "token").Validate(func(name string) error {
			_, err := readToken(h.Filesystem, name)
			return err
		}))

		k := kube.DefaultSettings()
		k.Name = r.Get(env("CONTROLLER_NAME").Default("databases-controller").Validate(kube.CheckName))
		k.Namespace = r.Get(env("CONTROLLER_NAMESPACE").Default(k.Namespace).Validate(kube.CheckWatchNamespace))
		k.Workers = r.Get(env("CONTROLLER_WORKERS").Parse(strconv.Atoi).Default(k.Workers).Validate(kube.CheckWorkers))
		k.Resync = r.Get(env("CONTROLLER_RESYNC").Parse(time.ParseDuration).Default(k.Resync).Validate(kube.CheckResync))
		k.LeaderElection.Namespace = r.Get(env("POD_NAMESPACE").Validate(kube.CheckNamespace))
		k.LeaderElection.Identity = r.Get(env("POD_NAME").Validate(kube.CheckIdentity))

		return settings{
			Connection: kube.Connection{
				Server: net.JoinHostPort(
					r.Get(env("KUBERNETES_SERVICE_HOST").Validate(checkAPIServerHost)),
					r.Get(env("KUBERNETES_SERVICE_PORT").Validate(checkAPIServerPort)),
				),
				TLS: r.Get(env("KUBERNETES_CA_FILE").Default(serviceAccount + "ca.crt").Parse(read).Parse(certPool).Map(trusting).Validate(kube.CheckTLS)),
				Token: func(context.Context) (string, error) {
					return readToken(h.Filesystem, tokenFile)
				},
			},
			Controller: k,
			Admin:      r.Get(env("POSTGRES_ADMIN_DSN_FILE").Parse(read).Parse(adminDSN)),
			Host:       r.Get(env("POSTGRES_HOST").Validate(notEmpty)),
			Port:       r.Get(env("POSTGRES_PORT").Parse(strconv.Atoi).Default(5432).Validate(checkPostgresPort)),
			Probes: httphealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
				TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(pemTLS(h.Filesystem)).Validate(httphealth.CheckTLS)),
			},
			TrustedPrefix: r.Get(env("FAULT_TRUSTED_NAMESPACE_PREFIX").Default("")),
		}
	}).Read(ctx)
}
```

| Variable | What it is | Default |
|---|---|---|
| `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT` | the API server, which the kubelet sets in every container | required |
| `KUBERNETES_CA_FILE`, `KUBERNETES_TOKEN_FILE` | the API server's CA, and the service account's token | the service account's, mounted in the pod |
| `CONTROLLER_NAME` | the controller's name, its Lease's, and its field manager | `databases-controller` |
| `CONTROLLER_NAMESPACE` | the one namespace it watches | every namespace |
| `CONTROLLER_WORKERS`, `CONTROLLER_RESYNC` | how many `Database`s are reconciled at once, and how often every one is reconciled again | `kube.DefaultSettings()`' |
| `POD_NAMESPACE`, `POD_NAME` | the Lease's namespace, and this replica's identity in it | required |
| `POSTGRES_ADMIN_DSN_FILE` | a file holding the admin connection's DSN, its password included | required |
| `POSTGRES_HOST`, `POSTGRES_PORT` | the server's address, as a client reaches it, which each Secret holds | required, and 5432 |
| `PROBES_ADDR`, `PROBES_TLS_PEM_FILE` | the probes' address, and a file holding their certificate and key, PEM | required |
| `FAULT_TRUSTED_NAMESPACE_PREFIX` | the namespace prefix whose `Database`s' scenarios are trusted | none |
| `FAULT_MAX_DELAY` | the longest delay a scenario injects | 10s |

The checks of their own:

```go
func checkAPIServerHost(host string) error {
	return kube.CheckServer(net.JoinHostPort(host, "443"))
}

func checkAPIServerPort(port string) error {
	return kube.CheckServer(net.JoinHostPort("kubernetes", port))
}

func checkPostgresPort(port int) error {
	if port < 1 || port > 65535 {
		return errors.New("must be from 1 to 65535")
	}
	return nil
}

func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	return nil
}
```

## The secrets, as files

The admin DSN, the service account's token and CA, and the probes'
certificate are files, read through the Host's `Filesystem`, so the tests can give
the controller files of their own:

```go
func readFile(fsys humus.Filesystem) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

func readToken(fsys humus.Filesystem, name string) (string, error) {
	b, err := readFile(fsys)(name)
	if err != nil {
		return "", err
	}
	token := string(bytes.TrimSpace(b))
	if token == "" {
		return "", errors.New("the file is empty")
	}
	return token, nil
}
```

The DSN must hold the admin's password. Its error doesn't quote the file, so
no secret reaches a log:

```go
func adminDSN(b []byte) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(string(bytes.TrimSpace(b)))
	if err != nil {
		return nil, errors.New("must hold a PostgreSQL DSN")
	}
	if cfg.ConnConfig.Password == "" {
		return nil, errors.New("must hold the admin's password")
	}
	return cfg, nil
}
```

The Deployment mounts each Secret read-only:

```yaml
volumeMounts:
  - name: postgres
    mountPath: /etc/databases/postgres
    readOnly: true
  - name: probes
    mountPath: /etc/databases/probes
    readOnly: true
```

## The Connection

The Connection reaches the API server from the pod, with the service
account's CA:

```go
func certPool(pem []byte) (*x509.CertPool, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM certificate in the file")
	}
	return roots, nil
}

func trusting(roots *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
}
```

Its `Token` reads the token file each time the Client asks, so a token the
kubelet rotates is picked up. The Deployment mounts the token:

```yaml
serviceAccountName: databases
automountServiceAccountToken: true
```

## The Settings and the Lease

The Lease's namespace and this replica's identity come from the downward
API:

```yaml
env:
  - name: POD_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
  - name: POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
```

The two replicas share the Lease, named `CONTROLLER_NAME`, and only its
holder reconciles.

## Trusting a namespace's scenarios

A `Database`'s scenario annotation is trusted only in a namespace whose name
starts with `FAULT_TRUSTED_NAMESPACE_PREFIX`. With the default, production
trusts none:

```go
func trustPrefix(prefix string) func(obj metav1.Object) bool {
	return func(obj metav1.Object) bool {
		return prefix != "" && strings.HasPrefix(obj.GetNamespace(), prefix)
	}
}
```

The kind overlay trusts the namespaces the cases create:

```yaml
- FAULT_TRUSTED_NAMESPACE_PREFIX=databases-cases-
```

## The Client and the pool

`Init` reads the settings, then builds the Client, a clientset on it for the
Secrets, and the `Server`:

```go
func Init(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
	set, err := readSettings(ctx, s, h)
	if err != nil {
		return nil, err
	}

	client, err := kube.NewClient(s, h, set.Connection)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(client.Config())
	if err != nil {
		return nil, err
	}
	admin, err := openPostgres(ctx, s, h, set.Admin)
	if err != nil {
		return nil, err
	}
```

`Init` is given the host, `h`, and hands it on as the network
`kube.NewClient` dials the API server through, and the probes' `httphealth`
server listens through.

`openPostgres` dials TCP through the Host, so the tests can give the pool
their own network, and a Unix socket with a `net.Dialer` of its own. It
closes the pool when the App stops. The `Server` gets the
App's `fault.Injector` and its tracer provider:

```go
func openPostgres(ctx context.Context, s *humus.Setup, h humus.Host, cfg *pgxpool.Config) (*postgres.Server, error) {
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "unix" {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		return h.DialTCP(ctx, network, addr)
	}
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
	return postgres.New(pool, s.Faults, s.Telemetry.TracerProvider), nil
}
```

## The Controller

The Controller reconciles `Database`s with the `Reconciler`. It owns
Secrets, so a changed or deleted Secret reconciles its `Database`. It
finalizes with `Finalize`, and trusts scenarios by namespace:

```go
rec := controller.Reconciler{
	Kube:  kc,
	Admin: admin,
	Host:  set.Host,
	Port:  set.Port,
}
ctrl, err := kube.NewController(s, client, set.Controller, v1alpha1.Database, rec,
	kube.Owns(corev1.SchemeGroupVersion.WithResource("secrets")),
	kube.Finalize(finalizer, rec.Finalize),
	kube.TrustScenario(scenarioAnnotation, trustPrefix(set.TrustedPrefix)),
)
if err != nil {
	return nil, err
}
```

## The probes

An `httphealth` server, ready once the Controller's caches have synced:

```go
	probes, err := httphealth.NewService(ctx, s, h, set.Probes, health.Readiness(ctrl.Synced()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{ctrl, probes}, nil
}
```

The probes are served over TLS. Their certificate and key are one PEM file,
read again when it changes, so a renewal needs no restart:

```go
func pemTLS(fsys humus.Filesystem) func(name string) (*tls.Config, error) {
	return func(name string) (*tls.Config, error) {
		return certfile.Config(fsys, name, name)
	}
}
```

```go
func Config(fsys humus.Filesystem, certFile, keyFile string) (*tls.Config, error) {
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

The kubelet probes both over HTTPS:

```yaml
livenessProbe:
  httpGet:
    path: /health/liveness
    port: probes
    scheme: HTTPS
readinessProbe:
  httpGet:
    path: /health/readiness
    port: probes
    scheme: HTTPS
```

## `BuildTelemetry`

The telemetry `Builder` names the service `databases`, and sources the OTLP
endpoint and the exporters from the standard `OTEL_*` variables:

```go
func BuildTelemetry(ctx context.Context, p bedrock.Process, n telemetry.Network) (telemetry.SDK, error) {
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
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("databases").Validate(telemetry.CheckServiceName))
		s.Traces = r.Get(exporter("OTEL_TRACES_EXPORTER", d.Traces))
		s.Metrics = r.Get(exporter("OTEL_METRICS_EXPORTER", d.Metrics))
		s.Logs = r.Get(exporter("OTEL_LOGS_EXPORTER", d.Logs))
		s.OTLP.Endpoint = r.Get(env("OTEL_EXPORTER_OTLP_ENDPOINT").Default(d.OTLP.Endpoint).Validate(telemetry.CheckOTLPEndpoint))
		return s
	}).Read(ctx)
	if err != nil {
		return telemetry.SDK{}, err
	}
	return telemetry.New(ctx, s, n, p.Stdout)
}
```

## `Faults`

The App's `fault.Injector` decodes scenarios with `jsonscenario`, under the
member `acme.fault`, and caps a delay at `FAULT_MAX_DELAY`:

```go
func Faults(ctx context.Context, s *humus.Setup, h humus.Host) (fault.Settings, error) {
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

## `main`

`main` passes all three to `humus.Main`, with a grace period of 20s, in
which SIGTERM finishes the reconciles in flight and releases the Lease:

```go
func main() {
	humus.Main(app.Init, humus.WithTelemetry(app.BuildTelemetry), humus.WithFaults(app.Faults), humus.WithShutdownGracePeriod(20*time.Second))
}
```

The Deployment's `terminationGracePeriodSeconds` covers the grace period,
the telemetry's shutdown, and a margin:

```yaml
terminationGracePeriodSeconds: 30
```

Next: [Testing](/humus/pr-preview/pr-1630/walkthroughs/kube/databases/testing/)
