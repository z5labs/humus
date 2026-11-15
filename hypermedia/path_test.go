// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus/hypermedia"
)

// Named types of each kind a parameter can have.
type (
	contactID int
	slug      string
	archived  bool
	cursor    int64
	ratio     float64
)

// String is a String method a link must not use: a value is formatted as
// its underlying type.
func (contactID) String() string { return "not-a-link" }

var (
	contactParam = hypermedia.PathParam[contactID]("contactId").Minimum(1)
	slugParam    = hypermedia.PathParam[slug]("slug")
	fileParam    = hypermedia.PathParam[string]("file")
	contacts     = hypermedia.Root().Lit("contacts")
	contact      = contacts.Param(contactParam)
)

func TestPath_String_gives_the_pattern(t *testing.T) {
	cases := map[string]struct {
		path hypermedia.Path
		want string
	}{
		"root":         {hypermedia.Root(), "/"},
		"zero":         {hypermedia.Path{}, "/"},
		"literal":      {contacts, "/contacts"},
		"parameter":    {contact, "/contacts/{contactId}"},
		"nested":       {contact.Lit("notes").Param(slugParam), "/contacts/{contactId}/notes/{slug}"},
		"rest":         {hypermedia.Root().Lit("static").Rest(fileParam), "/static/{file...}"},
		"rest at root": {hypermedia.Root().Rest(fileParam), "/{file...}"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.path.String())
		})
	}
}

func TestPath_methods_return_a_new_Path(t *testing.T) {
	base := hypermedia.Root().Lit("a")
	b := base.Lit("b")
	c := base.Lit("c")
	d := base.Param(slugParam)
	e := base.Rest(fileParam)

	require.Equal(t, "/a", base.String())
	require.Equal(t, "/a/b", b.String())
	require.Equal(t, "/a/c", c.String())
	require.Equal(t, "/a/{slug}", d.String())
	require.Equal(t, "/a/{file...}", e.String())

	// Growing one path past its prefix's length leaves its siblings alone.
	bb := b.Lit("x")
	bc := b.Lit("y")
	require.Equal(t, "/a/b/x", bb.String())
	require.Equal(t, "/a/b/y", bc.String())
	require.Equal(t, "/a/b", b.String())
}

func TestPath_building_never_panics(t *testing.T) {
	require.NotPanics(t, func() {
		p := hypermedia.Root().
			Lit("").
			Lit("..").
			Lit("a b/c?").
			Param(hypermedia.PathParam[int]("")).
			Param(hypermedia.PathParam[int]("not an identifier")).
			Param(hypermedia.PathParam[int]("n").Pattern(nil).MinLength(-1).MaxLength(-2)).
			Param(hypermedia.PathParam[string]("s").Minimum("z").Maximum("a")).
			Param(hypermedia.PathParam[bool]("b").Enum()).
			Param(hypermedia.PathParam[int]("n")).
			Rest(fileParam).
			Lit("after-rest").
			Rest(hypermedia.PathParameter[string]{})
		_ = p.String()
		_ = hypermedia.QueryParam[int]("")
	})
}

func TestPath_URL_builds_a_link(t *testing.T) {
	notes := contact.Lit("notes").Param(slugParam)
	cases := map[string]struct {
		url  hypermedia.URL
		want string
	}{
		"root":           {hypermedia.Root().URL(), "/"},
		"zero path":      {hypermedia.Path{}.URL(), "/"},
		"literal":        {contacts.URL(), "/contacts"},
		"parameter":      {contact.URL(hypermedia.Arg(contactParam, 7)), "/contacts/7"},
		"two parameters": {notes.URL(hypermedia.Arg(contactParam, 7), hypermedia.Arg(slugParam, "first")), "/contacts/7/notes/first"},
		"bool":           {hypermedia.Root().Param(hypermedia.PathParam[archived]("a")).URL(hypermedia.Arg(hypermedia.PathParam[archived]("a"), true)), "/true"},
		"int64":          {hypermedia.Root().Param(hypermedia.PathParam[cursor]("c")).URL(hypermedia.Arg(hypermedia.PathParam[cursor]("c"), -9007199254740993)), "/-9007199254740993"},
		"float64":        {hypermedia.Root().Param(hypermedia.PathParam[ratio]("r")).URL(hypermedia.Arg(hypermedia.PathParam[ratio]("r"), 0.25)), "/0.25"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.url.String())
		})
	}
}

