// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package authn

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// principal is the principal every scheme of the tests returns.
type principal struct {
	name   string
	scopes []string
}

// verifier verifies the credentials of one test, and counts its calls by
// scheme.
type verifier struct {
	secret string
	calls  map[string]int
}

func newVerifier() *verifier {
	return &verifier{secret: rand.Text(), calls: make(map[string]int)}
}

// verify returns a Verify for the scheme name, which authenticates v's
// secret as alice, and fails as the server's on "down".
func (v *verifier) verify(name string, scopes ...string) Verify {
	return func(_ context.Context, c Credential) (any, error) {
		v.calls[name]++
		got := c.Token + c.Key + c.Password
		switch {
		case got == "down":
			return nil, errors.New("the directory is down")
		case got == v.secret, len(c.Chains) > 0:
			return principal{name: "alice", scopes: scopes}, nil
		}
		return nil, ErrUnauthenticated
	}
}

func hasScope(p any, scope string) bool {
	return slices.Contains(p.(principal).scopes, scope)
}

func compile(t *testing.T, reqs ...Requirement) *List {
	t.Helper()
	l, errs := Compile(reqs)
	require.Empty(t, errs)
	return l
}

func get(edits ...func(*http.Request)) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://localhost/carts", nil)
	for _, e := range edits {
		e(r)
	}
	return r
}

func withAuthorization(v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Add("Authorization", v) }
}

func TestKind_String(t *testing.T) {
	for k, want := range map[Kind]string{
		Bearer: "Bearer", Basic: "Basic", APIKeyHeader: "APIKeyHeader", APIKeyQuery: "APIKeyQuery",
		APIKeyCookie: "APIKeyCookie", MutualTLS: "MutualTLS", OAuth2: "OAuth2", OpenIDConnect: "OpenIDConnect",
		Kind(0): "Kind(?)",
	} {
		require.Equal(t, want, k.String())
	}
}

func TestAuthenticate_reads_each_credential(t *testing.T) {
	v := newVerifier()
	schemes := []*Scheme{
		NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: v.verify("bearer")}),
		NewScheme(Scheme{Name: "oauth", Kind: OAuth2, Verify: v.verify("oauth"), Flows: Flows{ClientCredentials: &Flow{TokenURL: &url.URL{Scheme: "https", Host: "idp", Path: "/token"}}}}),
		NewScheme(Scheme{Name: "basic", Kind: Basic, Realm: "carts", Verify: v.verify("basic")}),
		NewScheme(Scheme{Name: "header", Kind: APIKeyHeader, Param: "X-API-Key", Verify: v.verify("header")}),
		NewScheme(Scheme{Name: "query", Kind: APIKeyQuery, Param: "api_key", Verify: v.verify("query")}),
		NewScheme(Scheme{Name: "cookie", Kind: APIKeyCookie, Param: "session", Verify: v.verify("cookie")}),
		NewScheme(Scheme{Name: "mtls", Kind: MutualTLS, Verify: v.verify("mtls")}),
	}
	for _, tc := range []struct {
		scheme int
		edit   func(*http.Request)
	}{
		{0, withAuthorization("Bearer " + v.secret)},
		{1, withAuthorization("bearer " + v.secret)},
		{2, withAuthorization("BASIC " + base64.StdEncoding.EncodeToString([]byte("alice:"+v.secret)))},
		{3, func(r *http.Request) { r.Header.Set("X-API-Key", v.secret) }},
		{4, func(r *http.Request) { r.URL.RawQuery = "api_key=" + v.secret }},
		{5, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "session", Value: v.secret}) }},
		{6, func(r *http.Request) {
			r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
		}},
	} {
		s := schemes[tc.scheme]
		l := compile(t, Require(s, nil, nil))
		r := get(tc.edit)
		res := l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, Served, res.Outcome, s.Name)
		p, ok := Principal(res.Context, s)
		require.True(t, ok)
		require.Equal(t, "alice", p.(principal).name)

		r = get()
		res = l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, Unauthenticated, res.Outcome, s.Name)
		require.ErrorIs(t, res.Err, ErrUnauthenticated)
	}
}

