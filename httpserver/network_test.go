// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver_test

import (
	"context"
	"net"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// ctxKey tags the context a test passes to NewService, so the test can tell
// that context from any other one ListenTCP is called with.
type ctxKey struct{}

func TestNewService_listens_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		var listens []string
		var tags []any
		n := hosttest.Seams{Network: h, OnListenTCP: func(ctx context.Context, network, address string) (net.Listener, error) {
			listens = append(listens, network+" "+address)
			tags = append(tags, ctx.Value(ctxKey{}))
			return h.ListenTCP(ctx, network, address)
		}}
		s := &humus.Setup{Telemetry: telemetry.Noop()}
		ctx := context.WithValue(t.Context(), ctxKey{}, "NewService's ctx")

		svc, err := httpserver.NewService(ctx, s, n, settingsFor(t, ca), hello)
		require.NoError(t, err)
		require.Equal(t, []string{"tcp :8443"}, listens, "NewService listens once, through n")
		require.Equal(t, []any{"NewService's ctx"}, tags, "ListenTCP is given NewService's ctx")

		served := make(chan error, 1)
		go func() { served <- svc.Serve(t.Context()) }()
		r := <-get(client(h, ca, offersH2), "https://localhost:8443/")
		require.NoError(t, r.err)
		require.Equal(t, "hello", r.body)

		require.NoError(t, svc.Shutdown(t.Context()))
		require.NoError(t, <-served)
	})
}
