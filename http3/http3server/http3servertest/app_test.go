// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3servertest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/http3/http3server/http3servertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// Init is the end user's Init of an HTTP/3 service: Addr and TLS have no
// default, and the certificate is a PEM file named by TLS_PEM_FILE. It
// serves greet on GET /hello/{name}.
func Init(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) http3server.Settings {
		settings := http3server.DefaultSettings()
		settings.Addr = r.Get(env("ADDR").Validate(http3server.CheckAddr))
		settings.TLS = r.Get(env("TLS_PEM_FILE").Parse(readFile(h.Filesystem)).Parse(serverTLS).Validate(http3server.CheckTLS))
		return settings
	}).Read(ctx)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello/{name}", greet)
	svc, err := http3server.NewService(ctx, s, h, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.Filesystem) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

// serverTLS returns a server's TLS config for a PEM file that holds its
// certificate chain and its private key.
func serverTLS(pemBytes []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(pemBytes, pemBytes)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func TestInit_serves_HTTP3(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, hosttest.WithFiles(map[string][]byte{
			"tls.pem": pemFile(t, ca.Issue(t, "localhost")),
		}))
		p, status := http3servertest.Start(t, h.Host(), Init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"ADDR": ":8443", "TLS_PEM_FILE": "tls.pem"}),
		)
		synctest.Wait()

		resp, err := http3servertest.NewClient(t, h, ca.Pool()).Get("https://localhost:8443/hello/humus")
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, "HTTP/3.0", resp.Proto)
		require.Equal(t, "hello, humus", string(body))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func TestInit_without_an_address_or_a_certificate_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := http3servertest.Start(t, humustest.NewHost(t).Host(), Init)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config ADDR: not found")
		require.ErrorContains(t, st.Cause(), "config TLS_PEM_FILE: not found")
	})
}

// pemFile returns c's certificate chain and private key as one PEM file.
func pemFile(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var b []byte
	for _, der := range cert.Certificate {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}
