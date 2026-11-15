// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/z5labs/humus/internal/param"
)

// ParamValue is the type of a parameter's value.
type ParamValue interface {
	~string | ~bool | ~int | ~int64 | ~float64
}

// PathParameter is a path parameter, such as {contactId}. Each constraint
// returns a copy with one more, and leaves the parameter it was called on
// unchanged, so a declaration can be shared:
//
//	var contactID = hypermedia.PathParam[ID]("contactId").Minimum(1)
//
// Enum, Minimum and Maximum take the parameter's own T, so a value of
// another type doesn't compile. A value that doesn't parse as T, or breaks
// a constraint, is a path no route serves.
type PathParameter[T ParamValue] struct {
	name        string
	constraints param.Constraints[T]
}

// PathParam returns the path parameter name. Path.Param or Path.Rest adds
// it to a path, and Arg gives it a value in a link.
func PathParam[T ParamValue](name string) PathParameter[T] {
	return PathParameter[T]{name: name}
}

// Enum returns p, whose value is one of values.
func (p PathParameter[T]) Enum(values ...T) PathParameter[T] {
	p.constraints.Enum = slices.Clone(values)
	return p
}

// Pattern returns p, whose value matches re. T is a string type.
func (p PathParameter[T]) Pattern(re *regexp.Regexp) PathParameter[T] {
	p.constraints.Pattern = re
	p.constraints.PatternSet = true
	return p
}

// MinLength returns p, whose value is at least n characters long. T is a
// string type.
func (p PathParameter[T]) MinLength(n int) PathParameter[T] {
	p.constraints.MinLength = &n
	return p
}

// MaxLength returns p, whose value is at most n characters long. T is a
// string type.
func (p PathParameter[T]) MaxLength(n int) PathParameter[T] {
	p.constraints.MaxLength = &n
	return p
}

// Minimum returns p, whose value is at least v. T is a number type.
func (p PathParameter[T]) Minimum(v T) PathParameter[T] {
	p.constraints.Minimum = &v
	return p
}

// Maximum returns p, whose value is at most v. T is a number type.
func (p PathParameter[T]) Maximum(v T) PathParameter[T] {
	p.constraints.Maximum = &v
	return p
}

// describe names p, for an error or a panic.
func (p PathParameter[T]) describe() string {
	return fmt.Sprintf("path parameter %q", p.name)
}

// check returns the mistakes in p's constraints, such as a Pattern on a
// number or a Minimum on a string, each naming p.
func (p PathParameter[T]) check() []error {
	var errs []error
	for _, err := range p.constraints.Check() {
		errs = append(errs, fmt.Errorf("%s: %w", p.describe(), err))
	}
	return errs
}

// parse parses raw as p's value, and checks it against p's constraints.
// Its error names p.
func (p PathParameter[T]) parse(raw string) (T, error) {
	v, err := p.constraints.Parse(raw)
	if err != nil {
		return v, fmt.Errorf("%s: %w", p.describe(), err)
	}
	return v, nil
}

// QueryParameter is a query parameter, such as ?page=2. URL.With gives it
// a value in a link.
type QueryParameter[T ParamValue] struct {
	name string
}

// QueryParam returns the query parameter name.
func QueryParam[T ParamValue](name string) QueryParameter[T] {
	return QueryParameter[T]{name: name}
}
