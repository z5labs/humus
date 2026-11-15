// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package hypermedia

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal/httproute"
	"github.com/z5labs/humus/internal/param"
)

// Handler serves one route, writing to w as any http.Handler does.
type Handler func(w http.ResponseWriter, r *http.Request) error

// Route is one method on one path, and its handler.
type Route struct {
	method string
	path   Path
	h      Handler
	set    bool
}

// NewRoute returns the route for method on p. A GET route also serves
// HEAD, and method may be any token, such as QUERY.
func NewRoute(method string, p Path, h Handler) Route {
	return Route{method: method, path: p, h: h, set: true}
}

// name names r, for an error, such as "GET /contacts/{contactId}".
func (r Route) name() string {
	return r.method + " " + r.path.String()
}

// ErrorPage answers every error the Site answers: a path no route serves,
// a method its path doesn't have, and a handler's error. The Site answers
// a path or a method with a *RequestError.
type ErrorPage func(w http.ResponseWriter, r *http.Request, err error) error

// Site serves its routes on an http.ServeMux. It is an http.Handler.
type Site struct {
	mux       *http.ServeMux
	errorPage ErrorPage

	// methods are the methods of the Site's routes, HEAD among them when
	// a GET is, in the order Allow lists them.
	methods []string
}

// NewSite returns the Site that serves routes, and answers a path or a
// method none of them serves with errorPage.
//
// It returns one error that names every mistake it finds:
//   - a nil s or errorPage;
//   - a zero Route, and a nil Handler;
//   - a method that isn't a token;
//   - a literal a path can't have, such as "" or one that would need
//     escaping, and a parameter name that isn't an ASCII Go identifier;
//   - a constraint that doesn't fit its parameter's type, such as Pattern
//     on a number or Minimum on a string, and a nil Pattern;
//   - a parameter that appears twice in one path;
//   - a Rest that isn't its path's last segment;
//   - two routes at one method and path;
//   - two paths that differ only in their parameters' names, such as
//     /contacts/{id} and /contacts/{contactId};
//   - two routes an http.ServeMux can't tell apart, such as
//     GET /contacts/{id}/edit and GET /{kind}/7/edit.
func NewSite(s *humus.Setup, errorPage ErrorPage, routes []Route) (*Site, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if s == nil {
		fail("s is nil")
	}
	if errorPage == nil {
		fail("errorPage is nil")
	}

	checked := make(map[string]bool)     // the paths checked, by pattern
	templates := make(map[string]string) // the first path of each template
	seen := make(map[string]bool)        // the routes, by name
	methods := make(map[string]bool)
	var valid []Route
	for i, r := range routes {
		if !r.set {
			fail("route %d is the zero Route", i)
			continue
		}
		before := len(errs)
		name := r.name()
		if !isToken(r.method) {
			fail("route %s: method %q isn't a token", name, r.method)
		}
		if r.h == nil {
			fail("route %s: its Handler is nil", name)
		}
		path := r.path.String()
		if !checked[path] {
			checked[path] = true
			for _, err := range r.path.check() {
				fail("path %s: %w", path, err)
			}
			tmpl := r.path.template()
			if first, ok := templates[tmpl]; ok {
				fail("paths %s and %s differ only in their parameters' names", first, path)
			} else {
				templates[tmpl] = path
			}
		}
		if seen[name] {
			fail("two routes are at %s", name)
		}
		seen[name] = true
		if len(errs) == before {
			valid = append(valid, r)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("hypermedia: NewSite: %w", errors.Join(errs...))
	}

	site := &Site{mux: http.NewServeMux(), errorPage: errorPage}
	for _, r := range valid {
		if err := site.handle(r.method+" "+r.path.muxPattern(), &route{site: site, Route: r}); err != nil {
			fail("route %s: %w", r.name(), err)
		}
		methods[r.method] = true
		if r.method == http.MethodGet {
			methods[http.MethodHead] = true
		}
	}
	if err := site.handle("/", http.HandlerFunc(site.unmatched)); err != nil {
		fail("%w", err)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("hypermedia: NewSite: %w", errors.Join(errs...))
	}
	for m := range methods {
		site.methods = append(site.methods, m)
	}
	slices.Sort(site.methods)
	return site, nil
}

// handle registers h at pattern, and returns the panic of an
// http.ServeMux that won't have it as an error.
func (s *Site) handle(pattern string, h http.Handler) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("%v", v)
		}
	}()
	s.mux.Handle(pattern, h)
	return nil
}

