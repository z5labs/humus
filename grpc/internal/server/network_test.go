// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package server_test

import (
	"context"
	"net"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/internal/server"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// ctxKey tags the context a test passes to New, so the test can tell that
// context from any other one ListenTCP is called with.
type ctxKey struct{}

func TestNew_listens_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		ctx := context.WithValue(t.Context(), ctxKey{}, "New's ctx")

		srv, err := server.New(ctx, s, n, callerScope, ":8081", nil)
		require.NoError(t, err)
		require.Equal(t, []string{"tcp :8081"}, listens, "New listens once, through n")
		require.Equal(t, []any{"New's ctx"}, tags, "ListenTCP is given New's ctx")
		require.NoError(t, srv.RegisterService(&testpb.TestService_ServiceDesc, testServer{}))

		served := make(chan error, 1)
		go func() { served <- srv.Serve(t.Context()) }()
		_, err = plaintext(t, h, "localhost:8081").EmptyCall(t.Context(), &testpb.Empty{})
		require.NoError(t, err)
		conn, err := setupHost.DialTCP(t.Context(), "tcp", "localhost:8081")
		if err == nil {
			_ = conn.Close()
			t.Fatal("a dial through the Setup's Host connected, want ECONNREFUSED")
		}
		require.ErrorIs(t, err, syscall.ECONNREFUSED)

		require.NoError(t, srv.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}