func TestAuthenticate_rejects_a_malformed_credential(t *testing.T) {
	v := newVerifier()
	bearer := NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: v.verify("bearer")})
	basic := NewScheme(Scheme{Name: "basic", Kind: Basic, Realm: "carts", Verify: v.verify("basic")})
	key := NewScheme(Scheme{Name: "key", Kind: APIKeyHeader, Param: "X-API-Key", Verify: v.verify("key")})
	l := compile(t, Require(bearer, nil, nil), Require(basic, nil, nil), Require(key, nil, nil), Anonymous())

	for name, edit := range map[string]func(*http.Request){
		"two Authorization headers": func(r *http.Request) {
			withAuthorization("Bearer " + v.secret)(r)
			withAuthorization("Bearer " + v.secret)(r)
		},
		"Bearer with no token":   withAuthorization("Bearer"),
		"Bearer with a space":    withAuthorization("Bearer a b"),
		"Bearer with only =":     withAuthorization("Bearer ==="),
		"Basic not base64":       withAuthorization("Basic ***"),
		"Basic with no colon":    withAuthorization("Basic " + base64.StdEncoding.EncodeToString([]byte("alice"))),
		"an empty API key":       func(r *http.Request) { r.Header["X-Api-Key"] = []string{""} },
		"an API key given twice": func(r *http.Request) { r.Header["X-Api-Key"] = []string{v.secret, v.secret} },
	} {
		r := get(edit)
		res := l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, Unauthenticated, res.Outcome, name)
	}
	require.Empty(t, v.calls, "a malformed credential reaches no verifier")

	r := get(withAuthorization("Digest " + v.secret))
	res := l.Authenticate(r.Context(), HTTPSource(r))
	require.Equal(t, Served, res.Outcome, "another scheme's Authorization is no credential of these")
}

func TestAuthenticate_challenges(t *testing.T) {
	v := newVerifier()
	bearer := NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: v.verify("bearer", "carts:read")})
	oidc := NewScheme(Scheme{Name: "oidc", Kind: OpenIDConnect, Discovery: &url.URL{Scheme: "https", Host: "idp"}, Verify: v.verify("oidc")})
	basic := NewScheme(Scheme{Name: "basic", Kind: Basic, Realm: "carts", Verify: v.verify("basic")})
	basic2 := NewScheme(Scheme{Name: "basic2", Kind: Basic, Realm: "carts", Verify: v.verify("basic2")})
	key := NewScheme(Scheme{Name: "key", Kind: APIKeyHeader, Param: "X-API-Key", Verify: v.verify("key")})
	mtls := NewScheme(Scheme{Name: "mtls", Kind: MutualTLS, Verify: v.verify("mtls")})

	t.Run("one per HTTP scheme, in order, without repeats", func(t *testing.T) {
		l := compile(t, Require(key, nil, nil), Require(basic, nil, nil), Require(bearer, nil, nil),
			Require(oidc, nil, nil), Require(basic2, nil, nil), Require(mtls, nil, nil))
		r := get()
		res := l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, []string{`Basic realm="carts", charset="UTF-8"`, "Bearer"}, res.Challenges)

		r = get(withAuthorization("Bearer " + rand.Text()))
		res = l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, []string{`Basic realm="carts", charset="UTF-8"`, `Bearer error="invalid_token"`}, res.Challenges)
	})

	t.Run("none for an API key and mutual TLS", func(t *testing.T) {
		l := compile(t, Require(key, nil, nil), Require(mtls, nil, nil))
		r := get()
		require.Empty(t, l.Authenticate(r.Context(), HTTPSource(r)).Challenges)
	})

	t.Run("insufficient_scope, with the scopes of each bearer requirement", func(t *testing.T) {
		l := compile(t,
			Require(bearer, []string{"carts:read", "carts:write"}, hasScope),
			All([]Requirement{Require(bearer, []string{"carts:admin", "carts:write"}, hasScope), Require(key, nil, nil)}),
		)
		r := get(withAuthorization("Bearer "+v.secret), func(r *http.Request) { r.Header.Set("X-API-Key", v.secret) })
		res := l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, Forbidden, res.Outcome)
		require.ErrorIs(t, res.Err, ErrForbidden)
		require.Equal(t, []string{`Bearer error="insufficient_scope", scope="carts:read carts:write carts:admin"`}, res.Challenges)
	})

	t.Run("no insufficient_scope for a scheme that isn't bearer", func(t *testing.T) {
		l := compile(t, All([]Requirement{Require(bearer, nil, nil), Require(key, []string{"admin"}, hasScope)}))
		r := get(withAuthorization("Bearer "+v.secret), func(r *http.Request) { r.Header.Set("X-API-Key", v.secret) })
		res := l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, Forbidden, res.Outcome)
		require.Empty(t, res.Challenges)
	})
}

