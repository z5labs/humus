// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"crypto/rand"
	"crypto/tls"
	"io"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

func TestSessionInit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		dir := memDirectory{"bob": {password: rand.Text()}}
		sessions := NewSessions()
		tlsConfig := &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}}
		p, status := httpservertest.Start(t, h.Host(), SessionInit(dir, sessions, tlsConfig),
			humustest.WithApp(humus.WithTelemetry(tel.Builder())))
		<-h.Listening("localhost:8443")
		client := httpservertest.NewClient(h, ca.Pool())
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

		do := func(method, path string, form url.Values, site string, cookies ...*http.Cookie) *http.Response {
			t.Helper()
			var body io.Reader
			if form != nil {
				body = strings.NewReader(form.Encode())
			}
			req, err := http.NewRequestWithContext(t.Context(), method, "https://localhost:8443"+path, body)
			require.NoError(t, err)
			if form != nil {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			req.Header.Set("Sec-Fetch-Site", site)
			for _, c := range cookies {
				req.AddCookie(c)
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			return resp
		}
		read := func(path string, cookies ...*http.Cookie) (int, string) {
			t.Helper()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://localhost:8443"+path, nil)
			require.NoError(t, err)
			for _, c := range cookies {
				req.AddCookie(c)
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.StatusCode, string(b)
		}

		resp := do(http.MethodGet, "/", nil, "none")
		require.Equal(t, http.StatusSeeOther, resp.StatusCode, "no cookie: sign in first")
		require.Equal(t, "/sign-in", resp.Header.Get("Location"))
		code, form := read("/sign-in")
		require.Equal(t, http.StatusOK, code)
		require.Contains(t, form, `<form method="post" action="/sign-in">`)

		resp = do(http.MethodGet, "/", nil, "none", &http.Cookie{Name: "__Host-session", Value: rand.Text()})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode, "a cookie naming no session")

		resp = do(http.MethodPost, "/sign-in", url.Values{"name": {"bob"}, "password": {rand.Text()}}, "same-origin")
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		require.Empty(t, resp.Cookies())

		resp = do(http.MethodPost, "/sign-in", url.Values{"name": {"bob"}, "password": {dir["bob"].password}}, "cross-site")
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "CrossOriginProtection refuses a cross-site POST")
		require.Empty(t, resp.Cookies())

		resp = do(http.MethodPost, "/sign-in", url.Values{"name": {"bob"}, "password": {dir["bob"].password}}, "same-origin")
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		require.Equal(t, "/", resp.Header.Get("Location"))
		cookies := resp.Cookies()
		require.Len(t, cookies, 1)
		cookie := cookies[0]
		require.Equal(t, "__Host-session", cookie.Name)
		require.Equal(t, "/", cookie.Path)
		require.True(t, cookie.Secure)
		require.True(t, cookie.HttpOnly)
		require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
		session := &http.Cookie{Name: cookie.Name, Value: cookie.Value}

		code, page := read("/", session)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "Signed in as bob", page)

		resp = do(http.MethodPost, "/sign-out", url.Values{}, "cross-site", session)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "a cross-site POST carrying the cookie is refused")
		code, _ = read("/", session)
		require.Equal(t, http.StatusOK, code, "and the session goes on")

		resp = do(http.MethodPost, "/sign-out", url.Values{}, "same-origin", session)
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		require.Equal(t, "/sign-in", resp.Header.Get("Location"))
		cleared := resp.Cookies()
		require.Len(t, cleared, 1)
		require.Equal(t, "__Host-session", cleared[0].Name)
		require.Negative(t, cleared[0].MaxAge, "the browser drops the cookie")
		resp = do(http.MethodGet, "/", nil, "same-origin", session)
		require.Equal(t, http.StatusSeeOther, resp.StatusCode, "the session ended")

		resp = do(http.MethodPost, "/sign-out", url.Values{}, "same-origin")
		require.Equal(t, http.StatusSeeOther, resp.StatusCode, "signing out needs a session")
		require.Empty(t, resp.Cookies())

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Empty(t, records(tel), "an unauthenticated request is answered, not recorded")
	})
}
