// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcservertest_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/status"

	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
)

// testService answers EmptyCall, and leaves every other method
// unimplemented.
type testService struct {
	testpb.UnimplementedTestServiceServer
}

func (testService) EmptyCall(context.Context, *testpb.Empty) (*testpb.Empty, error) {
	return &testpb.Empty{}, nil
}

// serve starts a *grpc.Server with creds, serving testService on a listener
// from h.Listen for address. The server is stopped when the test ends.
func serve(t *testing.T, h *humustest.Host, address string, creds credentials.TransportCredentials) {
	t.Helper()

	l, err := h.Listen("tcp", address)
	require.NoError(t, err)

	srv := grpc.NewServer(grpc.Creds(creds))
	testpb.RegisterTestServiceServer(srv, testService{})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		srv.Stop()
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("Serve: %v", err)
		}
	})
}

// serverTLS returns transport credentials that serve cert.
func serverTLS(cert *humustest.Certificate) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert.TLS()}})
}

func TestNewClient_makes_an_RPC_over_TLS(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		serve(t, h, "carts:9090", serverTLS(ca.Issue(t, "carts")))

		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		_, err := client.EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)
	})
}

func TestNewClient_verifies_the_certificate(t *testing.T) {
	testCases := []struct {
		name string
		// issue returns the certificate the server serves, given the CA the
		// client trusts.
		issue func(t *testing.T, trusted *humustest.CA) *humustest.Certificate
		want  string
	}{
		{
			name: "from another CA",
			issue: func(t *testing.T, _ *humustest.CA) *humustest.Certificate {
				return humustest.NewCA(t).Issue(t, "carts")
			},
			want: "certificate signed by unknown authority",
		},
		{
			name: "for another name",
			issue: func(t *testing.T, trusted *humustest.CA) *humustest.Certificate {
				return trusted.Issue(t, "orders")
			},
			want: "certificate is valid for orders, not carts",
		},
	}

	for _, tc := range testCases {
		t.Run("and fails the handshake against a certificate "+tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				h := humustest.NewHost(t)
				serve(t, h, "carts:9090", serverTLS(tc.issue(t, ca)))

				client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
				_, err := client.EmptyCall(t.Context(), &testpb.Empty{})
				require.Error(t, err)
				require.Equal(t, codes.Unavailable, status.Code(err), "err = %v", err)
				require.ErrorContains(t, err, tc.want)
			})
		})
	}
}

// TestNewClient_never_resolves_the_address dials a name under .invalid,
// which RFC 6761 reserves so that it resolves nowhere: the RPC reaches the
// Service only because the name exists on the test Host. Were the address
// looked up with the dns resolver, the lookup would crash the test binary
// inside the bubble.
func TestNewClient_never_resolves_the_address(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		serve(t, h, "carts.humus.invalid:9090", serverTLS(ca.Issue(t, "carts.humus.invalid")))

		conn := grpcservertest.NewClient(t, h, "carts.humus.invalid:9090", ca.Pool())
		require.Equal(t, "passthrough:///carts.humus.invalid:9090", conn.CanonicalTarget())

		_, err := testpb.NewTestServiceClient(conn).EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)
	})
}

func TestNewClient_speaks_plain_text_with_insecure_credentials_in_opts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		serve(t, h, "localhost:8081", insecure.NewCredentials())

		conn := grpcservertest.NewClient(t, h, "localhost:8081", nil,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		_, err := testpb.NewTestServiceClient(conn).EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)
	})
}

func TestNewClient_shuts_the_client_down_when_the_test_ends(t *testing.T) {
	var conn *grpc.ClientConn
	t.Run("a subtest builds a client", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ca := humustest.NewCA(t)
			h := humustest.NewHost(t)
			serve(t, h, "carts:9090", serverTLS(ca.Issue(t, "carts")))

			conn = grpcservertest.NewClient(t, h, "carts:9090", ca.Pool())
			_, err := testpb.NewTestServiceClient(conn).EmptyCall(t.Context(), &testpb.Empty{})
			require.NoError(t, err)
			require.NotEqual(t, connectivity.Shutdown, conn.GetState())
		})
	})

	require.NotNil(t, conn)
	require.Equal(t, connectivity.Shutdown, conn.GetState())
}

// fatalTB records the first Fatalf and ends its goroutine, as testing.T's
// Fatalf does, and counts the cleanups registered with it.
type fatalTB struct {
	testing.TB
	msg      string
	cleanups int
}

func (*fatalTB) Helper() {}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (f *fatalTB) Cleanup(func()) { f.cleanups++ }

// newClient calls NewClient with tb on a goroutine of its own, so that
// tb's Fatalf ends that goroutine and not the test's.
func newClient(tb *fatalTB, h *humustest.Host, opts ...grpc.DialOption) *grpc.ClientConn {
	var conn *grpc.ClientConn
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn = grpcservertest.NewClient(tb, h, "carts:9090", nil, opts...)
	}()
	<-done
	return conn
}

// secureOnly is per-RPC credentials that require transport security.
type secureOnly struct{}

func (secureOnly) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return nil, nil
}

func (secureOnly) RequireTransportSecurity() bool { return true }

func TestNewClient_fails_the_test_when_the_client_cannot_be_built(t *testing.T) {
	t.Run("with per-RPC credentials that need TLS over plain text", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tb := &fatalTB{TB: t}
			conn := newClient(tb, humustest.NewHost(t),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithPerRPCCredentials(secureOnly{}),
			)
			require.Nil(t, conn)
			require.Contains(t, tb.msg, `grpcservertest: NewClient("carts:9090"): `)
			require.Contains(t, tb.msg, "transport level security")
			require.Zero(t, tb.cleanups, "there is no client to close")
		})
	})

	t.Run("with a nil Host", func(t *testing.T) {
		tb := &fatalTB{TB: t}
		conn := newClient(tb, nil)
		require.Nil(t, conn)
		require.Equal(t, `grpcservertest: NewClient("carts:9090") called with a nil *humustest.Host`, tb.msg)
		require.Zero(t, tb.cleanups)
	})
}