func TestAuthenticate_fails_at_a_verifiers_other_error(t *testing.T) {
	v := newVerifier()
	bearer := NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: v.verify("bearer")})
	key := NewScheme(Scheme{Name: "key", Kind: APIKeyHeader, Param: "X-API-Key", Verify: v.verify("key")})

	for name, l := range map[string]*List{
		"a requirement":      compile(t, Require(bearer, nil, nil), Require(key, nil, nil)),
		"Anonymous":          compile(t, Require(key, nil, nil), Anonymous(), Require(bearer, nil, nil)),
		"a scheme of an All": compile(t, All([]Requirement{Require(bearer, nil, nil), Require(key, nil, nil)})),
	} {
		v.calls = make(map[string]int)
		r := get(withAuthorization("Bearer down"))
		res := l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, Failed, res.Outcome, name)
		require.Equal(t, "bearer", res.Scheme)
		require.EqualError(t, res.Err, "the directory is down")
	}
}

func TestAuthenticate_Anonymous(t *testing.T) {
	v := newVerifier()
	bearer := NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: v.verify("bearer")})
	key := NewScheme(Scheme{Name: "key", Kind: APIKeyHeader, Param: "X-API-Key", Verify: v.verify("key")})
	l := compile(t, Require(bearer, nil, nil), Anonymous(), Require(key, nil, nil))

	r := get()
	res := l.Authenticate(r.Context(), HTTPSource(r))
	require.Equal(t, Served, res.Outcome)
	_, ok := Principal(res.Context, bearer)
	require.False(t, ok)

	r = get(func(r *http.Request) { r.Header.Set("X-API-Key", rand.Text()) })
	res = l.Authenticate(r.Context(), HTTPSource(r))
	require.Equal(t, Unauthenticated, res.Outcome, "a key a later requirement's scheme rejects is never anonymous")

	r = get(func(r *http.Request) { r.Header.Set("X-API-Key", v.secret) })
	res = l.Authenticate(r.Context(), HTTPSource(r))
	require.Equal(t, Served, res.Outcome)
	_, ok = Principal(res.Context, key)
	require.False(t, ok, "Anonymous, before the key's requirement, met first")

	require.True(t, IsAnonymous(Anonymous()))
	require.True(t, IsAnonymous(All([]Requirement{Anonymous()})))
	require.False(t, IsAnonymous(Disabled()))
	require.False(t, IsAnonymous(Requirement{}))
	require.False(t, IsAnonymous(Require(bearer, nil, nil)))
}

func TestAuthenticate_Disabled(t *testing.T) {
	v := newVerifier()
	bearer := NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: v.verify("bearer")})
	l := compile(t, Disabled())
	require.Empty(t, l.Schemes())

	for _, token := range []string{v.secret, rand.Text(), "down"} {
		r := get(withAuthorization("Bearer " + token))
		res := l.Authenticate(r.Context(), HTTPSource(r))
		require.Equal(t, Served, res.Outcome)
		require.Equal(t, r.Context(), res.Context, "no principal is put in its context")
		_, ok := Principal(res.Context, bearer)
		require.False(t, ok)
	}
	require.Empty(t, v.calls)
	require.True(t, IsDisabled(Disabled()))
	require.False(t, IsDisabled(Anonymous()))
}

