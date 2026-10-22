// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/humustest"
)

// basicAuth returns an edit that sends username and password as Basic
// credentials.
func basicAuth(username, password string) func(*http.Request) {
	return header("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
}

// commonName is MutualTLS's verify function: the principal is the client
// certificate's common name.
func commonName(_ context.Context, chains [][]*x509.Certificate) (user, error) {
	return user{name: chains[0][0].Subject.CommonName}, nil
}

// adminMux is the end user's mux: /admin/ behind NewHandler, by mutual TLS
// or Basic, and /healthz beside it, open.
func adminMux(mtls, basic auth.Scheme[user]) func(s *humus.Setup) (http.Handler, error) {
	return func(s *humus.Setup) (http.Handler, error) {
		admin, err := auth.NewHandler(s,
			principals(map[string]auth.Scheme[user]{"mtls": mtls, "basic": basic}),
			answers,
			auth.Require(mtls), auth.Require(basic),
		)
		if err != nil {
			return nil, err
		}
		mux := http.NewServeMux()
		mux.Handle("/admin/", admin)
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})
		return mux, nil
	}
}

func TestNewHandler_on_a_mux(t *testing.T) {
	t.Run("a verified client certificate, Basic, and a probe left open", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(user{name: "root"})
			mtls := auth.MutualTLS("mtls", commonName)
			basic := auth.Basic("basic", "admin", creds.verifyBasic)
			s := serve(t, adminMux(mtls, basic), func(c *tls.Config, ca *humustest.CA) {
				c.ClientAuth = tls.VerifyClientCertIfGiven
				c.ClientCAs = ca.Pool()
			})

			r := s.send(s.clientWith(s.ca.Issue(t, "alice")), s.request("/admin/users"))
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "mtls:alice", r.body)

			r = s.get("/admin/users")
			require.Equal(t, http.StatusUnauthorized, r.status)
			require.Equal(t, "the end user's 401", r.body)
			require.Equal(t, []string{`Basic realm="admin", charset="UTF-8"`}, r.challenges())

			r = s.get("/admin/users", basicAuth("root", secrets["root"]))
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "basic:root", r.body)

			r = s.get("/admin/users", basicAuth("root", rand.Text()))
			require.Equal(t, http.StatusUnauthorized, r.status)
			require.Equal(t, []string{`Basic realm="admin", charset="UTF-8"`}, r.challenges())

			r = s.get("/healthz")
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "ok", r.body)

			s.stop()
			require.Empty(t, records(s.tel), "a refusal isn't recorded")
		})
	})

	t.Run("a certificate requested but not verified is no credential", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, _ := newCredentials()
			var calls atomic.Int64
			mtls := auth.MutualTLS("mtls", func(ctx context.Context, chains [][]*x509.Certificate) (user, error) {
				calls.Add(1)
				return commonName(ctx, chains)
			})
			s := serve(t, adminMux(mtls, auth.Basic("basic", "admin", creds.verifyBasic)), func(c *tls.Config, _ *humustest.CA) {
				c.ClientAuth = tls.RequestClientCert
			})

			r := s.send(s.clientWith(s.ca.Issue(t, "alice")), s.request("/admin/users"))
			require.Equal(t, http.StatusUnauthorized, r.status)
			require.Zero(t, calls.Load())

			s.stop()
		})
	})
}

func TestNewHandler_keeps_the_route_its_handler_sets_for_the_span(t *testing.T) {
	var names []string
	synctest.Test(t, func(t *testing.T) {
		creds, secrets := newCredentials(user{name: "root"})
		basic := auth.Basic("basic", "admin", creds.verifyBasic)
		s := serve(t, func(s *humus.Setup) (http.Handler, error) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /admin/users/{name}", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, r.PathValue("name"))
			})
			return auth.NewHandler(s, mux, answers, auth.Require(basic))
		}, nil)

		r := s.get("/admin/users/alice", basicAuth("root", secrets["root"]))
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "alice", r.body)

		s.stop()
		for _, span := range serverSpans(s.tel) {
			names = append(names, span.Name())
		}
	})
	require.Equal(t, []string{"GET /admin/users/{name}"}, names)
}

// everyKey is a list with one requirement for each way to send a bearer
// token, Basic credentials and an API key, and the credentials they take.
type everyKey struct {
	creds   *credentials
	secrets map[string]string
	reqs    []auth.Requirement
	schemes map[string]auth.Scheme[user]
}

