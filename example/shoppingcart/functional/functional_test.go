//go:build functional

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package functional runs the shoppingcart's shared cases against the kind overlay that up.sh brings up.
package functional

import (
	"bufio"
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
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/example/shoppingcart/cases"
)

const (
	namespace   = "shoppingcart"
	cart        = "cart"
	catalog     = "catalog"
	port        = 8443
	scenarioKey = "acme.fault"
	timeout     = 2 * time.Minute
)

var kubeContext = flag.String("context", "kind-kind", "the kubectl context of the cluster up.sh brought the shoppingcart up on")

var forwarding = regexp.MustCompile(`^Forwarding from (127\.0\.0\.1:\d+) -> \d+$`)

func TestShoppingcart_passes_the_shared_cases_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", "wiremock", catalog, cart} {
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
	cartEnv := settings(t, cart)
	catalogEnv := settings(t, catalog)
	issuer := cartEnv.get(t, "JWT_ISSUER")
	require.Equal(t, issuer, catalogEnv.get(t, "JWT_ISSUER"), "the cart and the catalog trust different issuers")

	client := newClient(t)
	return cases.Target{
		Cart:          &url.URL{Scheme: "https", Host: forward(t, cart, port)},
		CartClient:    client,
		Catalog:       &url.URL{Scheme: "https", Host: forward(t, catalog, port)},
		CatalogClient: client,
		Token:         signer(t, issuer, cartEnv.get(t, "JWT_AUDIENCE"), catalogEnv.get(t, "JWT_AUDIENCE")),
		CatalogPoint:  pointOf(t, cartEnv.get(t, "CATALOG_URL")),
		ScenarioKey:   scenarioKey,
		TrustScope:    cartEnv.get(t, "FAULT_TRUST_SCOPE"),
		Timeout:       timeout,
	}
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(secret(t, "cart-ca", "ca.crt")), "secret cart-ca holds no CA certificate")
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}}
}

func signer(t *testing.T, issuer string, audiences ...string) func(subject string, scopes ...string) (string, error) {
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
				Audience:  audiences,
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

func pointOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err, "CATALOG_URL")
	p := u.Port()
	if p == "" {
		p = "443"
	}
	return "http:" + net.JoinHostPort(u.Hostname(), p)
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
