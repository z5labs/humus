// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/httpserver"
)

// Operator is the end user's principal: who operates the admin API.
type Operator struct {
	Name  string
	Roles []string
}

// HasScope reports whether the operator has role, so that RequireScopes
// can require a role.
func (o Operator) HasScope(role string) bool { return slices.Contains(o.Roles, role) }

// Directory is where the end user keeps operators' passwords, such as an
// LDAP server.
type Directory interface {
	// Password returns name's password and roles, or ok false when there is
	// no operator name. Its error is a directory that can't be reached.
	Password(ctx context.Context, name string) (password string, roles []string, ok bool, err error)
}

// AdminInit serves an admin API, on a mux beside a health probe that stays
// open. An operator authenticates with a client certificate the server's
// TLS verified, or with a password, and changing anything needs the
// "admin" role.
func AdminInit(dir Directory, tlsConfig *tls.Config) func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		// A certificate is verified by the handshake, against ClientCAs:
		// its common name is the operator, who has every role.
		cert := auth.MutualTLS("cert", func(_ context.Context, chains [][]*x509.Certificate) (Operator, error) {
			return Operator{Name: chains[0][0].Subject.CommonName, Roles: []string{"admin"}}, nil
		})
		password := auth.Basic("password", "admin", func(ctx context.Context, name, pw string) (Operator, error) {
			want, roles, ok, err := dir.Password(ctx, name)
			if err != nil {
				return Operator{}, err // the directory is down: the server's error, never a 401
			}
			if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(want)) != 1 {
				return Operator{}, auth.ErrUnauthenticated
			}
			return Operator{Name: name, Roles: roles}, nil
		})

		answers := auth.Answers{
			Unauthenticated: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "sign in", http.StatusUnauthorized)
			}),
			Forbidden: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "you need the admin role", http.StatusForbidden)
			}),
			Failed: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "try again later", http.StatusServiceUnavailable)
			}),
		}
		whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			op, ok := cert.From(r.Context())
			if !ok {
				op, _ = password.From(r.Context())
			}
			_, _ = io.WriteString(w, op.Name)
		})
		reads, err := auth.NewHandler(s, whoami, answers, auth.Require(cert), auth.Require(password))
		if err != nil {
			return nil, err
		}
		writes, err := auth.NewHandler(s, whoami, answers,
			auth.RequireScopes(cert, "admin"), auth.RequireScopes(password, "admin"),
		)
		if err != nil {
			return nil, err
		}

		mux := http.NewServeMux()
		mux.Handle("GET /admin/whoami", reads)
		mux.Handle("POST /admin/whoami", writes)
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})

		settings := httpserver.DefaultSettings()
		settings.Addr = ":8443"
		settings.TLS = tlsConfig // with ClientAuth tls.VerifyClientCertIfGiven, and ClientCAs
		svc, err := httpserver.NewService(s, settings, mux)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}
}

// directory is a Directory that can't be reached.
type directory struct{}

func (directory) Password(context.Context, string) (string, []string, bool, error) {
	return "", nil, false, errors.New("the directory isn't configured")
}

// An admin API on a plain mux, with a probe beside it that stays open.
// This example has no Output comment, so it is compiled but never run:
// running it would exit the test binary.
func ExampleNewHandler() {
	httpserver.Main(AdminInit(directory{}, &tls.Config{ClientAuth: tls.VerifyClientCertIfGiven}))
}
