// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/kube"
	"github.com/z5labs/humus/telemetry"
)

func TestNewClient_dials_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		defer startAPIServer(t, h, ca, server, &apiServer{}, "apiserver")()

		n, d := recordDials(h)
		// The App's Host, which is Init's argument, is another host whose
		// every dial fails, so a dial through it would fail the request
		// rather than reach the API server.
		errAppHost := errors.New("dialled through the App's Host")
		appHost := humus.Host{
			IPNetwork: hosttest.Seams{Network: humustest.NewHost(t), OnDialTCP: func(context.Context, string, string) (net.Conn, error) {
				return nil, errAppHost
			}},
			Filesystem: h.Filesystem(),
		}

		var dialsAtNew []string
		var getErr error
		init := func(_ context.Context, s *humus.Setup, _ humus.Host) ([]humus.Service, error) {
			c, err := kube.NewClient(s, n, kube.Connection{Server: server, TLS: &tls.Config{RootCAs: ca.Pool()}})
			if err != nil {
				return nil, err
			}
			dialsAtNew = d.all()
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				getErr = getConfigMap(ctx, newClientset(t, c), "settings")
				return nil
			})}, nil
		}
		_, status := humustest.Start(t, appHost, init, humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)))
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())

		require.Empty(t, dialsAtNew, "NewClient dials nothing")
		require.NoError(t, getErr)
		require.Equal(t, []string{server}, d.all(), "the Client dials through n")
	})
}
