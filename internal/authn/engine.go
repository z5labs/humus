// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package authn

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// Source is where a request's credentials are read from: an HTTP request,
// or an RPC's metadata and peer.
type Source interface {
	// Header returns the values of the header name, Authorization among
	// them.
	Header(name string) []string

	// Query returns the values of the query parameter name.
	Query(name string) []string

	// Cookie returns the values of the cookies named name.
	Cookie(name string) []string

	// VerifiedChains returns the client certificate chains the TLS
	// handshake verified, none when it verified none.
	VerifiedChains() [][]*x509.Certificate
}

// HTTPSource returns the Source of r.
func HTTPSource(r *http.Request) Source { return httpSource{r} }

type httpSource struct{ r *http.Request }

func (s httpSource) Header(name string) []string { return s.r.Header.Values(name) }

func (s httpSource) Query(name string) []string { return s.r.URL.Query()[name] }

func (s httpSource) Cookie(name string) []string {
	var vals []string
	for _, c := range s.r.CookiesNamed(name) {
		vals = append(vals, c.Value)
	}
	return vals
}

func (s httpSource) VerifiedChains() [][]*x509.Certificate {
	if s.r.TLS == nil {
		return nil
	}
	return s.r.TLS.VerifiedChains
}

// Outcome is how a request's authentication ended.
type Outcome int

// The outcomes of Authenticate.
const (
	// Served is a request that met a requirement.
	Served Outcome = iota + 1

	// Unauthenticated is a request no requirement authenticated.
	Unauthenticated

	// Forbidden is a request authenticated without a scope it needs.
	Forbidden

	// Failed is a request a verifier failed on with an error of the
	// server's.
	Failed
)

// Result is the result of Authenticate.
type Result struct {
	Outcome Outcome

	// Context is a served request's context, with the principals of the
	// requirement it met. Otherwise it is the context Authenticate was
	// given.
	Context context.Context

	// Err is a failed request's error: the verifier's. Otherwise it is
	// ErrUnauthenticated or ErrForbidden, or nil for a served request.
	Err error

	// Scheme is the name of the scheme whose verifier failed.
	Scheme string

	// Challenges are the WWW-Authenticate values of an unauthenticated or
	// a forbidden request, in order, none when it has none.
	Challenges []string
}

// verdict is what became of one scheme's credential on one request.
type verdict struct {
	present   bool // the request carries the scheme's credential
	rejected  bool // it is malformed, or the verifier rejected it
	principal any
}

// Authenticate authenticates the request src is read from, whose context
// is ctx, against l.
func (l *List) Authenticate(ctx context.Context, src Source) Result {
	if l.disabled {
		return Result{Outcome: Served, Context: ctx}
	}
	a := attempt{ctx: ctx, src: src, verdicts: make(map[string]verdict)}
	var lacking [][]Item // the requirements that only lacked scopes
	for _, items := range l.reqs {
		if len(items) == 0 {
			// Anonymous: met when no scheme of the list rejects a
			// credential the request carries.
			rejected := false
			for _, s := range l.schemes {
				v, err := a.verify(s)
				if err != nil {
					return a.failed(s, err)
				}
				rejected = rejected || v.rejected
			}
			if !rejected {
				return Result{Outcome: Served, Context: ctx}
			}
			continue
		}
		met, scoped, res := a.require(items)
		if res != nil {
			return *res
		}
		if !met {
			continue
		}
		if !scoped {
			lacking = append(lacking, items)
			continue
		}
		ps := make(principals, len(items))
		for _, it := range items {
			ps[it.Scheme.id] = a.verdicts[it.Scheme.Name].principal
		}
		return Result{Outcome: Served, Context: context.WithValue(ctx, principalsKey{}, ps)}
	}
	if len(lacking) > 0 {
		return Result{Outcome: Forbidden, Context: ctx, Err: ErrForbidden, Challenges: a.insufficientScope(lacking)}
	}
	return Result{Outcome: Unauthenticated, Context: ctx, Err: ErrUnauthenticated, Challenges: a.challenges(l.schemes)}
}

// attempt is one request's authentication.
type attempt struct {
	ctx      context.Context
	src      Source
	verdicts map[string]verdict // by scheme name, so each is verified once
}

// require tries one requirement: met reports whether every scheme of it
// authenticated the request, and scoped whether every principal had its
// scopes too. A verifier's other error is the Result to return.
func (a *attempt) require(items []Item) (met, scoped bool, res *Result) {
	for _, it := range items {
		v, err := a.verify(it.Scheme)
		if err != nil {
			r := a.failed(it.Scheme, err)
			return false, false, &r
		}
		if !v.present || v.rejected {
			return false, false, nil
		}
	}
	for _, it := range items {
		if !hasScopes(it, a.verdicts[it.Scheme.Name].principal) {
			return true, false, nil
		}
	}
	return true, true, nil
}

// hasScopes reports whether principal has every scope it needs.
func hasScopes(it Item, principal any) bool {
	for _, scope := range it.Scopes {
		if !it.HasScope(principal, scope) {
			return false
		}
	}
	return true
}

