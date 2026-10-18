// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi_test

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus/auth"
	openapi "github.com/z5labs/humus/openapi/v3"
)

// user is the principal of every scheme the security tests declare.
type user struct {
	name   string
	scopes []string
}

func (u user) HasScope(scope string) bool { return slices.Contains(u.scopes, scope) }

// keyring holds the valid credentials of one test, each made with
// crypto/rand, so no credential is fixed.
type keyring struct {
	users map[string]user // by token or key
}

// newKeyring returns a keyring holding a new random credential for each of
// users, as creds[name].
func newKeyring(users ...user) (*keyring, map[string]string) {
	k := &keyring{users: make(map[string]user)}
	creds := make(map[string]string)
	for _, u := range users {
		c := rand.Text()
		k.users[c] = u
		creds[u.name] = c
	}
	return k, creds
}

// verify is a verify function of a token or a key: an unknown one wraps
// auth.ErrUnauthenticated.
func (k *keyring) verify(_ context.Context, credential string) (user, error) {
	u, ok := k.users[credential]
	if !ok {
		return user{}, auth.ErrUnauthenticated
	}
	return u, nil
}

// bearerHeader is the header of a request carrying token as a bearer
// token.
func bearerHeader(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// as sends a request of method to path with header, and body as
// contentType when contentType isn't "".
func (s *server) as(method, path string, header http.Header, contentType, body string) reply {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), method, "https://localhost:8443"+path, strings.NewReader(body))
	require.NoError(s.t, err)
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return s.send(req)
}

// The paths of the secured API.
var (
	orderID  = openapi.PathParam[int]("orderId")
	purchase = openapi.Root().Lit("orders").Param(orderID)
	notes    = purchase.Lit("notes")
	me       = openapi.Root().Lit("me")
)

// orderBody is an order, with who asked for it.
type orderBody struct {
	ID   int    `json:"id"`
	User string `json:"user,omitempty"`
	By   string `json:"by,omitempty"`
}

// meBody is who the request was authenticated as, "" for no one.
type meBody struct {
	User string `json:"user"`
}

