// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"bufio"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

func TestInit_htmx_edits_a_row_in_place_and_ends_its_feed_at_shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"tls.pem": pemFile(t, ca.Issue(t, "contacts.internal")),
		}))
		tel := humustest.NewTelemetry()
		p, status := httpservertest.Start(t, Init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())),
			humustest.WithEnv(map[string]string{
				"ADDR":         "contacts.internal:8443",
				"TLS_PEM_FILE": h.Path("tls.pem"),
			}),
		)
		<-h.Listening("contacts.internal:8443")
		c := httpservertest.NewClient(h, ca.Pool())
		const base = "https://contacts.internal:8443"
		htmx := map[string]string{"HX-Request": "true", "Sec-Fetch-Site": "same-origin"}

		changes := open(t, c, base+"/contacts/changes")

		bad := send(t, c, http.MethodPut, base+"/contacts/1", url.Values{"name": {""}, "email": {"joe"}}, htmx)
		require.Equal(t, http.StatusUnprocessableEntity, bad.status)
		require.Contains(t, bad.body, "Enter a name.")
		require.Contains(t, bad.body, "Enter an email address.")
		require.Contains(t, bad.body, `<input name="email" value="joe">`, "the form keeps the values sent")

		good := send(t, c, http.MethodPut, base+"/contacts/1", url.Values{"name": {"Joe"}, "email": {"joe@example.com"}}, htmx)
		require.Equal(t, http.StatusOK, good.status)
		require.Equal(t, "contactSaved", good.header.Get("HX-Trigger"))
		require.True(t, strings.HasPrefix(good.body, `<div id="contact-1"><span class="text">Joe joe@example.com</span>`), "body = %q", good.body)
		require.Contains(t, <-changes.events, "event: changed\ndata: <div id=\"contact-1\" hx-swap-oob=\"true\"><span class=\"text\">Joe joe@example.com")
		require.Equal(t, ": keep-alive", <-changes.events, "a keep-alive comment 15s after the feed opened, past the 10s WriteTimeout")

		crossSite := send(t, c, http.MethodPut, base+"/contacts/1", url.Values{"name": {"Mallory"}, "email": {"mallory@example.com"}}, map[string]string{"HX-Request": "true", "Sec-Fetch-Site": "cross-site"})
		require.Equal(t, http.StatusForbidden, crossSite.status)
		require.Equal(t, "Joe joe@example.com", text(t, send(t, c, http.MethodGet, base+"/contacts/1", nil, htmx).body), "the cross-site PUT changed nothing")

		page := send(t, c, http.MethodGet, base+"/contacts/1", nil, nil)
		fragment := send(t, c, http.MethodGet, base+"/contacts/1", nil, htmx)
		for _, r := range []response{page, fragment} {
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "text/html; charset=utf-8", r.header.Get("Content-Type"))
			require.Equal(t, []string{"HX-Request"}, r.header.Values("Vary"))
		}
		require.Contains(t, page.body, "<!DOCTYPE html>")
		require.Contains(t, page.body, `<script src="/static/htmx.min.js"></script>`)
		require.Contains(t, page.body, `<div hx-ext="sse" sse-connect="/contacts/changes" sse-swap="changed" hx-swap="none"></div>`)
		require.Contains(t, page.body, `<div id="contact-1"><span class="text">Joe joe@example.com</span>`)
		require.NotContains(t, fragment.body, "<html")
		require.True(t, strings.HasPrefix(fragment.body, `<div id="contact-1">`), "body = %q", fragment.body)

		edit := send(t, c, http.MethodGet, base+"/contacts/1/edit", nil, htmx)
		require.Equal(t, http.StatusOK, edit.status)
		require.Contains(t, edit.body, `<form id="contact-1" hx-put="/contacts/1" hx-swap="outerHTML">`)

		list := send(t, c, http.MethodGet, base+"/contacts", nil, nil)
		require.Equal(t, http.StatusOK, list.status)
		for _, id := range []string{"1", "2", "3"} {
			require.Contains(t, list.body, `<div id="contact-`+id+`">`)
		}

		script := send(t, c, http.MethodGet, base+"/static/sse.min.js", nil, nil)
		require.Equal(t, http.StatusOK, script.status)
		require.Equal(t, "text/javascript; charset=utf-8", script.header.Get("Content-Type"))
		require.Contains(t, script.body, `htmx.defineExtension("sse"`)

		for _, target := range []string{"/nowhere", "/contacts/0", "/contacts/99/edit", "/static/nothing.js", "/static/"} {
			r := send(t, c, http.MethodGet, base+target, nil, nil)
			require.Equal(t, http.StatusNotFound, r.status, target)
			require.Contains(t, r.body, "<h1>Not Found</h1>", target)
		}

		for _, m := range []struct{ method, target, allow string }{
			{http.MethodDelete, "/contacts/1", "GET, HEAD, PUT"},
			{http.MethodPost, "/contacts", "GET, HEAD"},
			{http.MethodPut, "/contacts/1/edit", "GET, HEAD"},
		} {
			r := send(t, c, m.method, base+m.target, url.Values{}, htmx)
			require.Equal(t, http.StatusMethodNotAllowed, r.status, "%s %s", m.method, m.target)
			require.Equal(t, m.allow, r.header.Get("Allow"), "%s %s", m.method, m.target)
			require.Contains(t, r.body, "<h1>Method Not Allowed</h1>", "%s %s", m.method, m.target)
		}

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		end := <-changes.ended
		require.NoError(t, end.err, "a clean end, which htmx's SSE extension reconnects after")
		require.Equal(t, 5*time.Second, end.at.Sub(sigterm), "the feed ends as Shutdown starts, after the drain")
		st := <-status
		require.Zero(t, st.Code(), "%v", st.Cause())

		names := make(map[string]bool)
		for _, s := range tel.Spans() {
			names[s.Name()] = true
		}
		for _, name := range []string{"GET /contacts/changes", "GET /contacts/{contactId}", "PUT /contacts/{contactId}"} {
			require.True(t, names[name], "no span named %q among %v", name, names)
		}
		require.Empty(t, errorRecords(tel), "a 422, a 403 and a 404 are answers, not failures")
	})
}