func TestAuthenticate_verifies_each_scheme_once(t *testing.T) {
	v := newVerifier()
	bearer := NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: v.verify("bearer")})
	key := NewScheme(Scheme{Name: "key", Kind: APIKeyHeader, Param: "X-API-Key", Verify: v.verify("key")})
	l := compile(t,
		Require(bearer, []string{"admin"}, hasScope),
		All([]Requirement{Require(bearer, []string{"other"}, hasScope), Require(key, nil, nil)}),
		Require(key, nil, nil),
		Anonymous(),
	)
	r := get(withAuthorization("Bearer "+v.secret), func(r *http.Request) { r.Header.Set("X-API-Key", v.secret) })
	res := l.Authenticate(r.Context(), HTTPSource(r))
	require.Equal(t, Served, res.Outcome)
	require.Equal(t, map[string]int{"bearer": 1, "key": 1}, v.calls)
	_, ok := Principal(res.Context, bearer)
	require.False(t, ok, "only the principals of the requirement met")
	_, ok = Principal(res.Context, key)
	require.True(t, ok)

	_, ok = Principal(res.Context, nil)
	require.False(t, ok)
	_, ok = Principal(res.Context, bearer.With(func(s *Scheme) { s.Description = "a copy" }))
	require.False(t, ok)
	_, ok = Principal(res.Context, key.With(func(s *Scheme) { s.Description = "a copy" }))
	require.True(t, ok, "a copy is the same scheme")
}

func TestItems(t *testing.T) {
	a := NewScheme(Scheme{Name: "a", Kind: Bearer})
	b := NewScheme(Scheme{Name: "b", Kind: Bearer})
	items := Items(All([]Requirement{Require(a, []string{"x"}, hasScope), All([]Requirement{Require(b, nil, nil), Anonymous()})}))
	require.Len(t, items, 2)
	require.Same(t, a, items[0].Scheme)
	require.Equal(t, []string{"x"}, items[0].Scopes)
	require.Same(t, b, items[1].Scheme)
	require.Nil(t, Items(Requirement{}))
	require.Nil(t, Items(Anonymous()))
	require.True(t, IsZero(Requirement{}))
	require.False(t, IsZero(Anonymous()))
}

