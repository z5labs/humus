//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package functional runs the wallet's shared cases against the kind overlay that up.sh brings up.
package functional

import (
	"bufio"
	"cmp"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"

	"github.com/z5labs/humus/example/grpc/wallet/cases"
)

const (
	namespace   = "wallet"
	wallet      = "wallet"
	ledger      = "ledger"
	port        = 8443
	scenarioKey = "acme.fault"
	timeout     = 3 * time.Minute
	pollEvery   = time.Second
)

var kubeContext = flag.String("context", "kind-"+cmp.Or(os.Getenv("KIND_CLUSTER_NAME"), "kind"), "the kubectl context of the cluster up.sh brought the wallet and the ledger up on")

var forwarding = regexp.MustCompile(`^Forwarding from (127\.0\.0\.1:\d+) -> \d+$`)

func TestWallet_passes_the_shared_cases_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", "wiremock", ledger, wallet} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

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
	env := settings(t, wallet)

	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(secret(t, "cases-tls", "ca.crt")), "secret cases-tls holds no CA certificate")
	pemFile := secret(t, "cases-tls", "tls.pem")
	cert, err := tls.X509KeyPair(pemFile, pemFile)
	require.NoError(t, err, "secret cases-tls's tls.pem")

	walletPort := newForward(t, wallet)
	ledgerPort := newForward(t, ledger)
	return cases.Target{
		Wallet:                   connect(t, wallet, walletPort, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}),
		Ledger:                   connect(t, ledger, ledgerPort, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}),
		LedgerWithoutCertificate: connect(t, ledger, ledgerPort, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}),
		Token:                    signer(t, env.get(t, "TOKEN_ISSUER"), env.get(t, "TOKEN_AUDIENCE")),
		RolloutWallet:            walletPort.rollout,
		RolloutLedger:            ledgerPort.rollout,
		ScenarioKey:              scenarioKey,
		TrustScope:               env.get(t, "FAULT_TRUST_SCOPE"),
		Timeout:                  timeout,
	}
}

func connect(t *testing.T, service string, f *forward, cfg *tls.Config) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+service,
		grpc.WithContextDialer(f.dial),
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 100 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: pollEvery},
			MinConnectTimeout: 20 * time.Second,
		}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func signer(t *testing.T, issuer, audience string) func(subject string, scopes ...string) (string, error) {
	t.Helper()
	block, _ := pem.Decode(secret(t, "token-signing-key", "key.pem"))
	require.NotNil(t, block, "secret token-signing-key holds no PEM block")
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err, "secret token-signing-key's key.pem")

	jwk := jose.JSONWebKey{Key: key, Algorithm: string(jose.ES256), Use: "sig"}
	thumbprint, err := jwk.Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	jwk.KeyID = base64.RawURLEncoding.EncodeToString(thumbprint)
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jwk}, (&jose.SignerOptions{}).WithType("JWT"))
	require.NoError(t, err)

	return func(subject string, scopes ...string) (string, error) {
		now := time.Now()
		claims := struct {
			josejwt.Claims
			Scope string `json:"scope,omitempty"`
		}{
			Claims: josejwt.Claims{
				Issuer:    issuer,
				Audience:  josejwt.Audience{audience},
				Subject:   subject,
				IssuedAt:  josejwt.NewNumericDate(now),
				NotBefore: josejwt.NewNumericDate(now),
				Expiry:    josejwt.NewNumericDate(now.Add(time.Hour)),
			},
			Scope: strings.Join(scopes, " "),
		}
		return josejwt.Signed(sig).Claims(claims).Serialize()
	}
}

type environment map[string]string

