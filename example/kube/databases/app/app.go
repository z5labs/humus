// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app is the databases controller's Init: its settings, its Client, its Controller, its probes and its telemetry.
package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/kube/databases/api/v1alpha1"
	"github.com/z5labs/humus/example/kube/databases/controller"
	"github.com/z5labs/humus/example/kube/databases/internal/certfile"
	"github.com/z5labs/humus/example/kube/databases/postgres"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/kube"
	"github.com/z5labs/humus/telemetry"
)

const (
	serviceAccount     = "/var/run/secrets/kubernetes.io/serviceaccount/"
	finalizer          = "postgres.example.com/finalizer"
	scenarioAnnotation = "postgres.example.com/failure-scenario"
)

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
	return telemetry.New(ctx, s, dial, p.Stdout)
}

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	client, err := kube.NewClient(s, set.Connection)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(client.Config())
	if err != nil {
		return nil, err
	}
	admin, err := openPostgres(ctx, s, set.Admin)
	if err != nil {
		return nil, err
	}

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
	probes, err := httphealth.NewService(s, set.Probes, health.Readiness(ctrl.Synced()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{ctrl, probes}, nil
}

type settings struct {
	Connection    kube.Connection
	Controller    kube.Settings
	Admin         *pgxpool.Config
	Host          string
	Port          int
	Probes        httphealth.Settings
	TrustedPrefix string
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	read := readFile(s.Host.FS)

	return config.Struct(func(r *config.Resolver) settings {
		tokenFile := r.Get(env("KUBERNETES_TOKEN_FILE").Default(serviceAccount + "token").Validate(func(name string) error {
			_, err := readToken(s.Host.FS, name)
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
					return readToken(s.Host.FS, tokenFile)
				},
			},
			Controller: k,
			Admin:      r.Get(env("POSTGRES_ADMIN_DSN_FILE").Parse(read).Parse(adminDSN)),
			Host:       r.Get(env("POSTGRES_HOST").Validate(notEmpty)),
			Port:       r.Get(env("POSTGRES_PORT").Parse(strconv.Atoi).Default(5432).Validate(checkPostgresPort)),
			Probes: httphealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
				TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(pemTLS(s.Host.FS)).Validate(httphealth.CheckTLS)),
			},
			TrustedPrefix: r.Get(env("FAULT_TRUSTED_NAMESPACE_PREFIX").Default("")),
		}
	}).Read(ctx)
}

func trustPrefix(prefix string) func(obj metav1.Object) bool {
	return func(obj metav1.Object) bool {
		return prefix != "" && strings.HasPrefix(obj.GetNamespace(), prefix)
	}
}

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

func openPostgres(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (*postgres.Server, error) {
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
	return postgres.New(pool, s.Faults, s.Telemetry.TracerProvider), nil
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

func readToken(fsys humus.FS, name string) (string, error) {
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

func pemTLS(fsys humus.FS) func(name string) (*tls.Config, error) {
	return func(name string) (*tls.Config, error) {
		return certfile.Config(fsys, name, name)
	}
}
