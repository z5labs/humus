// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

// startGateway runs a fake gateway on host, at gateway:8443, over TLS with
// a certificate for gateway from ca. It returns a function that stops the
// gateway and requires it to exit 0.
func startGateway(t *testing.T, h *humustest.Host, host humus.Host, ca *humustest.CA, handler http.Handler) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "gateway")
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening("gateway:8443")
	return func() {
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	}
}

// conflictsOnce answers each path 409 the first time it is asked for, and
// then 200 with the path, as an API does while another write to an account
// is in flight. It records every path it is asked for.
type conflictsOnce struct {
	mu    sync.Mutex
	paths []string
}

func (c *conflictsOnce) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	seen := 0
	for _, p := range c.paths {
		if p == r.URL.Path {
			seen++
		}
	}
	c.paths = append(c.paths, r.URL.Path)
	c.mu.Unlock()
	if seen == 0 {
		w.WriteHeader(http.StatusConflict)
		return
	}
	_, _ = w.Write([]byte(r.URL.Path))
}

func TestClient_two_clients_of_one_origin_share_one_Classify(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		host, conns := watchConns(h)
		gw := &conflictsOnce{}
		stop := startGateway(t, h, host, ca, gw)
		defer stop()

		paymentsSettings := httpclient.DefaultSettings()
		paymentsSettings.BaseURL = mustParse(t, "https://gateway:8443/payments")
		paymentsSettings.TLS = &tls.Config{RootCAs: ca.Pool()}
		ledgerSettings := paymentsSettings
		ledgerSettings.BaseURL = mustParse(t, "https://gateway:8443/ledger")

		// The APIs behind the gateway share a convention: a 409 says another
		// write to the account is in flight, which is over by the next
		// attempt. One Classify says so, and is set on the client of each.
		classify := httpclient.DefaultSettings().Classify
		ourAPIs := func(resp *http.Response, err error) httpclient.Classification {
			if resp != nil && resp.StatusCode == http.StatusConflict {
				return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
			}
			return classify(resp, err)
		}
		paymentsSettings.Classify = ourAPIs
		ledgerSettings.Classify = ourAPIs

		var got []response
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			// Two clients of one origin, each with its own Settings, built
			// once in Init and shared by everything that calls them.
			payments, err := httpclient.NewClient(s, paymentsSettings)
			if err != nil {
				return nil, err
			}
			ledger, err := httpclient.NewClient(s, ledgerSettings)
			if err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				for _, call := range []struct {
					c   *http.Client
					url string
				}{
					{payments, paymentsSettings.BaseURL.JoinPath("charges", "1").String()},
					{ledger, ledgerSettings.BaseURL.JoinPath("accounts", "1").String()},
				} {
					r, err := get(ctx, call.c, call.url)
					if err != nil {
						return err
					}
					got = append(got, r)
				}
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
		requireExitZero(t, <-status)

		require.Len(t, got, 2)
		require.Equal(t, http.StatusOK, got[0].code, "the 409 was retried")
		require.Equal(t, "/payments/charges/1", got[0].body)
		require.Equal(t, http.StatusOK, got[1].code, "the 409 was retried")
		require.Equal(t, "/ledger/accounts/1", got[1].body)
		require.Equal(t, []string{
			"/payments/charges/1", "/payments/charges/1",
			"/ledger/accounts/1", "/ledger/accounts/1",
		}, gw.paths)
		require.EqualValues(t, 2, conns.accepted.Load(), "each client opened a connection of its own")
	})
}
