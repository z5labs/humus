// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package authn

import (
	"fmt"
	"strings"
)

// Requirement is what a request must have to be served. auth.Requirement
// is an alias of it, so it has no exported method: it is read with
// Items, IsAnonymous and IsDisabled. The zero Requirement is a mistake that
// Compile reports.
type Requirement struct {
	r *requirement
}

// requirement is a Requirement's declaration.
type requirement struct {
	kind  reqKind
	item  Item          // reqScheme's
	parts []Requirement // reqAll's
}

type reqKind int

const (
	reqScheme reqKind = iota + 1
	reqAll
	reqAnonymous
	reqDisabled
)

// Item is one scheme of a requirement, with the scopes its principal must
// have.
type Item struct {
	// Scheme is the scheme, nil for the zero auth.Scheme.
	Scheme *Scheme

	// Scopes are the scopes the principal must have, each checked with
	// HasScope.
	Scopes []string

	// HasScope reports whether principal has scope. It is nil when Scopes
	// is empty.
	HasScope func(principal any, scope string) bool
}

// Require returns the requirement that s authenticates the request, with
// a principal that has every one of scopes, as has reports.
func Require(s *Scheme, scopes []string, has func(principal any, scope string) bool) Requirement {
	return Requirement{&requirement{kind: reqScheme, item: Item{Scheme: s, Scopes: scopes, HasScope: has}}}
}

// All returns the requirement that every one of reqs is met.
func All(reqs []Requirement) Requirement {
	return Requirement{&requirement{kind: reqAll, parts: reqs}}
}

// Anonymous returns the requirement a request meets without credentials.
func Anonymous() Requirement {
	return Requirement{&requirement{kind: reqAnonymous}}
}

// Disabled returns the requirement every request meets, with no credential
// read.
func Disabled() Requirement {
	return Requirement{&requirement{kind: reqDisabled}}
}

// IsZero reports whether r is the zero Requirement.
func IsZero(r Requirement) bool { return r.r == nil }

// IsDisabled reports whether r is Disabled.
func IsDisabled(r Requirement) bool { return r.r != nil && r.r.kind == reqDisabled }

// IsAnonymous reports whether r is met without credentials: Anonymous, or
// an All of nothing but Anonymous.
func IsAnonymous(r Requirement) bool {
	return r.r != nil && r.r.kind != reqDisabled && len(Items(r)) == 0
}

// Items returns r's schemes, each with its scopes, in order: an All's are
// its parts', flattened. Anonymous has none.
func Items(r Requirement) []Item {
	if r.r == nil {
		return nil
	}
	switch r.r.kind {
	case reqScheme:
		return []Item{r.r.item}
	case reqAll:
		var items []Item
		for _, p := range r.r.parts {
			items = append(items, Items(p)...)
		}
		return items
	}
	return nil
}

// List is a list of requirements, checked by Compile: a request must meet
// one of them.
type List struct {
	reqs     [][]Item  // each requirement's items; none for Anonymous
	schemes  []*Scheme // every scheme of the list, by name, in order
	disabled bool
}

// Schemes returns every scheme of l, one for each name, in the order the
// requirements name them first.
func (l *List) Schemes() []*Scheme { return l.schemes }

// Disabled reports whether l is Disabled.
func (l *List) Disabled() bool { return l.disabled }

// Requirements returns l's requirements, in order, each as its schemes
// with their scopes: Anonymous's has none. A Disabled list has none.
func (l *List) Requirements() [][]Item { return l.reqs }

// Scoped reports whether a requirement of l names a scope, so that a
// request can be forbidden.
func (l *List) Scoped() bool {
	for _, items := range l.reqs {
		for _, it := range items {
			if len(it.Scopes) > 0 {
				return true
			}
		}
	}
	return false
}

// Compile checks reqs, and returns them as a List, or every mistake it
// finds in them:
//   - no requirement;
//   - the zero Requirement, and the zero Scheme;
//   - All with no requirement, and one scheme twice in one All;
//   - Disabled with another requirement, or inside an All;
//   - a nil verify function;
//   - two different schemes with one name, and a name that is empty or
//     holds a character other than letters, digits, '.', '-' and '_';
//   - Format on a scheme that isn't Bearer;
//   - a Basic realm that is empty or holds a quote;
//   - an OAuth2 with no flow, a flow without the URLs it needs, or a scope
//     no flow declares;
//   - an OpenIDConnect whose discovery URL isn't absolute.
//
// Each mistake names the requirement it is in, as reqs[i].
func Compile(reqs []Requirement) (*List, []error) {
	c := compiler{byName: make(map[string]*Scheme), checked: make(map[*Scheme]bool), reported: make(map[string]bool)}
	if len(reqs) == 0 {
		c.fail("no requirement")
	}
	l := &List{}
	for i, r := range reqs {
		at := fmt.Sprintf("reqs[%d]", i)
		if r.r == nil {
			c.fail("%s is the zero Requirement", at)
			continue
		}
		if r.r.kind == reqDisabled {
			l.disabled = true
			if len(reqs) > 1 {
				c.fail("%s is Disabled, which stands alone, but the list holds %d requirements", at, len(reqs))
			}
			continue
		}
		c.parts(at, r)
		items := Items(r)
		seen := make(map[string]bool)
		for _, it := range items {
			if it.Scheme == nil {
				continue
			}
			if seen[it.Scheme.Name] && r.r.kind == reqAll {
				c.fail("%s: All names the scheme %q twice", at, it.Scheme.Name)
			}
			seen[it.Scheme.Name] = true
		}
		for _, it := range items {
			c.item(at, it, l)
		}
		l.reqs = append(l.reqs, items)
	}
	if len(c.errs) > 0 {
		return nil, c.errs
	}
	return l, nil
}

