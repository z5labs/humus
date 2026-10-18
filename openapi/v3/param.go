// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ParamValue is the type of a parameter's value.
type ParamValue interface {
	~string | ~bool | ~int | ~int64 | ~float64
}

// Param is a path, query or header parameter, as ParamFrom takes it. Its
// methods are unexported, so only this package's parameter types are
// Params.
type Param[T ParamValue] interface {
	// paramKey identifies the parameter within an operation.
	paramKey() string

	// describe names the parameter, for an error or a panic.
	describe() string

	// parseValue parses one value of the parameter, and checks it against
	// the parameter's constraints.
	parseValue(s string) (T, error)
}

// A parameter of each kind. A path parameter is always required. A query
// or header parameter is optional unless it is Required.
//
// Each method returns a copy of the parameter with one more part of its
// declaration, so a declaration can be shared:
//
//	var include = openapi.QueryParam[Include]("include").
//		Description("What the cart includes").
//		Enum(IncludeItems, IncludeCount).
//		Default(IncludeItems)
//
// Enum, Default, Example, Minimum and Maximum take the parameter's own T,
// so a value of another type doesn't compile. NewAPI reports the rest:
// Pattern, MinLength or MaxLength on a type that isn't a string, Minimum or
// Maximum on a type that isn't a number, and a Default or an Example that
// breaks the parameter's own Enum, Pattern or bounds.
type (
	PathParameter[T ParamValue]   struct{ spec paramSpec[T] }
	QueryParameter[T ParamValue]  struct{ spec paramSpec[T] }
	HeaderParameter[T ParamValue] struct{ spec paramSpec[T] }
)

// PathParam returns the path parameter name. Path.Param adds it to a path,
// and an operation reads every parameter of its path.
func PathParam[T ParamValue](name string) PathParameter[T] {
	return PathParameter[T]{spec: paramSpec[T]{name: name}}
}

// QueryParam returns the query parameter name. Query gives it to an
// operation.
func QueryParam[T ParamValue](name string) QueryParameter[T] {
	return QueryParameter[T]{spec: paramSpec[T]{name: name}}
}

// HeaderParam returns the header parameter name. Header gives it to an
// operation. Its name is matched as HTTP matches a header's, whatever its
// case.
func HeaderParam[T ParamValue](name string) HeaderParameter[T] {
	return HeaderParameter[T]{spec: paramSpec[T]{name: name}}
}

// Query adds a query parameter that the operation reads. An operation's
// path parameters are its path's.
func Query[T ParamValue](p QueryParameter[T]) Option {
	return paramOption{p}
}

// Header adds a header parameter that the operation reads. See Query.
func Header[T ParamValue](p HeaderParameter[T]) Option {
	return paramOption{p}
}

// Description returns p, described by s.
func (p PathParameter[T]) Description(s string) PathParameter[T] {
	p.spec = p.spec.withDescription(s)
	return p
}

// Example returns p, with the example value v.
func (p PathParameter[T]) Example(v T) PathParameter[T] {
	p.spec = p.spec.withExample(v)
	return p
}

// Enum returns p, whose value is one of values.
func (p PathParameter[T]) Enum(values ...T) PathParameter[T] {
	p.spec = p.spec.withEnum(values)
	return p
}

// Pattern returns p, whose value matches re. T is a string type.
func (p PathParameter[T]) Pattern(re *regexp.Regexp) PathParameter[T] {
	p.spec = p.spec.withPattern(re)
	return p
}

// MinLength returns p, whose value is at least n characters long. T is a
// string type.
func (p PathParameter[T]) MinLength(n int) PathParameter[T] {
	p.spec = p.spec.withMinLength(n)
	return p
}

// MaxLength returns p, whose value is at most n characters long. T is a
// string type.
func (p PathParameter[T]) MaxLength(n int) PathParameter[T] {
	p.spec = p.spec.withMaxLength(n)
	return p
}

// Minimum returns p, whose value is at least v. T is a number type.
func (p PathParameter[T]) Minimum(v T) PathParameter[T] {
	p.spec = p.spec.withMinimum(v)
	return p
}

// Maximum returns p, whose value is at most v. T is a number type.
func (p PathParameter[T]) Maximum(v T) PathParameter[T] {
	p.spec = p.spec.withMaximum(v)
	return p
}

// Description returns p, described by s.
func (p QueryParameter[T]) Description(s string) QueryParameter[T] {
	p.spec = p.spec.withDescription(s)
	return p
}

// Example returns p, with the example value v.
func (p QueryParameter[T]) Example(v T) QueryParameter[T] {
	p.spec = p.spec.withExample(v)
	return p
}

// Enum returns p, whose value is one of values.
func (p QueryParameter[T]) Enum(values ...T) QueryParameter[T] {
	p.spec = p.spec.withEnum(values)
	return p
}

