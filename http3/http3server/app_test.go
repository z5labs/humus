// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server_test

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
	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/http/httpserver/httpservertest"
	"github.com/z5labs/humus/http3/http3server"
	"github.com/z5labs/humus/http3/http3server/http3servertest"
	"github.com/z5labs/humus/humustest"
)

// Init is the end user's Init of the proposal's revision 2: one handler,
// served over HTTP/2 by httpserver and over HTTP/3 by http3server, on one
// host:port and with one TLS config, from ADDR and TLS_PEM_FILE.
func Init(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	type servers struct {
		tcp httpserver.Settings
		udp http3server.Settings
	}
	settings, err := config.Struct(func(r *config.Resolver) servers {
		addr := r.Get(env("ADDR").Validate(http3server.CheckAddr))
		tlsConfig := r.Get(env("TLS_PEM_FILE").
			Parse(readFile(h.Filesystem)).
			Parse(serverTLS).
			Validate(httpserver.CheckTLS).
			Validate(http3server.CheckTLS))

		tcp := httpserver.DefaultSettings()
		tcp.Addr, tcp.TLS = addr, tlsConfig
		udp := http3server.DefaultSettings()
		udp.Addr, udp.TLS = addr, tlsConfig
		return servers{tcp: tcp, udp: udp}
	}).Read(ctx)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello/{name}", greet)

	tcp, err := httpserver.NewService(ctx, s, h, settings.tcp, mux)
	if err != nil {
		return nil, err
	}
	udp, err := http3server.NewService(ctx, s, h, settings.udp, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{tcp, udp}, nil
}

// greet answers GET /hello/{name} with "hello, " and the name.
func greet(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "hello, "+r.PathValue("name"))
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

func TestInit_serves_HTTP3_beside_HTTP2(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, hosttest.WithFiles(map[string][]byte{
			"tls.pem": pemFile(t, ca.Issue(t, "api.internal")),
		}))
		tel := humustest.NewTelemetry()
		p, status := http3servertest.Start(t, h.Host(), Init,
			humustest.WithApp(humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"ADDR":         "api.internal:8443",
				"TLS_PEM_FILE": "tls.pem",
			}),
		)
		synctest.Wait()

		hello := func(c *http.Client) string {
			resp, err := c.Get("https://api.internal:8443/hello/humus")
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.Proto + " " + string(body)
		}
		require.Equal(t, "HTTP/2.0 hello, humus", hello(httpservertest.NewClient(h, ca.Pool())))
		require.Equal(t, "HTTP/3.0 hello, humus", hello(http3servertest.NewClient(t, h, ca.Pool())))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "%v", st.Cause())
		require.Empty(t, p.Stderr())

		var versions []string
		for _, span := range tel.Spans() {
			for _, a := range span.Attributes() {
				if a.Key == "network.protocol.version" {
					versions = append(versions, a.Value.AsString())
				}
			}
		}
		require.ElementsMatch(t, []string{"2.0", "3.0"}, versions)
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
