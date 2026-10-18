//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rabbitmq_test

// The tests in this file run RabbitMQ itself, in a container started with
// testcontainers-go, and dial it through humus.OSHost(). They are the only
// tests of the package that reach a broker, because humus writes and keeps no
// fake one: what needs a broker is tested against RabbitMQ, and everything
// else is a unit test. They open real sockets, so they sit behind the
// testcontainers build tag and run in a CI job of their own, outside the
// coverage gate:
//
//	go -C queue/rabbitmq test -race -tags testcontainers ./...
//
// humus declares no queue and no exchange, so each test declares the ones it
// needs over an amqp091 connection of its own, under names no other test
// uses.

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/queue/rabbitmq"
)

// rabbitmqImage is the RabbitMQ the tests run, the official image pinned to
// an exact release.
const rabbitmqImage = "docker.io/library/rabbitmq:4.3.6"

// The paths, in the container, of the broker's TLS files and of the config
// that turns its TLS listener on. The testcontainers module's WithSSL would
// turn the plain-text listener off, so TLS is turned on by a file in conf.d
// instead, which RabbitMQ reads after the config the module writes.
const (
	caFile     = "/etc/rabbitmq/humus-ca.pem"
	certFile   = "/etc/rabbitmq/humus-cert.pem"
	keyFile    = "/etc/rabbitmq/humus-key.pem"
	tlsConfig  = "/etc/rabbitmq/conf.d/90-humus-tls.conf"
	tlsListens = "started TLS (SSL) listener on [::]:5671"
)

// rabbitMQ is a RabbitMQ in a container. It listens for AMQP in plain text
// and over TLS, with a certificate from humustest.NewCA, and its login is
// new for each run.
type rabbitMQ struct {
	once      sync.Once
	host      string // the host its ports are reached on
	amqp      string // its plain-text AMQP port, on host
	amqps     string // its TLS AMQP port, on host
	username  string
	password  string
	ca        *humustest.CA
	dir       string // the TLS files given to the container
	container testcontainers.Container
	err       error
}

// broker is the one RabbitMQ every test in the package shares, started by
// the first test that needs it and stopped by TestMain.
var broker rabbitMQ

func TestMain(m *testing.M) {
	m.Run()
	if broker.container != nil {
		// The tests have finished; a container left behind is reaped by
		// testcontainers' own reaper.
		_ = broker.container.Terminate(context.Background())
	}
	if broker.dir != "" {
		_ = os.RemoveAll(broker.dir)
	}
}

// rabbit returns the shared RabbitMQ, starting it on first use.
func rabbit(t *testing.T) *rabbitMQ {
	t.Helper()
	broker.once.Do(func() {
		// Overwritten when the start returns, so a start that fails t
		// rather than returning still leaves an error behind.
		broker.err = errors.New("starting RabbitMQ did not finish")
		broker.err = startRabbitMQ(t)
	})
	require.NoError(t, broker.err, "starting RabbitMQ in a container")
	return &broker
}

// startRabbitMQ starts RabbitMQ with a new login and a TLS listener whose
// certificate is issued by a new CA, and fills in broker.
func startRabbitMQ(t *testing.T) error {
	ctx := context.Background()
	broker.username = "humus-" + strings.ToLower(rand.Text())
	broker.password = rand.Text()
	broker.ca = humustest.NewCA(t)

	dir, err := os.MkdirTemp("", "humus-rabbitmq-")
	if err != nil {
		return err
	}
	broker.dir = dir
	files, err := tlsFiles(t, dir, broker.ca)
	if err != nil {
		return err
	}

	c, err := tcrabbitmq.Run(ctx, rabbitmqImage,
		tcrabbitmq.WithAdminUsername(broker.username),
		tcrabbitmq.WithAdminPassword(broker.password),
		testcontainers.WithFiles(files...),
		testcontainers.WithAdditionalWaitStrategy(wait.ForLog(tlsListens).WithStartupTimeout(2*time.Minute)),
	)
	if c != nil {
		broker.container = c
	}
	if err != nil {
		return err
	}

	if broker.host, err = c.Host(ctx); err != nil {
		return err
	}
	amqpPort, err := c.MappedPort(ctx, tcrabbitmq.DefaultAMQPPort)
	if err != nil {
		return err
	}
	amqpsPort, err := c.MappedPort(ctx, tcrabbitmq.DefaultAMQPSPort)
	if err != nil {
		return err
	}
	broker.amqp, broker.amqps = amqpPort.Port(), amqpsPort.Port()
	return nil
}