func TestPath_URL_escapes_each_value_as_a_path_segment(t *testing.T) {
	cases := map[string]struct {
		value slug
		want  string
	}{
		"slash":     {"a/b", "/notes/a%2Fb"},
		"space":     {"a b", "/notes/a%20b"},
		"query":     {"a?b=c", "/notes/a%3Fb=c"},
		"fragment":  {"a#b", "/notes/a%23b"},
		"percent":   {"100%", "/notes/100%25"},
		"plus":      {"a+b", "/notes/a+b"},
		"unicode":   {"café", "/notes/caf%C3%A9"},
		"empty":     {"", "/notes/"},
		"plain":     {"a-b_c.d~e", "/notes/a-b_c.d~e"},
		"backslash": {`a\b`, "/notes/a%5Cb"},
	}
	notes := hypermedia.Root().Lit("notes").Param(slugParam)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, notes.URL(hypermedia.Arg(slugParam, tc.value)).String())
		})
	}
}

func TestPath_URL_takes_Args_in_any_order(t *testing.T) {
	p := hypermedia.Root().Param(contactParam).Lit("x").Param(slugParam).Rest(fileParam)
	a := hypermedia.Arg(contactParam, 3)
	b := hypermedia.Arg(slugParam, "s")
	c := hypermedia.Arg(fileParam, "f/g")
	const want = "/3/x/s/f/g"
	for _, args := range [][]hypermedia.PathArg{
		{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a},
	} {
		require.Equal(t, want, p.URL(args...).String())
	}
}

func TestPath_URL_formats_a_named_type_as_its_underlying_type(t *testing.T) {
	require.Equal(t, "/contacts/4", contact.URL(hypermedia.Arg(contactParam, 4)).String())
}

func TestPath_URL_does_not_check_constraints(t *testing.T) {
	bounded := hypermedia.PathParam[int]("n").Minimum(1).Maximum(10)
	short := hypermedia.PathParam[string]("s").MaxLength(1).Pattern(regexp.MustCompile(`^[a-z]$`)).Enum("a")
	p := hypermedia.Root().Param(bounded).Param(short)
	require.Equal(t, "/-5/TOO-LONG", p.URL(hypermedia.Arg(bounded, -5), hypermedia.Arg(short, "TOO-LONG")).String())
	require.Equal(t, "/contacts/0", contact.URL(hypermedia.Arg(contactParam, 0)).String())
}

func TestPath_URL_matches_an_Arg_by_its_parameters_name_and_type(t *testing.T) {
	// A copy with other constraints is the same parameter, so its Arg fits.
	unbounded := hypermedia.PathParam[contactID]("contactId")
	require.Equal(t, "/contacts/2", contact.URL(hypermedia.Arg(unbounded, 2)).String())
}

