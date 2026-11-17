//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package functional runs the polls walkthrough's shared cases against the kind overlay that up.sh brings up.
package functional

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/http/polls/cases"
)

const (
	namespace   = "polls"
	polls       = "polls"
	port        = 8443
	scenarioKey = "acme.fault"
	timeout     = 2 * time.Minute
)

var kubeContext = flag.String("context", "kind-kind", "the kubectl context of the cluster up.sh brought the polls service up on")

var forwarding = regexp.MustCompile(`^Forwarding from (127\.0\.0\.1:\d+) -> \d+$`)

func TestPolls_passes_the_shared_cases_on_its_two_pods_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", polls} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	target := sharedTarget(t)
	t.Run("cases", func(t *testing.T) {
		for _, c := range cases.Cases {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
}

func sharedTarget(t *testing.T) cases.Target {
	t.Helper()
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(secret(t, "polls-cases", "ca.crt")), "secret polls-cases holds no CA certificate")
	certificate, err := tls.X509KeyPair(secret(t, "polls-cases", "tls.crt"), secret(t, "polls-cases", "tls.key"))
	require.NoError(t, err, "secret polls-cases' tls.crt and tls.key")

	var replicas []*url.URL
	for _, pod := range readyPods(t) {
		replicas = append(replicas, &url.URL{Scheme: "https", Host: forward(t, "pod/"+pod, port)})
	}

	return cases.Target{
		Replicas:       replicas,
		Browser:        browser(t, newTransport(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})),
		TrustedBrowser: browser(t, newTransport(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})),
		ScenarioKey:    scenarioKey,
		Timeout:        timeout,
	}
}

func newTransport(cfg *tls.Config) *http.Transport {
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	return &http.Transport{TLSClientConfig: cfg, Protocols: &protocols}
}

func browser(t *testing.T, transport *http.Transport) func() *http.Client {
	t.Helper()
	t.Cleanup(transport.CloseIdleConnections)
	return func() *http.Client {
		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		return &http.Client{Transport: transport, Jar: jar}
	}
}

func readyPods(t *testing.T) []string {
	t.Helper()
	var d struct {
		Spec struct {
			Replicas int `json:"replicas"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal(kubectl(t, "get", "deployment/"+polls, "--output", "json"), &d))
	require.GreaterOrEqual(t, d.Spec.Replicas, 2, "deployment/%s's replicas", polls)

	var list struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(kubectl(t, "get", "pods", "--selector", "app.kubernetes.io/name="+polls, "--output", "json"), &list))

	var ready []string
	for _, p := range list.Items {
		if p.Metadata.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = append(ready, p.Metadata.Name)
			}
		}
	}
	require.Len(t, ready, d.Spec.Replicas, "deployment/%s's ready pods", polls)
	return ready
}

func command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "kubectl", append([]string{"--context", *kubeContext, "--namespace", namespace}, args...)...)
}

func kubectl(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := command(t.Context(), args...).Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		require.NoError(t, err, "kubectl %s: %s", strings.Join(args, " "), exit.Stderr)
	}
	require.NoError(t, err, "kubectl %s", strings.Join(args, " "))
	return out
}

func secret(t *testing.T, name, key string) []byte {
	t.Helper()
	encoded := kubectl(t, "get", "secret", name, "--output", "jsonpath={.data."+strings.ReplaceAll(key, ".", `\.`)+"}")
	require.NotEmpty(t, encoded, "secret %s has no %s", name, key)
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	require.NoError(t, err)
	return decoded
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

func forward(t *testing.T, resource string, port int) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := command(ctx, "port-forward", "--address", "127.0.0.1", resource, fmt.Sprintf(":%d", port))
	r, w := io.Pipe()
	stderr := &output{}
	cmd.Stdout = w
	cmd.Stderr = stderr
	require.NoError(t, cmd.Start(), "kubectl port-forward %s", resource)

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
			t.Logf("kubectl port-forward %s: %v\n%s", resource, waitErr, stderr)
		}
	})

	select {
	case addr := <-addrs:
		return addr
	case <-exited:
		require.FailNowf(t, "exited", "kubectl port-forward %s exited: %v: %s", resource, waitErr, stderr)
		return ""
	case <-time.After(time.Minute):
		require.FailNowf(t, "timed out", "kubectl port-forward %s didn't report its address: %s", resource, stderr)
		return ""
	}
}
