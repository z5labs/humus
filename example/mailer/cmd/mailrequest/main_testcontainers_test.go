//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/z5labs/bedrock"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/mailer/mail"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/rabbitmq"
)

const rabbitmqImage = "docker.io/library/rabbitmq:4.3.6"

const (
	definitionsFile = "/etc/rabbitmq/mail-definitions.json"
	brokerCAFile    = "/etc/rabbitmq/mailrequest-ca.pem"
	brokerCertFile  = "/etc/rabbitmq/mailrequest-cert.pem"
	brokerKeyFile   = "/etc/rabbitmq/mailrequest-key.pem"
	tlsConfigFile   = "/etc/rabbitmq/conf.d/90-mailrequest-tls.conf"
	tlsListens      = "started TLS (SSL) listener on [::]:5671"
)

type login struct {
	username string
	password string
}

type rabbitMQ struct {
	container *tcrabbitmq.RabbitMQContainer
	host      string
	amqp      string
	amqps     string
	caPEM     []byte
}

func startRabbitMQ(t *testing.T) *rabbitMQ {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "localhost", "127.0.0.1", "::1").TLS()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[1]})
	conf := strings.Join([]string{
		"listeners.ssl.default = 5671",
		"ssl_options.cacertfile = " + brokerCAFile,
		"ssl_options.certfile = " + brokerCertFile,
		"ssl_options.keyfile = " + brokerKeyFile,
		"ssl_options.verify = verify_none",
		"ssl_options.fail_if_no_peer_cert = false",
		"",
	}, "\n")

	c, err := tcrabbitmq.Run(t.Context(), rabbitmqImage,
		tcrabbitmq.WithAdminUsername("admin-"+strings.ToLower(rand.Text())),
		tcrabbitmq.WithAdminPassword(rand.Text()),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{
				HostFilePath:      filepath.Join("..", "..", "deploy", "rabbitmq", "definitions.json"),
				ContainerFilePath: definitionsFile,
				FileMode:          0o644,
			},
			containerFile(brokerCAFile, caPEM),
			containerFile(brokerCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})),
			containerFile(brokerKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})),
			containerFile(tlsConfigFile, []byte(conf)),
		),
		testcontainers.WithAdditionalWaitStrategy(wait.ForLog(tlsListens).WithStartupTimeout(2*time.Minute)),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting RabbitMQ in a container")

	host, err := c.Host(t.Context())
	require.NoError(t, err)
	amqpPort, err := c.MappedPort(t.Context(), tcrabbitmq.DefaultAMQPPort)
	require.NoError(t, err)
	amqpsPort, err := c.MappedPort(t.Context(), tcrabbitmq.DefaultAMQPSPort)
	require.NoError(t, err)

	r := &rabbitMQ{container: c, host: host, amqp: amqpPort.Port(), amqps: amqpsPort.Port(), caPEM: caPEM}
	r.ctl(t, "import_definitions", definitionsFile)
	return r
}

func containerFile(path string, contents []byte) testcontainers.ContainerFile {
	return testcontainers.ContainerFile{Reader: strings.NewReader(string(contents)), ContainerFilePath: path, FileMode: 0o644}
}

func (r *rabbitMQ) ctl(t *testing.T, args ...string) {
	t.Helper()
	code, out, err := r.container.Exec(t.Context(), append([]string{"rabbitmqctl"}, args...), tcexec.Multiplexed())
	require.NoError(t, err, "rabbitmqctl %s", args[0])
	b, err := io.ReadAll(out)
	require.NoError(t, err)
	require.Zero(t, code, "rabbitmqctl %s: %s", args[0], b)
}

func (r *rabbitMQ) addUser(t *testing.T, configure, write, read string) login {
	t.Helper()
	l := login{username: strings.ToLower(rand.Text()), password: rand.Text()}
	r.ctl(t, "add_user", l.username, l.password)
	r.ctl(t, "set_permissions", "--vhost", "/", l.username, configure, write, read)
	return l
}