func TestCompile_reports_each_mistake(t *testing.T) {
	ok := func(context.Context, Credential) (any, error) { return nil, nil }
	u := func(s string) *url.URL {
		parsed, err := url.Parse(s)
		require.NoError(t, err)
		return parsed
	}
	bearer := NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: ok})
	oauth := func(f Flows) *Scheme { return NewScheme(Scheme{Name: "oauth", Kind: OAuth2, Flows: f, Verify: ok}) }

	for _, tc := range []struct {
		reqs []Requirement
		want string
	}{
		{nil, "no requirement"},
		{[]Requirement{{}}, "reqs[0] is the zero Requirement"},
		{[]Requirement{Require(nil, nil, nil)}, "reqs[0] names the zero Scheme"},
		{[]Requirement{All(nil)}, "reqs[0] is All with no requirement"},
		{[]Requirement{All([]Requirement{{}})}, "reqs[0]: All's reqs[0] is the zero Requirement"},
		{[]Requirement{All([]Requirement{Disabled()})}, "reqs[0]: All's reqs[0] is Disabled, which stands alone, never inside an All"},
		{[]Requirement{All([]Requirement{All([]Requirement{Disabled()})})}, "reqs[0]: All's reqs[0]: All's reqs[0] is Disabled"},
		{[]Requirement{Disabled(), Anonymous()}, "reqs[0] is Disabled, which stands alone, but the list holds 2 requirements"},
		{[]Requirement{All([]Requirement{Require(bearer, nil, nil), Require(bearer, nil, nil)})}, `reqs[0]: All names the scheme "bearer" twice`},
		{[]Requirement{Require(NewScheme(Scheme{Name: "x", Kind: Bearer}), nil, nil)}, `the Bearer scheme "x" has a nil verify function`},
		{[]Requirement{Require(NewScheme(Scheme{Kind: Bearer, Verify: ok}), nil, nil)}, "reqs[0]: a Bearer scheme has an empty name"},
		{[]Requirement{Require(NewScheme(Scheme{Name: "a/b", Kind: Bearer, Verify: ok}), nil, nil)}, "has a name with a character other than"},
		{
			[]Requirement{Require(bearer, nil, nil), Require(NewScheme(Scheme{Name: "bearer", Kind: Bearer, Verify: ok}), nil, nil)},
			`reqs[1]: two different schemes are named "bearer"`,
		},
		{[]Requirement{Require(NewScheme(Scheme{Name: "k", Kind: APIKeyQuery, Param: "k", Formatted: true, Verify: ok}), nil, nil)}, "has a Format, which is for Bearer alone"},
		{[]Requirement{Require(NewScheme(Scheme{Name: "b", Kind: Basic, Verify: ok}), nil, nil)}, "has an empty realm"},
		{[]Requirement{Require(NewScheme(Scheme{Name: "b", Kind: Basic, Realm: `a"b`, Verify: ok}), nil, nil)}, "has a realm that holds a quote"},
		{[]Requirement{Require(NewScheme(Scheme{Name: "k", Kind: APIKeyHeader, Verify: ok}), nil, nil)}, "reads its key from an empty name"},
		{[]Requirement{Require(oauth(Flows{}), nil, nil)}, `the OAuth2 scheme "oauth" has no flow`},
		{[]Requirement{Require(oauth(Flows{AuthorizationCode: &Flow{}}), nil, nil)}, "its AuthorizationCode flow has no AuthorizationURL"},
		{[]Requirement{Require(oauth(Flows{ClientCredentials: &Flow{}}), nil, nil)}, "its ClientCredentials flow has no TokenURL"},
		{[]Requirement{Require(oauth(Flows{DeviceAuthorization: &Flow{TokenURL: u("https://idp/token")}}), nil, nil)}, "its DeviceAuthorization flow has no DeviceAuthorizationURL"},
		{
			[]Requirement{Require(oauth(Flows{ClientCredentials: &Flow{TokenURL: u("https://idp/token")}}), []string{"carts:read"}, hasScope)},
			`the scope "carts:read" is declared by no flow of the OAuth2 scheme "oauth"`,
		},
		{[]Requirement{Require(NewScheme(Scheme{Name: "o", Kind: OpenIDConnect, Discovery: u("idp/.well-known"), Verify: ok}), nil, nil)}, "has a discovery URL that isn't absolute"},
	} {
		l, errs := Compile(tc.reqs)
		require.Nil(t, l, tc.want)
		var msgs []string
		for _, err := range errs {
			msgs = append(msgs, err.Error())
		}
		require.Contains(t, strings.Join(msgs, "\n"), tc.want)
	}
}

func TestCompile_takes_flows_with_their_scopes(t *testing.T) {
	ok := func(context.Context, Credential) (any, error) { return nil, nil }
	token, err := url.Parse("https://idp.example.com/token")
	require.NoError(t, err)
	scopes := map[string]string{"carts:read": "Read carts."}
	s := NewScheme(Scheme{Name: "oauth", Kind: OAuth2, Verify: ok, Flows: Flows{
		ClientCredentials:   &Flow{TokenURL: token},
		DeviceAuthorization: &Flow{DeviceAuthorizationURL: token, TokenURL: token, Scopes: scopes},
	}})
	scopes["carts:write"] = "added after the scheme was declared"
	l := compile(t, Require(s, []string{"carts:read"}, hasScope))
	require.Equal(t, []*Scheme{s}, l.Schemes())

	_, errs := Compile([]Requirement{Require(s, []string{"carts:write"}, hasScope)})
	require.Len(t, errs, 1, "the scheme holds a copy of its scopes")
}
