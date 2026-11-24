// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

func TestNewClient_dials_only_through_its_Network(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		stop := startPayments(t, h, h.Host(), ca, &ok{})
		defer stop()

		n, d := recordDials(h)
		// The Setup's Host is another host whose every dial fails, so a
		// dial through it would fail the call rather than reach payments.
		errSetupHost := errors.New("dialled through the Setup's Host")
		setupHost := hosttest.Seams{Network: humustest.NewHost(t), OnDialTCP: func(context.Context, string, string) (net.Conn, error) {
			return nil, errSetupHost
		}}
		s := &humus.Setup{
			Host:      humus.Host{IPNetwork: setupHost, Filesystem: h.Filesystem()},
			Telemetry: telemetry.Noop(),
		}

		c, err := httpclient.NewClient(s, n, paymentsSettings(t, ca))
		require.NoError(t, err)
		require.Empty(t, d.all(), "NewClient dials nothing")

		r, err := get(t.Context(), c, "https://payments:8443/")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, []string{"payments:8443"}, d.all(), "the client dials through n")
		c.CloseIdleConnections()
	})
}