func pemFile(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var b []byte
	for _, der := range cert.Certificate {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

type response struct {
	status int
	header http.Header
	body   string
}

func send(t *testing.T, c *http.Client, method, target string, form url.Values, header map[string]string) response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

func text(t *testing.T, row string) string {
	t.Helper()
	_, rest, ok := strings.Cut(row, `<span class="text">`)
	require.True(t, ok, "row = %q", row)
	text, _, ok := strings.Cut(rest, "</span>")
	require.True(t, ok, "row = %q", row)
	return text
}

type feed struct {
	events <-chan string
	ended  <-chan feedEnd
}

type feedEnd struct {
	err error
	at  time.Time
}

func open(t *testing.T, c *http.Client, target string) feed {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	events := make(chan string, 16)
	ended := make(chan feedEnd, 1)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		r := bufio.NewReader(resp.Body)
		var block []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				if errors.Is(err, io.EOF) && line == "" && len(block) == 0 {
					err = nil
				}
				ended <- feedEnd{err: err, at: time.Now()}
				return
			}
			line = strings.TrimSuffix(line, "\n")
			if strings.HasPrefix(line, ":") {
				events <- line
				continue
			}
			if line != "" {
				block = append(block, line)
				continue
			}
			events <- strings.Join(block, "\n")
			block = nil
		}
	}()
	return feed{events: events, ended: ended}
}

func errorRecords(tel *humustest.Telemetry) []sdklog.Record {
	var rs []sdklog.Record
	for _, r := range tel.Logs() {
		if r.Severity() >= log.SeverityError {
			rs = append(rs, r)
		}
	}
	return rs
}
