// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth

import (
	"crypto/tls"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// TestService_limits_are_httpservers_defaults keeps the probes server's
// idle timeout and header limit in step with httpserver's defaults, and its
// read timeouts at 2s and 5s, which are httpserver's too.
func TestService_limits_are_httpservers_defaults(t *testing.T) {
	t.Parallel()

	d := httpserver.DefaultSettings()
	require.Equal(t, d.ReadHeaderTimeout, readHeaderTimeout)
	require.Equal(t, d.ReadTimeout, readTimeout)
	require.Equal(t, d.IdleTimeout, idleTimeout)
	require.Equal(t, d.MaxHeaderBytes, maxHeaderBytes)
}

// newTestService returns a Service on :8081 of h, with a certificate from
// a CA generated for the test.
func newTestService(t *testing.T, h *humustest.Host) *Service {
	t.Helper()
	cert := humustest.NewCA(t).Issue(t, "localhost")
	s := &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}
	svc, err := NewService(s, Settings{
		Addr: ":8081",
		TLS:  &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
	})
	require.NoError(t, err)
	return svc
}

// ready reports the readiness the Service answers with.
func ready(t *testing.T, svc *Service) bool {
	t.Helper()
	healthy, err := svc.hs.Probes().Readiness.Healthy(t.Context())
	require.NoError(t, err)
	return healthy
}

func TestService_Shutdown_marks_it_not_ready_for_good(t *testing.T) {
	t.Run("while it serves", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc := newTestService(t, humustest.NewHost(t))
			require.False(t, ready(t, svc), "ready before Serve")

			served := make(chan error, 1)
			go func() { served <- svc.Serve(t.Context()) }()
			synctest.Wait()
			require.True(t, ready(t, svc), "not ready while Serve runs")

			require.NoError(t, svc.Shutdown(t.Context()))
			require.False(t, ready(t, svc), "ready after Shutdown")
			require.NoError(t, <-served)
			require.False(t, ready(t, svc), "ready after Serve returned")
		})
	})

	t.Run("before Serve", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc := newTestService(t, humustest.NewHost(t))

			require.NoError(t, svc.Shutdown(t.Context()))
			require.NoError(t, svc.Serve(t.Context()))
			require.False(t, ready(t, svc), "ready after a Serve that followed Shutdown")
		})
	})
}
