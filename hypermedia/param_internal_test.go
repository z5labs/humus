// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

type contactID int

func TestPathParameter_constraints_return_a_copy(t *testing.T) {
	base := PathParam[int]("n")
	values := []int{1, 2}
	small := base.Maximum(10).Enum(values...)
	large := base.Maximum(100)
	floor := large.Minimum(50)
	values[0] = 50 // Enum keeps its own copy of its values.

	_, err := small.parse("50")
	require.EqualError(t, err, `path parameter "n": 50 isn't one of 1, 2`)
	v, err := small.parse("2")
	require.NoError(t, err)
	require.Equal(t, 2, v)

	v, err = large.parse("50")
	require.NoError(t, err)
	require.Equal(t, 50, v)
	_, err = large.parse("101")
	require.EqualError(t, err, `path parameter "n": 101 is greater than the maximum, 100`)
	v, err = large.parse("49")
	require.NoError(t, err, "Minimum on floor leaves large unchanged")
	require.Equal(t, 49, v)

	_, err = floor.parse("49")
	require.EqualError(t, err, `path parameter "n": 49 is less than the minimum, 50`)

	v, err = base.parse("5000")
	require.NoError(t, err, "base has no constraints")
	require.Equal(t, 5000, v)

	name := PathParam[string]("s")
	short := name.MaxLength(2)
	long := name.MinLength(3)
	letters := name.Pattern(regexp.MustCompile(`^[a-z]+$`))
	_, err = short.parse("abc")
	require.EqualError(t, err, `path parameter "s": "abc" is longer than 2 characters`)
	_, err = long.parse("ab")
	require.EqualError(t, err, `path parameter "s": "ab" is shorter than 3 characters`)
	_, err = letters.parse("A1")
	require.EqualError(t, err, `path parameter "s": "A1" doesn't match the pattern ^[a-z]+$`)
	for _, p := range []PathParameter[string]{name, short, long, letters} {
		_, err := p.parse("a")
		if p.constraints.MinLength != nil {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
	}
}

func TestPathParameter_parse_parses_its_type(t *testing.T) {
	v, err := PathParam[contactID]("id").parse("7")
	require.NoError(t, err)
	require.Equal(t, contactID(7), v)

	_, err = PathParam[contactID]("id").parse("seven")
	require.EqualError(t, err, `path parameter "id": "seven" isn't an integer of 64 bits`)
}

func TestPathParameter_check_reports_a_constraint_that_doesnt_fit_its_type(t *testing.T) {
	require.Empty(t, PathParam[int]("n").Minimum(1).Maximum(2).Enum(1).check())
	require.Empty(t, PathParam[string]("s").MinLength(1).MaxLength(2).Pattern(regexp.MustCompile(`x`)).check())

	errs := PathParam[int]("n").Pattern(regexp.MustCompile(`x`)).MinLength(1).check()
	require.Len(t, errs, 2)
	require.EqualError(t, errs[0], `path parameter "n": Pattern needs a string type, but its type is int`)
	require.EqualError(t, errs[1], `path parameter "n": MinLength needs a string type, but its type is int`)

	errs = PathParam[string]("s").Minimum("a").check()
	require.Len(t, errs, 1)
	require.EqualError(t, errs[0], `path parameter "s": Minimum needs a number type, but its type is string`)

	errs = PathParam[string]("s").Pattern(nil).check()
	require.Len(t, errs, 1)
	require.EqualError(t, errs[0], `path parameter "s": its Pattern is nil`)
}

func TestPath_segments_keep_their_parameters(t *testing.T) {
	id := PathParam[contactID]("contactId").Minimum(1)
	file := PathParam[string]("file").Pattern(nil)
	p := Root().Lit("contacts").Param(id).Rest(file)
	require.Len(t, p.segs, 3)

	seg := p.segs[1].param
	require.Equal(t, "contactId", seg.name)
	require.False(t, seg.rest)
	require.Empty(t, seg.check())
	v, err := seg.parse("3")
	require.NoError(t, err)
	require.Equal(t, contactID(3), v)
	_, err = seg.parse("0")
	require.EqualError(t, err, `path parameter "contactId": 0 is less than the minimum, 1`)

	rest := p.segs[2].param
	require.Equal(t, "file", rest.name)
	require.True(t, rest.rest)
	require.Len(t, rest.check(), 1)
}
