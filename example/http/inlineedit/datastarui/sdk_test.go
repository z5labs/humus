// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/starfederation/datastar-go/datastar"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/host/hosttest"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/inlineedit/contacts"
	"github.com/z5labs/humus/http/httpserver"
	"github.com/z5labs/humus/http/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/hypermedia"
)

const sdkBase = "https://sdk.internal:8443"

type sdkApp struct {
	client *http.Client
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
	tel    *humustest.Telemetry
}

func startSDK(t *testing.T, routes ...hypermedia.Route) sdkApp {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t, hosttest.WithFiles(map[string][]byte{
		"tls.pem": pemFile(t, ca.Issue(t, "sdk.internal")),
	}))
	tel := humustest.NewTelemetry()
	init := func(ctx context.Context, s *humus.Setup, h humus.Host) ([]humus.Service, error) {
		settings, err := readSettings(ctx, s, h)
		if err != nil {
			return nil, err
		}
		site, err := hypermedia.NewSite(s, contacts.ErrorPage, routes)
		if err != nil {
			return nil, err
		}
		svc, err := httpserver.NewService(ctx, s, h, settings, site)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
	p, status := httpservertest.Start(t, h.Host(), init,
		humustest.WithApp(humus.WithTelemetry(tel.Builder())),
		humustest.WithEnv(map[string]string{
			"ADDR":         "sdk.internal:8443",
			"TLS_PEM_FILE": h.Path("tls.pem"),
		}),
	)
	<-h.Listening("sdk.internal:8443")
	return sdkApp{client: httpservertest.NewClient(h, ca.Pool()), p: p, status: status, tel: tel}
}

func (a sdkApp) stop(t *testing.T) {
	t.Helper()
	<-a.p.Notified(syscall.SIGTERM)
	a.p.Signal(syscall.SIGTERM)
	st := <-a.status
	require.Zero(t, st.Code(), "%v", st.Cause())
}

func TestDatastarGo_NewSSE_PatchElements_gets_its_patch_to_the_client(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app := startSDK(t, hypermedia.NewRoute(http.MethodGet, contacts.List, func(w http.ResponseWriter, r *http.Request) error {
			return datastar.NewSSE(w, r).PatchElements(`<div id="greeting">Hello</div>`)
		}))

		r := send(t, app.client, http.MethodGet, sdkBase+"/contacts", nil, nil)
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "text/event-stream", r.header.Get("Content-Type"))
		require.Contains(t, r.body, "event: datastar-patch-elements\ndata: elements <div id=\"greeting\">Hello</div>\n")

		app.stop(t)
		require.Empty(t, errorRecords(app.tel))
	})
}

func TestDatastarGo_an_action_in_flight_at_SIGTERM_delivers_beside_a_text_html_action(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan string, 2)
		app := startSDK(t,
			hypermedia.NewRoute(http.MethodGet, contacts.List, func(w http.ResponseWriter, r *http.Request) error {
				sse := datastar.NewSSE(w, r)
				started <- "text/event-stream"
				time.Sleep(7 * time.Second)
				return sse.PatchElements(`<div id="report">Done</div>`)
			}),
			hypermedia.NewRoute(http.MethodGet, contacts.Changes, func(w http.ResponseWriter, r *http.Request) error {
				started <- "text/html"
				time.Sleep(7 * time.Second)
				contacts.HTML(w, http.StatusOK)
				_, err := io.WriteString(w, `<div id="report">Done</div>`)
				return err
			}),
		)

		patch := make(chan response, 1)
		page := make(chan response, 1)
		go func() { patch <- send(t, app.client, http.MethodGet, sdkBase+"/contacts", nil, nil) }()
		go func() { page <- send(t, app.client, http.MethodGet, sdkBase+"/contacts/changes", nil, nil) }()
		require.ElementsMatch(t, []string{"text/event-stream", "text/html"}, []string{<-started, <-started})

		<-app.p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		app.p.Signal(syscall.SIGTERM)

		sse := <-patch
		require.Equal(t, http.StatusOK, sse.status)
		require.Equal(t, "text/event-stream", sse.header.Get("Content-Type"))
		require.Contains(t, sse.body, "event: datastar-patch-elements\ndata: elements <div id=\"report\">Done</div>\n")
		html := <-page
		require.Equal(t, http.StatusOK, html.status)
		require.Equal(t, "text/html; charset=utf-8", html.header.Get("Content-Type"))
		require.Equal(t, `<div id="report">Done</div>`, html.body)
		require.Equal(t, 7*time.Second, time.Since(sigterm), "both finish their work, past the 5s drain")

		st := <-app.status
		require.Zero(t, st.Code(), "%v", st.Cause())
		require.Empty(t, errorRecords(app.tel))
	})
}

func TestDatastarGo_an_action_that_fails_after_NewSSE_ends_cleanly_and_is_recorded_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errReport := errors.New("the report could not be built")
		app := startSDK(t, hypermedia.NewRoute(http.MethodGet, contacts.List, func(w http.ResponseWriter, r *http.Request) error {
			sse := datastar.NewSSE(w, r)
			if err := sse.PatchElements(`<div id="report">The report could not be built.</div>`); err != nil {
				return err
			}
			return errReport
		}))

		r := send(t, app.client, http.MethodGet, sdkBase+"/contacts", nil, nil)
		require.Equal(t, http.StatusOK, r.status, "send read the whole body without an error: a clean end")
		require.Equal(t, "text/event-stream", r.header.Get("Content-Type"))
		require.Contains(t, r.body, "data: elements <div id=\"report\">The report could not be built.</div>\n")

		app.stop(t)
		recs := errorRecords(app.tel)
		require.Len(t, recs, 1)
		require.Equal(t, "response failed", recs[0].Body().AsString())
	})
}
