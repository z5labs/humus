// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia

import (
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/z5labs/humus/internal/param"
)

// Path is a path, built from Root one segment at a time. Each method
// returns a new Path, so a prefix can be shared:
//
//	contacts := hypermedia.Root().Lit("contacts") // /contacts
//	contact := contacts.Param(contactID)          // /contacts/{contactId}
//	edit := contact.Lit("edit")                   // /contacts/{contactId}/edit
//
// Building a Path never panics, whatever its literals, names or
// constraints, so a path declared as a package-level variable never panics
// while its package initializes. The zero Path is the root.
type Path struct {
	segs []segment
}

// segment is one segment of a Path: a literal, or a path parameter.
type segment struct {
	lit   string
	param *pathParam
}

// pathParam is a PathParameter, erased.
type pathParam struct {
	name string

	// typ is the parameter's T, which an Arg for it has.
	typ reflect.Type

	// rest reports whether the parameter takes the rest of the path.
	rest bool

	// check returns the mistakes in the parameter's declaration.
	check func() []error

	// parse parses and checks one value of the parameter.
	parse func(string) (any, error)
}

// Root returns the root path, "/".
func Root() Path {
	return Path{}
}

// Lit returns p followed by the literal segment s, such as "contacts".
func (p Path) Lit(s string) Path {
	return p.with(segment{lit: s})
}

// Param returns p followed by the path parameter pp, such as {contactId}.
func (p Path) Param[T ParamValue](pp PathParameter[T]) Path {
	return p.with(segment{param: erase(pp, false)})
}

// Rest returns p followed by the path parameter pp, which takes the rest
// of the path, slashes included, such as {file...}. Its value may be
// empty. It is p's last segment. A link keeps the slashes in its value,
// and escapes each segment between them.
func (p Path) Rest[T ~string](pp PathParameter[T]) Path {
	return p.with(segment{param: erase(pp, true)})
}

// erase returns pp as a path's segment holds it.
func erase[T ParamValue](pp PathParameter[T], rest bool) *pathParam {
	return &pathParam{
		name:  pp.name,
		typ:   reflect.TypeFor[T](),
		rest:  rest,
		check: pp.check,
		parse: func(s string) (any, error) {
			return pp.parse(s)
		},
	}
}

// with returns p followed by seg, in a new backing array, so two paths
// built from one prefix never share one.
func (p Path) with(seg segment) Path {
	segs := make([]segment, len(p.segs), len(p.segs)+1)
	copy(segs, p.segs)
	return Path{segs: append(segs, seg)}
}

// String returns p's pattern, such as "/contacts/{contactId}" or
// "/static/{file...}". The root is "/".
func (p Path) String() string {
	if len(p.segs) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, seg := range p.segs {
		b.WriteByte('/')
		switch {
		case seg.param == nil:
			b.WriteString(seg.lit)
		case seg.param.rest:
			b.WriteString("{" + seg.param.name + "...}")
		default:
			b.WriteString("{" + seg.param.name + "}")
		}
	}
	return b.String()
}

// URL returns a link to p, with one PathArg for each of p's parameters, in
// any order. A missing, repeated or extra argument panics, naming p and
// the parameter, as does an argument whose parameter has p's parameter's
// name but another type.
//
// Each value is escaped as a path segment, so a "/" in it is "%2F". A Rest
// value keeps its slashes, escapes each segment between them, and may be
// empty. A value isn't checked against its parameter's constraints: a link
// out of bounds is built, and leads to the 404 page.
func (p Path) URL(args ...PathArg) URL {
	used := make([]bool, len(args))
	var b strings.Builder
	for _, seg := range p.segs {
		b.WriteByte('/')
		if seg.param == nil {
			b.WriteString(url.PathEscape(seg.lit))
			continue
		}
		i := p.argFor(seg.param, args)
		used[i] = true
		if !seg.param.rest {
			b.WriteString(url.PathEscape(args[i].value))
			continue
		}
		parts := strings.Split(args[i].value, "/")
		for j, part := range parts {
			parts[j] = url.PathEscape(part)
		}
		b.WriteString(strings.Join(parts, "/"))
	}
	if i := slices.Index(used, false); i >= 0 {
		panic(fmt.Sprintf("hypermedia: link to %s: an Arg for path parameter %q, which the path doesn't have", p, args[i].name))
	}
	if b.Len() == 0 {
		return URL{path: "/"}
	}
	return URL{path: b.String()}
}