// compiler collects Compile's mistakes.
type compiler struct {
	errs     []error
	byName   map[string]*Scheme
	checked  map[*Scheme]bool
	reported map[string]bool // the names already reported as two schemes'
}

func (c *compiler) fail(format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf(format, args...))
}

// parts checks the parts of an All, at any depth: none, a zero part,
// and Disabled among them.
func (c *compiler) parts(at string, r Requirement) {
	if r.r.kind != reqAll {
		return
	}
	if len(r.r.parts) == 0 {
		c.fail("%s is All with no requirement", at)
	}
	for j, p := range r.r.parts {
		in := fmt.Sprintf("%s: All's reqs[%d]", at, j)
		switch {
		case p.r == nil:
			c.fail("%s is the zero Requirement", in)
		case p.r.kind == reqDisabled:
			c.fail("%s is Disabled, which stands alone, never inside an All", in)
		default:
			c.parts(in, p)
		}
	}
}

// item checks one scheme of a requirement, and its scopes, and adds the
// scheme to l's.
func (c *compiler) item(at string, it Item, l *List) {
	s := it.Scheme
	if s == nil {
		c.fail("%s names the zero Scheme", at)
		return
	}
	if first, ok := c.byName[s.Name]; !ok {
		c.byName[s.Name] = s
		l.schemes = append(l.schemes, s)
	} else if !first.Same(s) && !c.reported[s.Name] {
		c.reported[s.Name] = true
		c.fail("%s: two different schemes are named %q", at, s.Name)
	}
	if s.Kind == OAuth2 {
		for _, scope := range it.Scopes {
			if !s.Flows.declares(scope) {
				c.fail("%s: the scope %q is declared by no flow of the OAuth2 scheme %q", at, scope, s.Name)
			}
		}
	}
	if c.checked[s] {
		return
	}
	c.checked[s] = true
	c.scheme(at, s)
}

// scheme checks a scheme's own declaration.
func (c *compiler) scheme(at string, s *Scheme) {
	name := fmt.Sprintf("%s: the %s scheme %q", at, s.Kind, s.Name)
	if s.Name == "" {
		c.fail("%s: a %s scheme has an empty name", at, s.Kind)
	} else if !validName(s.Name) {
		c.fail("%s has a name with a character other than letters, digits, '.', '-' and '_'", name)
	}
	if s.Verify == nil {
		c.fail("%s has a nil verify function", name)
	}
	if s.Formatted && s.Kind != Bearer {
		c.fail("%s has a Format, which is for Bearer alone", name)
	}
	switch s.Kind {
	case Basic:
		if s.Realm == "" {
			c.fail("%s has an empty realm", name)
		} else if strings.Contains(s.Realm, `"`) {
			c.fail("%s has a realm that holds a quote", name)
		}
	case APIKeyHeader, APIKeyQuery, APIKeyCookie:
		if s.Param == "" {
			c.fail("%s reads its key from an empty name", name)
		}
	case OAuth2:
		c.flows(name, s.Flows)
	case OpenIDConnect:
		if s.Discovery == nil || !s.Discovery.IsAbs() || s.Discovery.Host == "" {
			c.fail("%s has a discovery URL that isn't absolute", name)
		}
	}
}

// flows checks an OAuth2 scheme's flows: one at least, each with the URLs
// it needs.
func (c *compiler) flows(name string, f Flows) {
	if f.AuthorizationCode == nil && f.ClientCredentials == nil && f.DeviceAuthorization == nil {
		c.fail("%s has no flow", name)
		return
	}
	need := func(flow string, field string, u bool) {
		if !u {
			c.fail("%s: its %s flow has no %s", name, flow, field)
		}
	}
	if fl := f.AuthorizationCode; fl != nil {
		need("AuthorizationCode", "AuthorizationURL", fl.AuthorizationURL != nil)
		need("AuthorizationCode", "TokenURL", fl.TokenURL != nil)
	}
	if fl := f.ClientCredentials; fl != nil {
		need("ClientCredentials", "TokenURL", fl.TokenURL != nil)
	}
	if fl := f.DeviceAuthorization; fl != nil {
		need("DeviceAuthorization", "DeviceAuthorizationURL", fl.DeviceAuthorizationURL != nil)
		need("DeviceAuthorization", "TokenURL", fl.TokenURL != nil)
	}
}

// validName reports whether name is made of letters, digits, '.', '-' and
// '_' alone, as OpenAPI requires of a components key.
func validName(name string) bool {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
