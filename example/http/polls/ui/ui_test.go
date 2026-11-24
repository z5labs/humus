// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package ui_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/example/http/polls/store/storetest"
	"github.com/z5labs/humus/example/http/polls/ui"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/hypermedia"
)

const (
	csp  = "default-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	base = "https://polls.internal:8443"
)

type backend interface {
	ui.Store
	ui.Results
}

func start(t *testing.T, polls backend, opts ...humus.Option) (*http.Client, func()) {
	t.Helper()
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, "polls.internal")
	h := humustest.NewHost(t)
	p, status := httpservertest.Start(t, h.Host(), func(ctx context.Context, s *humus.Setup, _ humus.Host) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = "polls.internal:8443"
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		u, err := ui.New(polls, polls, settings.WriteTimeout)
		if err != nil {
			return nil, err
		}
		site, err := u.Site(s)
		if err != nil {
			return nil, err
		}
		svc, err := httpserver.NewService(s, settings, site)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(append([]humus.Option{}, opts...)...))
	<-h.Listening("polls.internal:8443")
	c := httpservertest.NewClient(h, ca.Pool())
	return c, func() {
		t.Helper()
		<-p.Notified(syscall.SIGTERM)
		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "%v", st.Cause())
	}
}

type script struct {
	name string
	body []byte
	url  string
}

func readScript(t *testing.T, name string) script {
	t.Helper()
	b, err := os.ReadFile("static/" + name)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return script{name: name, body: b, url: "/scripts/" + hex.EncodeToString(sum[:]) + "/" + name}
}

func TestSite_serves_each_script_at_its_content_hashed_URL_under_the_CSP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, stop := start(t, storetest.New())

		htmx := readScript(t, "htmx.min.js")
		sse := readScript(t, "sse.min.js")
		for _, s := range []script{htmx, sse} {
			get := do(t, c, http.MethodGet, base+s.url)
			require.Equal(t, http.StatusOK, get.status, s.url)
			require.Equal(t, s.body, []byte(get.body), "%s is served byte for byte", s.url)
			require.Equal(t, "text/javascript; charset=utf-8", get.header.Get("Content-Type"), s.url)
			require.Equal(t, "public, max-age=31536000, immutable", get.header.Get("Cache-Control"), s.url)
			require.Equal(t, csp, get.header.Get("Content-Security-Policy"), s.url)

			head := do(t, c, http.MethodHead, base+s.url)
			require.Equal(t, http.StatusOK, head.status, s.url)
			require.Empty(t, head.body, s.url)
			require.Equal(t, strconv.Itoa(len(s.body)), head.header.Get("Content-Length"), s.url)
			require.Equal(t, "text/javascript; charset=utf-8", head.header.Get("Content-Type"), s.url)
			require.Equal(t, "public, max-age=31536000, immutable", head.header.Get("Cache-Control"), s.url)
			require.Equal(t, csp, head.header.Get("Content-Security-Policy"), s.url)

			del := do(t, c, http.MethodDelete, base+s.url)
			require.Equal(t, http.StatusMethodNotAllowed, del.status, s.url)
			require.Equal(t, "GET, HEAD", del.header.Get("Allow"), s.url)
			require.Equal(t, "text/html; charset=utf-8", del.header.Get("Content-Type"), s.url)
			require.Equal(t, csp, del.header.Get("Content-Security-Policy"), s.url)
			require.Contains(t, del.body, "<h1>Method Not Allowed</h1>", s.url)
		}

		stale := sha256.Sum256([]byte("an older htmx.min.js"))
		htmxHash := htmx.url[len("/scripts/") : len("/scripts/")+64]
		for _, target := range []string{
			"/scripts/" + hex.EncodeToString(stale[:]) + "/htmx.min.js",
			"/scripts/" + htmxHash + "/sse.min.js",
			"/scripts/" + htmxHash + "/nothing.js",
			"/scripts/" + htmxHash + "/static/htmx.min.js",
			"/scripts/" + htmxHash + "/",
			"/scripts/" + htmxHash,
			"/scripts/",
			"/nowhere",
		} {
			r := do(t, c, http.MethodGet, base+target)
			require.Equal(t, http.StatusNotFound, r.status, target)
			require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"), target)
			require.Equal(t, csp, r.header.Get("Content-Security-Policy"), target)
			require.Empty(t, r.header.Get("Cache-Control"), target)
			require.Contains(t, r.body, "<h1>Not Found</h1>", target)
			require.Contains(t, r.body, "<p>There&#39;s no such page.</p>", target)
		}

		page := do(t, c, http.MethodGet, base+"/nowhere")
		require.Contains(t, page.body, "<!DOCTYPE html>")
		require.Contains(t, page.body, `<script src="`+htmx.url+`"></script>`, "the layout names htmx's current URL")
		require.Contains(t, page.body, `<script src="`+sse.url+`"></script>`, "the layout names the SSE extension's current URL")
		require.Contains(t, page.body, `<meta name="htmx-config" content='{"allowEval":false,"includeIndicatorStyles":false,"responseHandling":[{"code":"204","swap":false},{"code":"[23]..","swap":true},{"code":"409","swap":true},{"code":"422","swap":true},{"code":"[45]..","swap":false,"error":true}]}'>`)
		require.NotContains(t, page.body, "<script>", "no inline script")

		stop()
	})
}

func TestErrorPage_writes_an_HTML_page_with_each_errors_status_and_none_of_its_text(t *testing.T) {
	polls := storetest.New()
	u, err := ui.New(polls, polls, 0)
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"a 404", &hypermedia.RequestError{Status: http.StatusNotFound, Err: errors.New("no route serves /secret-404")}, http.StatusNotFound, "There&#39;s no such page."},
		{"a 415", &hypermedia.RequestError{Status: http.StatusUnsupportedMediaType, Err: errors.New("secret-415 isn't a form")}, http.StatusUnsupportedMediaType, "What was sent isn&#39;t a form."},
		{"a 409", &hypermedia.RequestError{Status: http.StatusConflict, Err: errors.New("secret-409")}, http.StatusConflict, "Conflict."},
		{"an unreachable database", fmt.Errorf("reading poll: %w: dial tcp 10.0.0.7:5432: secret-503", store.ErrUnavailable), http.StatusServiceUnavailable, "The polls can&#39;t be reached right now. Try again in a moment."},
		{"any other error", errors.New("secret-500"), http.StatusInternalServerError, "Something went wrong on our side."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			require.NoError(t, u.ErrorPage(w, r, tc.err))
			require.Equal(t, tc.status, w.Code)
			require.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
			body := w.Body.String()
			require.Contains(t, body, "<h1>"+http.StatusText(tc.status)+"</h1>")
			require.Contains(t, body, "<p>"+tc.message+"</p>")
			require.NotContains(t, body, "secret")
			require.NotContains(t, body, "10.0.0.7")
		})
	}
}

type response struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, c *http.Client, method, target string) response {
	t.Helper()
	return send(t, c, method, target, "", nil, nil)
}

func send(t *testing.T, c *http.Client, method, target, contentType string, body io.Reader, header map[string]string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{status: resp.StatusCode, header: resp.Header, body: string(b)}
}