func TestSecurity_answers_each_request_by_its_credentials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		keys, creds := newKeyring(
			user{name: "alice", scopes: []string{"carts:read"}},
			user{name: "admin", scopes: []string{"carts:read", "carts:write"}},
			user{name: "batch"},
		)
		bearer := auth.Bearer("bearer", keys.verify).Format("JWT")
		apiKey := auth.APIKeyHeader("apiKey", "X-API-Key", keys.verify)

		getOrder := func(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (orderBody, error) {
			o := orderBody{ID: openapi.ParamFrom(req, orderID)}
			if u, ok := bearer.From(ctx); ok {
				o.User = u.name
			}
			if u, ok := apiKey.From(ctx); ok {
				o.By = u.name
			}
			return o, nil
		}
		deleted := make(chan string, 1)
		deleteOrder := func(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
			u, _ := bearer.From(ctx)
			deleted <- u.name
			return openapi.EmptyBody{}, nil
		}
		getMe := func(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (meBody, error) {
			u, _ := bearer.From(ctx)
			return meBody{User: u.name}, nil
		}
		var decoded atomic.Int64
		noteDecoder := func() openapi.Decoder[item] {
			return countingDecoder[item]{Decoder: jsonDecoder[item](), calls: &decoded}
		}

		s := serveAPI(t, []openapi.Operation{
			openapi.Get(purchase, getOrder, jsonEncoder),
			openapi.Delete(purchase, deleteOrder, openapi.EmptyEncoder, openapi.Security(auth.RequireScopes(bearer, "carts:write"))),
			openapi.Post(notes, addItem, noteDecoder, jsonEncoder),
			openapi.Get(me, getMe, jsonEncoder, openapi.Security(auth.Require(bearer), auth.Anonymous())),
			openapi.Document(docPath),
		}, openapi.Security(auth.RequireScopes(bearer, "carts:read"), auth.Require(apiKey)))
		forged := rand.Text()

		r := s.as(http.MethodGet, "/orders/7", nil, "", "")
		require.Equal(t, http.StatusUnauthorized, r.status)
		require.Equal(t, []string{"Bearer"}, r.header.Values("WWW-Authenticate"))
		require.Equal(t, "application/problem+json", r.contentType)
		require.JSONEq(t, `{"status":401,"title":"Unauthorized","detail":"unauthenticated"}`, r.body)

		r = s.as(http.MethodGet, "/orders/abc", nil, "", "")
		require.Equal(t, http.StatusUnauthorized, r.status, "authenticated before the parameters: %s", r.body)

		r = s.as(http.MethodGet, "/orders/7", bearerHeader(forged), "", "")
		require.Equal(t, http.StatusUnauthorized, r.status)
		require.Equal(t, []string{`Bearer error="invalid_token"`}, r.header.Values("WWW-Authenticate"))
		require.JSONEq(t, `{"status":401,"title":"Unauthorized","detail":"unauthenticated"}`, r.body, "no verifier's text")

		r = s.as(http.MethodGet, "/orders/7", bearerHeader(creds["alice"]), "", "")
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `{"id":7,"user":"alice"}`, r.body)
		r = s.as(http.MethodGet, "/orders/7", http.Header{"X-Api-Key": {creds["batch"]}}, "", "")
		require.Equal(t, http.StatusOK, r.status)
		require.JSONEq(t, `{"id":7,"by":"batch"}`, r.body)
		r = s.as(http.MethodGet, "/orders/abc", bearerHeader(creds["alice"]), "", "")
		require.Equal(t, http.StatusBadRequest, r.status, "the parameters, once authenticated")

		r = s.as(http.MethodDelete, "/orders/7", bearerHeader(creds["alice"]), "", "")
		require.Equal(t, http.StatusForbidden, r.status)
		require.Equal(t, []string{`Bearer error="insufficient_scope", scope="carts:write"`}, r.header.Values("WWW-Authenticate"))
		require.JSONEq(t, `{"status":403,"title":"Forbidden","detail":"forbidden"}`, r.body)
		r = s.as(http.MethodDelete, "/orders/7", http.Header{"X-Api-Key": {creds["batch"]}}, "", "")
		require.Equal(t, http.StatusUnauthorized, r.status, "the operation's Security replaces the API's")
		r = s.as(http.MethodDelete, "/orders/7", bearerHeader(creds["admin"]), "", "")
		require.Equal(t, http.StatusNoContent, r.status)
		require.Equal(t, "admin", <-deleted)

		r = s.as(http.MethodPost, "/orders/7/notes", nil, "text/plain", "not JSON")
		require.Equal(t, http.StatusUnauthorized, r.status, "authenticated before the Content-Type")
		r = s.as(http.MethodPost, "/orders/7/notes", nil, "application/json", `{"name":"tea","quantity":1}`)
		require.Equal(t, http.StatusUnauthorized, r.status)
		require.Zero(t, decoded.Load(), "an unauthenticated request's body is never read")
		r = s.as(http.MethodPost, "/orders/7/notes", http.Header{"X-Api-Key": {creds["batch"]}}, "text/plain", "not JSON")
		require.Equal(t, http.StatusUnsupportedMediaType, r.status)

		r = s.as(http.MethodGet, "/me", nil, "", "")
		require.Equal(t, http.StatusOK, r.status, "anonymous")
		require.JSONEq(t, `{"user":""}`, r.body)
		r = s.as(http.MethodGet, "/me", bearerHeader(forged), "", "")
		require.Equal(t, http.StatusUnauthorized, r.status, "a rejected credential is never served anonymously")
		r = s.as(http.MethodGet, "/me", bearerHeader(creds["batch"]), "", "")
		require.JSONEq(t, `{"user":"batch"}`, r.body)

		r = s.as(http.MethodGet, docPath.String(), nil, "", "")
		require.Equal(t, http.StatusUnauthorized, r.status, "the document is secured like the API")
		require.Equal(t, []string{"Bearer"}, r.header.Values("WWW-Authenticate"))
		r = s.as(http.MethodGet, docPath.String(), bearerHeader(creds["alice"]), "", "")
		require.Equal(t, http.StatusOK, r.status)
		requireValid(t, []byte(r.body))

		r = s.as(http.MethodGet, "/nowhere", nil, "", "")
		require.Equal(t, http.StatusNotFound, r.status, "Unmatched authenticates nothing")
		require.Empty(t, r.header.Values("WWW-Authenticate"))

		s.stop()
		require.Empty(t, records(s.tel), "a refusal isn't logged")
	})
}

// countingDecoder is a decoder that counts its Decode calls.
type countingDecoder[T any] struct {
	openapi.Decoder[T]
	calls *atomic.Int64
}