// verify returns s's verdict on the request, reading its credential and
// calling its verifier the first time it is asked, and returns a
// verifier's error that doesn't wrap ErrUnauthenticated.
func (a *attempt) verify(s *Scheme) (verdict, error) {
	if v, ok := a.verdicts[s.Name]; ok {
		return v, nil
	}
	c, st := read(s, a.src)
	v := verdict{present: st != absent, rejected: st == malformed}
	if st == wellFormed {
		p, err := s.Verify(a.ctx, c)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			v.rejected = true
		case err != nil:
			return verdict{}, err
		default:
			v.principal = p
		}
	}
	a.verdicts[s.Name] = v
	return v, nil
}

func (a *attempt) failed(s *Scheme, err error) Result {
	return Result{Outcome: Failed, Context: a.ctx, Err: err, Scheme: s.Name}
}

// challenges returns a 401's WWW-Authenticate values: one for each scheme
// of the list that HTTP authentication defines, in order, without
// repeats. Bearer, OAuth2 and OpenIDConnect share one, which says
// invalid_token when one of them rejected the request's token.
func (a *attempt) challenges(schemes []*Scheme) []string {
	var out []string
	add := func(c string) {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	bearerAt := -1
	invalid := false
	for _, s := range schemes {
		switch {
		case s.Kind.ReadsBearer():
			if bearerAt < 0 {
				bearerAt = len(out)
				out = append(out, "Bearer")
			}
			invalid = invalid || a.verdicts[s.Name].rejected
		case s.Kind == Basic:
			add(`Basic realm="` + s.Realm + `", charset="UTF-8"`)
		}
	}
	if invalid {
		out[bearerAt] = `Bearer error="invalid_token"`
	}
	return out
}

// insufficientScope returns a 403's WWW-Authenticate values: one bearer
// challenge naming the scopes of each bearer scheme that lacked them, or
// none when no bearer scheme did.
func (a *attempt) insufficientScope(lacking [][]Item) []string {
	var scopes []string
	for _, items := range lacking {
		for _, it := range items {
			if !it.Scheme.Kind.ReadsBearer() || hasScopes(it, a.verdicts[it.Scheme.Name].principal) {
				continue
			}
			for _, s := range it.Scopes {
				if !slices.Contains(scopes, s) {
					scopes = append(scopes, s)
				}
			}
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	return []string{`Bearer error="insufficient_scope", scope="` + strings.Join(scopes, " ") + `"`}
}

// state is what a request carries of a scheme's credential.
type state int

const (
	absent state = iota
	malformed
	wellFormed
)

// read reads s's credential from src.
func read(s *Scheme, src Source) (Credential, state) {
	switch s.Kind {
	case Bearer, OAuth2, OpenIDConnect:
		rest, st := authorization(src, "Bearer")
		if st != wellFormed {
			return Credential{}, st
		}
		if !isToken68(rest) {
			return Credential{}, malformed
		}
		return Credential{Token: rest}, wellFormed
	case Basic:
		rest, st := authorization(src, "Basic")
		if st != wellFormed {
			return Credential{}, st
		}
		b, err := base64.StdEncoding.Strict().DecodeString(rest)
		if err != nil {
			return Credential{}, malformed
		}
		user, pass, ok := strings.Cut(string(b), ":")
		if !ok {
			return Credential{}, malformed
		}
		return Credential{Username: user, Password: pass}, wellFormed
	case APIKeyHeader:
		return key(src.Header(s.Param))
	case APIKeyQuery:
		return key(src.Query(s.Param))
	case APIKeyCookie:
		return key(src.Cookie(s.Param))
	case MutualTLS:
		chains := src.VerifiedChains()
		if len(chains) == 0 {
			return Credential{}, absent
		}
		return Credential{Chains: chains}, wellFormed
	}
	return Credential{}, absent
}

// authorization returns what follows the scheme name in the Authorization
// header, when the name is scheme, whatever its case. Two Authorization
// headers are malformed for every scheme that reads it, and a header with
// the name and nothing after it is malformed.
func authorization(src Source, scheme string) (string, state) {
	vals := src.Header("Authorization")
	switch len(vals) {
	case 0:
		return "", absent
	case 1:
	default:
		return "", malformed
	}
	name, rest, _ := strings.Cut(strings.TrimSpace(vals[0]), " ")
	if !strings.EqualFold(name, scheme) {
		return "", absent
	}
	rest = strings.TrimLeft(rest, " ")
	if rest == "" {
		return "", malformed
	}
	return rest, wellFormed
}

// key returns an API key from the values of its header, query parameter
// or cookie: one, not empty.
func key(vals []string) (Credential, state) {
	switch {
	case len(vals) == 0:
		return Credential{}, absent
	case len(vals) > 1, vals[0] == "":
		return Credential{}, malformed
	}
	return Credential{Key: vals[0]}, wellFormed
}

// isToken68 reports whether s is a token68, as RFC 9110 has it, which is
// the form of a bearer token in RFC 6750.
func isToken68(s string) bool {
	body := strings.TrimRight(s, "=")
	if body == "" {
		return false
	}
	for _, r := range body {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_', r == '~', r == '+', r == '/':
		default:
			return false
		}
	}
	return true
}
