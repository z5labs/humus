// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver_test

import (
	"context"
	"net"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcserver"
	"github.com/z5labs/humus/grpc/grpcserver/grpcservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// ctxKey tags the context a test passes to NewService, so the test can tell
// that context from any other one ListenTCP is called with.
type ctxKey struct{}

func TestNewService_listens_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		setupHost := humustest.NewHost(t)
		h := humustest.NewHost(t)
		var listens []string
		var tags []any
		n := hosttest.Seams{Network: h, OnListenTCP: func(ctx context.Context, network, address string) (net.Listener, error) {
			listens = append(listens, network+" "+address)
			tags = append(tags, ctx.Value(ctxKey{}))
			return h.ListenTCP(ctx, network, address)
		}}
		// The Setup's Host is another host, so a listen through it would
		// be seen there rather than through n.
		s := &humus.Setup{Host: setupHost.Host(), Telemetry: telemetry.Noop()}
		ctx := context.WithValue(t.Context(), ctxKey{}, "NewService's ctx")

		svc, err := grpcserver.NewService(ctx, s, n, settingsFor(t, ca, ":9090"))
		require.NoError(t, err)
		require.Equal(t, []string{"tcp :9090"}, listens, "NewService listens once, through n")
		require.Equal(t, []any{"NewService's ctx"}, tags, "ListenTCP is given NewService's ctx")
		testpb.RegisterTestServiceServer(svc, &testServer{})

		served := make(chan error, 1)
		go func() { served <- svc.Serve(t.Context()) }()
		client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
		_, err = client.EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)
		requireRefused(t, setupHost, "carts:9090")

		require.NoError(t, svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}