func (d countingDecoder[T]) Decode(r *http.Request) (T, error) {
	d.calls.Add(1)
	return d.Decoder.Decode(r)
}

// authenticated is whether the bearer scheme authenticated a request.
type authenticated struct {
	Authenticated bool `json:"authenticated"`
}

func TestSecurity_Disabled_opens_the_document_and_an_operation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		keys, creds := newKeyring(user{name: "alice"})
		bearer := auth.Bearer("bearer", keys.verify)
		public := openapi.Root().Lit("public")
		s := serveAPI(t, []openapi.Operation{
			openapi.Get(public, func(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (authenticated, error) {
				_, ok := bearer.From(ctx)
				return authenticated{Authenticated: ok}, nil
			}, jsonEncoder, openapi.Security(auth.Disabled())),
			openapi.Get(purchase, func(context.Context, openapi.Request[openapi.EmptyBody]) (orderBody, error) {
				return orderBody{}, nil
			}, jsonEncoder),
			openapi.Document(docPath, openapi.Security(auth.Disabled())),
		}, openapi.Security(auth.Require(bearer)))

		for _, h := range []http.Header{nil, bearerHeader(rand.Text()), bearerHeader(creds["alice"])} {
			r := s.as(http.MethodGet, "/public", h, "", "")
			require.Equal(t, http.StatusOK, r.status)
			require.JSONEq(t, `{"authenticated":false}`, r.body, "Disabled reads no credential")
		}
		require.Equal(t, http.StatusUnauthorized, s.as(http.MethodGet, "/orders/1", nil, "", "").status)

		r := s.as(http.MethodGet, docPath.String(), nil, "", "")
		require.Equal(t, http.StatusOK, r.status)
		requireValid(t, []byte(r.body))

		s.stop()
		require.Empty(t, records(s.tel))
	})
}

// directoryBusy is a verifier's error that an Error answers.
type directoryBusy struct{ problem }

func TestSecurity_answers_a_verifiers_error_as_the_servers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		down, busy, panics := rand.Text(), rand.Text(), rand.Text()
		bearer := auth.Bearer("bearer", func(_ context.Context, token string) (user, error) {
			switch token {
			case down:
				return user{}, errors.New("the directory is down")
			case busy:
				return user{}, directoryBusy{problem{Status: http.StatusServiceUnavailable, Title: "Service Unavailable"}}
			case panics:
				panic(secret)
			}
			return user{}, auth.ErrUnauthenticated
		})
		s := serveAPI(t, []openapi.Operation{openapi.Get(purchase, func(context.Context, openapi.Request[openapi.EmptyBody]) (orderBody, error) {
			t.Error("the handler ran")
			return orderBody{}, nil
		}, jsonEncoder)},
			openapi.Security(auth.Require(bearer)),
			openapi.Error[directoryBusy](http.StatusServiceUnavailable, problemEncoder),
		)

		r := s.as(http.MethodGet, "/orders/1", bearerHeader(down), "", "")
		require.Equal(t, http.StatusInternalServerError, r.status)
		require.Empty(t, r.header.Values("WWW-Authenticate"))
		require.JSONEq(t, `{"status":500,"title":"Internal Server Error"}`, r.body)

		r = s.as(http.MethodGet, "/orders/1", bearerHeader(busy), "", "")
		require.Equal(t, http.StatusServiceUnavailable, r.status)
		require.Empty(t, r.header.Values("WWW-Authenticate"))
		require.JSONEq(t, `{"status":503,"title":"Service Unavailable"}`, r.body)

		r = s.as(http.MethodGet, "/orders/1", bearerHeader(panics), "", "")
		require.Equal(t, http.StatusInternalServerError, r.status)
		require.NotContains(t, r.body, secret)

		s.stop()
		failed := logged(s, "request failed")
		require.Len(t, failed, 1)
		require.Equal(t, log.SeverityError, failed[0].Severity())
		require.Equal(t, "the directory is down", recordAttr(failed[0], "exception.message"))
		require.Equal(t, "/orders/{orderId}", recordAttr(failed[0], "http.route"))
		panicked := logged(s, "operation panicked")
		require.Len(t, panicked, 1)
		require.Equal(t, secret, recordAttr(panicked[0], "exception.message"))
		require.Contains(t, recordAttr(panicked[0], "exception.stacktrace"), "security_test.go", "the stack is the verifier's")
		require.Len(t, records(s.tel), 2, "the 503 isn't logged")
		require.Empty(t, logged(s, "handler panicked"))
	})
}

