// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strconv"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/kube/kubebuilder/humus/api/v1alpha1"
	"github.com/z5labs/humus/example/kube/kubebuilder/humus/internal/controller"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/kube"
	"github.com/z5labs/humus/telemetry"
)

const serviceAccount = "/var/run/secrets/kubernetes.io/serviceaccount/"

const memcachedFinalizer = "cache.example.com/finalizer"

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	client, err := kube.NewClient(s, settings.Connection)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(client.Config())
	if err != nil {
		return nil, err
	}

	r := controller.Reconciler{Kube: kc, Instance: settings.Controller.LeaderElection.Identity}
	ctrl, err := kube.NewController(s, client, settings.Controller, v1alpha1.Memcached, r,
		kube.Owns(appsv1.SchemeGroupVersion.WithResource("deployments")),
		kube.Finalize(memcachedFinalizer, r.Finalize),
	)
	if err != nil {
		return nil, err
	}
	probes, err := httphealth.NewService(s, settings.Probes, health.Readiness(ctrl.Synced()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{ctrl, probes}, nil
}

type settings struct {
	Connection kube.Connection
	Controller kube.Settings
	Probes     httphealth.Settings
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	return config.Struct(func(r *config.Resolver) settings {
		k := kube.DefaultSettings()
		k.Name = r.Get(env("CONTROLLER_NAME").Validate(kube.CheckName))
		k.LeaderElection.Namespace = r.Get(env("POD_NAMESPACE").Validate(kube.CheckNamespace))
		k.LeaderElection.Identity = r.Get(env("POD_NAME").Validate(kube.CheckIdentity))

		tokenFile := r.Get(env("KUBERNETES_TOKEN_FILE").Default(serviceAccount + "token"))
		return settings{
			Connection: kube.Connection{
				Server: net.JoinHostPort(r.Get(env("KUBERNETES_SERVICE_HOST")), r.Get(env("KUBERNETES_SERVICE_PORT"))),
				TLS:    r.Get(env("KUBERNETES_CA_FILE").Default(serviceAccount + "ca.crt").Parse(trusting(s.Host.FS)).Validate(kube.CheckTLS)),
				Token: func(context.Context) (string, error) {
					b, err := readFile(s.Host.FS, tokenFile)
					return string(bytes.TrimSpace(b)), err
				},
			},
			Controller: k,
			Probes: httphealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
				TLS: r.Get(env("PROBES_TLS_CERT_FILE").FlatMap(func(certFile string) config.Value[*tls.Config] {
					return env("PROBES_TLS_KEY_FILE").Parse(serverTLS(s.Host.FS, certFile))
				}).Validate(httphealth.CheckTLS)),
			},
		}
	}).Read(ctx)
}

func trusting(fsys humus.FS) func(caFile string) (*tls.Config, error) {
	return func(caFile string) (*tls.Config, error) {
		ca, err := readFile(fsys, caFile)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("holds no PEM certificate")
		}
		return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, nil
	}
}

func serverTLS(fsys humus.FS, certFile string) func(keyFile string) (*tls.Config, error) {
	return func(keyFile string) (*tls.Config, error) {
		certPEM, err := readFile(fsys, certFile)
		if err != nil {
			return nil, err
		}
		keyPEM, err := readFile(fsys, keyFile)
		if err != nil {
			return nil, err
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
	}
}

func readFile(fsys humus.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

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
		s.Disabled = r.Get(env("OTEL_SDK_DISABLED").Parse(strconv.ParseBool).Default(d.Disabled))
		s.ServiceName = r.Get(env("OTEL_SERVICE_NAME").Default("memcached-operator").Validate(telemetry.CheckServiceName))
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