// tlsFiles writes the broker's CA, certificate and key to dir, as PEM, and
// returns them, with the config that turns the TLS listener on, as files to
// copy into the container. The certificate names every host a container's
// ports are commonly reached on.
func tlsFiles(t *testing.T, dir string, ca *humustest.CA) ([]testcontainers.ContainerFile, error) {
	cert := ca.Issue(t, "localhost", "127.0.0.1", "::1").TLS()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return nil, err
	}
	blocks := map[string]*pem.Block{
		caFile:   {Type: "CERTIFICATE", Bytes: cert.Certificate[1]},
		certFile: {Type: "CERTIFICATE", Bytes: cert.Certificate[0]},
		keyFile:  {Type: "PRIVATE KEY", Bytes: key},
	}
	var files []testcontainers.ContainerFile
	for path, block := range blocks {
		host := filepath.Join(dir, filepath.Base(path))
		if err := os.WriteFile(host, pem.EncodeToMemory(block), 0o600); err != nil {
			return nil, err
		}
		files = append(files, testcontainers.ContainerFile{HostFilePath: host, ContainerFilePath: path, FileMode: 0o644})
	}
	conf := strings.Join([]string{
		"listeners.ssl.default = 5671",
		"ssl_options.cacertfile = " + caFile,
		"ssl_options.certfile = " + certFile,
		"ssl_options.keyfile = " + keyFile,
		"ssl_options.verify = verify_none",
		"ssl_options.fail_if_no_peer_cert = false",
		"",
	}, "\n")
	files = append(files, testcontainers.ContainerFile{Reader: strings.NewReader(conf), ContainerFilePath: tlsConfig, FileMode: 0o644})
	return files, nil
}

// plainText returns DefaultSettings' Connection to the shared RabbitMQ in
// plain text, with its login.
func plainText(t *testing.T) rabbitmq.Connection {
	t.Helper()
	b := rabbit(t)
	c := rabbitmq.DefaultSettings().Connection
	c.Addr = net.JoinHostPort(b.host, b.amqp)
	c.SASL = &amqp.PlainAuth{Username: b.username, Password: b.password}
	return c
}

// overTLS returns DefaultSettings' Connection to the shared RabbitMQ over
// TLS, trusting its CA through RootCAs, with its login.
func overTLS(t *testing.T) rabbitmq.Connection {
	t.Helper()
	b := rabbit(t)
	c := plainText(t)
	c.Addr = net.JoinHostPort(b.host, b.amqps)
	c.TLS = &tls.Config{RootCAs: b.ca.Pool(), MinVersion: tls.VersionTLS12}
	return c
}

// unique returns prefix followed by a random suffix, so that each test has
// names of its own.
func unique(prefix string) string {
	return prefix + "-" + strings.ToLower(rand.Text())
}

// declareQueue declares a queue of the test's own over an amqp091 connection
// that isn't humus's, and returns its name. The queue is deleted when the
// test ends.
func declareQueue(t *testing.T) string {
	t.Helper()
	b := rabbit(t)
	var d net.Dialer
	nc, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort(b.host, b.amqp))
	require.NoError(t, err)
	conn, err := amqp.Open(nc, amqp.Config{
		SASL:   []amqp.Authentication{&amqp.PlainAuth{Username: b.username, Password: b.password}},
		Vhost:  "/",
		Locale: "en_US",
	})
	if err != nil {
		_ = nc.Close()
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	require.NoError(t, err)

	name := unique("orders")
	// RabbitMQ 4 refuses transient queues that aren't exclusive.
	_, err = ch.QueueDeclare(name, true, false, false, false, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = ch.QueueDelete(name, false, false, false)
	})
	return name
}

// open connects c through humus.OSHost(), and closes the connection when
// the test ends.
func open(t *testing.T, c rabbitmq.Connection) *amqp.Connection {
	t.Helper()
	conn, err := rabbitmq.Connect(t.Context(), humus.OSHost(), c)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// Each test below shows what only RabbitMQ can: that the handshake the
// connecting code runs is one the broker accepts, or refuses as amqp091
// says it does.

func TestConnect_opens_a_connection_and_a_channel(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]func(*testing.T) rabbitmq.Connection{
		"in plain text": plainText,
		"over TLS":      overTLS,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			queue := declareQueue(t)
			conn := open(t, c(t))
			ch, err := conn.Channel()
			require.NoError(t, err)
			defer func() { _ = ch.Close() }()

			// A passive declare is answered by the broker, so the channel
			// works, and it finds the queue the test declared.
			q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
			require.NoError(t, err)
			require.Equal(t, queue, q.Name)
		})
	}
}

func TestConnect_negotiates_Heartbeat(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		heartbeat time.Duration
	}{
		{name: "the default", heartbeat: rabbitmq.DefaultSettings().Connection.Heartbeat},
		{name: "2s", heartbeat: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := plainText(t)
			c.Heartbeat = tc.heartbeat
			conn := open(t, c)
			require.Equal(t, tc.heartbeat, conn.Config.Heartbeat)
		})
	}
	require.Equal(t, 10*time.Second, rabbitmq.DefaultSettings().Connection.Heartbeat)
}

func TestConnect_fails_with_ErrCredentials_for_a_wrong_password(t *testing.T) {
	t.Parallel()

	c := plainText(t)
	c.SASL = &amqp.PlainAuth{Username: rabbit(t).username, Password: rand.Text()}
	conn, err := rabbitmq.Connect(t.Context(), humus.OSHost(), c)
	require.ErrorIs(t, err, amqp.ErrCredentials)
	require.Nil(t, conn)
}

func TestConnect_fails_with_ErrVhost_for_a_vhost_that_does_not_exist(t *testing.T) {
	t.Parallel()

	c := plainText(t)
	c.Vhost = unique("missing")
	conn, err := rabbitmq.Connect(t.Context(), humus.OSHost(), c)
	require.ErrorIs(t, err, amqp.ErrVhost)
	require.Nil(t, conn)
}
