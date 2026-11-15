// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package param parses and checks the parameters of a request, for openapi
// and hypermedia alike: a parameter's value types, parsing a raw value into
// its type and formatting it back, and the constraints a value is checked
// against.
package param

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Value is the type of a parameter's value.
type Value interface {
	~string | ~bool | ~int | ~int64 | ~float64
}

// Constraints are the constraints a parameter's value is checked against.
// A nil or empty field is no constraint.
type Constraints[T Value] struct {
	// Enum is the values the value is one of.
	Enum []T

	// Pattern is a regular expression the value matches. T is a string
	// type.
	Pattern *regexp.Regexp

	// PatternSet reports whether the parameter was given a Pattern, so a
	// nil one is reported by Check rather than read as no constraint.
	PatternSet bool

	// MinLength and MaxLength bound the value's length, in characters. T
	// is a string type.
	MinLength, MaxLength *int

	// Minimum and Maximum bound the value. T is a number type.
	Minimum, Maximum *T
}

// Check returns the mistakes in c: a constraint that doesn't fit T, such as
// Pattern on a number or Minimum on a string, and a Pattern given as nil.
func (c Constraints[T]) Check() []error {
	typ := reflect.TypeFor[T]()
	var errs []error
	if !isString(typ) {
		for _, p := range []struct {
			name string
			set  bool
		}{{"Pattern", c.PatternSet}, {"MinLength", c.MinLength != nil}, {"MaxLength", c.MaxLength != nil}} {
			if p.set {
				errs = append(errs, fmt.Errorf("%s needs a string type, but its type is %s", p.name, typ))
			}
		}
	}
	if !isNumber(typ) {
		for _, p := range []struct {
			name string
			set  bool
		}{{"Minimum", c.Minimum != nil}, {"Maximum", c.Maximum != nil}} {
			if p.set {
				errs = append(errs, fmt.Errorf("%s needs a number type, but its type is %s", p.name, typ))
			}
		}
	}
	if c.PatternSet && c.Pattern == nil {
		errs = append(errs, errors.New("its Pattern is nil"))
	}
	return errs
}

// Parse parses raw as a T, and checks it against c.
func (c Constraints[T]) Parse(raw string) (T, error) {
	v, err := Parse[T](raw)
	if err != nil {
		return v, err
	}
	return v, c.Validate(v)
}

// Validate checks v against c. A constraint that doesn't fit T is Check's
// to report, so it is skipped here.
func (c Constraints[T]) Validate(v T) error {
	if len(c.Enum) > 0 && !slices.Contains(c.Enum, v) {
		quoted := make([]string, len(c.Enum))
		for i, e := range c.Enum {
			quoted[i] = Quote(e)
		}
		return fmt.Errorf("%s isn't one of %s", Quote(v), strings.Join(quoted, ", "))
	}
	typ := reflect.TypeFor[T]()
	if isString(typ) {
		str := reflect.ValueOf(v).String()
		n := utf8.RuneCountInString(str)
		switch {
		case c.Pattern != nil && !c.Pattern.MatchString(str):
			return fmt.Errorf("%q doesn't match the pattern %s", str, c.Pattern)
		case c.MinLength != nil && n < *c.MinLength:
			return fmt.Errorf("%q is shorter than %d characters", str, *c.MinLength)
		case c.MaxLength != nil && n > *c.MaxLength:
			return fmt.Errorf("%q is longer than %d characters", str, *c.MaxLength)
		}
	}
	if isNumber(typ) {
		switch {
		case c.Minimum != nil && compareNumbers(v, *c.Minimum) < 0:
			return fmt.Errorf("%s is less than the minimum, %s", Quote(v), Quote(*c.Minimum))
		case c.Maximum != nil && compareNumbers(v, *c.Maximum) > 0:
			return fmt.Errorf("%s is greater than the maximum, %s", Quote(v), Quote(*c.Maximum))
		}
	}
	return nil
}

// Parse parses s as a T. Its error doesn't name the parameter.
func Parse[T Value](s string) (T, error) {
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
	default: // reflect.Float64, the last type Value allows
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return v, fmt.Errorf("%q isn't a number", s)
		}
		rv.SetFloat(f)
	}
	return v, nil
}

// Format formats v as text that Parse parses back to v, as its underlying
// type would print, so a named type's String method doesn't change it.
func Format[T Value](v T) string {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool())
	case reflect.Int, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	default: // reflect.Float64, the last type Value allows
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64)
	}
}

// Quote formats v as Format does, a string quoted, as an error shows it.
func Quote[T Value](v T) string {
	s := Format(v)
	if reflect.TypeFor[T]().Kind() == reflect.String {
		return strconv.Quote(s)
	}
	return s
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
func compareNumbers[T Value](a, b T) int {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Kind() == reflect.Float64 {
		return cmp.Compare(ra.Float(), rb.Float())
	}
	return cmp.Compare(ra.Int(), rb.Int())
}
