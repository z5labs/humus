// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"net/http"
	"net/url"
	"slices"

	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/internal/authn"
)

// Security is what a request must have: one of reqs. Given to NewAPI, it
// is every operation's, and the document's. Given to an operation or to
// Document, it replaces the API's. Security(auth.Disabled()) makes what it
// is given to public. Security() with no requirement is a mistake that
// NewAPI reports.
//
// A request no requirement authenticated is a RequestError with status 401,
// and one that only lacked a scope a RequestError with status 403, which
// RequestErrors answers. Either is answered before the operation reads its
// parameters or its body.
func Security(reqs ...auth.Requirement) APIOption {
	return securityOption{reqs: slices.Clone(reqs)}
}

type securityOption struct {
	reqs []auth.Requirement
}

func (o securityOption) apply(p *parts) { p.security = &o }

func (securityOption) apiOption() {}

// security compiles o's requirements, reporting their mistakes after
// where, which is "" for the API's and ends in ": " otherwise. It records
// the schemes they name, so that two different schemes with one name in
// two lists are reported too. It returns nil when o is nil, or has a
// mistake.
func (b *builder) security(where string, o *securityOption) *authn.List {
	if o == nil {
		return nil
	}
	if len(o.reqs) == 0 {
		b.fail("%sSecurity() has no requirement: give it auth.Disabled() to make it public", where)
		return nil
	}
	l, errs := authn.Compile(o.reqs)
	for _, err := range errs {
		b.fail("%sSecurity: %w", where, err)
	}
	if l == nil {
		return nil
	}
	for _, s := range l.Schemes() {
		first, ok := b.schemes[s.Name]
		switch {
		case !ok:
			b.schemes[s.Name] = s
			b.schemeOrder = append(b.schemeOrder, s)
		case !first.Same(s) && !b.reportedSchemes[s.Name]:
			b.reportedSchemes[s.Name] = true
			b.fail("%sSecurity: two different schemes are named %q", where, s.Name)
		}
	}
	return l
}

// canAnswer401 reports whether a request can be unauthenticated under l.
func canAnswer401(l *authn.List) bool {
	return l != nil && !l.Disabled()
}

// canAnswer403 reports whether a request can be forbidden under l.
func canAnswer403(l *authn.List) bool {
	return canAnswer401(l) && l.Scoped()
}

// authenticate authenticates r against the operation's requirements. It
// returns the request to serve, with its principals in its context, and
// true, or answers r itself and returns false:
//   - a request no requirement authenticated, or one that only lacked a
//     scope, is answered by RequestErrors, at 401 or 403, after
//     WWW-Authenticate is set;
//   - a verifier's other error is answered as the handler's error is;
//   - a verifier's panic is answered by the catch-all, and recorded.
func (rt *route) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	l := rt.answers.security
	if l == nil {
		return r, true
	}
	var res authn.Result
	if p := guard(func() { res = l.Authenticate(r.Context(), authn.HTTPSource(r)) }); p != nil {
		rt.answerPanic(w, r, p)
		return nil, false
	}
	switch res.Outcome {
	case authn.Served:
		if res.Context != r.Context() {
			r = r.WithContext(res.Context)
		}
		return r, true
	case authn.Unauthenticated, authn.Forbidden:
		status := http.StatusUnauthorized
		if res.Outcome == authn.Forbidden {
			status = http.StatusForbidden
		}
		for _, c := range res.Challenges {
			w.Header().Add("WWW-Authenticate", c)
		}
		rt.reject(w, r, &RequestError{Status: status, Err: res.Err})
	default:
		rt.answerError(w, r, res.Err)
	}
	return nil, false
}

