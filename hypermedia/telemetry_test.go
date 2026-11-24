// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/http/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/hypermedia"
)

func TestSite_names_its_span_after_the_route_under_the_handlers_it_is_wrapped_in(t *testing.T) {
	// unmatched is the span name of a request no route serves: the
	// method alone, or the pattern of a ServeMux above the Site.
	testCases := []struct {
		name      string
		wrap      func(site http.Handler) http.Handler
		unmatched string
	}{
		{
			name:      "alone",
			wrap:      func(site http.Handler) http.Handler { return site },
			unmatched: "GET",
		},
		{
			name: "under a ServeMux",
			wrap: func(site http.Handler) http.Handler {
				mux := http.NewServeMux()
				mux.Handle("/", site)
				return mux
			},
			unmatched: "GET /",
		},
		{
			name: "behind MaxBytesHandler",
			wrap: func(site http.Handler) http.Handler {
				return http.MaxBytesHandler(site, 64<<10)
			},
			unmatched: "GET",
		},
		{
			name: "behind TimeoutHandler",
			wrap: func(site http.Handler) http.Handler {
				return http.TimeoutHandler(site, time.Minute, "timed out")
			},
			unmatched: "GET",
		},
		{
			name: "under a ServeMux, behind handlers of the end user's that each pass on a clone",
			wrap: func(site http.Handler) http.Handler {
				clone := func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						next.ServeHTTP(w, r.Clone(r.Context()))
					})
				}
				mux := http.NewServeMux()
				mux.Handle("/", clone(clone(site)))
				return mux
			},
			unmatched: "GET /",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var names []string
			synctest.Test(t, func(t *testing.T) {
				ca := humustest.NewCA(t)
				cert := ca.Issue(t, "localhost")
				h := humustest.NewHost(t)
				tel := humustest.NewTelemetry()
				p, status := httpservertest.Start(t, h.Host(), func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
					site, err := hypermedia.NewSite(s, func(w http.ResponseWriter, _ *http.Request, _ error) error {
						w.WriteHeader(http.StatusNotFound)
						return nil
					}, []hypermedia.Route{
						hypermedia.NewRoute(http.MethodGet, One, says("one")),
						hypermedia.NewRoute(http.MethodPut, One, says("updated")),
						hypermedia.NewRoute(http.MethodGet, Static, says("asset")),
					})
					if err != nil {
						return nil, err
					}
					settings := httpserver.DefaultSettings()
					settings.Addr = ":8443"
					settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
					svc, err := httpserver.NewService(ctx, s, h, settings, tc.wrap(site))
					if err != nil {
						return nil, err
					}
					return []humus.Service{svc}, nil
				}, humustest.WithApp(humus.WithTelemetry(tel.Builder())))
				select {
				case <-h.Listening("localhost:8443"):
				case st := <-status:
					t.Fatalf("the App exited before it listened: %v", st.Cause())
				}
				client := httpservertest.NewClient(h, ca.Pool())

				for _, req := range []struct{ method, path string }{
					{http.MethodPut, "/contacts/7"},
					{http.MethodGet, "/contacts/7"},
					{http.MethodHead, "/contacts/7"},
					{http.MethodGet, "/static/css/main.css"},
					{http.MethodGet, "/orders"},
				} {
					r, err := http.NewRequestWithContext(t.Context(), req.method, "https://localhost:8443"+req.path, strings.NewReader(""))
					require.NoError(t, err)
					resp, err := client.Do(r)
					require.NoError(t, err)
					_, err = io.Copy(io.Discard, resp.Body)
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
				}

				p.Signal(syscall.SIGTERM)
				st := <-status
				require.Zero(t, st.Code(), "cause: %v", st.Cause())
				for _, s := range tel.Spans() {
					if s.InstrumentationScope().Name == otelhttp.ScopeName {
						names = append(names, s.Name())
					}
				}
			})
			require.Equal(t, []string{
				"PUT /contacts/{contactId}",
				"GET /contacts/{contactId}",
				"HEAD /contacts/{contactId}",
				"GET /static/{file...}",
				tc.unmatched,
			}, names)
		})
	}
}