// Pattern returns p, whose value matches re. T is a string type.
func (p QueryParameter[T]) Pattern(re *regexp.Regexp) QueryParameter[T] {
	p.spec = p.spec.withPattern(re)
	return p
}

// MinLength returns p, whose value is at least n characters long. T is a
// string type.
func (p QueryParameter[T]) MinLength(n int) QueryParameter[T] {
	p.spec = p.spec.withMinLength(n)
	return p
}

// MaxLength returns p, whose value is at most n characters long. T is a
// string type.
func (p QueryParameter[T]) MaxLength(n int) QueryParameter[T] {
	p.spec = p.spec.withMaxLength(n)
	return p
}

// Minimum returns p, whose value is at least v. T is a number type.
func (p QueryParameter[T]) Minimum(v T) QueryParameter[T] {
	p.spec = p.spec.withMinimum(v)
	return p
}

// Maximum returns p, whose value is at most v. T is a number type.
func (p QueryParameter[T]) Maximum(v T) QueryParameter[T] {
	p.spec = p.spec.withMaximum(v)
	return p
}

// Required returns p, which every request has.
func (p QueryParameter[T]) Required() QueryParameter[T] {
	p.spec.required = true
	return p
}

// Default returns p, whose value is v when a request doesn't have it.
// Without Default, it is T's zero value.
func (p QueryParameter[T]) Default(v T) QueryParameter[T] {
	p.spec = p.spec.withDefault(v)
	return p
}

// Description returns p, described by s.
func (p HeaderParameter[T]) Description(s string) HeaderParameter[T] {
	p.spec = p.spec.withDescription(s)
	return p
}

// Example returns p, with the example value v.
func (p HeaderParameter[T]) Example(v T) HeaderParameter[T] {
	p.spec = p.spec.withExample(v)
	return p
}

// Enum returns p, whose value is one of values.
func (p HeaderParameter[T]) Enum(values ...T) HeaderParameter[T] {
	p.spec = p.spec.withEnum(values)
	return p
}

// Pattern returns p, whose value matches re. T is a string type.
func (p HeaderParameter[T]) Pattern(re *regexp.Regexp) HeaderParameter[T] {
	p.spec = p.spec.withPattern(re)
	return p
}

// MinLength returns p, whose value is at least n characters long. T is a
// string type.
func (p HeaderParameter[T]) MinLength(n int) HeaderParameter[T] {
	p.spec = p.spec.withMinLength(n)
	return p
}

// MaxLength returns p, whose value is at most n characters long. T is a
// string type.
func (p HeaderParameter[T]) MaxLength(n int) HeaderParameter[T] {
	p.spec = p.spec.withMaxLength(n)
	return p
}

// Minimum returns p, whose value is at least v. T is a number type.
func (p HeaderParameter[T]) Minimum(v T) HeaderParameter[T] {
	p.spec = p.spec.withMinimum(v)
	return p
}

// Maximum returns p, whose value is at most v. T is a number type.
func (p HeaderParameter[T]) Maximum(v T) HeaderParameter[T] {
	p.spec = p.spec.withMaximum(v)
	return p
}

// Required returns p, which every request has.
func (p HeaderParameter[T]) Required() HeaderParameter[T] {
	p.spec.required = true
	return p
}

// Default returns p, whose value is v when a request doesn't have it.
// Without Default, it is T's zero value.
func (p HeaderParameter[T]) Default(v T) HeaderParameter[T] {
	p.spec = p.spec.withDefault(v)
	return p
}

func (p PathParameter[T]) paramKey() string { return pathKey(p.spec.name) }

func (p PathParameter[T]) describe() string { return fmt.Sprintf("path parameter %q", p.spec.name) }

func (p PathParameter[T]) parseValue(s string) (T, error) {
	return p.spec.parseValue(p.describe(), s)
}

func (p QueryParameter[T]) paramKey() string { return "query " + p.spec.name }

func (p QueryParameter[T]) describe() string { return fmt.Sprintf("query parameter %q", p.spec.name) }

func (p QueryParameter[T]) parseValue(s string) (T, error) {
	return p.spec.parseValue(p.describe(), s)
}

func (p QueryParameter[T]) paramName() string { return p.spec.name }

func (p QueryParameter[T]) check() []error {
	return p.spec.checkNamed(p.describe(), "QueryParameter")
}

func (p QueryParameter[T]) read(_ *http.Request, query url.Values) (any, error) {
	values := query[p.spec.name]
	if len(values) > 1 {
		return nil, fmt.Errorf("%s appears %d times, but has one value", p.describe(), len(values))
	}
	return p.spec.read(p.describe(), values)
}

func (p HeaderParameter[T]) paramKey() string {
	return "header " + http.CanonicalHeaderKey(p.spec.name)
}

