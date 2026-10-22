//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package functional runs the mailer's shared cases against the kind overlay that up.sh brings up.
package functional

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/mailer/cases"
)

const (
	namespace     = "mailer"
	providerPoint = "http:wiremock:443"
	scenarioKey   = "acme.fault"
	timeout       = 2 * time.Minute
)

var kubeContext = flag.String("context", "kind-"+cmp.Or(os.Getenv("KIND_CLUSTER_NAME"), "kind"), "the kubectl context of the cluster up.sh brought the mailer up on")

var forwarding = regexp.MustCompile(`^Forwarding from (127\.0\.0\.1:\d+) -> \d+$`)

func TestMailer_passes_the_shared_cases_on_kind(t *testing.T) {
	kubectl(t, "rollout", "status", "deployment/mailer", "--timeout", "5m")

	target := sharedTarget(t)
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
}

func sharedTarget(t *testing.T) cases.Target {
	t.Helper()
	brokerCA := pool(t, "mailer-rabbitmq-ca")
	providerCA := pool(t, "mailer-provider-ca")
	broker := forward(t, "rabbitmq", 5671)
	provider := forward(t, "wiremock", 443)

	return cases.Target{
		Addr:     broker,
		Vhost:    "/",
		TLS:      &tls.Config{RootCAs: brokerCA, ServerName: "rabbitmq", MinVersion: tls.VersionTLS12},
		Username: secret(t, "cases-rabbitmq", "username"),
		Password: secret(t, "cases-rabbitmq", "password"),
		WireMock: &url.URL{Scheme: "https", Host: provider},
		Client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: providerCA, ServerName: "wiremock", MinVersion: tls.VersionTLS12},
			},
		},
		ProviderPoint:    providerPoint,
		ScenarioKey:      scenarioKey,
		CloseConnections: closeConnections,
		Timeout:          timeout,
	}
}

func command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "kubectl", append([]string{"--context", *kubeContext, "--namespace", namespace}, args...)...)
}

func run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := command(ctx, args...).Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil, fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, exit.Stderr)
	}
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func kubectl(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := run(t.Context(), args...)
	require.NoError(t, err)
	return out
}

func secret(t *testing.T, name, key string) string {
	t.Helper()
	encoded := kubectl(t, "get", "secret", name, "--output", "jsonpath={.data."+key+"}")
	require.NotEmpty(t, encoded, "secret %s has no %s", name, key)
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	require.NoError(t, err)
	return string(decoded)
}

func pool(t *testing.T, configMap string) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	require.True(t, p.AppendCertsFromPEM(kubectl(t, "get", "configmap", configMap, "--output", `jsonpath={.data.ca\.crt}`)), "configmap %s holds no CA certificate", configMap)
	return p
}

func closeConnections(ctx context.Context) error {
	_, err := run(ctx, "exec", "deployment/rabbitmq", "--", "rabbitmqctl", "close_all_connections", "closed by the shared cases")
	return err
}

type output struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func forward(t *testing.T, service string, port int) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := command(ctx, "port-forward", "--address", "127.0.0.1", "service/"+service, fmt.Sprintf(":%d", port))
	r, w := io.Pipe()
	stderr := &output{}
	cmd.Stdout = w
	cmd.Stderr = stderr
	require.NoError(t, cmd.Start(), "kubectl port-forward service/%s", service)

	addrs := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(r)
		for s.Scan() {
			if m := forwarding.FindStringSubmatch(s.Text()); m != nil {
				select {
				case addrs <- m[1]:
				default:
				}
			}
		}
		_, _ = io.Copy(io.Discard, r)
	}()
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		_ = w.Close()
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		<-exited
		if t.Failed() {
			t.Logf("kubectl port-forward service/%s: %v\n%s", service, waitErr, stderr)
		}
	})

	select {
	case addr := <-addrs:
		return addr
	case <-exited:
		require.FailNowf(t, "exited", "kubectl port-forward service/%s exited: %v: %s", service, waitErr, stderr)
		return ""
	case <-time.After(time.Minute):
		require.FailNowf(t, "timed out", "kubectl port-forward service/%s didn't report its address: %s", service, stderr)
		return ""
	}
}
