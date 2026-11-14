// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const probesAddr = "localhost:8443"

func TestInit_requires_the_AMQP_URI_and_the_probes_settings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		for _, name := range []string{"AMQP_URI", "PROBES_ADDR", "PROBES_TLS_CERT_FILE"} {
			require.ErrorContains(t, st.Cause(), "config "+name+": not found")
		}
	})
}

func TestInit_rejects_an_AMQP_URI_it_cannot_log_in_with(t *testing.T) {
	password := rand.Text()
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{"not AMQP", "http://invoicer:" + password + "@rabbitmq.internal/", "config AMQP_URI: is not an AMQP URI"},
		{"no login", "amqp://rabbitmq.internal/", "config AMQP_URI: has no login of its own"},
		{"guest", "amqp://guest:" + password + "@rabbitmq.internal/", "config AMQP_URI: has no login of its own"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, status := humustest.Start(t, Init,
					humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
					humustest.WithEnv(map[string]string{"AMQP_URI": c.uri}),
				)

				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), c.want)
				require.NotContains(t, st.Cause().Error(), password)
			})
		})
	}
}

func TestInit_rejects_a_retry_setting_that_is_not_positive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{"RETRY_ATTEMPTS": "0"}),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config RETRY_ATTEMPTS: must be positive")
	})
}

func TestInit_waits_for_the_broker_alive_but_not_ready(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		certPEM, keyPEM := pemPair(t, ca.Issue(t, "localhost"))
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls/tls.crt": certPEM,
			"tls/tls.key": keyPEM,
		}))
		env := map[string]string{
			"AMQP_URI":             "amqp://invoicer:" + rand.Text() + "@rabbitmq.internal:5672/",
			"PROBES_ADDR":          probesAddr,
			"PROBES_TLS_CERT_FILE": "tls/tls.crt",
			"PROBES_TLS_KEY_FILE":  "tls/tls.key",
		}
		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(env),
		)
		<-h.Listening(probesAddr)
		probes := h.HTTPClient()
		probes.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}

		time.Sleep(time.Minute)
		synctest.Wait()

		require.Equal(t, http.StatusOK, probe(t, probes, "/health/liveness"))
		require.Equal(t, http.StatusServiceUnavailable, probe(t, probes, "/health/readiness"))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func probe(t *testing.T, client *http.Client, path string) int {
	t.Helper()
	resp, err := client.Get("https://" + probesAddr + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
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

func TestFromURI_fills_the_connection_from_an_AMQP_URI(t *testing.T) {
	password := rand.Text()
	c, err := fromURI("amqps://invoicer:" + password + "@rabbitmq.internal/shop")
	require.NoError(t, err)
	require.Equal(t, "rabbitmq.internal:5671", c.Addr)
	require.Equal(t, "shop", c.Vhost)
	require.NotNil(t, c.TLS)
	require.Contains(t, c.SASL.Response(), password)

	c, err = fromURI("amqp://invoicer:" + password + "@[::1]:5673")
	require.NoError(t, err)
	require.Equal(t, "[::1]:5673", c.Addr)
	require.Equal(t, "/", c.Vhost)
	require.Nil(t, c.TLS)
}