func (p HeaderParameter[T]) describe() string { return fmt.Sprintf("header parameter %q", p.spec.name) }

func (p HeaderParameter[T]) parseValue(s string) (T, error) {
	return p.spec.parseValue(p.describe(), s)
}

func (p HeaderParameter[T]) paramName() string { return p.spec.name }

func (p HeaderParameter[T]) check() []error {
	return p.spec.checkNamed(p.describe(), "HeaderParameter")
}

// read reads the header's field lines as one value. HTTP reads several
// lines of one field as their values joined by commas, so that is how they
// are parsed.
func (p HeaderParameter[T]) read(r *http.Request, _ url.Values) (any, error) {
	values := r.Header.Values(p.spec.name)
	if len(values) > 1 {
		values = []string{strings.Join(values, ", ")}
	}
	return p.spec.read(p.describe(), values)
}

// pathKey is the key of the path parameter name in a request's parameters.
func pathKey(name string) string { return "path " + name }

// paramOption is Query's and Header's Option.
type paramOption struct {
	p operationParam
}

func (o paramOption) apply(p *parts) { p.params = append(p.params, o.p) }

// operationParam is a query or header parameter, erased.
type operationParam interface {
	paramKey() string
	describe() string

	// paramName is the parameter's name, as a RequestError's Param.
	paramName() string

	// check returns the mistakes in the parameter's declaration.
	check() []error

	// read returns the parameter's value in r, whose query is query. Its
	// error names the parameter.
	read(r *http.Request, query url.Values) (any, error)
}

// paramSpec is a parameter's declaration.
type paramSpec[T ParamValue] struct {
	name        string
	description string
	example     *T
	enum        []T
	pattern     *regexp.Regexp
	patternSet  bool
	minLength   *int
	maxLength   *int
	minimum     *T
	maximum     *T
	required    bool
	def         *T
}

// The with methods return s with one more part. s is a copy, and a part is
// never changed once it is set, so a part shared between copies is never
// written through.

func (s paramSpec[T]) withDescription(d string) paramSpec[T] {
	s.description = d
	return s
}

func (s paramSpec[T]) withExample(v T) paramSpec[T] {
	s.example = &v
	return s
}

func (s paramSpec[T]) withEnum(values []T) paramSpec[T] {
	s.enum = slices.Clone(values)
	return s
}

func (s paramSpec[T]) withPattern(re *regexp.Regexp) paramSpec[T] {
	s.pattern = re
	s.patternSet = true
	return s
}

func (s paramSpec[T]) withMinLength(n int) paramSpec[T] {
	s.minLength = &n
	return s
}

func (s paramSpec[T]) withMaxLength(n int) paramSpec[T] {
	s.maxLength = &n
	return s
}

func (s paramSpec[T]) withMinimum(v T) paramSpec[T] {
	s.minimum = &v
	return s
}

func (s paramSpec[T]) withMaximum(v T) paramSpec[T] {
	s.maximum = &v
	return s
}

func (s paramSpec[T]) withDefault(v T) paramSpec[T] {
	s.def = &v
	return s
}

// checkNamed returns the mistakes in a query or header parameter's
// declaration, each naming it as what. typeName is its type's name, for a
// zero parameter.
func (s paramSpec[T]) checkNamed(what, typeName string) []error {
	if s.name == "" {
		return []error{fmt.Errorf("a %s has no name: is it the zero %s?", typeName, typeName)}
	}
	var errs []error
	for _, err := range s.check() {
		errs = append(errs, fmt.Errorf("%s: %w", what, err))
	}
	return errs
}

// check returns the mistakes in s's constraints: a constraint that doesn't
// fit T, and a Default or an Example that breaks another constraint.
func (s paramSpec[T]) check() []error {
	typ := reflect.TypeFor[T]()
	var errs []error
	if !isString(typ) {
		for _, c := range []struct {
			name string
			set  bool
		}{{"Pattern", s.patternSet}, {"MinLength", s.minLength != nil}, {"MaxLength", s.maxLength != nil}} {
			if c.set {
				errs = append(errs, fmt.Errorf("%s needs a string type, but its type is %s", c.name, typ))
			}
		}
	}
	if !isNumber(typ) {
		for _, c := range []struct {
			name string
			set  bool
		}{{"Minimum", s.minimum != nil}, {"Maximum", s.maximum != nil}} {
			if c.set {
				errs = append(errs, fmt.Errorf("%s needs a number type, but its type is %s", c.name, typ))
			}
		}
	}
	if s.patternSet && s.pattern == nil {
		errs = append(errs, errors.New("its Pattern is nil"))
	}
	if s.def != nil {
		if err := s.validate(*s.def); err != nil {
			errs = append(errs, fmt.Errorf("its Default: %w", err))
		}
	}
	if s.example != nil {
		if err := s.validate(*s.example); err != nil {
			errs = append(errs, fmt.Errorf("its Example: %w", err))
		}
	}
	return errs
}

