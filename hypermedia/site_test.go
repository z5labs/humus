// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia_test

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/hypermedia"
)

// site is a Site under test, which records each error its ErrorPage
// answers.
type site struct {
	t    *testing.T
	h    *hypermedia.Site
	errs []error
}

// newSite returns the Site that serves routes, with an ErrorPage that
// writes the error's Status, or 500, and its text.
func newSite(t *testing.T, routes ...hypermedia.Route) *site {
	t.Helper()
	s := &site{t: t}
	h, err := hypermedia.NewSite(&humus.Setup{}, func(w http.ResponseWriter, _ *http.Request, err error) error {
		s.errs = append(s.errs, err)
		status := http.StatusInternalServerError
		if re, ok := errors.AsType[*hypermedia.RequestError](err); ok {
			status = re.Status
		}
		w.WriteHeader(status)
		_, werr := fmt.Fprintf(w, "error page: %v", err)
		return werr
	}, routes)
	require.NoError(t, err)
	s.h = h
	return s
}

// do serves a request of method for target.
func (s *site) do(method, target string) *httptest.ResponseRecorder {
	s.t.Helper()
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

// requestError returns the one error the ErrorPage answered since the
// last call, as the *RequestError it must be.
func (s *site) requestError() *hypermedia.RequestError {
	s.t.Helper()
	require.Len(s.t, s.errs, 1, "the ErrorPage answers once")
	re, ok := errors.AsType[*hypermedia.RequestError](s.errs[0])
	require.True(s.t, ok, "the error is a %T: %v", s.errs[0], s.errs[0])
	s.errs = nil
	return re
}

// says returns a Handler that writes body.
func says(body string) hypermedia.Handler {
	return func(w http.ResponseWriter, _ *http.Request) error {
		_, err := io.WriteString(w, body)
		return err
	}
}

func TestNewSite_returns_one_error_naming_every_mistake(t *testing.T) {
	id := hypermedia.PathParam[int]("id")
	name := hypermedia.PathParam[string]("name")
	file := hypermedia.PathParam[string]("file")
	contacts := hypermedia.Root().Lit("contacts")
	routes := []hypermedia.Route{
		{},
		hypermedia.NewRoute("GET POST", contacts, says("")),
		hypermedia.NewRoute("", contacts.Lit("all"), says("")),
		hypermedia.NewRoute(http.MethodGet, contacts.Lit("new"), nil),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("a b"), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit(""), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit(".."), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("x").Param(hypermedia.PathParam[int]("contact-id")), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("n").Param(id.Pattern(regexp.MustCompile(`^\d+$`))), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("s").Param(name.Minimum("a")), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("p").Param(name.Pattern(nil)), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("twice").Param(id).Lit("and").Param(id), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("static").Rest(file).Lit("raw"), says("")),
		hypermedia.NewRoute(http.MethodGet, contacts.Param(id), says("")),
		hypermedia.NewRoute(http.MethodGet, contacts.Param(id), says("")),
		hypermedia.NewRoute(http.MethodGet, contacts.Param(hypermedia.PathParam[int]("contactId")), says("")),
	}

	_, err := hypermedia.NewSite(nil, nil, routes)
	require.Error(t, err)
	t.Log(err)
	for _, want := range []string{
		"hypermedia: NewSite: ",
		"s is nil",
		"errorPage is nil",
		"route 0 is the zero Route",
		`route GET POST /contacts: method "GET POST" isn't a token`,
		`route  /contacts/all: method "" isn't a token`,
		"route GET /contacts/new: its Handler is nil",
		`path /a b: literal "a b" holds ' '`,
		"path /: a literal is empty",
		`path /..: literal ".." is a dot segment`,
		`path /x/{contact-id}: path parameter "contact-id" isn't an ASCII Go identifier`,
		`path /n/{id}: path parameter "id": Pattern needs a string type, but its type is int`,
		`path /s/{name}: path parameter "name": Minimum needs a number type, but its type is string`,
		`path /p/{name}: path parameter "name": its Pattern is nil`,
		`path /twice/{id}/and/{id}: path parameter "id" appears more than once`,
		`path /static/{file...}/raw: path parameter "file" is a Rest, but isn't the path's last segment`,
		"two routes are at GET /contacts/{id}",
		"paths /contacts/{id} and /contacts/{contactId} differ only in their parameters' names",
	} {
		require.ErrorContains(t, err, want)
	}
}

