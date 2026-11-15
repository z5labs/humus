// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package param_test

import (
	"math"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/internal/param"
)

// Named types of each kind a parameter can have, each with a String method
// that Format and Quote must not use.
type (
	include string
	flag    bool
	page    int
	cursor  int64
	ratio   float64
)

func (include) String() string { return "include!" }
func (flag) String() string    { return "flag!" }
func (page) String() string    { return "page!" }
func (cursor) String() string  { return "cursor!" }
func (ratio) String() string   { return "ratio!" }

func ptr[T any](v T) *T { return &v }

func TestParse_parses_each_type(t *testing.T) {
	t.Run("string", func(t *testing.T) {
		v, err := param.Parse[include]("green tea")
		require.NoError(t, err)
		require.Equal(t, include("green tea"), v)
	})
	t.Run("bool", func(t *testing.T) {
		v, err := param.Parse[flag]("1")
		require.NoError(t, err)
		require.Equal(t, flag(true), v)
	})
	t.Run("int", func(t *testing.T) {
		v, err := param.Parse[page]("-3")
		require.NoError(t, err)
		require.Equal(t, page(-3), v)
	})
	t.Run("int64", func(t *testing.T) {
		v, err := param.Parse[cursor]("9000000000")
		require.NoError(t, err)
		require.Equal(t, cursor(9000000000), v)
	})
	t.Run("float64", func(t *testing.T) {
		v, err := param.Parse[ratio]("1.5")
		require.NoError(t, err)
		require.Equal(t, ratio(1.5), v)
	})
}

func TestParse_reports_a_value_that_doesnt_parse(t *testing.T) {
	_, err := param.Parse[bool]("maybe")
	require.EqualError(t, err, `"maybe" isn't a bool`)

	_, err = param.Parse[int64]("ten")
	require.EqualError(t, err, `"ten" isn't an integer of 64 bits`)

	_, err = param.Parse[int64]("99999999999999999999")
	require.EqualError(t, err, `"99999999999999999999" isn't an integer of 64 bits`)

	_, err = param.Parse[float64]("half")
	require.EqualError(t, err, `"half" isn't a number`)
}

func TestFormat_formats_a_value_that_Parse_parses_back(t *testing.T) {
	require.Equal(t, "green tea", param.Format(include("green tea")))
	require.Equal(t, "true", param.Format(flag(true)))
	require.Equal(t, "-3", param.Format(page(-3)))
	require.Equal(t, "9000000000", param.Format(cursor(9000000000)))
	require.Equal(t, "0.1", param.Format(ratio(0.1)))

	for _, f := range []float64{0.1, 1e21, -2.5e-8, math.MaxFloat64, math.SmallestNonzeroFloat64} {
		v, err := param.Parse[float64](param.Format(f))
		require.NoError(t, err)
		require.Equal(t, f, v)
	}
	for _, n := range []int64{math.MinInt64, math.MaxInt64} {
		v, err := param.Parse[int64](param.Format(n))
		require.NoError(t, err)
		require.Equal(t, n, v)
	}
}

func TestQuote_quotes_a_string_and_only_a_string(t *testing.T) {
	require.Equal(t, `"a \"b\""`, param.Quote(include(`a "b"`)))
	require.Equal(t, "false", param.Quote(flag(false)))
	require.Equal(t, "7", param.Quote(page(7)))
	require.Equal(t, "7", param.Quote(cursor(7)))
	require.Equal(t, "NaN", param.Quote(ratio(math.NaN())))
}

