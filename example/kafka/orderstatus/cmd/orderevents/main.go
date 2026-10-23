// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command orderevents produces an order's events by hand, to try the order status processor.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/kafka/orderstatus/internal/certfile"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue/kafka"
	"github.com/z5labs/humus/telemetry"
)

func main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}

func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "orderevents"
	return telemetry.New(ctx, s, dial, p.Stderr)
}

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}
	producer, err := kafka.NewProducer(s, settings.Producer)
	if err != nil {
		return nil, err
	}
	return []humus.Service{job.NewService(s, func(ctx context.Context) error {
		records := make([]kafka.Record, 0, len(settings.Types))
		for _, typ := range settings.Types {
			value, err := json.Marshal(event{Type: typ, At: time.Now().UTC()})
			if err != nil {
				return err
			}
			records = append(records, kafka.Record{
				Topic: settings.Topic,
				Key:   []byte(settings.OrderID),
				Value: value,
			})
		}
		if err := producer.Produce(ctx, records...); err != nil {
			return err
		}
		_, err := fmt.Fprintln(s.Process.Stdout, settings.OrderID)
		return err
	})}, nil
}

type event struct {
	Type string    `json:"type"`
	At   time.Time `json:"at"`
}

type settings struct {
	Producer kafka.ProducerSettings
	Topic    string
	OrderID  string
	Types    []string
}

type sourced struct {
	settings
	KafkaCA  *x509.CertPool
	KafkaTLS *tls.Config
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	args, err := parseArgs(s.Process)
	if err != nil {
		return settings{}, err
	}
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	list := func(v string) []string { return strings.Split(v, ",") }

	got, err := config.Struct(func(r *config.Resolver) sourced {
		producer := kafka.DefaultProducerSettings()
		producer.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))

		return sourced{
			settings: settings{
				Producer: producer,
				Topic:    r.Get(env("KAFKA_EVENTS_TOPIC").Validate(notEmpty)),
				OrderID:  r.Get(args.orderIDValue().Default(rand.Text()).Validate(notEmpty)),
				Types:    r.Get(args.typesValue().Validate(noneEmpty)),
			},
			KafkaCA:  r.Get(env("KAFKA_TLS_CA_FILE").Parse(readFile(s.Host.FS)).Parse(certPool)),
			KafkaTLS: r.Get(env("KAFKA_TLS_CERT_FILE").Parse(pemTLS(s.Host.FS))),
		}
	}).Read(ctx)
	if err != nil {
		return settings{}, err
	}

	set := got.settings
	kafkaTLS := got.KafkaTLS
	kafkaTLS.RootCAs = got.KafkaCA
	kafkaTLS.MinVersion = tls.VersionTLS12
	set.Producer.TLS = kafkaTLS
	return set, nil
}

type arguments struct {
	orderID    string
	hasOrderID bool
	types      []string
}

func parseArgs(p bedrock.Process) (arguments, error) {
	var a arguments
	fs := flag.NewFlagSet(p.Args[0], flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(fs.Output(), "Usage: %s [-order-id ID] TYPE...\n", fs.Name())
		fs.PrintDefaults()
	}
	fs.Func("order-id", "the order's ID, random if not given", func(v string) error {
		a.orderID, a.hasOrderID = v, true
		return nil
	})
	if err := fs.Parse(p.Args[1:]); err != nil {
		return arguments{}, &config.Error{Name: "arguments", Err: err}
	}
	a.types = fs.Args()
	return a, nil
}

func (a arguments) orderIDValue() config.Value[string] {
	return config.Func("-order-id", func(context.Context) (string, error) {
		if !a.hasOrderID {
			return "", config.ErrNotFound
		}
		return a.orderID, nil
	})
}

func (a arguments) typesValue() config.Value[[]string] {
	return config.Func("types", func(context.Context) ([]string, error) {
		if len(a.types) == 0 {
			return nil, config.ErrNotFound
		}
		return a.types, nil
	})
}

func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	return nil
}

func noneEmpty(types []string) error {
	if slices.Contains(types, "") {
		return errors.New("must not name an empty type")
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
		return certfile.Config(fsys, name, name)
	}
}