func TestNewSite_checks_a_path_once_for_its_routes(t *testing.T) {
	bad := hypermedia.Root().Lit("")
	_, err := hypermedia.NewSite(&humus.Setup{}, func(http.ResponseWriter, *http.Request, error) error { return nil }, []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, bad, says("")),
		hypermedia.NewRoute(http.MethodPost, bad, says("")),
	})
	require.EqualError(t, err, "hypermedia: NewSite: path /: a literal is empty")
}

func TestNewSite_reports_routes_a_ServeMux_cant_tell_apart(t *testing.T) {
	a := hypermedia.PathParam[string]("a")
	k := hypermedia.PathParam[string]("k")
	_, err := hypermedia.NewSite(&humus.Setup{}, func(http.ResponseWriter, *http.Request, error) error { return nil }, []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("c").Param(a), says("")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Param(k).Lit("d"), says("")),
	})
	require.ErrorContains(t, err, "hypermedia: NewSite: route GET /{k}/d: ")
	require.ErrorContains(t, err, "conflicts with")
}

func TestSite_serves_each_route(t *testing.T) {
	s := newSite(t,
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root(), says("home")),
		hypermedia.NewRoute(http.MethodGet, List, says("list")),
		hypermedia.NewRoute(http.MethodPost, List, says("created")),
		hypermedia.NewRoute("QUERY", List, says("searched")),
		hypermedia.NewRoute(http.MethodGet, Edit, says("edit")),
	)
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/", "home"},
		{http.MethodGet, "/contacts", "list"},
		{http.MethodPost, "/contacts", "created"},
		{"QUERY", "/contacts", "searched"},
		{http.MethodGet, "/contacts/7/edit", "edit"},
	} {
		w := s.do(tc.method, tc.target)
		require.Equal(t, http.StatusOK, w.Code, "%s %s", tc.method, tc.target)
		require.Equal(t, tc.body, w.Body.String(), "%s %s", tc.method, tc.target)
	}
	require.Empty(t, s.errs)
}

func TestSite_serves_HEAD_on_a_GET_route(t *testing.T) {
	var method string
	s := newSite(t, hypermedia.NewRoute(http.MethodGet, One, func(w http.ResponseWriter, r *http.Request) error {
		method = r.Method
		require.Equal(t, ID(7), hypermedia.ParamFrom(r, ContactID))
		return nil
	}))
	w := s.do(http.MethodHead, "/contacts/7")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, http.MethodHead, method)
	require.Empty(t, s.errs)
}

func TestSite_answers_a_path_no_route_serves_with_a_404(t *testing.T) {
	s := newSite(t,
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root(), says("home")),
		hypermedia.NewRoute(http.MethodGet, One, says("one")),
	)
	for _, target := range []string{"/orders", "/contacts", "/contacts/7/edit", "/contacts/7/"} {
		w := s.do(http.MethodGet, target)
		require.Equal(t, http.StatusNotFound, w.Code, target)
		re := s.requestError()
		require.Equal(t, http.StatusNotFound, re.Status, target)
		require.Empty(t, re.Param, target)
		require.EqualError(t, re, "hypermedia: request rejected with 404 Not Found: no route serves "+target)
		require.Empty(t, w.Header().Get("Allow"), target)
	}
}