func TestConstraints_Validate_checks_a_value_against_each_constraint(t *testing.T) {
	sku := param.Constraints[string]{Pattern: regexp.MustCompile(`^[A-Z]{3}-[0-9]+$`), PatternSet: true}
	name := param.Constraints[string]{MinLength: ptr(2), MaxLength: ptr(5)}
	limit := param.Constraints[int]{Minimum: ptr(1), Maximum: ptr(100)}
	r := param.Constraints[float64]{Minimum: ptr(-1.0), Maximum: ptr(1.0)}
	mode := param.Constraints[include]{Enum: []include{"fast", "slow"}}
	pages := param.Constraints[page]{Enum: []page{1, 2}}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"a value not in Enum", mode.Validate("turbo"), `"turbo" isn't one of "fast", "slow"`},
		{"a number not in Enum", pages.Validate(3), `3 isn't one of 1, 2`},
		{"a value that doesn't match Pattern", sku.Validate("abc"), `"abc" doesn't match the pattern ^[A-Z]{3}-[0-9]+$`},
		{"a value under MinLength", name.Validate("a"), `"a" is shorter than 2 characters`},
		{"a value over MaxLength", name.Validate("abcdef"), `"abcdef" is longer than 5 characters`},
		{"a value under Minimum", limit.Validate(0), `0 is less than the minimum, 1`},
		{"a value over Maximum", limit.Validate(101), `101 is greater than the maximum, 100`},
		{"a number under Minimum", r.Validate(-1.5), `-1.5 is less than the minimum, -1`},
		{"a number over Maximum", r.Validate(1.5), `1.5 is greater than the maximum, 1`},
	}
	for _, tc := range cases {
		require.EqualError(t, tc.err, tc.want, tc.name)
	}

	require.NoError(t, mode.Validate("slow"))
	require.NoError(t, pages.Validate(2))
	require.NoError(t, sku.Validate("ABC-1"))
	require.NoError(t, name.Validate("ab"))
	require.NoError(t, name.Validate("héllo"), "a length counts characters, not bytes")
	require.NoError(t, limit.Validate(1))
	require.NoError(t, limit.Validate(100))
	require.NoError(t, r.Validate(1))
	require.NoError(t, param.Constraints[bool]{}.Validate(true))
}

func TestConstraints_Validate_skips_a_constraint_that_doesnt_fit_its_type(t *testing.T) {
	c := param.Constraints[bool]{Pattern: regexp.MustCompile(`^x$`), PatternSet: true, MinLength: ptr(9), Minimum: ptr(true)}
	require.NoError(t, c.Validate(false))

	n := param.Constraints[int]{Pattern: regexp.MustCompile(`^x$`), PatternSet: true, MaxLength: ptr(0)}
	require.NoError(t, n.Validate(42))

	s := param.Constraints[string]{Minimum: ptr("m"), Maximum: ptr("n")}
	require.NoError(t, s.Validate("z"))
}

func TestConstraints_Parse_parses_and_validates(t *testing.T) {
	limit := param.Constraints[int]{Minimum: ptr(1)}

	v, err := limit.Parse("5")
	require.NoError(t, err)
	require.Equal(t, 5, v)

	_, err = limit.Parse("ten")
	require.EqualError(t, err, `"ten" isn't an integer of 64 bits`)

	v, err = limit.Parse("0")
	require.EqualError(t, err, `0 is less than the minimum, 1`)
	require.Equal(t, 0, v, "the value parsed is returned with its error")
}

func TestConstraints_Check_reports_a_constraint_that_doesnt_fit_its_type(t *testing.T) {
	re := regexp.MustCompile(`^x$`)

	t.Run("a string constraint on a number", func(t *testing.T) {
		c := param.Constraints[cursor]{Pattern: re, PatternSet: true, MinLength: ptr(1), MaxLength: ptr(2)}
		require.Equal(t, []string{
			"Pattern needs a string type, but its type is param_test.cursor",
			"MinLength needs a string type, but its type is param_test.cursor",
			"MaxLength needs a string type, but its type is param_test.cursor",
		}, messages(c.Check()))
	})

	t.Run("a number constraint on a string", func(t *testing.T) {
		c := param.Constraints[include]{Minimum: ptr[include]("a"), Maximum: ptr[include]("z")}
		require.Equal(t, []string{
			"Minimum needs a number type, but its type is param_test.include",
			"Maximum needs a number type, but its type is param_test.include",
		}, messages(c.Check()))
	})

	t.Run("every constraint on a bool", func(t *testing.T) {
		c := param.Constraints[bool]{Pattern: re, PatternSet: true, Minimum: ptr(false)}
		require.Equal(t, []string{
			"Pattern needs a string type, but its type is bool",
			"Minimum needs a number type, but its type is bool",
		}, messages(c.Check()))
	})

	t.Run("a nil Pattern", func(t *testing.T) {
		c := param.Constraints[string]{PatternSet: true}
		require.Equal(t, []string{"its Pattern is nil"}, messages(c.Check()))
	})

	t.Run("constraints that fit", func(t *testing.T) {
		require.Empty(t, param.Constraints[string]{Pattern: re, PatternSet: true, MinLength: ptr(1), MaxLength: ptr(2), Enum: []string{"x"}}.Check())
		require.Empty(t, param.Constraints[int]{Minimum: ptr(1), Maximum: ptr(2)}.Check())
		require.Empty(t, param.Constraints[float64]{Minimum: ptr(0.5)}.Check())
		require.Empty(t, param.Constraints[bool]{Enum: []bool{true}}.Check())
	})
}

func messages(errs []error) []string {
	var ms []string
	for _, err := range errs {
		ms = append(ms, err.Error())
	}
	return ms
}
