//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/mailer/app"
	"github.com/z5labs/humus/example/mailer/cases"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	rabbitmqImage = "docker.io/library/rabbitmq:4.3.6"
	wiremockImage = "docker.io/wiremock/wiremock:3.13.2"
)

const (
	definitionsFile = "/etc/rabbitmq/mail-definitions.json"
	wiremockPort    = "8080/tcp"
	waitLimit       = time.Minute
)

const (
	sendPolicy = `{"delivery-limit":2,"dead-letter-exchange":"mail.dead","dead-letter-strategy":"at-least-once","overflow":"reject-publish"}`

	retryPolicy = `{"message-ttl":1000,"dead-letter-exchange":"mail","dead-letter-routing-key":"mail.send","dead-letter-strategy":"at-least-once","overflow":"reject-publish"}`
)

type login struct {
	username string
	password string
}

type rabbitMQ struct {
	container *tcrabbitmq.RabbitMQContainer
	addr      string
}

func startRabbitMQ(t *testing.T) *rabbitMQ {
	t.Helper()
	c, err := tcrabbitmq.Run(t.Context(), rabbitmqImage,
		tcrabbitmq.WithAdminUsername("admin-"+strings.ToLower(rand.Text())),
		tcrabbitmq.WithAdminPassword(rand.Text()),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      filepath.Join("..", "deploy", "rabbitmq", "definitions.json"),
			ContainerFilePath: definitionsFile,
			FileMode:          0o644,
		}),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting RabbitMQ in a container")
	addr, err := c.PortEndpoint(t.Context(), tcrabbitmq.DefaultAMQPPort, "")
	require.NoError(t, err)

	r := &rabbitMQ{container: c, addr: addr}
	r.ctl(t, "import_definitions", definitionsFile)
	r.ctl(t, "set_policy", "--vhost", "/", "--priority", "1", "--apply-to", "queues", "tests.mail.send", `^mail\.send$`, sendPolicy)
	r.ctl(t, "set_policy", "--vhost", "/", "--priority", "1", "--apply-to", "queues", "tests.mail.send.retry", `^mail\.send\.retry$`, retryPolicy)
	return r
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

func (r *rabbitMQ) closeConnections(ctx context.Context) error {
	code, out, err := r.container.Exec(ctx, []string{"rabbitmqctl", "close_all_connections", "closed by the shared cases"}, tcexec.Multiplexed())
	if err != nil {
		return err
	}
	b, err := io.ReadAll(out)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("rabbitmqctl close_all_connections exited %d: %s", code, b)
	}
	return nil
}

func startWireMock(t *testing.T) *url.URL {
	t.Helper()
	mappings, err := filepath.Glob(filepath.Join("..", "deploy", "kind", "wiremock", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, mappings)
	var files []testcontainers.ContainerFile
	for _, m := range mappings {
		files = append(files, testcontainers.ContainerFile{
			HostFilePath:      m,
			ContainerFilePath: "/home/wiremock/mappings/" + filepath.Base(m),
			FileMode:          0o644,
		})
	}

	c, err := testcontainers.Run(t.Context(), wiremockImage,
		testcontainers.WithExposedPorts(wiremockPort),
		testcontainers.WithFiles(files...),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/__admin/health").WithPort(wiremockPort).WithStartupTimeout(2*time.Minute)),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting WireMock in a container")
	endpoint, err := c.PortEndpoint(t.Context(), wiremockPort, "http")
	require.NoError(t, err)
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	return u
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func writeFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}

func TestInit_passes_the_shared_cases_against_RabbitMQ_and_WireMock(t *testing.T) {
	broker := startRabbitMQ(t)
	provider := startWireMock(t)
	mailer := broker.addUser(t, `^$`, `^mail\.(retry|dead|events)$`, `^mail\.send$`)
	cased := broker.addUser(t, `^amq\.gen-.*$`, `^(mail|amq\.gen-.*)$`, `^(mail\.(events|dead|retry)|amq\.gen-.*)$`)

	ca := humustest.NewCA(t)
	dir := t.TempDir()
	probesAddr := freeAddr(t)
	p, status := humustest.Start(t, app.Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(app.Faults)),
		humustest.WithEnv(map[string]string{
			"RABBITMQ_ADDR":           broker.addr,
			"RABBITMQ_USERNAME":       mailer.username,
			"RABBITMQ_PASSWORD_FILE":  writeFile(t, dir, "rabbitmq-password", []byte(mailer.password+"\n")),
			"MAIL_FROM":               "mailer@example.com",
			"MAIL_TRUSTED_PUBLISHERS": cased.username,
			"PROVIDER_URL":            provider.String(),
			"PROVIDER_API_KEY_FILE":   writeFile(t, dir, "provider-api-key", []byte(rand.Text())),
			"PROBES_ADDR":             probesAddr,
			"PROBES_TLS_PEM_FILE":     writeFile(t, dir, "probes.pem", pemOf(t, ca.Issue(t, "127.0.0.1"))),
		}),
	)

	probes := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}},
	}
	require.Eventually(t, func() bool {
		resp, err := probes.Get("https://" + probesAddr + "/health/readiness")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, waitLimit, 100*time.Millisecond, "the mailer didn't become ready")

	target := cases.Target{
		Addr:             broker.addr,
		Vhost:            "/",
		Username:         cased.username,
		Password:         cased.password,
		WireMock:         provider,
		Client:           &http.Client{Timeout: waitLimit},
		ProviderPoint:    "http:" + provider.Host,
		ScenarioKey:      "acme.fault",
		CloseConnections: broker.closeConnections,
		Timeout:          waitLimit,
	}
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, target)
		})
	}

	<-p.Notified(syscall.SIGTERM)
	p.Signal(syscall.SIGTERM)
	select {
	case st := <-status:
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the mailer didn't exit")
	}
}
