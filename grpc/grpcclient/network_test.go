// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"
	testpb "google.golang.org/grpc/interop/grpc_testing"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/grpc/grpcclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

func TestNewClient_dials_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		stop := startBackend(t, h, serving(t, ca, "backend"))
		defer stop()

		n, d := recordDials(h)
		// The App's Host, which is Init's argument, is another host whose
		// every dial fails, so a dial through it would fail the call
		// rather than reach the backend.
		errAppHost := errors.New("dialled through the App's Host")
		appHost := humus.Host{
			IPNetwork: hosttest.Seams{Network: humustest.NewHost(t), OnDialTCP: func(context.Context, string, string) (net.Conn, error) {
				return nil, errAppHost
			}},
			Filesystem: h.Filesystem(),
		}

		var dialsAtNew []string
		var callErr error
		init := func(_ context.Context, s *humus.Setup, _ humus.Host) ([]humus.Service, error) {
			c, err := grpcclient.NewClient(s, n, settingsFor(ca, "backend:9090"))
			if err != nil {
				return nil, err
			}
			dialsAtNew = d.all()
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				callErr = emptyCall(ctx, testpb.NewTestServiceClient(c))
				return nil
			})}, nil
		}
		_, status := humustest.Start(t, appHost, init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Empty(t, dialsAtNew, "NewClient dials nothing")
		require.NoError(t, callErr)
		require.Equal(t, []string{"backend:9090"}, d.all(), "the Client dials through n")
	})
}