// ServeHTTP serves r with the route at its method and path, its path
// values parsed and checked, or with the Site's ErrorPage:
//   - a path no route serves, or whose values don't parse or break a
//     constraint, gets a *RequestError of Status 404;
//   - a method its path doesn't have gets a *RequestError of Status 405,
//     with Allow already set on w. Allow lists the methods of the routes
//     that match the path and whose values parse;
//   - a handler's error is passed on as it is.
//
// The route names the request's span, such as "PUT /contacts/{contactId}",
// whatever handlers above the Site pass on a copy of r, such as
// http.MaxBytesHandler.
func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := &state{request: r}
	routed := r.WithContext(context.WithValue(r.Context(), stateKey{}, st))
	s.mux.ServeHTTP(w, routed)
	if st.pattern != "" {
		r.Pattern = st.pattern
		httproute.Record(r.Context(), st.pattern)
	}
}

// unmatched serves the requests no route matches, and the probes of
// allowed that no route matches.
func (s *Site) unmatched(w http.ResponseWriter, r *http.Request) {
	st, _ := r.Context().Value(stateKey{}).(*state)
	if st == nil || st.probe {
		return
	}
	s.answerUnmatched(w, st.request, nil)
}

// answerUnmatched answers r, which no route serves: with a 405 when a
// route of another method matches its path and parses its values, and
// otherwise with a 404. paramErr is the 404 of a route that matched r but
// whose values don't parse, or nil.
func (s *Site) answerUnmatched(w http.ResponseWriter, r *http.Request, paramErr *RequestError) {
	if allow := s.allowed(r); len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		_ = s.errorPage(w, r, &RequestError{
			Status: http.StatusMethodNotAllowed,
			Err:    fmt.Errorf("%s isn't a method of %s, which allows %s", r.Method, r.URL.EscapedPath(), strings.Join(allow, ", ")),
		})
		return
	}
	if paramErr != nil {
		_ = s.errorPage(w, r, paramErr)
		return
	}
	_ = s.errorPage(w, r, &RequestError{
		Status: http.StatusNotFound,
		Err:    fmt.Errorf("no route serves %s", r.URL.EscapedPath()),
	})
}

// allowed returns the methods of the routes that match r's path and parse
// its values. It asks the Site's ServeMux, a method at a time, so the path
// is matched and unescaped as it would be for a request of that method.
func (s *Site) allowed(r *http.Request) []string {
	var allow []string
	for _, m := range s.methods {
		st := &state{probe: true}
		probe := r.WithContext(context.WithValue(r.Context(), stateKey{}, st))
		probe.Method = m
		probe.Pattern = ""
		s.mux.ServeHTTP(discard{header: make(http.Header)}, probe)
		if st.pattern != "" {
			allow = append(allow, m)
		}
	}
	return allow
}

// state is what routing one request, or one probe of allowed, found. It
// travels in the request's context, since an http.ServeMux routes the
// request it is given.
type state struct {
	// probe reports whether the request is a probe of allowed, which no
	// handler serves.
	probe bool

	// request is the request the Site was given, as the ErrorPage gets it.
	request *http.Request

	// pattern is the route's name, such as "GET /contacts/{contactId}",
	// once a route has matched the request and parsed its values.
	pattern string
}

type stateKey struct{}

// route serves one Route on the Site's ServeMux.
type route struct {
	Route
	site *Site
}