func newEveryKey() everyKey {
	creds, secrets := newCredentials(user{name: "alice"})
	schemes := map[string]auth.Scheme[user]{
		"bearer": auth.Bearer("bearer", creds.verify),
		"basic":  auth.Basic("basic", "carts", creds.verifyBasic),
		"header": auth.APIKeyHeader("header", "X-API-Key", creds.verify),
		"query":  auth.APIKeyQuery("query", "api_key", creds.verify),
		"cookie": auth.APIKeyCookie("cookie", "session", creds.verify),
	}
	var reqs []auth.Requirement
	for _, name := range []string{"bearer", "basic", "header", "query", "cookie"} {
		reqs = append(reqs, auth.Require(schemes[name]))
	}
	return everyKey{creds: creds, secrets: secrets, reqs: reqs, schemes: schemes}
}

func (k everyKey) serve(t *testing.T) *server {
	return serve(t, func(s *humus.Setup) (http.Handler, error) {
		return auth.NewHandler(s, principals(k.schemes), answers, k.reqs...)
	}, nil)
}

func TestNewHandler_reads_each_credential(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k := newEveryKey()
		s := k.serve(t)
		secret := k.secrets["alice"]

		for name, edit := range map[string]func(*http.Request){
			"bearer": bearer(secret),
			"basic":  basicAuth("alice", secret),
			"header": header("X-API-Key", secret),
			"query":  func(r *http.Request) { r.URL.RawQuery = "api_key=" + secret },
			"cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "session", Value: secret}) },
		} {
			r := s.get("/carts", edit)
			require.Equal(t, http.StatusOK, r.status, name)
			require.Equal(t, name+":alice", r.body)
		}

		for _, scheme := range []string{"bearer", "BEARER", "bEaReR"} {
			r := s.get("/carts", header("Authorization", scheme+" "+secret))
			require.Equal(t, http.StatusOK, r.status, "the scheme name %q matches whatever its case", scheme)
		}
		r := s.get("/carts", basicAuth("alice", secret), func(r *http.Request) {
			r.Header.Set("Authorization", "BASIC "+r.Header.Get("Authorization")[len("Basic "):])
		})
		require.Equal(t, http.StatusOK, r.status)

		s.stop()
	})
}

func TestNewHandler_answers_a_malformed_credential_with_a_401(t *testing.T) {
	everyChallenge := []string{"Bearer", `Basic realm="carts", charset="UTF-8"`}
	invalidToken := []string{`Bearer error="invalid_token"`, `Basic realm="carts", charset="UTF-8"`}
	for _, tc := range []struct {
		name       string
		edit       func(secret string) func(*http.Request)
		challenges []string
	}{
		{
			name: "two Authorization headers",
			edit: func(secret string) func(*http.Request) {
				return header("Authorization", "Bearer "+secret, "Bearer "+secret)
			},
			challenges: invalidToken,
		},
		{
			name:       "Bearer with no token",
			edit:       func(string) func(*http.Request) { return header("Authorization", "Bearer") },
			challenges: invalidToken,
		},
		{
			name:       "Bearer with spaces for a token",
			edit:       func(string) func(*http.Request) { return header("Authorization", "Bearer    ") },
			challenges: invalidToken,
		},
		{
			name:       "Bearer with a token that isn't a token68",
			edit:       func(secret string) func(*http.Request) { return header("Authorization", "Bearer "+secret+" "+secret) },
			challenges: invalidToken,
		},
		{
			name:       "Basic that isn't base64",
			edit:       func(string) func(*http.Request) { return header("Authorization", "Basic not*base64") },
			challenges: everyChallenge,
		},
		{
			name: "Basic with no colon",
			edit: func(secret string) func(*http.Request) {
				return header("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alice"+secret)))
			},
			challenges: everyChallenge,
		},
		{
			name:       "an empty API key",
			edit:       func(string) func(*http.Request) { return header("X-API-Key", "") },
			challenges: everyChallenge,
		},
		{
			name:       "an API key given twice",
			edit:       func(secret string) func(*http.Request) { return header("X-API-Key", secret, secret) },
			challenges: everyChallenge,
		},
		{
			name: "an empty query API key",
			edit: func(string) func(*http.Request) {
				return func(r *http.Request) { r.URL.RawQuery = "api_key=" }
			},
			challenges: everyChallenge,
		},
		{
			name: "a query API key given twice",
			edit: func(secret string) func(*http.Request) {
				return func(r *http.Request) { r.URL.RawQuery = "api_key=" + secret + "&api_key=" + secret }
			},
			challenges: everyChallenge,
		},
		{
			name: "an empty cookie API key",
			edit: func(string) func(*http.Request) {
				return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "session", Value: ""}) }
			},
			challenges: everyChallenge,
		},
		{
			name: "a cookie API key given twice",
			edit: func(secret string) func(*http.Request) {
				return func(r *http.Request) {
					r.AddCookie(&http.Cookie{Name: "session", Value: secret})
					r.AddCookie(&http.Cookie{Name: "session", Value: secret})
				}
			},
			challenges: everyChallenge,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				k := newEveryKey()
				s := k.serve(t)

				r := s.get("/carts", tc.edit(k.secrets["alice"]))
				require.Equal(t, http.StatusUnauthorized, r.status)
				require.Equal(t, "the end user's 401", r.body)
				require.Equal(t, tc.challenges, r.challenges())
				require.Zero(t, k.creds.calls.Load(), "a malformed credential reaches no verifier")

				s.stop()
				require.Empty(t, records(s.tel))
			})
		})
	}
}

