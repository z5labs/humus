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
	"github.com/z5labs/bedrock/host/hosttest"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

func TestInit_datastar_edits_a_row_in_place_and_aborts_its_feed_at_shutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, hosttest.WithFiles(map[string][]byte{
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
		datastar := map[string]string{"Datastar-Request": "true", "Sec-Fetch-Site": "same-origin"}

		changes := open(t, c, base+"/contacts/changes")

		bad := send(t, c, http.MethodPut, base+"/contacts/1", url.Values{"name": {""}, "email": {"joe"}}, datastar)
		require.Equal(t, http.StatusOK, bad.status, "Datastar processes only a 200")
		require.Equal(t, "text/event-stream", bad.header.Get("Content-Type"))
		require.True(t, strings.HasPrefix(bad.body, "event: datastar-patch-elements\ndata: elements <form id=\"contact-1\""), "body = %q", bad.body)
		require.Contains(t, bad.body, "Enter a name.")
		require.Contains(t, bad.body, "Enter an email address.")
		require.Contains(t, bad.body, `<input name="email" value="joe">`, "the form keeps the values sent")
		require.Contains(t, bad.body, `<form id="contact-1" data-on:submit="@put('\/contacts\/1', {contentType: 'form'})">`, "html/template escapes the URL as a JavaScript string")
		for _, line := range strings.Split(strings.TrimSuffix(bad.body, "\n\n"), "\n")[1:] {
			require.True(t, strings.HasPrefix(line, "data: elements "), "line = %q", line)
		}

		good := send(t, c, http.MethodPut, base+"/contacts/1", url.Values{"name": {"Joe"}, "email": {"joe@example.com"}}, datastar)
		require.Equal(t, http.StatusOK, good.status)
		require.Equal(t, "text/event-stream", good.header.Get("Content-Type"))
		const patched = "event: datastar-patch-elements\ndata: elements <div id=\"contact-1\"><span class=\"text\">Joe joe@example.com</span>"
		require.True(t, strings.HasPrefix(good.body, patched), "body = %q", good.body)
		require.True(t, strings.HasPrefix(<-changes.events, patched), "the open feed gets the same row")
		require.Equal(t, ": keep-alive", <-changes.events, "a keep-alive comment 15s after the feed opened, past the 10s WriteTimeout")

		crossSite := send(t, c, http.MethodPut, base+"/contacts/1", url.Values{"name": {"Mallory"}, "email": {"mallory@example.com"}}, map[string]string{"Datastar-Request": "true", "Sec-Fetch-Site": "cross-site"})
		require.Equal(t, http.StatusForbidden, crossSite.status)

		list := send(t, c, http.MethodGet, base+"/contacts", nil, nil)
		require.Equal(t, http.StatusOK, list.status)
		require.Equal(t, "text/html; charset=utf-8", list.header.Get("Content-Type"))
		require.Contains(t, list.body, "<!DOCTYPE html>")
		require.Contains(t, list.body, `<script type="module" src="/static/datastar.js"></script>`)
		require.Contains(t, list.body, `<div data-init="@get('/contacts/changes', {openWhenHidden: true})"></div>`)
		require.Contains(t, list.body, `<div id="contact-1"><span class="text">Joe joe@example.com</span>`, "the cross-site PUT changed nothing")
		require.Contains(t, list.body, `<button data-on:click="@get('\/contacts\/1\/edit')">Edit</button>`)
		for _, id := range []string{"2", "3"} {
			require.Contains(t, list.body, `<div id="contact-`+id+`">`)
		}

		editPatch := send(t, c, http.MethodGet, base+"/contacts/2/edit", nil, datastar)
		editPage := send(t, c, http.MethodGet, base+"/contacts/2/edit", nil, nil)
		for _, r := range []response{editPatch, editPage} {
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, []string{"Datastar-Request"}, r.header.Values("Vary"))
		}
		require.Equal(t, "text/event-stream", editPatch.header.Get("Content-Type"))
		require.True(t, strings.HasPrefix(editPatch.body, "event: datastar-patch-elements\ndata: elements <form id=\"contact-2\" data-on:submit="), "body = %q", editPatch.body)
		require.Equal(t, "text/html; charset=utf-8", editPage.header.Get("Content-Type"))
		require.Contains(t, editPage.body, "<!DOCTYPE html>")
		require.Contains(t, editPage.body, `<form id="contact-2" data-on:submit=`)

		script := send(t, c, http.MethodGet, base+"/static/datastar.js", nil, nil)
		require.Equal(t, http.StatusOK, script.status)
		require.Equal(t, "text/javascript; charset=utf-8", script.header.Get("Content-Type"))
		require.True(t, strings.HasPrefix(script.body, "// Datastar v"), "body = %.40q", script.body)

		for _, target := range []string{"/nowhere", "/contacts/0/edit", "/contacts/99/edit", "/static/nothing.js", "/static/"} {
			r := send(t, c, http.MethodGet, base+target, nil, nil)
			require.Equal(t, http.StatusNotFound, r.status, target)
			require.Contains(t, r.body, "<h1>Not Found</h1>", target)
		}

		for _, m := range []struct{ method, target, allow string }{
			{http.MethodDelete, "/contacts/1", "PUT"},
			{http.MethodGet, "/contacts/1", "PUT"},
			{http.MethodPut, "/contacts/1/edit", "GET, HEAD"},
		} {
			r := send(t, c, m.method, base+m.target, url.Values{}, datastar)
			require.Equal(t, http.StatusMethodNotAllowed, r.status, "%s %s", m.method, m.target)
			require.Equal(t, m.allow, r.header.Get("Allow"), "%s %s", m.method, m.target)
			require.Contains(t, r.body, "<h1>Method Not Allowed</h1>", "%s %s", m.method, m.target)
		}

		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)
		end := <-changes.ended
		require.Error(t, end.err, "a network error, which Datastar reconnects after")
		require.Equal(t, 5*time.Second, end.at.Sub(sigterm), "the feed fails as Shutdown starts, after the drain")
		st := <-status
		require.Zero(t, st.Code(), "%v", st.Cause())

		names := make(map[string]bool)
		for _, s := range tel.Spans() {
			names[s.Name()] = true
		}
		for _, name := range []string{"GET /contacts", "GET /contacts/{contactId}/edit", "PUT /contacts/{contactId}"} {
			require.True(t, names[name], "no span named %q among %v", name, names)
		}
		require.Empty(t, errorRecords(tel), "a 403, a 404 and an aborted feed are answers, not failures")
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
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

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
