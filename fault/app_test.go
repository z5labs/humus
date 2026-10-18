// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package fault_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/config"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run Init and Faults, from example_app_test.go, as
// a whole App inside a synctest bubble, on a humustest Host, next to a fake
// payments dependency. The test is the edge, and the mesh: its CA issues
// every certificate, so a request sent with one is a caller in the mesh.

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
// certPool reads it.
func caPEM(c *humustest.Certificate) []byte {
	chain := c.TLS().Certificate
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]})
}

// charges records whether each request payments served carried a
// scenario.
type charges struct {
	mu        sync.Mutex
	scenarios []bool
}

func (c *charges) seen() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scenarios
}

// startPayments runs a fake payments dependency on h, at payments:8443,
// over TLS with a certificate from ca, recording what it serves in c. It
// returns a function that stops it.
func startPayments(t *testing.T, h *humustest.Host, ca *humustest.CA, c *charges) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "payments")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := baggage.Parse(r.Header.Get("baggage"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.scenarios = append(c.scenarios, b.Member("acme.fault").Key() != "")
		c.mu.Unlock()
		_, _ = io.WriteString(w, "paid")
	})
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("payments:8443")
	return func() {
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}

// ordersEnv is the environment orders runs with: its own certificate, the
// mesh's CA, and payments.
var ordersEnv = map[string]string{
	"ADDR":             ":9443",
	"TLS_PEM_FILE":     "tls.pem",
	"MESH_CA_FILE":     "ca.pem",
	"PAYMENTS_URL":     "https://payments:8443",
	"PAYMENTS_CA_FILE": "ca.pem",
}

// edge sends GET /orders/7/payment to orders, as a caller in the mesh when
// cert is set, its baggage carrying s, written with the service's codec.
func edge(t *testing.T, h *humustest.Host, ca *humustest.CA, cert *humustest.Certificate, s fit.Scenario) int {
	t.Helper()
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	ctx, err := fit.ContextWithScenario(t.Context(), codec, s)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://orders:9443/orders/7/payment", nil)
	require.NoError(t, err)
	propagation.Baggage{}.Inject(ctx, propagation.HeaderCarrier(req.Header))

	c := h.HTTPClient()
	cfg := &tls.Config{RootCAs: ca.Pool()}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{cert.TLS()}
	}
	c.Transport.(*http.Transport).TLSClientConfig = cfg
	resp, err := c.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

func TestInit_trusts_the_mesh_and_forwards_to_payments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		orders := ca.Issue(t, "orders")
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls.pem": keyPairPEM(t, orders),
			"ca.pem":  caPEM(orders),
		}))
		var payments charges
		defer startPayments(t, h, ca, &payments)()

		p, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithApp(humus.WithFaults(Faults)),
			humustest.WithEnv(ordersEnv),
		)
		<-h.Listening("orders:9443")

		abort := fit.Scenario{Rules: []fit.Rule{{Point: "http:payments:8443", Fault: fit.Fault{Abort: true}}}}
		spare := fit.Scenario{Rules: []fit.Rule{{Point: "http:ledger:8443", Fault: fit.Fault{Abort: true}}}}
		mesh := ca.Issue(t, "checkout")

		require.Equal(t, http.StatusBadGateway, edge(t, h, ca, mesh, abort), "a caller in the mesh aborts payments")
		require.Empty(t, payments.seen(), "an aborted call reached payments")

		require.Equal(t, http.StatusOK, edge(t, h, ca, nil, abort), "the scenario of a caller outside the mesh is stripped")
		require.Equal(t, http.StatusOK, edge(t, h, ca, mesh, spare))
		require.Equal(t, []bool{false, true}, payments.seen(), "only the trusted scenario is forwarded")

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

func TestFaults_a_bad_FAULT_MAX_DELAY_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init,
			humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(Faults)),
			humustest.WithEnv(map[string]string{"FAULT_MAX_DELAY": "0s"}),
		)
		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		_, ok := errors.AsType[*config.Error](st.Cause())
		require.True(t, ok, "cause = %v, want a *config.Error", st.Cause())
		require.ErrorContains(t, st.Cause(), "FAULT_MAX_DELAY")
	})
}

func TestQueryOrders_fails_for_a_request_under_test(t *testing.T) {
	settings := fault.DefaultSettings()
	settings.Decoder = Registry{
		"orders-down": {Rules: []fit.Rule{{Point: "postgresql:orders", Fault: fit.Fault{Abort: true}}}},
	}
	faults, err := fault.New(settings)
	require.NoError(t, err)
	s := &humus.Setup{Faults: faults}

	id, err := baggage.NewMemberRaw("acme.scenario", "orders-down")
	require.NoError(t, err)
	b, err := baggage.New(id)
	require.NoError(t, err)
	ctx := baggage.ContextWithBaggage(t.Context(), b)

	err = queryOrders(ctx, s.Faults)
	var injected *fault.InjectedError
	require.ErrorAs(t, err, &injected)
	require.Equal(t, "postgresql:orders", injected.Point)
	require.ErrorIs(t, err, fit.ErrInjected)
	require.NoError(t, queryOrders(t.Context(), s.Faults), "a request with no scenario")
}