func TestNewHandler_answers_a_missing_scope_with_a_403(t *testing.T) {
	t.Run("a bearer scheme, with the scopes in its challenge", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(
				user{name: "alice", scopes: []string{"carts:read"}},
				user{name: "admin", scopes: []string{"carts:read", "carts:write"}},
			)
			bearerScheme := auth.Bearer("bearer", creds.verify).Format("JWT")
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s, principals(map[string]auth.Scheme[user]{"bearer": bearerScheme}), answers,
					auth.RequireScopes(bearerScheme, "carts:read", "carts:write"),
				)
			}, nil)

			r := s.get("/carts", bearer(secrets["alice"]))
			require.Equal(t, http.StatusForbidden, r.status)
			require.Equal(t, "the end user's 403", r.body)
			require.Equal(t, []string{`Bearer error="insufficient_scope", scope="carts:read carts:write"`}, r.challenges())

			r = s.get("/carts", bearer(secrets["admin"]))
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "bearer:admin", r.body)

			r = s.get("/carts", bearer(rand.Text()))
			require.Equal(t, http.StatusUnauthorized, r.status, "a rejected token is a 401, never a 403")
			require.Equal(t, []string{`Bearer error="invalid_token"`}, r.challenges())

			s.stop()
			require.Empty(t, records(s.tel), "a refusal isn't recorded")
		})
	})

	t.Run("a role on Basic, with no challenge", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(user{name: "alice"})
			basic := auth.Basic("basic", "carts", creds.verifyBasic)
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s, principals(map[string]auth.Scheme[user]{"basic": basic}), answers,
					auth.RequireScopes(basic, "admin"),
				)
			}, nil)

			r := s.get("/carts", basicAuth("alice", secrets["alice"]))
			require.Equal(t, http.StatusForbidden, r.status)
			require.Empty(t, r.challenges())

			s.stop()
			require.Empty(t, records(s.tel))
		})
	})

	t.Run("one requirement lacking a scope, beside one unauthenticated", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(user{name: "alice"})
			bearerScheme := auth.Bearer("bearer", creds.verify)
			apiKey := auth.APIKeyHeader("apiKey", "X-API-Key", creds.verify)
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s, principals(nil), answers,
					auth.Require(apiKey),
					auth.RequireScopes(bearerScheme, "carts:write"),
				)
			}, nil)

			r := s.get("/carts", bearer(secrets["alice"]))
			require.Equal(t, http.StatusForbidden, r.status)
			require.Equal(t, []string{`Bearer error="insufficient_scope", scope="carts:write"`}, r.challenges())

			s.stop()
		})
	})
}