func TestSite_answers_a_value_that_doesnt_parse_or_breaks_a_constraint_with_a_404(t *testing.T) {
	type kind string
	number := hypermedia.PathParam[int]("n").Minimum(1).Maximum(10)
	price := hypermedia.PathParam[float64]("price").Minimum(0.5)
	flag := hypermedia.PathParam[bool]("flag")
	big := hypermedia.PathParam[int64]("big")
	k := hypermedia.PathParam[kind]("kind").Enum("a", "b")
	code := hypermedia.PathParam[string]("code").Pattern(regexp.MustCompile(`^[a-z]+$`)).MinLength(2).MaxLength(4)
	s := newSite(t,
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("n").Param(number), says("n")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("price").Param(price), says("price")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("flag").Param(flag), says("flag")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("big").Param(big), says("big")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("kind").Param(k), says("kind")),
		hypermedia.NewRoute(http.MethodGet, hypermedia.Root().Lit("code").Param(code), says("code")),
		hypermedia.NewRoute(http.MethodGet, One, says("one")),
	)
	for _, tc := range []struct{ target, param, err string }{
		{"/contacts/0", "contactId", `path parameter "contactId": 0 is less than the minimum, 1`},
		{"/n/11", "n", `path parameter "n": 11 is greater than the maximum, 10`},
		{"/n/seven", "n", `path parameter "n": "seven" isn't an integer of 64 bits`},
		{"/price/0.25", "price", `path parameter "price": 0.25 is less than the minimum, 0.5`},
		{"/price/cheap", "price", `path parameter "price": "cheap" isn't a number`},
		{"/flag/maybe", "flag", `path parameter "flag": "maybe" isn't a bool`},
		{"/big/99999999999999999999", "big", `path parameter "big": "99999999999999999999" isn't an integer of 64 bits`},
		{"/kind/c", "kind", `path parameter "kind": "c" isn't one of "a", "b"`},
		{"/code/AB", "code", `path parameter "code": "AB" doesn't match the pattern ^[a-z]+$`},
		{"/code/a", "code", `path parameter "code": "a" is shorter than 2 characters`},
		{"/code/abcde", "code", `path parameter "code": "abcde" is longer than 4 characters`},
	} {
		w := s.do(http.MethodGet, tc.target)
		require.Equal(t, http.StatusNotFound, w.Code, tc.target)
		re := s.requestError()
		require.Equal(t, http.StatusNotFound, re.Status, tc.target)
		require.Equal(t, tc.param, re.Param, tc.target)
		require.EqualError(t, re.Err, tc.err, tc.target)
	}
	for _, target := range []string{"/contacts/1", "/n/10", "/price/0.5", "/flag/true", "/big/9223372036854775807", "/kind/b", "/code/abcd"} {
		w := s.do(http.MethodGet, target)
		require.Equal(t, http.StatusOK, w.Code, target)
	}
	require.Empty(t, s.errs)
}

func TestSite_answers_a_method_its_path_doesnt_have_with_a_405(t *testing.T) {
	changes := List.Lit("changes")
	s := newSite(t,
		hypermedia.NewRoute(http.MethodGet, List, says("list")),
		hypermedia.NewRoute("QUERY", hypermedia.Root().Lit("search"), says("searched")),
		hypermedia.NewRoute(http.MethodGet, changes, says("changes")),
		hypermedia.NewRoute(http.MethodGet, One, says("one")),
		hypermedia.NewRoute(http.MethodPut, One, says("updated")),
		hypermedia.NewRoute(http.MethodGet, Static, says("asset")),
	)
	for _, tc := range []struct {
		name, method, target, allow string
	}{
		{"a GET route's path allows GET and HEAD", http.MethodPost, "/contacts", "GET, HEAD"},
		{"a QUERY route's path allows QUERY", http.MethodPost, "/search", "QUERY"},
		{"a QUERY to a GET route's path", "QUERY", "/contacts", "GET, HEAD"},
		{"a GET to a QUERY route's path", http.MethodGet, "/search", "QUERY"},
		{"a path served by routes of two methods", http.MethodDelete, "/contacts/7", "GET, HEAD, PUT"},
		{"a literal beside a sibling wildcard whose value doesn't parse", http.MethodPut, "/contacts/changes", "GET, HEAD"},
		{"a Rest route", http.MethodPost, "/static/css/main.css", "GET, HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.errs = nil
			w := s.do(tc.method, tc.target)
			require.Equal(t, http.StatusMethodNotAllowed, w.Code)
			require.Equal(t, tc.allow, w.Header().Get("Allow"))
			re := s.requestError()
			require.Equal(t, http.StatusMethodNotAllowed, re.Status)
			require.Empty(t, re.Param)
			require.ErrorContains(t, re, tc.method+" isn't a method of "+tc.target+", which allows "+tc.allow)
		})
	}

	t.Run("a wildcard whose value doesn't parse isn't counted", func(t *testing.T) {
		s.errs = nil
		w := s.do(http.MethodDelete, "/contacts/0")
		require.Equal(t, http.StatusNotFound, w.Code)
		require.Empty(t, w.Header().Get("Allow"))
		re := s.requestError()
		require.Equal(t, http.StatusNotFound, re.Status)

		w = s.do(http.MethodPut, "/contacts/abc")
		require.Equal(t, http.StatusNotFound, w.Code, "only GET and PUT match, and neither parses abc")
		require.Empty(t, w.Header().Get("Allow"))
		re = s.requestError()
		require.Equal(t, "contactId", re.Param)
	})

	t.Run("a literal beside a sibling wildcard is served", func(t *testing.T) {
		s.errs = nil
		require.Equal(t, "changes", s.do(http.MethodGet, "/contacts/changes").Body.String())
		require.Equal(t, "updated", s.do(http.MethodPut, "/contacts/7").Body.String())
		require.Empty(t, s.errs)
	})
}