// mustURL parses raw.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestDocument_lists_the_schemes_and_requirements(t *testing.T) {
	ring, _ := newKeyring()
	verifyBasic := func(context.Context, string, string) (user, error) { return user{}, auth.ErrUnauthenticated }
	bearer := auth.Bearer("bearer", ring.verify).Format("JWT").Description("A token from the IdP")
	apiKey := auth.APIKeyHeader("apiKey", "X-API-Key", ring.verify)
	basic := auth.Basic("basic", "admin", verifyBasic)
	cookie := auth.APIKeyCookie("session", "sid", ring.verify)
	query := auth.APIKeyQuery("queryKey", "key", ring.verify)
	mtls := auth.MutualTLS("mtls", func(context.Context, [][]*x509.Certificate) (user, error) { return user{}, nil })
	oauth := auth.OAuth2("oauth", auth.Flows{
		AuthorizationCode: &auth.AuthorizationCodeFlow{
			AuthorizationURL: mustURL(t, "https://idp.example.com/authorize"),
			TokenURL:         mustURL(t, "https://idp.example.com/token"),
			RefreshURL:       mustURL(t, "https://idp.example.com/refresh"),
			Scopes:           map[string]string{"carts:admin": "Administer carts"},
		},
		ClientCredentials: &auth.ClientCredentialsFlow{
			TokenURL: mustURL(t, "https://idp.example.com/token"),
		},
		DeviceAuthorization: &auth.DeviceAuthorizationFlow{
			DeviceAuthorizationURL: mustURL(t, "https://idp.example.com/device"),
			TokenURL:               mustURL(t, "https://idp.example.com/token"),
			Scopes:                 map[string]string{"carts:admin": "Administer carts"},
		},
	}, ring.verify)
	oidc := auth.OpenIDConnect("oidc", mustURL(t, "https://idp.example.com/.well-known/openid-configuration"), ring.verify)

	admin := openapi.Root().Lit("admin")
	api, err := newAPI(setup(), []openapi.Operation{
		openapi.Get(cart, getCart, jsonEncoder),
		openapi.Get(carts, noBody, jsonEncoder, openapi.Security(auth.Disabled())),
		openapi.Get(me, noBody, jsonEncoder, openapi.Security(auth.Require(bearer), auth.Anonymous())),
		openapi.Delete(cart, nothingBack, openapi.EmptyEncoder,
			openapi.Security(auth.All(auth.RequireScopes(oauth, "carts:admin"), auth.Require(apiKey))),
		),
		openapi.Get(admin, noBody, jsonEncoder, openapi.Security(
			auth.Require(basic), auth.Require(cookie), auth.Require(query), auth.Require(mtls), auth.Require(oidc),
		)),
		openapi.Document(docPath, openapi.Security(auth.Disabled())),
	}, openapi.Security(auth.RequireScopes(bearer, "carts:read"), auth.Require(apiKey)))
	require.NoError(t, err)
	doc := serveDocument(t, api) // valid against OpenAPI 3.2's schema-base

	require.JSONEq(t, `{
		"bearer": {"type":"http","scheme":"bearer","bearerFormat":"JWT","description":"A token from the IdP"},
		"apiKey": {"type":"apiKey","name":"X-API-Key","in":"header"},
		"basic": {"type":"http","scheme":"basic"},
		"session": {"type":"apiKey","name":"sid","in":"cookie"},
		"queryKey": {"type":"apiKey","name":"key","in":"query"},
		"mtls": {"type":"mutualTLS"},
		"oauth": {"type":"oauth2","flows":{
			"authorizationCode":{"authorizationUrl":"https://idp.example.com/authorize","tokenUrl":"https://idp.example.com/token","refreshUrl":"https://idp.example.com/refresh","scopes":{"carts:admin":"Administer carts"}},
			"clientCredentials":{"tokenUrl":"https://idp.example.com/token","scopes":{}},
			"deviceAuthorization":{"deviceAuthorizationUrl":"https://idp.example.com/device","tokenUrl":"https://idp.example.com/token","scopes":{"carts:admin":"Administer carts"}}
		}},
		"oidc": {"type":"openIdConnect","openIdConnectUrl":"https://idp.example.com/.well-known/openid-configuration"}
	}`, field(t, doc, "components", "securitySchemes"))

	require.JSONEq(t, `[{"bearer":["carts:read"]},{"apiKey":[]}]`, field(t, doc, "security"))

	getCartDoc := field(t, doc, "paths", "/carts/{cartId}", "get")
	require.NotContains(t, getCartDoc, `"security"`, "an operation without its own Security inherits the top level's")
	require.Equal(t, []string{"200", "400", "401", "403", "500"}, keys(t, doc, "paths", "/carts/{cartId}", "get", "responses"))
	require.Equal(t, field(t, doc, "paths", "/carts/{cartId}", "get", "responses", "400", "content"),
		field(t, doc, "paths", "/carts/{cartId}", "get", "responses", "401", "content"), "RequestErrors' E at 401")

	require.JSONEq(t, `[]`, field(t, doc, "paths", "/carts", "get", "security"))
	require.Equal(t, []string{"200", "500"}, keys(t, doc, "paths", "/carts", "get", "responses"))

	require.JSONEq(t, `[{"bearer":[]},{}]`, field(t, doc, "paths", "/me", "get", "security"))
	require.Equal(t, []string{"200", "401", "500"}, keys(t, doc, "paths", "/me", "get", "responses"))

	require.JSONEq(t, `[{"oauth":["carts:admin"],"apiKey":[]}]`, field(t, doc, "paths", "/carts/{cartId}", "delete", "security"))
	require.Equal(t, []string{"204", "400", "401", "403", "500"}, keys(t, doc, "paths", "/carts/{cartId}", "delete", "responses"))

	require.JSONEq(t, `[{"basic":[]},{"session":[]},{"queryKey":[]},{"mtls":[]},{"oidc":[]}]`, field(t, doc, "paths", "/admin", "get", "security"))
	require.Equal(t, []string{"200", "401", "500"}, keys(t, doc, "paths", "/admin", "get", "responses"))

	require.NotContains(t, string(doc), docPath.String(), "the document doesn't describe itself")
}