func TestNewHandler_records_a_verifiers_other_error(t *testing.T) {
	t.Run("once, then Answers.Failed, whatever the requirements after it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(user{name: "alice"})
			down := rand.Text()
			bearerScheme := auth.Bearer("bearer", func(ctx context.Context, token string) (user, error) {
				if token == down {
					return user{}, errors.New("the directory is down")
				}
				return creds.verify(ctx, token)
			})
			apiKey := auth.APIKeyHeader("apiKey", "X-API-Key", creds.verify)
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s, principals(nil), answers, auth.Require(bearerScheme), auth.Require(apiKey))
			}, nil)

			r := s.get("/carts", bearer(down), header("X-API-Key", secrets["alice"]))
			require.Equal(t, http.StatusServiceUnavailable, r.status)
			require.Equal(t, "the end user's 503", r.body)
			require.Empty(t, r.challenges())
			require.Zero(t, creds.calls.Load(), "the request ended at the verifier's error")

			s.stop()
			spans := serverSpans(s.tel)
			require.Len(t, spans, 1)
			require.Equal(t, []string{"the directory is down"}, exceptionMessages(spans[0]))

			recs := records(s.tel)
			require.Len(t, recs, 1)
			require.Equal(t, "authentication failed", recs[0].Body().AsString())
			require.Equal(t, log.SeverityError, recs[0].Severity())
			require.Equal(t, spans[0].SpanContext().TraceID(), recs[0].TraceID())
			require.Equal(t, spans[0].SpanContext().SpanID(), recs[0].SpanID())
			require.Equal(t, map[string]string{
				"humus.auth.scheme":   "bearer",
				"error.type":          "*errors.errorString",
				"exception.message":   "the directory is down",
				"http.request.method": "GET",
			}, attrs(recs[0]), "no principal, subject or key is recorded")
		})
	})

	t.Run("an error that wraps ErrUnauthenticated is a 401, its text never sent", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			bearerScheme := auth.Bearer("bearer", func(context.Context, string) (user, error) {
				return user{}, fmt.Errorf("the token expired at 10:04: %w", auth.ErrUnauthenticated)
			})
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s, principals(nil), answers, auth.Require(bearerScheme))
			}, nil)

			r := s.get("/carts", bearer(rand.Text()))
			require.Equal(t, http.StatusUnauthorized, r.status)
			require.NotContains(t, r.body, "10:04")
			require.NotContains(t, fmt.Sprint(r.header), "10:04")

			s.stop()
			require.Empty(t, records(s.tel))
		})
	})

	t.Run("after the client went away, on the span only", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			started := make(chan struct{})
			bearerScheme := auth.Bearer("bearer", func(ctx context.Context, _ string) (user, error) {
				close(started)
				<-ctx.Done()
				return user{}, ctx.Err()
			})
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s, principals(nil), answers, auth.Require(bearerScheme))
			}, nil)

			ctx, cancel := context.WithCancel(t.Context())
			req := s.request("/carts", bearer(rand.Text())).WithContext(ctx)
			done := make(chan error, 1)
			go func() {
				resp, err := s.client.Do(req)
				if err == nil {
					_ = resp.Body.Close()
				}
				done <- err
			}()
			<-started
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)

			s.stop()
			spans := serverSpans(s.tel)
			require.Len(t, spans, 1)
			require.Equal(t, []string{"context canceled"}, exceptionMessages(spans[0]))
			require.Empty(t, records(s.tel))
		})
	})
}

func TestNewHandler_passes_a_verifiers_panic_to_httpserver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bearerScheme := auth.Bearer("bearer", func(context.Context, string) (user, error) {
			panic("the verifier broke")
		})
		s := serve(t, func(s *humus.Setup) (http.Handler, error) {
			return auth.NewHandler(s, principals(nil), answers, auth.Require(bearerScheme))
		}, nil)

		_, err := s.client.Do(s.request("/carts", bearer(rand.Text())))
		require.Error(t, err, "httpserver aborts the response")

		s.stop()
		require.Empty(t, records(s.tel))
		var recs []sdklog.Record
		for _, r := range recordsOf(s.tel, "github.com/z5labs/humus/httpserver") {
			if r.Severity() == log.SeverityError {
				recs = append(recs, r)
			}
		}
		require.Len(t, recs, 1)
		require.Equal(t, "handler panicked", recs[0].Body().AsString())
		require.Equal(t, "the verifier broke", attrs(recs[0])["exception.message"])
	})
}

func TestNewHandler_serves_Anonymous(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		creds, secrets := newCredentials(user{name: "alice"})
		bearerScheme := auth.Bearer("bearer", creds.verify)
		apiKey := auth.APIKeyHeader("apiKey", "X-API-Key", creds.verify)
		s := serve(t, func(s *humus.Setup) (http.Handler, error) {
			return auth.NewHandler(s,
				principals(map[string]auth.Scheme[user]{"bearer": bearerScheme, "apiKey": apiKey}),
				answers,
				auth.Require(bearerScheme), auth.Require(apiKey), auth.Anonymous(),
			)
		}, nil)

		r := s.get("/me")
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "none", r.body, "From returns false after Anonymous")

		r = s.get("/me", bearer(secrets["alice"]))
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "bearer:alice", r.body)

		r = s.get("/me", bearer(rand.Text()))
		require.Equal(t, http.StatusUnauthorized, r.status, "a rejected token is never served anonymously")
		require.Equal(t, []string{`Bearer error="invalid_token"`}, r.challenges())

		r = s.get("/me", header("X-API-Key", rand.Text()))
		require.Equal(t, http.StatusUnauthorized, r.status, "a rejected key is never served anonymously")
		require.Equal(t, []string{"Bearer"}, r.challenges())

		r = s.get("/me", header("X-API-Key", ""))
		require.Equal(t, http.StatusUnauthorized, r.status, "a malformed key is never served anonymously")

		s.stop()
		require.Empty(t, records(s.tel))
	})
}