// read returns the value of the parameter what, given the values a request
// has for it, which are none or one.
func (s paramSpec[T]) read(what string, values []string) (any, error) {
	if len(values) == 0 {
		switch {
		case s.required:
			return nil, fmt.Errorf("%s is required, but the request has none", what)
		case s.def != nil:
			return *s.def, nil
		default:
			var zero T
			return zero, nil
		}
	}
	return s.parseValue(what, values[0])
}

// parseValue parses raw as a T, and checks it against s's constraints. Its
// error names the parameter what.
func (s paramSpec[T]) parseValue(what, raw string) (T, error) {
	v, err := parseValue[T](raw)
	if err == nil {
		err = s.validate(v)
	}
	if err != nil {
		return v, fmt.Errorf("%s: %w", what, err)
	}
	return v, nil
}

// validate checks v against s's constraints. A constraint that doesn't fit
// T is check's to report, so it is skipped here.
func (s paramSpec[T]) validate(v T) error {
	if len(s.enum) > 0 && !slices.Contains(s.enum, v) {
		quoted := make([]string, len(s.enum))
		for i, e := range s.enum {
			quoted[i] = formatValue(e)
		}
		return fmt.Errorf("%s isn't one of %s", formatValue(v), strings.Join(quoted, ", "))
	}
	typ := reflect.TypeFor[T]()
	if isString(typ) {
		str := reflect.ValueOf(v).String()
		n := utf8.RuneCountInString(str)
		switch {
		case s.pattern != nil && !s.pattern.MatchString(str):
			return fmt.Errorf("%q doesn't match the pattern %s", str, s.pattern)
		case s.minLength != nil && n < *s.minLength:
			return fmt.Errorf("%q is shorter than %d characters", str, *s.minLength)
		case s.maxLength != nil && n > *s.maxLength:
			return fmt.Errorf("%q is longer than %d characters", str, *s.maxLength)
		}
	}
	if isNumber(typ) {
		switch {
		case s.minimum != nil && compareNumbers(v, *s.minimum) < 0:
			return fmt.Errorf("%s is less than the minimum, %s", formatValue(v), formatValue(*s.minimum))
		case s.maximum != nil && compareNumbers(v, *s.maximum) > 0:
			return fmt.Errorf("%s is greater than the maximum, %s", formatValue(v), formatValue(*s.maximum))
		}
	}
	return nil
}

func isString(typ reflect.Type) bool { return typ.Kind() == reflect.String }

func isNumber(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Int, reflect.Int64, reflect.Float64:
		return true
	default:
		return false
	}
}

// compareNumbers compares a and b, of a number type.
func compareNumbers[T ParamValue](a, b T) int {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Kind() == reflect.Float64 {
		return cmp.Compare(ra.Float(), rb.Float())
	}
	return cmp.Compare(ra.Int(), rb.Int())
}

// formatValue formats v as its underlying type would print, a string
// quoted, so a named type's String method doesn't change an error's text.
func formatValue[T ParamValue](v T) string {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return strconv.Quote(rv.String())
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool())
	case reflect.Int, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	default: // reflect.Float64, the last type ParamValue allows
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64)
	}
}

// parseValue parses s as a T.
func parseValue[T ParamValue](s string) (T, error) {
	var v T
	rv := reflect.ValueOf(&v).Elem()
	switch rv.Kind() {
	case reflect.String:
		rv.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return v, fmt.Errorf("%q isn't a bool", s)
		}
		rv.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, rv.Type().Bits())
		if err != nil {
			return v, fmt.Errorf("%q isn't an integer of %d bits", s, rv.Type().Bits())
		}
		rv.SetInt(n)
	default: // reflect.Float64, the last type ParamValue allows
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return v, fmt.Errorf("%q isn't a number", s)
		}
		rv.SetFloat(f)
	}
	return v, nil
}

// ParamFrom returns p's value in req: the value the request has, or, for
// an optional query or header parameter it doesn't have, the parameter's
// Default or T's zero value. It panics if req's operation doesn't read p,
// naming p and the operation.
func ParamFrom[T ParamValue, B any](req Request[B], p Param[T]) T {
	var op string
	var v any
	var ok bool
	if req.state != nil {
		op = req.state.op
		v, ok = req.state.params[p.paramKey()]
	}
	if !ok {
		panic(fmt.Sprintf("openapi: ParamFrom: operation %s doesn't read %s", opName(op), p.describe()))
	}
	t, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("openapi: ParamFrom: operation %s reads %s as %T, not as %s", opName(op), p.describe(), v, reflect.TypeFor[T]()))
	}
	return t
}

// opName names an operation in a panic. A Request that no API made has no
// operation.
func opName(op string) string {
	if op == "" {
		return "(none)"
	}
	return op
}
