// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcservertest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/humustest"
)

// Start is humustest.Start with grpcserver.Defaults applied before opts, so
// a test runs its App with the drain and the grace period that
// grpcserver.Main gives it in production. Options given with
// humustest.WithApp come after the defaults, and win over them.
func Start(t testing.TB, init func(ctx context.Context, s *humus.Setup) ([]humus.Service, error), opts ...humustest.Option) (*bedrocktest.Process, <-chan bedrock.ExitStatus) {
	return humustest.Start(t, init, append([]humustest.Option{humustest.WithApp(grpcserver.Defaults())}, opts...)...)
}

// NewClient returns a client of the Service listening on address in h. It
// dials through h, never resolves address, and verifies the Service's
// certificate for address's host against roots. Transport credentials in
// opts replace those. The client is closed when the test ends. It works
// with or without Start.
func NewClient(t testing.TB, h *humustest.Host, address string, roots *x509.CertPool, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	if h == nil {
		t.Fatalf("grpcservertest: NewClient(%q) called with a nil *humustest.Host", address)
		return nil
	}

	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		return h.Dial(ctx, "tcp", addr)
	}
	conn, err := grpc.NewClient("passthrough:///"+address, append([]grpc.DialOption{
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: roots})),
	}, opts...)...)
	if err != nil {
		t.Fatalf("grpcservertest: NewClient(%q): %v", address, err)
		return nil
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