// argFor returns the index in args of pp's one argument, and panics if
// pp has none, more than one, or one of another type.
func (p Path) argFor(pp *pathParam, args []PathArg) int {
	found := -1
	for i, arg := range args {
		if arg.name != pp.name {
			continue
		}
		if found >= 0 {
			panic(fmt.Sprintf("hypermedia: link to %s: more than one Arg for path parameter %q", p, pp.name))
		}
		found = i
	}
	if found < 0 {
		panic(fmt.Sprintf("hypermedia: link to %s: no Arg for path parameter %q", p, pp.name))
	}
	if typ := args[found].typ; typ != pp.typ {
		panic(fmt.Sprintf("hypermedia: link to %s: the Arg for path parameter %q has type %s, but the parameter has type %s", p, pp.name, typ, pp.typ))
	}
	return found
}

// PathArg is one path parameter's value in a link.
type PathArg struct {
	name  string
	typ   reflect.Type
	value string
}

// Arg returns v as p's value, for Path.URL.
func Arg[T ParamValue](p PathParameter[T], v T) PathArg {
	return PathArg{name: p.name, typ: reflect.TypeFor[T](), value: param.Format(v)}
}

// URL is a link to a path, escaped, as it goes in an href or a Location.
type URL struct {
	path  string
	query []queryValue
}

// queryValue is one query parameter's value in a URL.
type queryValue struct {
	name, value string
}

// With returns u with v as q's value, escaped. A query parameter has one
// value, so a value u already has for q is replaced, in its place. u is
// unchanged.
func (u URL) With[T ParamValue](q QueryParameter[T], v T) URL {
	qv := queryValue{name: q.name, value: param.Format(v)}
	query := slices.Clone(u.query)
	if i := slices.IndexFunc(query, func(e queryValue) bool { return e.name == q.name }); i >= 0 {
		query[i] = qv
	} else {
		query = append(query, qv)
	}
	u.query = query
	return u
}

// String returns u as it goes in an href or a Location, such as
// "/contacts/7/edit" or "/contacts?page=2". Its query parameters are in the
// order With first added them.
func (u URL) String() string {
	if len(u.query) == 0 {
		return u.path
	}
	var b strings.Builder
	b.WriteString(u.path)
	for i, qv := range u.query {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(qv.name))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(qv.value))
	}
	return b.String()
}

// check returns every mistake in p's segments: a literal a path can't
// have, a parameter name that isn't an ASCII Go identifier, a parameter
// that appears twice, a constraint that doesn't fit its parameter's type,
// and a Rest that isn't p's last segment.
func (p Path) check() []error {
	var errs []error
	seen := make(map[string]bool)
	for i, seg := range p.segs {
		if seg.param == nil {
			if err := checkLiteral(seg.lit); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		name := seg.param.name
		if seg.param.rest && i != len(p.segs)-1 {
			errs = append(errs, fmt.Errorf("path parameter %q is a Rest, but isn't the path's last segment", name))
		}
		if !isIdentifier(name) {
			errs = append(errs, fmt.Errorf("path parameter %q isn't an ASCII Go identifier", name))
			continue
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("path parameter %q appears more than once", name))
			continue
		}
		seen[name] = true
		errs = append(errs, seg.param.check()...)
	}
	return errs
}

// template returns p's pattern with its parameters' names left out, such
// as "/contacts/{}", so two paths that differ only in their names have
// one template.
func (p Path) template() string {
	var b strings.Builder
	for _, seg := range p.segs {
		b.WriteByte('/')
		switch {
		case seg.param == nil:
			b.WriteString(seg.lit)
		case seg.param.rest:
			b.WriteString("{...}")
		default:
			b.WriteString("{}")
		}
	}
	return b.String()
}

// muxPattern returns p's path as an http.ServeMux pattern. The root is
// "/{$}", since "/" is a prefix of every path to a ServeMux.
func (p Path) muxPattern() string {
	if len(p.segs) == 0 {
		return "/{$}"
	}
	return p.String()
}

// checkLiteral reports a literal that would need escaping, or that a
// client would resolve away.
func checkLiteral(s string) error {
	switch s {
	case "":
		return fmt.Errorf("a literal is empty")
	case ".", "..":
		return fmt.Errorf("literal %q is a dot segment", s)
	}
	for _, r := range s {
		if !isUnreserved(r) {
			return fmt.Errorf("literal %q holds %q, but a literal holds only letters, digits, \"-\", \".\", \"_\" and \"~\"", s, r)
		}
	}
	return nil
}

// isUnreserved reports whether r is an unreserved character of RFC 3986.
func isUnreserved(r rune) bool {
	return isLetter(r) || isDigit(r) || r == '-' || r == '.' || r == '_' || r == '~'
}

// isIdentifier reports whether s is an ASCII Go identifier, as an
// http.ServeMux wildcard's name is.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !isLetter(r) && r != '_' && (i == 0 || !isDigit(r)) {
			return false
		}
	}
	return true
}

func isLetter(r rune) bool { return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' }

func isDigit(r rune) bool { return '0' <= r && r <= '9' }
