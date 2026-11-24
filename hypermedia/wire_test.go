// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/hypermedia"
)

// The tests in this file serve a Site on httpserver, inside synctest
// bubbles, on humustest Hosts, so its writer is net/http's own.

// served is a Site served by an App on httpserver.
type served struct {
	t      *testing.T
	h      *humustest.Host
	tel    *humustest.Telemetry
	pool   *x509.CertPool
	pages  atomic.Int32 // how many times the ErrorPage answered
	signal func()
	status <-chan bedrock.ExitStatus
}

// serveSite serves routes on an App, with an ErrorPage that writes 500
// and "error page", and returns once it listens. It must be called inside
// a synctest bubble.
func serveSite(t *testing.T, routes ...hypermedia.Route) *served {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "localhost")
	s := &served{t: t, h: humustest.NewHost(t), tel: humustest.NewTelemetry(), pool: ca.Pool()}
	p, status := httpservertest.Start(t, s.h.Host(), func(ctx context.Context, setup *humus.Setup, h humus.Host) ([]humus.Service, error) {
		site, err := hypermedia.NewSite(setup, func(w http.ResponseWriter, _ *http.Request, _ error) error {
			s.pages.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, err := io.WriteString(w, "error page")
			return err
		}, routes)
		if err != nil {
			return nil, err
		}
		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		svc, err := httpserver.NewService(ctx, setup, h, settings, site)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithTelemetry(s.tel.Builder())))
	select {
	case <-s.h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("the App exited before it listened: %v", st.Cause())
	}
	s.signal = func() { p.Signal(syscall.SIGTERM) }
	s.status = status
	return s
}

// client returns a client that speaks only HTTP/2, or only HTTP/1.1.
func (s *served) client(http2 bool) *http.Client {
	c := httpservertest.NewClient(s.h, s.pool)
	if !http2 {
		tr := c.Transport.(*http.Transport)
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
		var protos http.Protocols
		protos.SetHTTP1(true)
		tr.Protocols = &protos
	}
	return c
}

// stop stops the App, and requires that it exit 0.
func (s *served) stop() {
	s.t.Helper()
	s.signal()
	st := <-s.status
	require.Zero(s.t, st.Code(), "cause: %v", st.Cause())
}

// errors returns the body and scope of each ERROR record.
func (s *served) errors() []string {
	var out []string
	for _, r := range recordsOf(s.tel) {
		if r.severity >= log.SeverityError {
			out = append(out, r.scope+": "+r.body)
		}
	}
	return out
}

// get GETs path, and returns the response's status, its Content-Type, its
// body and the error of reading it, or the error of the request.
type got struct {
	status      int
	contentType string
	body        string
	err         error
}

func (s *served) get(c *http.Client, path string, header http.Header) got {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodGet, "https://localhost:8443"+path, nil)
	require.NoError(s.t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return got{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return got{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(b), err: err}
}

// overBoth is each protocol a client speaks to the Site.
var overBoth = []struct {
	name  string
	http2 bool
}{
	{"over HTTP/2", true},
	{"over HTTP/1.1", false},
}

func TestSite_a_write_reaches_the_client_before_its_handler_returns(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 64<<10)
	for _, tc := range overBoth {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				read := make(chan struct{})
				s := serveSite(t, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, _ *http.Request) error {
					if _, err := w.Write(big); err != nil {
						return err
					}
					<-read
					_, err := io.WriteString(w, "end")
					return err
				}))
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443/contacts/7", nil)
				require.NoError(t, err)
				resp, err := s.client(tc.http2).Do(req)
				require.NoError(t, err)
				first := make([]byte, len(big))
				_, err = io.ReadFull(resp.Body, first)
				require.NoError(t, err, "the write reached the client while its handler waited")
				close(read)
				rest, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				require.Equal(t, big, first)
				require.Equal(t, "end", string(rest))
				s.stop()
				require.Empty(t, s.errors())
			})
		})
	}
}

