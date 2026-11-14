// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/fiber/cases"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const addr = "localhost:8443"

func TestInit_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := start(t, telemetry.NoopBuilder)
				c.Run(t, s.target)
				s.stop(t)
			})
		})
	}
}

func TestInit_answers_the_probes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, telemetry.NoopBuilder)
		for _, probe := range []string{"/health/liveness", "/health/readiness"} {
			resp, err := s.target.Client.Get("https://" + addr + probe)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode, probe)
			require.Equal(t, "OK\n", string(body), probe)
		}
		s.stop(t)
	})
}

func TestInit_names_each_span_after_its_route(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tel := humustest.NewTelemetry()
		s := start(t, tel.Builder())
		for _, path := range []string{"/v1/users", "/v1/users/1"} {
			resp, err := s.target.Client.Get("https://" + addr + path)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		}
		s.stop(t)

		var names []string
		for _, s := range tel.Spans() {
			names = append(names, s.Name())
		}
		require.Equal(t, []string{"GET /v1/users", "GET /v1/users/{id}"}, names)
	})
}

func TestInit_writes_the_Fiber_request_log_to_the_process_stdout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, telemetry.NoopBuilder)
		resp, err := s.target.Client.Get("https://" + addr + "/v1/users")
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		s.stop(t)

		require.Regexp(t, `\| 200 \| .* \| GET +\| /v1/users`, s.p.Stdout())
	})
}

func TestInit_requires_an_address_and_a_certificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := httpservertest.Start(t, Init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config ADDR: not found")
		require.ErrorContains(t, st.Cause(), "config TLS_CERT_FILE: not found")
	})
}

type service struct {
	target cases.Target
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

func start(t *testing.T, tel telemetry.Builder) service {
	t.Helper()
	ca := humustest.NewCA(t)
	certPEM, keyPEM := pemPair(t, ca.Issue(t, "localhost"))
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"tls/tls.crt": certPEM,
		"tls/tls.key": keyPEM,
	}))
	host := h.Host()
	host.Listen = withClientAddr(host.Listen)
	p, status := httpservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(tel)),
		humustest.WithEnv(map[string]string{
			"ADDR":          ":8443",
			"TLS_CERT_FILE": "tls/tls.crt",
			"TLS_KEY_FILE":  "tls/tls.key",
		}),
	)
	select {
	case <-h.Listening(addr):
	case st := <-status:
		t.Fatalf("the service exited %d: %v", st.Code(), st.Cause())
	}
	return service{
		target: cases.Target{URL: &url.URL{Scheme: "https", Host: addr}, Client: httpservertest.NewClient(h, ca.Pool())},
		p:      p,
		status: status,
	}
}

func (s service) stop(t *testing.T) {
	t.Helper()
	<-s.p.Notified(syscall.SIGTERM)
	s.p.Signal(syscall.SIGTERM)
	st := <-s.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

func withClientAddr(listen func(network, address string) (net.Listener, error)) func(network, address string) (net.Listener, error) {
	return func(network, address string) (net.Listener, error) {
		l, err := listen(network, address)
		if err != nil {
			return nil, err
		}
		return clientAddrListener{l}, nil
	}
}

type clientAddrListener struct{ net.Listener }

func (l clientAddrListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return clientAddrConn{c}, nil
}

type clientAddrConn struct{ net.Conn }

func (clientAddrConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 49152}
}

func pemPair(t *testing.T, c *humustest.Certificate) (certPEM, keyPEM []byte) {
	t.Helper()
	cert := c.TLS()
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}