func (r *rabbitMQ) get(t *testing.T, l login) amqp.Delivery {
	t.Helper()
	conn, err := amqp.DialConfig("amqp://"+net.JoinHostPort(r.host, r.amqp)+"/", amqp.Config{
		SASL: []amqp.Authentication{&amqp.PlainAuth{Username: l.username, Password: l.password}},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	ch, err := conn.Channel()
	require.NoError(t, err)

	d, ok, err := ch.Get(mail.SendQueue, false)
	require.NoError(t, err)
	require.True(t, ok, "a request on %s", mail.SendQueue)
	require.NoError(t, d.Ack(false))
	return d
}

func TestMain_publishes_a_request_to_mail_send_over_TLS(t *testing.T) {
	broker := startRabbitMQ(t)
	publisher := broker.addUser(t, `^$`, `^mail$`, `^$`)
	reader := broker.addUser(t, `^$`, `^$`, `^mail\.send$`)

	dir := t.TempDir()
	env := map[string]string{
		"RABBITMQ_ADDR":          net.JoinHostPort(broker.host, broker.amqps),
		"RABBITMQ_CA_FILE":       writeFile(t, dir, "rabbitmq-ca.pem", broker.caPEM),
		"RABBITMQ_USERNAME":      publisher.username,
		"RABBITMQ_PASSWORD_FILE": writeFile(t, dir, "rabbitmq-password", []byte(publisher.password+"\n")),
	}
	publish := func(t *testing.T, args ...string) string {
		t.Helper()
		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(buildTelemetry)),
			humustest.WithEnv(env),
			humustest.WithArgs(args...),
		)
		var st bedrock.ExitStatus
		select {
		case s := <-status:
			st = s
		case <-time.After(time.Minute):
			t.Fatal("mailrequest did not exit within a minute")
		}
		require.Zero(t, st.Code(), "cause: %v\nstderr: %s", st.Cause(), p.Stderr())
		return p.Stdout()
	}
	decode := func(t *testing.T, d amqp.Delivery) mail.Request {
		t.Helper()
		require.Equal(t, mail.Exchange, d.Exchange)
		require.Equal(t, mail.SendKey, d.RoutingKey)
		require.Equal(t, "application/json", d.ContentType)
		require.Equal(t, uint8(amqp.Persistent), d.DeliveryMode)
		req, err := mail.Decode(rabbitmq.Message{
			Queue:       mail.SendQueue,
			Exchange:    d.Exchange,
			RoutingKey:  d.RoutingKey,
			ContentType: d.ContentType,
			MessageID:   d.MessageId,
			Body:        d.Body,
		})
		require.NoError(t, err)
		return req
	}

	t.Run("with the MessageId it is given", func(t *testing.T) {
		id := rand.Text()
		stdout := publish(t, "-to", "someone@example.com", "-subject", "Hello", "-text", "Hello, world.", "-message-id", id)

		require.Equal(t, id+"\n", stdout)
		require.Equal(t, mail.Request{
			MessageID: id,
			To:        "someone@example.com",
			Subject:   "Hello",
			Text:      "Hello, world.",
		}, decode(t, broker.get(t, reader)))
	})

	t.Run("with a random MessageId it prints", func(t *testing.T) {
		first := strings.TrimSuffix(publish(t, "-to", "someone@example.com", "-subject", "Hi", "-text", "Hi there."), "\n")
		require.NotEmpty(t, first)
		require.Equal(t, mail.Request{
			MessageID: first,
			To:        "someone@example.com",
			Subject:   "Hi",
			Text:      "Hi there.",
		}, decode(t, broker.get(t, reader)))

		second := strings.TrimSuffix(publish(t, "-to", "someone@example.com", "-subject", "Hi", "-text", "Hi there."), "\n")
		require.NotEqual(t, first, second)
		require.Equal(t, second, decode(t, broker.get(t, reader)).MessageID)
	})
}

func writeFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}