func TestDocument_without_Security_lists_none(t *testing.T) {
	doc := documentOf(t, []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder)})
	require.Equal(t, []string{"info", "openapi", "paths"}, keys(t, doc))
	require.Equal(t, []string{"200", "400", "500"}, keys(t, doc, "paths", "/carts/{cartId}", "get", "responses"))
}

func TestNewAPI_reports_every_Security_mistake(t *testing.T) {
	ring, _ := newKeyring()
	bearer := auth.Bearer("bearer", ring.verify)
	otherBearer := auth.Bearer("bearer", ring.verify)
	oauth := auth.OAuth2("oauth", auth.Flows{ClientCredentials: &auth.ClientCredentialsFlow{
		TokenURL: mustURL(t, "https://idp.example.com/token"),
		Scopes:   map[string]string{"carts:read": "Read carts"},
	}}, ring.verify)

	cases := []struct {
		name   string
		change func(a *apiArgs)
		want   []string
	}{
		{
			name: "two schemes named bearer in one list",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Security(auth.Require(bearer), auth.Require(otherBearer))}
			},
			want: []string{`Security: reqs[1]: two different schemes are named "bearer"`},
		},
		{
			name: "two schemes named bearer in two lists",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Security(auth.Require(bearer))}
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder, openapi.Security(auth.Require(otherBearer)))}
			},
			want: []string{`GET /carts/{cartId}: Security: two different schemes are named "bearer"`},
		},
		{
			name: "an OAuth2 scope no flow declares",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Security(auth.RequireScopes(oauth, "carts:admin"))}
			},
			want: []string{`Security: reqs[0]: the scope "carts:admin" is declared by no flow of the OAuth2 scheme "oauth"`},
		},
		{
			name: "a nil verify function",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Security(auth.Require(auth.Bearer[user]("bearer", nil)))}
			},
			want: []string{`Security: reqs[0]: the Bearer scheme "bearer" has a nil verify function`},
		},
		{
			name:   "the zero Requirement",
			change: func(a *apiArgs) { a.opts = []openapi.APIOption{openapi.Security(auth.Requirement{})} },
			want:   []string{"Security: reqs[0] is the zero Requirement"},
		},
		{
			name:   "Security()",
			change: func(a *apiArgs) { a.opts = []openapi.APIOption{openapi.Security()} },
			want:   []string{"Security() has no requirement: give it auth.Disabled() to make it public"},
		},
		{
			name: "an operation's Security()",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder, openapi.Security())}
			},
			want: []string{"GET /carts/{cartId}: Security() has no requirement"},
		},
		{
			name: "Disabled beside another requirement",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Security(auth.Disabled(), auth.Require(bearer))}
			},
			want: []string{"Security: reqs[0] is Disabled, which stands alone, but the list holds 2 requirements"},
		},
		{
			name: "Disabled inside an All",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Security(auth.All(auth.Require(bearer), auth.Disabled()))}
			},
			want: []string{"Security: reqs[0]: All's reqs[1] is Disabled, which stands alone, never inside an All"},
		},
		{
			name: "an Error at 401 on a secured operation",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{
					openapi.Security(auth.Require(bearer)),
					openapi.Error[notFound](http.StatusUnauthorized, problemEncoder),
				}
			},
			want: []string{"GET /carts/{cartId}: RequestErrors and Error[openapi_test.notFound] both answer with 401"},
		},
		{
			name: "an Error at 403 on an operation whose requirement names a scope",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.Security(auth.RequireScopes(oauth, "carts:read")),
					openapi.Error[notFound](http.StatusForbidden, problemEncoder),
				)}
			},
			want: []string{"GET /carts/{cartId}: RequestErrors and Error[openapi_test.notFound] both answer with 403"},
		},
		{
			name: "a catch-all at 401 on a secured operation",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.Security(auth.Require(bearer)),
					openapi.CatchAll(http.StatusUnauthorized, internalError, problemEncoder),
				)}
			},
			want: []string{"GET /carts/{cartId}: RequestErrors and the catch-all both answer with 401"},
		},
		{
			name: "an Error at 401 under a secured Document",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Document(docPath, openapi.Security(auth.Require(bearer)))}
				a.opts = []openapi.APIOption{openapi.Error[notFound](http.StatusUnauthorized, problemEncoder)}
			},
			want: []string{"Document at /openapi.json: RequestErrors and Error[openapi_test.notFound] both answer with 401"},
		},
		{
			name: "an option given to Document that isn't Security",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Document(docPath, openapi.Summary("The document"))}
			},
			want: []string{"Document at /openapi.json: its option 0 isn't Security, which is the only option Document takes"},
		},
		{
			name: "a nil option given to Document",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Document(docPath, openapi.Security(auth.Disabled()), nil)}
			},
			want: []string{"Document at /openapi.json: its option 1 is nil"},
		},
		{
			name: "Document's Security()",
			change: func(a *apiArgs) {
				a.ops = []openapi.Operation{openapi.Document(docPath, openapi.Security())}
			},
			want: []string{"Document at /openapi.json: Security() has no requirement"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := validArgs()
			tc.change(&a)
			api, err := a.newAPI()
			require.Nil(t, api)
			require.Error(t, err)
			msg := err.Error()
			require.True(t, strings.HasPrefix(msg, "openapi: NewAPI: "), msg)
			for _, want := range tc.want {
				require.Contains(t, msg, want)
			}
			lines := strings.Split(strings.TrimPrefix(msg, "openapi: NewAPI: "), "\n")
			require.Len(t, lines, len(tc.want), "one line per mistake: %s", msg)
		})
	}
}