func (rt *route) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st, _ := r.Context().Value(stateKey{}).(*state)
	if st == nil {
		return
	}
	values := make(map[string]any)
	for _, seg := range rt.path.segs {
		if seg.param == nil {
			continue
		}
		v, err := seg.param.parse(r.PathValue(seg.param.name))
		if err != nil {
			if st.probe {
				return
			}
			rt.site.answerUnmatched(w, st.request, &RequestError{Status: http.StatusNotFound, Param: seg.param.name, Err: err})
			return
		}
		values[seg.param.name] = v
	}
	st.pattern = rt.name()
	if st.probe {
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), valuesKey{}, &routeValues{pattern: st.pattern, values: values}))
	if err := rt.h(w, r); err != nil {
		_ = rt.site.errorPage(w, r, err)
	}
}

// discard is the http.ResponseWriter of a probe, which nothing reads.
type discard struct {
	header http.Header
}

func (d discard) Header() http.Header       { return d.header }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}

// isToken reports whether s is a token of RFC 9110, as a method is.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isLetter(r) && !isDigit(r) && !strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			return false
		}
	}
	return true
}

// routeValues are the values of a request's route, parsed and checked.
type routeValues struct {
	pattern string
	values  map[string]any
}

type valuesKey struct{}

// ParamFrom returns p's value, parsed and checked before the handler ran.
// It panics, naming p and the route's pattern, if r's route has no p, and
// naming p if no route of a Site serves r.
func ParamFrom[T ParamValue](r *http.Request, p PathParameter[T]) T {
	rv, _ := r.Context().Value(valuesKey{}).(*routeValues)
	if rv == nil {
		panic(fmt.Sprintf("hypermedia: ParamFrom(%q): no route of a Site serves the request", p.name))
	}
	v, ok := rv.values[p.name]
	if !ok {
		panic(fmt.Sprintf("hypermedia: ParamFrom(%q): route %s has no path parameter %q", p.name, rv.pattern, p.name))
	}
	t, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("hypermedia: ParamFrom(%q): route %s's path parameter %q has type %T, but p has type %s", p.name, rv.pattern, p.name, v, reflect.TypeFor[T]()))
	}
	return t
}

// QueryFrom returns q's value in r's query, or T's zero value when the
// query doesn't have it. A value that doesn't parse as T, or a key that
// appears twice, is a *RequestError with Status 400.
func QueryFrom[T ParamValue](r *http.Request, q QueryParameter[T]) (T, error) {
	var zero T
	var raw []string
	for piece := range strings.SplitSeq(r.URL.RawQuery, "&") {
		key, value, _ := strings.Cut(piece, "=")
		if k, err := url.QueryUnescape(key); err != nil || k != q.name {
			continue
		}
		v, err := url.QueryUnescape(value)
		if err != nil {
			return zero, &RequestError{Status: http.StatusBadRequest, Param: q.name, Err: fmt.Errorf("query parameter %q: %w", q.name, err)}
		}
		raw = append(raw, v)
	}
	switch len(raw) {
	case 0:
		return zero, nil
	case 1:
	default:
		return zero, &RequestError{Status: http.StatusBadRequest, Param: q.name, Err: fmt.Errorf("query parameter %q appears %d times", q.name, len(raw))}
	}
	v, err := param.Parse[T](raw[0])
	if err != nil {
		return zero, &RequestError{Status: http.StatusBadRequest, Param: q.name, Err: fmt.Errorf("query parameter %q: %w", q.name, err)}
	}
	return v, nil
}

// RequestError is a request the client got wrong: 404 for a path no route
// serves, or whose values don't parse or break a constraint, 405 for a
// method its path doesn't have, with Allow already set on w, or 400 for a
// query parameter. A handler returns one for the client's error, such as a
// contact that doesn't exist.
type RequestError struct {
	// Status is the response's status: 400, 404 or 405 from the Site, or
	// the handler's own.
	Status int

	// Param names the parameter that doesn't parse, or breaks a
	// constraint, and is "" otherwise.
	Param string

	// Err is why.
	Err error
}

// Error returns the status and Err's text.
func (e *RequestError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("hypermedia: request rejected with %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("hypermedia: request rejected with %d %s: %v", e.Status, http.StatusText(e.Status), e.Err)
}

// Unwrap returns Err.
func (e *RequestError) Unwrap() error {
	return e.Err
}