func TestPath_URL_panics_for_a_missing_repeated_or_extra_Arg(t *testing.T) {
	notes := contact.Lit("notes").Param(slugParam)
	cases := map[string]struct {
		build func() hypermedia.URL
		want  string
	}{
		"none": {
			func() hypermedia.URL { return contact.URL() },
			`hypermedia: link to /contacts/{contactId}: no Arg for path parameter "contactId"`,
		},
		"missing": {
			func() hypermedia.URL { return notes.URL(hypermedia.Arg(contactParam, 1)) },
			`hypermedia: link to /contacts/{contactId}/notes/{slug}: no Arg for path parameter "slug"`,
		},
		"repeated": {
			func() hypermedia.URL {
				return contact.URL(hypermedia.Arg(contactParam, 1), hypermedia.Arg(contactParam, 2))
			},
			`hypermedia: link to /contacts/{contactId}: more than one Arg for path parameter "contactId"`,
		},
		"extra": {
			func() hypermedia.URL {
				return contact.URL(hypermedia.Arg(contactParam, 1), hypermedia.Arg(slugParam, "x"))
			},
			`hypermedia: link to /contacts/{contactId}: an Arg for path parameter "slug", which the path doesn't have`,
		},
		"extra on a path without parameters": {
			func() hypermedia.URL { return contacts.URL(hypermedia.Arg(slugParam, "x")) },
			`hypermedia: link to /contacts: an Arg for path parameter "slug", which the path doesn't have`,
		},
		"another type": {
			func() hypermedia.URL {
				return contact.URL(hypermedia.Arg(hypermedia.PathParam[int]("contactId"), 1))
			},
			`hypermedia: link to /contacts/{contactId}: the Arg for path parameter "contactId" has type int, but the parameter has type hypermedia_test.contactID`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.PanicsWithValue(t, tc.want, func() { tc.build() })
		})
	}
}

func TestPath_URL_keeps_a_Rest_values_slashes(t *testing.T) {
	static := hypermedia.Root().Lit("static").Rest(fileParam)
	cases := map[string]struct {
		value string
		want  string
	}{
		"one segment":         {"app.js", "/static/app.js"},
		"nested":              {"css/site/main.css", "/static/css/site/main.css"},
		"each segment escape": {"my dir/a?b#c%.txt", "/static/my%20dir/a%3Fb%23c%25.txt"},
		"empty":               {"", "/static/"},
		"trailing slash":      {"css/", "/static/css/"},
		"leading slash":       {"/css", "/static//css"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, static.URL(hypermedia.Arg(fileParam, tc.value)).String())
		})
	}

	type page string
	wiki := hypermedia.PathParam[page]("page")
	require.Equal(t, "/wiki/Go/Generics", hypermedia.Root().Lit("wiki").Rest(wiki).URL(hypermedia.Arg(wiki, "Go/Generics")).String())
}

func TestURL_With_adds_a_query_parameter_escaped(t *testing.T) {
	page := hypermedia.QueryParam[int]("page")
	q := hypermedia.QueryParam[string]("q")
	sort := hypermedia.QueryParam[string]("sort order")
	desc := hypermedia.QueryParam[bool]("desc")
	cases := map[string]struct {
		url  hypermedia.URL
		want string
	}{
		"none":     {contacts.URL(), "/contacts"},
		"one":      {contacts.URL().With(page, 2), "/contacts?page=2"},
		"in order": {contacts.URL().With(q, "x").With(page, 2).With(desc, true), "/contacts?q=x&page=2&desc=true"},
		"escaped":  {contacts.URL().With(q, "a b&c=d/e?f#g%+é"), "/contacts?q=a+b%26c%3Dd%2Fe%3Ff%23g%25%2B%C3%A9"},
		"name":     {contacts.URL().With(sort, "name"), "/contacts?sort+order=name"},
		"empty":    {contacts.URL().With(q, ""), "/contacts?q="},
		"replaced": {contacts.URL().With(page, 1).With(q, "x").With(page, 3), "/contacts?page=3&q=x"},
		"on a link with parameters": {
			contact.URL(hypermedia.Arg(contactParam, 7)).With(page, 2),
			"/contacts/7?page=2",
		},
		"named type": {
			contacts.URL().With(hypermedia.QueryParam[contactID]("id"), 5),
			"/contacts?id=5",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.url.String())
		})
	}
}

func TestURL_With_leaves_the_URL_it_was_called_on_unchanged(t *testing.T) {
	page := hypermedia.QueryParam[int]("page")
	q := hypermedia.QueryParam[string]("q")
	base := contacts.URL().With(q, "x")
	first := base.With(page, 1)
	second := base.With(page, 2)
	replaced := base.With(q, "y")

	require.Equal(t, "/contacts?q=x", base.String())
	require.Equal(t, "/contacts?q=x&page=1", first.String())
	require.Equal(t, "/contacts?q=x&page=2", second.String())
	require.Equal(t, "/contacts?q=y", replaced.String())
}
