//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package functional runs the order status processor's shared cases against the kind overlay that up.sh brings up.
package functional

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/z5labs/humus/example/kafka/orderstatus/cases"
	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	namespace   = "orderstatus"
	deployment  = "deployment/orderstatus"
	broker      = "kafka:9093"
	scenarioKey = "acme.fault"
	timeout     = 2 * time.Minute
)

var kubeContext = flag.String("context", "kind-"+cmp.Or(os.Getenv("KIND_CLUSTER_NAME"), "kind"), "the kubectl context of the cluster up.sh brought the order status processor up on")

var forwarding = regexp.MustCompile(`^Forwarding from (127\.0\.0\.1:\d+) -> \d+$`)

func TestOrderStatus_passes_the_shared_cases_on_kind(t *testing.T) {
	kubectl(t, "rollout", "status", deployment, "--timeout", "5m")

	target := sharedTarget(t)
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, target)
		})
	}
}

func sharedTarget(t *testing.T) cases.Target {
	t.Helper()
	env := settings(t)
	codec, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)

	return cases.Target{
		Kafka:           []kgo.Opt{kgo.SeedBrokers(broker), kgo.Dialer(dialer(t, forward(t, "kafka", 9093)))},
		EventsTopic:     env.get(t, "KAFKA_EVENTS_TOPIC"),
		StatusTopic:     env.get(t, "KAFKA_STATUS_TOPIC"),
		RetryTopic:      env.get(t, "KAFKA_RETRY_TOPIC"),
		DeadLetterTopic: env.get(t, "KAFKA_DEAD_LETTER_TOPIC"),
		Group:           env.get(t, "KAFKA_GROUP_ID"),
		Scenario:        codec,
		ScenarioKey:     scenarioKey,
		RetryAttempts:   env.attempts(t, "RETRY_ATTEMPTS"),
		RetryMinDelay:   env.duration(t, "RETRY_MIN_DELAY"),
		RetryMaxDelay:   env.duration(t, "RETRY_MAX_DELAY"),
		Timeout:         timeout,
		Rollout:         rollout,
	}
}

type environment map[string]string

func settings(t *testing.T) environment {
	t.Helper()
	name := kubectl(t, "get", deployment, "--output", "jsonpath={.spec.template.spec.containers[0].envFrom[0].configMapRef.name}")
	require.NotEmpty(t, name, "%s takes its settings from no ConfigMap", deployment)
	var cm struct {
		Data environment `json:"data"`
	}
	require.NoError(t, json.Unmarshal(kubectl(t, "get", "configmap", string(name), "--output", "json"), &cm))
	return cm.Data
}

func (e environment) get(t *testing.T, name string) string {
	t.Helper()
	v, ok := e[name]
	require.True(t, ok, "%s is not set", name)
	return v
}

func (e environment) attempts(t *testing.T, name string) int {
	t.Helper()
	n, err := strconv.Atoi(e.get(t, name))
	require.NoError(t, err, name)
	return n
}

func (e environment) duration(t *testing.T, name string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(e.get(t, name))
	require.NoError(t, err, name)
	return d
}

func dialer(t *testing.T, forwarded string) func(ctx context.Context, network, host string) (net.Conn, error) {
	t.Helper()
	pem := []byte(secret(t, "cases-kafka", "tls.pem"))
	cert, err := tls.X509KeyPair(pem, pem)
	require.NoError(t, err, "secret cases-kafka's tls.pem")
	ca := x509.NewCertPool()
	require.True(t, ca.AppendCertsFromPEM([]byte(secret(t, "cases-kafka", "ca.crt"))), "secret cases-kafka holds no CA certificate")

	d := &tls.Dialer{Config: &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      ca,
		ServerName:   "kafka",
		MinVersion:   tls.VersionTLS12,
	}}
	return func(ctx context.Context, network, host string) (net.Conn, error) {
		if host != broker {
			return nil, fmt.Errorf("no port forward to %s, only to %s", host, broker)
		}
		return d.DialContext(ctx, network, forwarded)
	}
}

func rollout(ctx context.Context) error {
	if _, err := run(ctx, "rollout", "restart", deployment); err != nil {
		return err
	}
	_, err := run(ctx, "rollout", "status", deployment, "--timeout", "5m")
	return err
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
	encoded := kubectl(t, "get", "secret", name, "--output", "jsonpath={.data."+strings.ReplaceAll(key, ".", `\.`)+"}")
	require.NotEmpty(t, encoded, "secret %s has no %s", name, key)
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	require.NoError(t, err)
	return string(decoded)
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