func TestSite_serves_a_Rest_route(t *testing.T) {
	var got []string
	s := newSite(t, hypermedia.NewRoute(http.MethodGet, Static, func(_ http.ResponseWriter, r *http.Request) error {
		got = append(got, hypermedia.ParamFrom(r, Asset))
		return nil
	}))
	for _, target := range []string{
		"/static/css/main.css",
		"/static/",
		Static.URL(hypermedia.Arg(Asset, "css/main page.css")).String(),
		Static.URL(hypermedia.Arg(Asset, "")).String(),
	} {
		require.Equal(t, http.StatusOK, s.do(http.MethodGet, target).Code, target)
	}
	require.Equal(t, []string{"css/main.css", "", "css/main page.css", ""}, got)

	w := s.do(http.MethodPut, "/static/css/main.css")
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
	require.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
}

func TestSite_gives_the_handler_the_values_of_the_link_it_built(t *testing.T) {
	type values struct {
		s   string
		b   bool
		i   int
		i64 int64
		f   float64
		id  ID
		r   string
	}
	var (
		s   = hypermedia.PathParam[string]("s")
		b   = hypermedia.PathParam[bool]("b")
		i   = hypermedia.PathParam[int]("i")
		i64 = hypermedia.PathParam[int64]("i64")
		f   = hypermedia.PathParam[float64]("f")
		id  = hypermedia.PathParam[ID]("id")
		r   = hypermedia.PathParam[string]("r")

		qs   = hypermedia.QueryParam[string]("qs")
		qb   = hypermedia.QueryParam[bool]("qb")
		qi   = hypermedia.QueryParam[int]("qi")
		qi64 = hypermedia.QueryParam[int64]("qi64")
		qf   = hypermedia.QueryParam[float64]("qf")
		qid  = hypermedia.QueryParam[ID]("qid")

		p = hypermedia.Root().Lit("v").Param(s).Param(b).Param(i).Param(i64).Param(f).Param(id).Lit("files").Rest(r)
	)
	var gotPath, gotQuery values
	site := newSite(t, hypermedia.NewRoute(http.MethodGet, p, func(_ http.ResponseWriter, req *http.Request) error {
		gotPath = values{
			s:   hypermedia.ParamFrom(req, s),
			b:   hypermedia.ParamFrom(req, b),
			i:   hypermedia.ParamFrom(req, i),
			i64: hypermedia.ParamFrom(req, i64),
			f:   hypermedia.ParamFrom(req, f),
			id:  hypermedia.ParamFrom(req, id),
			r:   hypermedia.ParamFrom(req, r),
		}
		var err error
		read := func(e error) {
			if err == nil {
				err = e
			}
		}
		var v values
		v.s, err = hypermedia.QueryFrom(req, qs)
		var e error
		v.b, e = hypermedia.QueryFrom(req, qb)
		read(e)
		v.i, e = hypermedia.QueryFrom(req, qi)
		read(e)
		v.i64, e = hypermedia.QueryFrom(req, qi64)
		read(e)
		v.f, e = hypermedia.QueryFrom(req, qf)
		read(e)
		v.id, e = hypermedia.QueryFrom(req, qid)
		read(e)
		gotQuery = v
		return err
	}))

	want := values{s: "a/b c?#%&+", b: true, i: -7, i64: math.MaxInt64, f: 1e21, id: 42, r: "css/main page.css"}
	link := p.URL(
		hypermedia.Arg(s, want.s),
		hypermedia.Arg(b, want.b),
		hypermedia.Arg(i, want.i),
		hypermedia.Arg(i64, want.i64),
		hypermedia.Arg(f, want.f),
		hypermedia.Arg(id, want.id),
		hypermedia.Arg(r, want.r),
	).With(qs, want.s).With(qb, want.b).With(qi, want.i).With(qi64, want.i64).With(qf, want.f).With(qid, want.id)
	t.Log(link)

	w := site.do(http.MethodGet, link.String())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Empty(t, site.errs)
	require.Equal(t, want, gotPath)
	want.r = ""
	require.Equal(t, want, gotQuery)
}