func TestNewAPI_accepts_Security(t *testing.T) {
	ring, _ := newKeyring()
	bearer := auth.Bearer("bearer", ring.verify)
	cases := []struct {
		name   string
		change func(a *apiArgs)
	}{
		{
			name: "an Error at 401 on an operation that Disabled opens",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Security(auth.Require(bearer))}
				a.ops = []openapi.Operation{openapi.Get(cart, getCart, jsonEncoder,
					openapi.Security(auth.Disabled()),
					openapi.Error[notFound](http.StatusUnauthorized, problemEncoder),
				)}
			},
		},
		{
			name: "an Error at 403 on an operation whose requirements name no scope",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{
					openapi.Security(auth.Require(bearer), auth.Anonymous()),
					openapi.Error[notFound](http.StatusForbidden, problemEncoder),
				}
			},
		},
		{
			name: "one scheme, through its copies, in every list",
			change: func(a *apiArgs) {
				a.opts = []openapi.APIOption{openapi.Security(auth.Require(bearer))}
				a.ops = []openapi.Operation{
					openapi.Get(cart, getCart, jsonEncoder, openapi.Security(auth.Require(bearer), auth.Anonymous())),
					openapi.Document(docPath, openapi.Security(auth.Require(bearer))),
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := validArgs()
			tc.change(&a)
			api, err := a.newAPI()
			require.NoError(t, err)
			require.NotNil(t, api)
		})
	}
}