func settings(t *testing.T, deployment string) environment {
	t.Helper()
	name := kubectl(t, "get", "deployment/"+deployment, "--output", "jsonpath={.spec.template.spec.containers[0].envFrom[0].configMapRef.name}")
	require.NotEmpty(t, name, "deployment/%s takes its settings from no ConfigMap", deployment)
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

func secret(t *testing.T, name, key string) []byte {
	t.Helper()
	encoded := kubectl(t, "get", "secret", name, "--output", "jsonpath={.data."+strings.ReplaceAll(key, ".", `\.`)+"}")
	require.NotEmpty(t, encoded, "secret %s has no %s", name, key)
	decoded, err := base64.StdEncoding.DecodeString(string(encoded))
	require.NoError(t, err)
	return decoded
}

func pods(ctx context.Context, service string) ([]string, error) {
	out, err := run(ctx, "get", "pods", "--selector", "app.kubernetes.io/name="+service, "--output", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

type forward struct {
	service string

	mu       sync.Mutex
	current  *portForward
	forwards []*portForward
}

func newForward(t *testing.T, service string) *forward {
	t.Helper()
	f := &forward{service: service}
	t.Cleanup(func() {
		f.mu.Lock()
		forwards := slices.Clone(f.forwards)
		f.mu.Unlock()
		for _, p := range forwards {
			p.stop()
			if t.Failed() {
				t.Logf("kubectl port-forward service/%s: %v\n%s", service, p.waitErr, p.stderr)
			}
		}
	})
	require.NoError(t, f.again(t.Context()))
	return f
}

func (f *forward) dial(ctx context.Context, _ string) (net.Conn, error) {
	f.mu.Lock()
	addr := f.current.addr
	f.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (f *forward) again(ctx context.Context) error {
	p, err := startPortForward(ctx, f.service, port)
	if p != nil {
		f.mu.Lock()
		f.forwards = append(f.forwards, p)
		f.mu.Unlock()
	}
	if err != nil {
		return err
	}
	f.mu.Lock()
	previous := f.current
	f.current = p
	f.mu.Unlock()
	if previous != nil {
		previous.stop()
	}
	return nil
}

func (f *forward) rollout(ctx context.Context) error {
	deployment := "deployment/" + f.service
	before, err := pods(ctx, f.service)
	if err != nil {
		return err
	}
	if _, err := run(ctx, "rollout", "restart", deployment); err != nil {
		return err
	}
	if _, err := run(ctx, "rollout", "status", deployment, "--timeout", "5m"); err != nil {
		return err
	}
	for {
		now, err := pods(ctx, f.service)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(before, func(name string) bool { return slices.Contains(now, name) }) {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s's old pods to go: %w", deployment, context.Cause(ctx))
		case <-time.After(pollEvery):
		}
	}
	return f.again(ctx)
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

type portForward struct {
	addr    string
	cancel  context.CancelFunc
	exited  chan struct{}
	waitErr error
	stderr  *output
}

func (p *portForward) stop() {
	p.cancel()
	<-p.exited
}

func startPortForward(ctx context.Context, service string, remotePort int) (*portForward, error) {
	procCtx, cancel := context.WithCancel(context.Background())
	cmd := command(procCtx, "port-forward", "--address", "127.0.0.1", "service/"+service, fmt.Sprintf(":%d", remotePort))
	r, w := io.Pipe()
	p := &portForward{cancel: cancel, exited: make(chan struct{}), stderr: &output{}}
	cmd.Stdout = w
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("kubectl port-forward service/%s: %w", service, err)
	}

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
	go func() {
		p.waitErr = cmd.Wait()
		_ = w.Close()
		close(p.exited)
	}()

	select {
	case p.addr = <-addrs:
		return p, nil
	case <-p.exited:
		return p, fmt.Errorf("kubectl port-forward service/%s exited: %w: %s", service, p.waitErr, p.stderr)
	case <-time.After(time.Minute):
		p.stop()
		return p, fmt.Errorf("kubectl port-forward service/%s didn't report its address: %s", service, p.stderr)
	case <-ctx.Done():
		p.stop()
		return p, fmt.Errorf("kubectl port-forward service/%s: %w", service, context.Cause(ctx))
	}
}