func TestParamFrom_panics_for_a_parameter_the_route_doesnt_have(t *testing.T) {
	other := hypermedia.PathParam[ID]("otherId")
	retyped := hypermedia.PathParam[string]("contactId")
	var panics []any
	s := newSite(t, hypermedia.NewRoute(http.MethodPut, One, func(_ http.ResponseWriter, r *http.Request) error {
		for _, f := range []func(){
			func() { hypermedia.ParamFrom(r, other) },
			func() { hypermedia.ParamFrom(r, retyped) },
		} {
			func() {
				defer func() { panics = append(panics, recover()) }()
				f()
			}()
		}
		return nil
	}))
	require.Equal(t, http.StatusOK, s.do(http.MethodPut, "/contacts/7").Code)
	require.Equal(t, []any{
		`hypermedia: ParamFrom("otherId"): route PUT /contacts/{contactId} has no path parameter "otherId"`,
		`hypermedia: ParamFrom("contactId"): route PUT /contacts/{contactId}'s path parameter "contactId" has type hypermedia_test.ID, but p has type string`,
	}, panics)

	require.PanicsWithValue(t, `hypermedia: ParamFrom("contactId"): no route of a Site serves the request`, func() {
		hypermedia.ParamFrom(httptest.NewRequest(http.MethodGet, "/contacts/7", nil), ContactID)
	})
}

func TestQueryFrom(t *testing.T) {
	page := hypermedia.QueryParam[int]("page")
	for _, tc := range []struct {
		name, query string
		want        int
		err         string
	}{
		{name: "absent is the zero value", query: "q=tea"},
		{name: "no query is the zero value", query: ""},
		{name: "a value", query: "q=tea&page=3", want: 3},
		{name: "an escaped key", query: "pa%67e=4", want: 4},
		{name: "a value that doesn't parse", query: "page=three", err: `query parameter "page": "three" isn't an integer of 64 bits`},
		{name: "an empty value", query: "page=", err: `query parameter "page": "" isn't an integer of 64 bits`},
		{name: "a key that appears twice", query: "page=1&q=tea&page=2", err: `query parameter "page" appears 2 times`},
		{name: "a value that doesn't unescape", query: "page=%zz", err: `query parameter "page": invalid URL escape "%zz"`},
		{name: "another key that doesn't unescape", query: "%zz=1&page=5", want: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hypermedia.QueryFrom(httptest.NewRequest(http.MethodGet, "/contacts?"+tc.query, nil), page)
			if tc.err == "" {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
				return
			}
			re, ok := errors.AsType[*hypermedia.RequestError](err)
			require.True(t, ok, "%T: %v", err, err)
			require.Equal(t, http.StatusBadRequest, re.Status)
			require.Equal(t, "page", re.Param)
			require.EqualError(t, re.Err, tc.err)
			require.Zero(t, got)
		})
	}
}

func TestRequestError(t *testing.T) {
	gone := errors.New("contact 7 doesn't exist")
	err := error(&hypermedia.RequestError{Status: http.StatusNotFound, Err: gone})
	require.EqualError(t, err, "hypermedia: request rejected with 404 Not Found: contact 7 doesn't exist")
	require.ErrorIs(t, err, gone)
	require.EqualError(t, &hypermedia.RequestError{Status: http.StatusGone}, "hypermedia: request rejected with 410 Gone")
	require.NoError(t, (&hypermedia.RequestError{}).Unwrap())
}

func TestSite_answers_a_handlers_error_with_the_ErrorPage(t *testing.T) {
	gone := &hypermedia.RequestError{Status: http.StatusNotFound, Err: errors.New("contact 7 doesn't exist")}
	failed := errors.New("the database is down")
	var contact ID
	h, err := hypermedia.NewSite(&humus.Setup{}, func(w http.ResponseWriter, r *http.Request, err error) error {
		contact = hypermedia.ParamFrom(r, ContactID)
		status := http.StatusInternalServerError
		if re, ok := errors.AsType[*hypermedia.RequestError](err); ok {
			status = re.Status
		}
		http.Error(w, err.Error(), status)
		return nil
	}, []hypermedia.Route{
		hypermedia.NewRoute(http.MethodGet, One, func(http.ResponseWriter, *http.Request) error { return gone }),
		hypermedia.NewRoute(http.MethodPut, One, func(http.ResponseWriter, *http.Request) error { return failed }),
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/contacts/7", nil))
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, gone.Error()+"\n", w.Body.String())
	require.Equal(t, ID(7), contact, "the ErrorPage reads the route's values")

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/contacts/8", nil))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Equal(t, failed.Error()+"\n", w.Body.String())
	require.Equal(t, ID(8), contact)
}
