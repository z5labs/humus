// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Command mailrequest publishes a mail request by hand, to try the mailer.
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
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/mailer/mail"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/queue/rabbitmq"
	"github.com/z5labs/humus/telemetry"
)

func main() {
	humus.Main(Init, humus.WithTelemetry(buildTelemetry))
}

func buildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "mailrequest"
	return telemetry.New(ctx, s, dial, p.Stderr)
}

func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(settings.Request)
	if err != nil {
		return nil, err
	}
	publisher, err := rabbitmq.NewPublisher(s, settings.Publisher)
	if err != nil {
		return nil, err
	}
	publishing := rabbitmq.Publishing{
		Exchange:    mail.Exchange,
		RoutingKey:  mail.SendKey,
		ContentType: "application/json",
		MessageID:   settings.MessageID,
		Body:        body,
	}
	return []humus.Service{job.NewService(s, func(ctx context.Context) error {
		if err := publisher.Publish(ctx, publishing); err != nil {
			return err
		}
		_, err := fmt.Fprintln(s.Process.Stdout, publishing.MessageID)
		return err
	})}, nil
}

type request struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

type settings struct {
	Publisher rabbitmq.PublisherSettings
	MessageID string
	Request   request
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	args, err := parseArgs(s.Process)
	if err != nil {
		return settings{}, err
	}
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := readFile(s.Host.FS)

	return config.Struct(func(r *config.Resolver) settings {
		publisher := rabbitmq.DefaultPublisherSettings()
		conn := publisher.Connection
		conn.Addr = r.Get(env("RABBITMQ_ADDR").Validate(rabbitmq.CheckAddr))
		conn.Vhost = r.Get(env("RABBITMQ_VHOST").Default(conn.Vhost).Validate(rabbitmq.CheckVhost))
		conn.TLS = r.Get(env("RABBITMQ_CA_FILE").Parse(file).Parse(certPool).Map(trusting).Default(nil))
		conn.SASL = &amqp.PlainAuth{
			Username: r.Get(env("RABBITMQ_USERNAME").Validate(notEmpty)),
			Password: r.Get(env("RABBITMQ_PASSWORD_FILE").Parse(file).Parse(secret)),
		}
		publisher.Connection = conn

		return settings{
			Publisher: publisher,
			MessageID: r.Get(args.value("message-id").Default(rand.Text()).Validate(notEmpty)),
			Request: request{
				To:      r.Get(args.value("to").Validate(notEmpty)),
				Subject: r.Get(args.value("subject").Validate(notEmpty)),
				Text:    r.Get(args.value("text").Validate(notEmpty)),
			},
		}
	}).Read(ctx)
}

type flags map[string]string

func parseArgs(p bedrock.Process) (flags, error) {
	set := flags{}
	fs := flag.NewFlagSet(p.Args[0], flag.ContinueOnError)
	fs.SetOutput(p.Stderr)
	for _, f := range []struct{ name, usage string }{
		{"to", "the address to send the email to"},
		{"subject", "the email's subject"},
		{"text", "the email's text"},
		{"message-id", "the request's MessageId, random if not given"},
	} {
		fs.Func(f.name, f.usage, func(v string) error {
			set[f.name] = v
			return nil
		})
	}
	if err := fs.Parse(p.Args[1:]); err != nil {
		return nil, &config.Error{Name: "arguments", Err: err}
	}
	if fs.NArg() > 0 {
		return nil, &config.Error{Name: "arguments", Err: fmt.Errorf("takes flags only, not %q", fs.Arg(0))}
	}
	return set, nil
}

func (f flags) value(name string) config.Value[string] {
	return config.Func("-"+name, func(context.Context) (string, error) {
		v, ok := f[name]
		if !ok {
			return "", config.ErrNotFound
		}
		return v, nil
	})
}

func notEmpty(s string) error {
	if s == "" {
		return errors.New("must not be empty")
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

func trusting(roots *x509.CertPool) *tls.Config {
	return &tls.Config{RootCAs: roots}
}
