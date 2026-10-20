//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package functional runs the databases controller's shared cases against the kind overlay that up.sh brings up.
package functional

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/z5labs/humus/example/databases/cases"
)

const (
	namespace   = "databases"
	prefix      = "databases-cases-"
	annotation  = "postgres.example.com/failure-scenario"
	scenarioKey = "acme.fault"
	timeout     = 2 * time.Minute
)

var kubeContext = flag.String("context", "kind-kind", "the kubectl context of the cluster up.sh brought the databases controller up on")

var forwarding = regexp.MustCompile(`^Forwarding from (127\.0\.0\.1:\d+) -> \d+$`)

func TestDatabases_passes_the_shared_cases_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", "databases"} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

	target := sharedTarget(t)
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
}

func sharedTarget(t *testing.T) cases.Target {
	t.Helper()
	cfg := kubeconfig(t)
	return cases.Target{
		Config:      cfg,
		Prefix:      prefix,
		Annotation:  annotation,
		ScenarioKey: scenarioKey,
		Postgres:    adminDSN(t, cfg, forward(t, "postgres", 5432)),
		Timeout:     timeout,
	}
}

func kubeconfig(t *testing.T) *rest.Config {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: *kubeContext},
	).ClientConfig()
	require.NoError(t, err, "the kubeconfig's context %s", *kubeContext)
	return cfg
}

func adminDSN(t *testing.T, cfg *rest.Config, forwarded string) string {
	t.Helper()
	kc, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	s, err := kc.CoreV1().Secrets(namespace).Get(t.Context(), "postgres", metav1.GetOptions{})
	require.NoError(t, err, "the secret %s/postgres", namespace)
	password := s.Data["password"]
	require.NotEmpty(t, password, "the secret %s/postgres has no password", namespace)

	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword("postgres", string(password)),
		Host:     forwarded,
		Path:     "/postgres",
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

func command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "kubectl", append([]string{"--context", *kubeContext, "--namespace", namespace}, args...)...)
}

func kubectl(t *testing.T, args ...string) {
	t.Helper()
	_, err := command(t.Context(), args...).Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		require.NoError(t, err, "kubectl %s: %s", strings.Join(args, " "), exit.Stderr)
	}
	require.NoError(t, err, "kubectl %s", strings.Join(args, " "))
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