// The security parts of the document, as they are encoded.
type (
	componentsDoc struct {
		SecuritySchemes map[string]securitySchemeDoc `json:"securitySchemes"`
	}

	securitySchemeDoc struct {
		Type             string         `json:"type"`
		Description      string         `json:"description,omitempty"`
		Name             string         `json:"name,omitempty"`
		In               string         `json:"in,omitempty"`
		Scheme           string         `json:"scheme,omitempty"`
		BearerFormat     string         `json:"bearerFormat,omitempty"`
		Flows            *oauthFlowsDoc `json:"flows,omitempty"`
		OpenIDConnectURL string         `json:"openIdConnectUrl,omitempty"`
	}

	oauthFlowsDoc struct {
		AuthorizationCode   *oauthFlowDoc `json:"authorizationCode,omitempty"`
		ClientCredentials   *oauthFlowDoc `json:"clientCredentials,omitempty"`
		DeviceAuthorization *oauthFlowDoc `json:"deviceAuthorization,omitempty"`
	}

	oauthFlowDoc struct {
		AuthorizationURL       string            `json:"authorizationUrl,omitempty"`
		DeviceAuthorizationURL string            `json:"deviceAuthorizationUrl,omitempty"`
		TokenURL               string            `json:"tokenUrl,omitempty"`
		RefreshURL             string            `json:"refreshUrl,omitempty"`
		Scopes                 map[string]string `json:"scopes"`
	}

	// requirementDoc is a security requirement object: each scheme's name
	// to its scopes. Anonymous is {}.
	requirementDoc map[string][]string
)

// components returns the document's components: every scheme of every
// requirement, by its name. It is nil when the API names no scheme.
func components(schemes []*authn.Scheme) *componentsDoc {
	if len(schemes) == 0 {
		return nil
	}
	c := &componentsDoc{SecuritySchemes: make(map[string]securitySchemeDoc, len(schemes))}
	for _, s := range schemes {
		c.SecuritySchemes[s.Name] = schemeDoc(s)
	}
	return c
}

// schemeDoc returns s as a security scheme object.
func schemeDoc(s *authn.Scheme) securitySchemeDoc {
	d := securitySchemeDoc{Description: s.Description}
	switch s.Kind {
	case authn.Bearer:
		d.Type, d.Scheme, d.BearerFormat = "http", "bearer", s.Format
	case authn.Basic:
		d.Type, d.Scheme = "http", "basic"
	case authn.APIKeyHeader:
		d.Type, d.Name, d.In = "apiKey", s.Param, "header"
	case authn.APIKeyQuery:
		d.Type, d.Name, d.In = "apiKey", s.Param, "query"
	case authn.APIKeyCookie:
		d.Type, d.Name, d.In = "apiKey", s.Param, "cookie"
	case authn.MutualTLS:
		d.Type = "mutualTLS"
	case authn.OAuth2:
		d.Type = "oauth2"
		d.Flows = &oauthFlowsDoc{
			AuthorizationCode:   flowDoc(s.Flows.AuthorizationCode),
			ClientCredentials:   flowDoc(s.Flows.ClientCredentials),
			DeviceAuthorization: flowDoc(s.Flows.DeviceAuthorization),
		}
	case authn.OpenIDConnect:
		d.Type, d.OpenIDConnectURL = "openIdConnect", s.Discovery.String()
	}
	return d
}

// flowDoc returns f as an OAuth flow object, or nil when the scheme lacks
// it.
func flowDoc(f *authn.Flow) *oauthFlowDoc {
	if f == nil {
		return nil
	}
	str := func(u *url.URL) string {
		if u == nil {
			return ""
		}
		return u.String()
	}
	scopes := f.Scopes
	if scopes == nil {
		scopes = map[string]string{}
	}
	return &oauthFlowDoc{
		AuthorizationURL:       str(f.AuthorizationURL),
		DeviceAuthorizationURL: str(f.DeviceAuthorizationURL),
		TokenURL:               str(f.TokenURL),
		RefreshURL:             str(f.RefreshURL),
		Scopes:                 scopes,
	}
}

// requirementsDoc returns l as a list of security requirement objects: []
// for a Disabled list.
func requirementsDoc(l *authn.List) []requirementDoc {
	out := []requirementDoc{}
	for _, items := range l.Requirements() {
		req := requirementDoc{}
		for _, it := range items {
			scopes := req[it.Scheme.Name]
			if scopes == nil {
				scopes = []string{}
			}
			req[it.Scheme.Name] = append(scopes, it.Scopes...)
		}
		out = append(out, req)
	}
	return out
}
