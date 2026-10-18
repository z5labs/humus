// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"fmt"
	"strings"
)

// Path is a path of the API, built from the root one segment at a time.
// Each method returns a new Path, so a prefix can be shared:
//
//	carts := openapi.Root().Lit("carts") // /carts
//	cart := carts.Param(cartID)          // /carts/{cartId}
//	items := cart.Lit("items")           // /carts/{cartId}/items
//
// A Path is checked by NewAPI, not by its methods, so a path declared as a
// package-level variable never panics while its package initializes. The
// zero Path is the root.
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

	// check returns the mistakes in the parameter's declaration.
	check func() []error

	// parse parses and checks one value of the parameter.
	parse func(string) (any, error)

	// document returns the parameter as the document lists it.
	document func() paramDoc
}

// Root returns the root path, "/".
func Root() Path {
	return Path{}
}

// Lit returns p followed by the literal segment s, such as "carts". A
// literal is made only of letters, digits, "-", ".", "_" and "~", and isn't
// empty, "." or "..": NewAPI reports any other.
func (p Path) Lit(s string) Path {
	return p.with(segment{lit: s})
}

// Param returns p followed by the path parameter pp, such as {cartId}. Its
// name is an ASCII Go identifier, and appears once in the path, and its
// constraints fit its type: NewAPI reports any other.
func (p Path) Param[T ParamValue](pp PathParameter[T]) Path {
	return p.with(segment{param: &pathParam{
		name:  pp.spec.name,
		check: pp.spec.check,
		parse: func(s string) (any, error) {
			return pp.parseValue(s)
		},
		document: func() paramDoc {
			return pp.spec.document("path", true)
		},
	}})
}

// with returns p followed by seg, in a new backing array, so two paths
// built from one prefix never share one.
func (p Path) with(seg segment) Path {
	segs := make([]segment, len(p.segs), len(p.segs)+1)
	copy(segs, p.segs)
	return Path{segs: append(segs, seg)}
}

// String returns p as an OpenAPI path, such as "/carts/{cartId}/items".
// The root is "/".
func (p Path) String() string {
	return p.render(func(pp *pathParam) string { return "{" + pp.name + "}" })
}

// template returns p with every parameter written as {}, so two paths that
// differ only in their parameters' names have the same template.
func (p Path) template() string {
	return p.render(func(*pathParam) string { return "{}" })
}

func (p Path) render(param func(*pathParam) string) string {
	if len(p.segs) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, seg := range p.segs {
		b.WriteByte('/')
		if seg.param != nil {
			b.WriteString(param(seg.param))
			continue
		}
		b.WriteString(seg.lit)
	}
	return b.String()
}

// check returns every mistake in p's segments.
func (p Path) check() []error {
	var errs []error
	seen := make(map[string]bool)
	for _, seg := range p.segs {
		if seg.param == nil {
			if err := checkLiteral(seg.lit); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		name := seg.param.name
		if !isIdentifier(name) {
			errs = append(errs, fmt.Errorf("path parameter %q isn't an ASCII Go identifier", name))
			continue
		}
		if seen[name] {
			errs = append(errs, fmt.Errorf("path parameter %q appears more than once", name))
			continue
		}
		seen[name] = true
		for _, err := range seg.param.check() {
			errs = append(errs, fmt.Errorf("path parameter %q: %w", name, err))
		}
	}
	return errs
}

// checkLiteral reports a literal that would need escaping, in the document
// or on the wire, or that a client would resolve away.
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

// isIdentifier reports whether s is an ASCII Go identifier.
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
