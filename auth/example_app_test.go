// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net/http"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

// memDirectory is a Directory in memory, whose passwords each test makes
// with crypto/rand.
type memDirectory map[string]struct {
	password string
	roles    []string
}

func (d memDirectory) Password(_ context.Context, name string) (string, []string, bool, error) {
	if name == "down" {
		return "", nil, false, errors.New("the directory is down")
	}
	op, ok := d[name]
	return op.password, op.roles, ok, nil
}

func TestAdminInit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		tel := humustest.NewTelemetry()
		dir := memDirectory{
			"bob":   {password: rand.Text()},
			"carol": {password: rand.Text(), roles: []string{"admin"}},
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    ca.Pool(),
		}
		p, status := httpservertest.Start(t, AdminInit(dir, tlsConfig),
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(tel.Builder())))
		<-h.Listening("localhost:8443")
		s := &server{t: t, h: h, ca: ca, tel: tel, client: httpservertest.NewClient(h, ca.Pool()), p: p, status: status}

		post := func(r *http.Request) { r.Method = http.MethodPost }
		withCert := s.clientWith(ca.Issue(t, "alice"))
		r := s.send(withCert, s.request("/admin/whoami", post))
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "alice", r.body)

		r = s.get("/admin/whoami", basicAuth("bob", dir["bob"].password))
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "bob", r.body)

		r = s.get("/admin/whoami", basicAuth("bob", dir["bob"].password), post)
		require.Equal(t, http.StatusForbidden, r.status)
		require.Empty(t, r.challenges())

		r = s.get("/admin/whoami", basicAuth("carol", dir["carol"].password), post)
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "carol", r.body)

		r = s.get("/admin/whoami", basicAuth("carol", dir["bob"].password))
		require.Equal(t, http.StatusUnauthorized, r.status)
		require.Equal(t, []string{`Basic realm="admin", charset="UTF-8"`}, r.challenges())

		r = s.get("/admin/whoami", basicAuth("down", rand.Text()))
		require.Equal(t, http.StatusServiceUnavailable, r.status)

		r = s.get("/healthz")
		require.Equal(t, http.StatusOK, r.status)

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		recs := records(tel)
		require.Len(t, recs, 1)
		require.Equal(t, "password", attrs(recs[0])["humus.auth.scheme"])
	})
}
