// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"io"
	"net/http"
	"sync"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/httpserver"
)

// Sessions is where the end user keeps browser sessions, such as a
// database table. A session's ID is its cookie's value.
type Sessions struct {
	mu    sync.Mutex
	names map[string]string
}

// NewSessions returns a store holding no session.
func NewSessions() *Sessions { return &Sessions{names: make(map[string]string)} }

// Start starts a session for name and returns its ID.
func (s *Sessions) Start(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := rand.Text()
	s.names[id] = name
	return id
}

// Name returns the name of the session id, or ok false when there is none.
func (s *Sessions) Name(id string) (name string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok = s.names[id]
	return name, ok
}

// End ends the session id.
func (s *Sessions) End(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.names, id)
}

// SessionInit serves a browser that signs in with a password and is then
// authenticated by its session cookie, behind http.CrossOriginProtection.
func SessionInit(dir Directory, sessions *Sessions, tlsConfig *tls.Config) func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		session := auth.APIKeyCookie("session", "__Host-session", func(_ context.Context, id string) (string, error) {
			name, ok := sessions.Name(id)
			if !ok {
				return "", auth.ErrUnauthenticated // no such session, or it ended
			}
			return name, nil
		})

		signIn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := r.PostFormValue("name")
			want, _, ok, err := dir.Password(r.Context(), name)
			if err != nil {
				http.Error(w, "try again later", http.StatusServiceUnavailable)
				return
			}
			if !ok || subtle.ConstantTimeCompare([]byte(r.PostFormValue("password")), []byte(want)) != 1 {
				http.Error(w, "wrong name or password", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     "__Host-session",
				Value:    sessions.Start(name),
				Path:     "/",
				Secure:   true,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
		})
		signInForm := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<form method="post" action="/sign-in"><input name="name"> <input name="password" type="password"> <button>Sign in</button></form>`)
		})
		home := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, _ := session.From(r.Context())
			_, _ = io.WriteString(w, "Signed in as "+name)
		})
		signOut := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c, err := r.Cookie("__Host-session"); err == nil {
				sessions.End(c.Value)
			}
			http.SetCookie(w, &http.Cookie{Name: "__Host-session", Path: "/", Secure: true, HttpOnly: true, MaxAge: -1})
			http.Redirect(w, r, "/sign-in", http.StatusSeeOther)
		})

		answers := auth.Answers{
			Unauthenticated: http.RedirectHandler("/sign-in", http.StatusSeeOther),
			Forbidden: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "forbidden", http.StatusForbidden)
			}),
			Failed: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "try again later", http.StatusServiceUnavailable)
			}),
		}
		signedIn := auth.Require(session)
		homePage, err := auth.NewHandler(s, home, answers, signedIn)
		if err != nil {
			return nil, err
		}
		signOutAction, err := auth.NewHandler(s, signOut, answers, signedIn)
		if err != nil {
			return nil, err
		}

		mux := http.NewServeMux()
		mux.Handle("GET /{$}", homePage)
		mux.Handle("GET /sign-in", signInForm)
		mux.Handle("POST /sign-in", signIn)
		mux.Handle("POST /sign-out", signOutAction)

		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = tlsConfig
		svc, err := httpserver.NewService(s, settings, http.NewCrossOriginProtection().Handler(mux))
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}