func TestSite_lets_net_http_sniff_a_body_with_no_Content_Type(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := serveSite(t,
			hypermedia.NewRoute(http.MethodGet, One, says("<!DOCTYPE html><p>Ada</p>")),
			hypermedia.NewRoute(http.MethodGet, Edit, says("<tr><td>Ada</td></tr>")),
		)
		c := s.client(true)
		require.Equal(t, got{status: http.StatusOK, contentType: "text/html; charset=utf-8", body: "<!DOCTYPE html><p>Ada</p>"}, s.get(c, "/contacts/7", nil))
		require.Equal(t, "text/plain; charset=utf-8", s.get(c, "/contacts/7/edit", nil).contentType, "a fragment sniffs as text, as on net/http")
		s.stop()
	})
}

func TestSite_on_the_wire(t *testing.T) {
	failed := errors.New("the template is broken")
	partial := func(end func() error) hypermedia.Handler {
		return func(w http.ResponseWriter, _ *http.Request) error {
			if _, err := io.WriteString(w, "<p>Ad"); err != nil {
				return err
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return end()
		}
	}
	cases := []struct {
		name   string
		h      hypermedia.Handler
		want   got  // what the client gets; err only says whether there is one
		failed bool // the client gets an error
		pages  int32
		errors []string
	}{
		{
			name:   "an error after the first write ends the response as written",
			h:      partial(func() error { return failed }),
			want:   got{status: http.StatusOK, contentType: "text/html; charset=utf-8", body: "<p>Ad"},
			errors: []string{"github.com/z5labs/humus/hypermedia: response failed"},
		},
		{
			name:   "a panic after the first write aborts the response, recorded once",
			h:      partial(func() error { panic("the template is broken") }),
			failed: true,
			errors: []string{"github.com/z5labs/humus/hypermedia: handler panicked"},
		},
		{
			name:   "a panic before the first write gets the error page, recorded once",
			h:      func(http.ResponseWriter, *http.Request) error { panic("the template is broken") },
			want:   got{status: http.StatusInternalServerError, contentType: "text/plain; charset=utf-8", body: "error page"},
			pages:  1,
			errors: []string{"github.com/z5labs/humus/hypermedia: handler panicked"},
		},
		{
			name:   "a panic with ErrAbortHandler before the first write passes through",
			h:      func(http.ResponseWriter, *http.Request) error { panic(http.ErrAbortHandler) },
			failed: true,
		},
		{
			name:   "a panic with ErrAbortHandler after the first write passes through",
			h:      partial(func() error { panic(http.ErrAbortHandler) }),
			failed: true,
		},
	}
	for _, tc := range cases {
		for _, proto := range overBoth {
			t.Run(tc.name+" "+proto.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := serveSite(t, hypermedia.NewRoute(http.MethodGet, One, tc.h))
					g := s.get(s.client(proto.http2), "/contacts/7", nil)
					s.stop()
					if tc.failed {
						require.Error(t, g.err)
					} else {
						require.Equal(t, tc.want, g)
					}
					require.Equal(t, tc.pages, s.pages.Load())
					require.Equal(t, tc.errors, s.errors())
				})
			})
		}
	}
}

func TestSite_serves_a_file_with_ServeFileFS_on_a_Rest_route(t *testing.T) {
	fsys := fstest.MapFS{"css/main.css": {Data: []byte("body { color: teal }")}}
	for _, tc := range overBoth {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := serveSite(t, hypermedia.NewRoute(http.MethodGet, Static, func(w http.ResponseWriter, r *http.Request) error {
					http.ServeFileFS(w, r, fsys, hypermedia.ParamFrom(r, Asset))
					return nil
				}))
				c := s.client(tc.http2)
				require.Equal(t, got{status: http.StatusOK, contentType: "text/css; charset=utf-8", body: "body { color: teal }"}, s.get(c, "/static/css/main.css", nil))
				require.Equal(t, got{status: http.StatusPartialContent, contentType: "text/css; charset=utf-8", body: "color"}, s.get(c, "/static/css/main.css", http.Header{"Range": {"bytes=7-11"}}))
				s.stop()
				require.Empty(t, s.errors())
				require.Zero(t, s.pages.Load())
			})
		})
	}
}