func TestNewHandler_serves_Disabled_through_the_engine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		creds, secrets := newCredentials(user{name: "alice"})
		bearerScheme := auth.Bearer("bearer", creds.verify)
		s := serve(t, func(s *humus.Setup) (http.Handler, error) {
			return auth.NewHandlerTakingDisabled(s, principals(map[string]auth.Scheme[user]{"bearer": bearerScheme}), answers, auth.Disabled())
		}, nil)

		r := s.get("/carts", bearer(rand.Text()))
		require.Equal(t, http.StatusOK, r.status, "a forged credential is served")
		require.Equal(t, "none", r.body)

		r = s.get("/carts", bearer(secrets["alice"]))
		require.Equal(t, http.StatusOK, r.status)
		require.Equal(t, "none", r.body, "From returns false, even for a valid credential")
		require.Zero(t, creds.calls.Load(), "no verifier is called")

		s.stop()
	})
}

func TestScheme_From(t *testing.T) {
	t.Run("after All, each scheme's principal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(user{name: "alice"}, user{name: "batch"})
			bearerScheme := auth.Bearer("bearer", creds.verify)
			apiKey := auth.APIKeyHeader("apiKey", "X-API-Key", creds.verify)
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s,
					principals(map[string]auth.Scheme[user]{"bearer": bearerScheme, "apiKey": apiKey}),
					answers,
					auth.All(auth.Require(bearerScheme), auth.Require(apiKey)),
				)
			}, nil)

			r := s.get("/carts", bearer(secrets["alice"]), header("X-API-Key", secrets["batch"]))
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "apiKey:batch bearer:alice", r.body)

			r = s.get("/carts", bearer(secrets["alice"]))
			require.Equal(t, http.StatusUnauthorized, r.status, "All needs every scheme")

			s.stop()
		})
	})

	t.Run("a scheme two requirements name is verified once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(user{name: "alice"})
			bearerScheme := auth.Bearer("bearer", creds.verify)
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s,
					principals(map[string]auth.Scheme[user]{"bearer": bearerScheme}),
					answers,
					auth.RequireScopes(bearerScheme, "admin"), auth.Require(bearerScheme),
				)
			}, nil)

			r := s.get("/carts", bearer(secrets["alice"]))
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "bearer:alice", r.body)
			require.EqualValues(t, 1, creds.calls.Load())

			s.stop()
		})
	})

	t.Run("only the principals of the requirement met", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(user{name: "alice"}, user{name: "batch"})
			bearerScheme := auth.Bearer("bearer", creds.verify)
			apiKey := auth.APIKeyHeader("apiKey", "X-API-Key", creds.verify)
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s,
					principals(map[string]auth.Scheme[user]{"bearer": bearerScheme, "apiKey": apiKey}),
					answers,
					auth.RequireScopes(bearerScheme, "admin"), auth.Require(apiKey),
				)
			}, nil)

			r := s.get("/carts", bearer(secrets["alice"]), header("X-API-Key", secrets["batch"]))
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "apiKey:batch", r.body, "the bearer scheme verified alice, but didn't serve her")

			s.stop()
		})
	})

	t.Run("through a copy of the scheme", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			creds, secrets := newCredentials(user{name: "alice"})
			bearerScheme := auth.Bearer("bearer", creds.verify)
			s := serve(t, func(s *humus.Setup) (http.Handler, error) {
				return auth.NewHandler(s,
					principals(map[string]auth.Scheme[user]{"bearer": bearerScheme}),
					answers,
					auth.Require(bearerScheme.Format("JWT").Description("A token from the IdP.")),
				)
			}, nil)

			r := s.get("/carts", bearer(secrets["alice"]))
			require.Equal(t, http.StatusOK, r.status)
			require.Equal(t, "bearer:alice", r.body)

			s.stop()
		})
	})

	t.Run("the zero Scheme and a context without principals", func(t *testing.T) {
		_, ok := auth.Scheme[user]{}.From(t.Context())
		require.False(t, ok)
		_, ok = auth.Bearer[user]("bearer", nil).From(t.Context())
		require.False(t, ok)
		zero := auth.Scheme[user]{}.Description("d").Format("JWT")
		_, ok = zero.From(t.Context())
		require.False(t, ok)
	})
}
