// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package catalogclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/circuit"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/catalogclient"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/httpclient"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/telemetry"
)

const catalogAddr = "catalog.internal:8443"

func startCatalog(t *testing.T, h *humustest.Host, ca *humustest.CA, handler http.Handler) (stop func()) {
	t.Helper()
	cert := ca.Issue(t, "catalog.internal")
	p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = catalogAddr
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(s, settings, handler)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	<-h.Listening(catalogAddr)
	return func() {
		t.Helper()
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "the catalog's cause: %v", st.Cause())
	}
}

func settings(t *testing.T, ca *humustest.CA, rawURL string) httpclient.Settings {
	t.Helper()
	base, err := url.Parse(rawURL)
	require.NoError(t, err)
	s := httpclient.DefaultSettings()
	s.BaseURL = base
	s.TLS = &tls.Config{RootCAs: ca.Pool()}
	return s
}

func run(t *testing.T, h *humustest.Host, settings httpclient.Settings, call func(ctx context.Context, c *catalogclient.Client)) {
	t.Helper()
	_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		client, err := httpclient.NewClient(s, settings)
		if err != nil {
			return nil, err
		}
		c := catalogclient.New(client, settings.BaseURL)
		return []humus.Service{job.NewService(s, func(ctx context.Context) error {
			call(ctx, c)
			return nil
		})}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))
	st := <-status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

type result struct {
	price float64
	err   error
}

func price(t *testing.T, handler http.HandlerFunc, productID string) result {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	stop := startCatalog(t, h, ca, handler)
	var r result
	run(t, h, settings(t, ca, "https://"+catalogAddr), func(ctx context.Context, c *catalogclient.Client) {
		r.price, r.err = c.Price(ctx, productID)
	})
	stop()
	return r
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestClient_Price_is_the_price_the_catalog_answers_with(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var method, path string
		r := price(t, func(w http.ResponseWriter, req *http.Request) {
			method, path = req.Method, req.URL.EscapedPath()
			answer(http.StatusOK, `{"productId":"sku-1","unitPrice":9.99}`)(w, req)
		}, "sku-1")
		require.NoError(t, r.err)
		require.InDelta(t, 9.99, r.price, 1e-9)
		require.Equal(t, http.MethodGet, method)
		require.Equal(t, "/products/sku-1", path)
	})
}

func TestClient_Price_escapes_the_product_and_keeps_the_base_path(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		var path atomic.Value
		stop := startCatalog(t, h, ca, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			path.Store(req.URL.EscapedPath())
			answer(http.StatusOK, `{"productId":"a/b c","unitPrice":0}`)(w, req)
		}))
		var r result
		run(t, h, settings(t, ca, "https://"+catalogAddr+"/v1"), func(ctx context.Context, c *catalogclient.Client) {
			r.price, r.err = c.Price(ctx, "a/b c")
		})
		stop()

		require.NoError(t, r.err)
		require.Zero(t, r.price)
		require.Equal(t, "/v1/products/a%2Fb%20c", path.Load())
	})
}

func TestClient_Price_answers_a_404_with_ErrProductNotFound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := price(t, answer(http.StatusNotFound, `{"status":404}`), "sku-1")
		require.ErrorIs(t, r.err, endpoint.ErrProductNotFound)
		require.NotErrorIs(t, r.err, endpoint.ErrCatalogUnavailable)
	})
}

func TestClient_Price_answers_a_502_503_or_504_after_the_last_attempt_with_ErrCatalogUnavailable(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts atomic.Int32
				r := price(t, func(w http.ResponseWriter, req *http.Request) {
					attempts.Add(1)
					answer(status, `{}`)(w, req)
				}, "sku-1")
				require.ErrorIs(t, r.err, endpoint.ErrCatalogUnavailable)
				require.NotErrorIs(t, r.err, endpoint.ErrProductNotFound)
				require.ErrorContains(t, r.err, strconv.Itoa(status))
				require.EqualValues(t, httpclient.DefaultSettings().MaxAttempts, attempts.Load())
			})
		})
	}
}

func TestClient_Price_answers_a_transport_error_with_ErrCatalogUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		var r result
		run(t, h, settings(t, ca, "https://"+catalogAddr), func(ctx context.Context, c *catalogclient.Client) {
			r.price, r.err = c.Price(ctx, "sku-1")
		})
		require.ErrorIs(t, r.err, endpoint.ErrCatalogUnavailable)
		require.NotErrorIs(t, r.err, circuit.ErrRejected)
	})
}

func TestClient_Price_answers_a_timeout_with_ErrCatalogUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := price(t, func(_ http.ResponseWriter, req *http.Request) {
			<-req.Context().Done()
		}, "sku-1")
		require.ErrorIs(t, r.err, endpoint.ErrCatalogUnavailable)
		require.ErrorIs(t, r.err, context.DeadlineExceeded)
	})
}

func TestClient_Price_answers_a_call_its_open_breaker_rejected_with_ErrCatalogUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		var attempts atomic.Int32
		stop := startCatalog(t, h, ca, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			attempts.Add(1)
			answer(http.StatusServiceUnavailable, `{}`)(w, req)
		}))
		var errs []error
		var sentWhenRejected int32
		run(t, h, settings(t, ca, "https://"+catalogAddr), func(ctx context.Context, c *catalogclient.Client) {
			for range 10 {
				before := attempts.Load()
				_, err := c.Price(ctx, "sku-1")
				errs = append(errs, err)
				if errors.Is(err, circuit.ErrRejected) {
					sentWhenRejected = attempts.Load() - before
					return
				}
			}
		})
		stop()

		last := errs[len(errs)-1]
		require.ErrorIs(t, last, circuit.ErrRejected)
		require.ErrorIs(t, last, endpoint.ErrCatalogUnavailable)
		require.Zero(t, sentWhenRejected)
		for _, err := range errs[:len(errs)-1] {
			require.ErrorIs(t, err, endpoint.ErrCatalogUnavailable)
		}
	})
}

func TestClient_Price_answers_anything_else_with_an_error_of_its_own(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"500":                    answer(http.StatusInternalServerError, `{}`),
		"400":                    answer(http.StatusBadRequest, `{}`),
		"401":                    answer(http.StatusUnauthorized, `{}`),
		"a body that isn't JSON": answer(http.StatusOK, `not json`),
		"no unitPrice":           answer(http.StatusOK, `{"productId":"sku-1"}`),
		"a negative unitPrice":   answer(http.StatusOK, `{"productId":"sku-1","unitPrice":-1}`),
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := price(t, handler, "sku-1")
				require.Error(t, r.err)
				require.NotErrorIs(t, r.err, endpoint.ErrCatalogUnavailable)
				require.NotErrorIs(t, r.err, endpoint.ErrProductNotFound)
			})
		})
	}
}
