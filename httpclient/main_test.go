// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// keyPairPEM returns c's certificate chain and private key, PEM-encoded in
// one file, as serverTLS reads it.
func keyPairPEM(t *testing.T, c *humustest.Certificate) []byte {
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

// caPEM returns the certificate of the CA that issued c, PEM-encoded, as
// trusting reads it.
func caPEM(c *humustest.Certificate) []byte {
	chain := c.TLS().Certificate
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]})
}

func TestInit_calls_payments_for_each_request(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		orders := ca.Issue(t, "orders")
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls.pem":         keyPairPEM(t, orders),
			"payments-ca.pem": caPEM(orders),
		}))
		stop := startPayments(t, h, h.Host(), ca, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "charge for "+r.URL.Path+": paid")
		}))
		defer stop()

		p, status := humustest.Start(t, Init,
			humustest.WithApp(httpserver.Defaults()),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{
				"ADDR":             ":9443",
				"TLS_PEM_FILE":     "tls.pem",
				"PAYMENTS_URL":     "https://payments:8443/v1",
				"PAYMENTS_CA_FILE": "payments-ca.pem",
			}),
		)
		<-h.Listening("orders:9443")

		c := h.HTTPClient()
		c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
		resp, err := c.Get("https://orders:9443/orders/7/payment")
		require.NoError(t, err)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", b)
		require.Equal(t, "charge for /v1/charges/7: paid", string(b))

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})

	t.Run("fails with exit 78 when PAYMENTS_URL is not set", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			_, status := humustest.Start(t, Init,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				humustest.WithEnv(map[string]string{"PAYMENTS_CA_FILE": "payments-ca.pem"}),
			)
			st := <-status
			require.Equal(t, 78, int(st.Code()), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "PAYMENTS_URL")
		})
	})
}
