// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// pemFile returns c's certificate chain and private key, PEM-encoded in one
// file, as Init's serverTLS reads them.
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

// caPEM returns the certificate of the CA that issued c, PEM-encoded, as
// Init's clientTLS reads it.
func caPEM(c *humustest.Certificate) []byte {
	chain := c.TLS().Certificate
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]})
}

// carts is the dependency, whose UnaryCall answers with the username
// "carts".
type carts struct {
	testpb.UnimplementedTestServiceServer
}

func (carts) UnaryCall(context.Context, *testpb.SimpleRequest) (*testpb.SimpleResponse, error) {
	return &testpb.SimpleResponse{Username: "carts"}, nil
}

// startCarts runs the dependency on h, at carts:9090, over TLS with a
// certificate for carts from ca. It returns a function that stops the
// dependency and requires it to exit 0.
func startCarts(t *testing.T, h *humustest.Host, ca *humustest.CA) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "carts")
	p, exit := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		svc, err := grpcserver.NewService(s, grpcserver.Settings{
			Addr: "carts:9090",
			TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
		})
		if err != nil {
			return nil, err
		}
		testpb.RegisterTestServiceServer(svc, carts{})
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("carts:9090")
	return func() {
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}

func TestInit_calls_carts_for_each_RPC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		orders := ca.Issue(t, "orders")
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls.pem":      pemFile(t, orders),
			"carts-ca.pem": caPEM(orders),
		}))
		stop := startCarts(t, h, ca)
		defer stop()

		p, exit := grpcservertest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{
				"GRPC_ADDR":         ":8443",
				"GRPC_TLS_PEM_FILE": "tls.pem",
				"CARTS_ADDR":        "carts:9090",
				"CARTS_CA_FILE":     "carts-ca.pem",
			}),
		)
		<-h.Listening("orders:8443")

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "orders:8443", ca.Pool()))
		for range 2 {
			resp, err := client.UnaryCall(t.Context(), &testpb.SimpleRequest{})
			require.NoError(t, err)
			require.Equal(t, "orders, via carts", resp.GetUsername())
		}

		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-exit
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})

	t.Run("fails with exit 78 when CARTS_ADDR is not set", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			_, exit := grpcservertest.Start(t, Init,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				humustest.WithEnv(map[string]string{"CARTS_CA_FILE": "carts-ca.pem"}),
			)
			st := <-exit
			require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "config CARTS_ADDR: not found")
		})
	})

	t.Run("fails with exit 78 when CARTS_ADDR names a scheme", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			_, exit := grpcservertest.Start(t, Init,
				humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
				humustest.WithEnv(map[string]string{"CARTS_ADDR": "dns:///carts:9090"}),
			)
			st := <-exit
			require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
			require.ErrorContains(t, st.Cause(), "config CARTS_ADDR:")
			require.ErrorContains(t, st.Cause(), "names a scheme")
		})
	})
}
