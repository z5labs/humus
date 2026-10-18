// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
)

// loggerName is the name of the logger the API logs through.
const loggerName = "github.com/z5labs/humus/openapi/v3"

// Info is the document's info object.
type Info struct {
	// Title is the document's info.title. Required.
	Title string

	// Version is the document's info.version: the version of the API, not
	// of humus or of OpenAPI. Required.
	Version string

	// Description is the document's info.description. Optional.
	Description string
}

// Unmatched answers the requests no operation matches. Its answers aren't
// in the document. NewAPI requires both handlers.
type Unmatched struct {
	// NotFound answers a request whose path no operation serves.
	NotFound http.Handler

	// MethodNotAllowed answers a request whose path an operation serves,
	// but not with the request's method. The API sets the Allow header,
	// listing the path's methods, before it calls MethodNotAllowed.
	MethodNotAllowed http.Handler
}

// API serves its operations, routed with chi. It is an http.Handler, which
// the end user's Init hands to httpserver.NewService.
type API struct {
	mux *chi.Mux
}

// NewAPI returns an API that serves ops:
//   - unmatched answers the requests no operation matches;
//   - requestErrors answers the requests an operation rejects;
//   - catchAll is the catch-all of every operation that doesn't give its
//     own;
//   - opts are given to every operation, before the operation's own parts,
//     which add to them or replace them.
//
// It calls each codec function, and each codec's Schema, once.
//
// It returns one error that names every mistake it finds:
//   - a missing Title or Version;
//   - a nil handler in unmatched;
//   - a nil codec function, or one that returns nil, and an error from a
//     codec's Schema, or a schema that isn't a JSON object or boolean;
//   - a Document whose path has a parameter, and a second Document;
//   - two operations with one OperationID;
//   - a zero Operation, a nil option, and a literal or a parameter name a
//     path can't have, or a parameter given twice in one path;
//   - a zero parameter, and a parameter given twice to one operation;
//   - Pattern, MinLength or MaxLength on a parameter whose type isn't a
//     string, Minimum or Maximum on one whose type isn't a number, a nil
//     Pattern, and a Default or an Example that breaks the parameter's own
//     Enum, Pattern or bounds;
//   - two operations at one method and path, and two paths that differ only
//     in their parameters' names, such as /carts/{id} and /carts/{cartId};
//   - two answers at one status on one operation: two Errors, an Error and
//     the catch-all, or either of them and RequestErrors' 400, 413 or 415
//     on an operation that can answer with it;
//   - Status(http.StatusNoContent) with an encoder whose media type isn't
//     "";
//   - with a Document, a parameter's value that JSON can't encode, such as
//     a float64 Default of +Inf.
//
// With a Document, it builds the API's document, and encodes it once.
//
// It logs through s.Telemetry's LoggerProvider, with the logger name
// "github.com/z5labs/humus/openapi/v3".
func NewAPI(s *humus.Setup, info Info, unmatched Unmatched, requestErrors RequestErrorAnswer, catchAll Fallback, ops []Operation, opts ...APIOption) (*API, error) {
	b := builder{
		seenRoutes:   make(map[string]bool),
		templates:    make(map[string]string),
		checkedPaths: make(map[string]bool),
		operationIDs: make(map[string]string),
	}
	if info.Title == "" {
		b.fail("Info.Title is empty")
	}
	if info.Version == "" {
		b.fail("Info.Version is empty")
	}
	if unmatched.NotFound == nil {
		b.fail("Unmatched.NotFound is nil")
	}
	if unmatched.MethodNotAllowed == nil {
		b.fail("Unmatched.MethodNotAllowed is nil")
	}

	// The API's own answers are bound once, and shared by every operation
	// that doesn't replace them.
	var api resolved
	if requestErrors == nil {
		b.fail("requestErrors is nil")
	} else {
		api.requestErrors = b.bind("requestErrors", requestErrors.(answerer))
	}
	if catchAll == nil {
		b.fail("catchAll is nil")
	} else {
		api.catchAll = b.bind("catchAll", catchAll.(answerer))
	}
	var apiParts parts
	for i, opt := range opts {
		if opt == nil {
			b.fail("opts[%d] is nil", i)
			continue
		}
		opt.apply(&apiParts)
	}
	for _, e := range apiParts.errors {
		if a := b.bind(e.name(), e); a != nil {
			api.errors = append(api.errors, boundError{answer: a, errType: e.errType()})
		}
	}
	checkAPIConflicts(&b, api)

	logger := s.Telemetry.LoggerProvider.Logger(loggerName)
	var routes []*route
	for i, o := range ops {
		if o.op == nil {
			b.fail("ops[%d] is the zero Operation", i)
			continue
		}
		if o.op.document {
			b.document(o.op)
			continue
		}
		if r := b.operation(o.op, api, logger); r != nil {
			routes = append(routes, r)
		}
	}
	var doc []byte
	if len(b.errs) == 0 && b.docPath != nil {
		var err error
		if doc, err = encodeDocument(info, routes); err != nil {
			b.fail("encoding the document: %w", err)
		}
	}
	if err := errors.Join(b.errs...); err != nil {
		return nil, fmt.Errorf("openapi: NewAPI: %w", err)
	}

	a := &API{mux: chi.NewRouter()}
	for _, r := range routes {
		a.mux.Method(r.method, r.path.String(), r)
	}
	if b.docPath != nil {
		a.mux.Method(http.MethodGet, b.docPath.String(), documentHandler(doc))
	}
	a.mux.NotFound(unmatched.NotFound.ServeHTTP)
	a.mux.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		allowed := a.allowed(r)
		if len(allowed) == 0 {
			// chi answers a method it doesn't know with its 405, whatever
			// the path, so a path no operation serves is a 404 here.
			unmatched.NotFound.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		unmatched.MethodNotAllowed.ServeHTTP(w, r)
	})
	return a, nil
}

// ServeHTTP serves r with the operation at its method and path, or with
// the API's Unmatched.
//
// chi routes a copy of the request, so ServeHTTP hands chi a copy that
// already carries a chi routing context, which chi routes without copying
// again. It then sets the pattern chi found on r, so that httpserver's
// spans are named after the route, such as GET /carts/{cartId}.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	routed := r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, chi.NewRouteContext()))
	defer func() {
		if routed.Pattern != "" {
			r.Pattern = routed.Pattern
		}
	}()
	a.mux.ServeHTTP(w, routed)
}

// allowed returns the methods of the operations at r's path.
func (a *API) allowed(r *http.Request) []string {
	path := routePath(r)
	var allowed []string
	// The methods an operation can have, in the order Allow lists them.
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if a.mux.Find(chi.NewRouteContext(), m, path) != "" {
			allowed = append(allowed, m)
		}
	}
	return allowed
}

// routePath is the path chi routes r by: its escaped form when it has one.
func routePath(r *http.Request) string {
	switch {
	case r.URL.RawPath != "":
		return r.URL.RawPath
	case r.URL.Path != "":
		return r.URL.Path
	default:
		return "/"
	}
}

// resolved is a set of bound answers: the API's, or one operation's.
type resolved struct {
	requestErrors *answer
	catchAll      *answer
	errors        []boundError
}

// boundError is a bound Error, with the type it answers.
type boundError struct {
	*answer
	errType any
}

// builder collects NewAPI's mistakes, and what it needs to find them.
type builder struct {
	errs         []error
	seenRoutes   map[string]bool   // "GET /carts/{cartId}"
	templates    map[string]string // "/carts/{}" to the first path with it
	checkedPaths map[string]bool
	operationIDs map[string]string // an OperationID to its operation
	docPath      *Path             // the first Document's
}

func (b *builder) fail(format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf(format, args...))
}

// failEach reports each error err joins, or err itself, after prefix.
func (b *builder) failEach(prefix string, err error) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			b.failEach(prefix, e)
		}
		return
	}
	b.fail("%s: %w", prefix, err)
}

// bind binds a, reporting its mistakes as what's.
func (b *builder) bind(what string, a answerer) *answer {
	bound, err := a.bind()
	if err != nil {
		b.failEach(what, err)
		return nil
	}
	return bound
}

// route checks op's path, once for each path, and reports a second
// operation at op's method and path.
func (b *builder) route(op *operation) {
	path := op.path.String()
	if !b.checkedPaths[path] {
		b.checkedPaths[path] = true
		for _, err := range op.path.check() {
			b.fail("path %s: %w", path, err)
		}
		tmpl := op.path.template()
		if first, ok := b.templates[tmpl]; ok && first != path {
			b.fail("paths %s and %s differ only in their parameters' names", first, path)
		} else if !ok {
			b.templates[tmpl] = path
		}
	}
	name := op.method + " " + path
	if b.seenRoutes[name] {
		b.fail("two operations are at %s", name)
	}
	b.seenRoutes[name] = true
}

// document checks the operation Document returned: its path, which has no
// parameter, and that it is the API's only Document.
func (b *builder) document(op *operation) {
	b.route(op)
	path := op.path.String()
	if op.path.hasParam() {
		b.fail("Document's path %s has a parameter", path)
	}
	if b.docPath != nil {
		b.fail("two Documents, at %s and %s", b.docPath, path)
		return
	}
	b.docPath = &op.path
}

// operation checks op and binds its codecs and its own parts. It returns
// nil when op has a mistake.
func (b *builder) operation(op *operation, api resolved, logger log.Logger) *route {
	name := op.method + " " + op.path.String()
	before := len(b.errs)
	fail := func(format string, args ...any) {
		b.fail("%s: "+format, append([]any{name}, args...)...)
	}

	b.route(op)

	c, err := op.bind()
	if err != nil {
		b.failEach(name, err)
	}

	var own parts
	for i, opt := range op.opts {
		if opt == nil {
			fail("its option %d is nil", i)
			continue
		}
		opt.apply(&own)
	}
	if id := own.operationID; id != "" {
		if first, ok := b.operationIDs[id]; ok {
			fail("its OperationID %q is %s's too", id, first)
		} else {
			b.operationIDs[id] = name
		}
	}

	seenParams := make(map[string]bool)
	for _, p := range own.params {
		errs := p.check()
		for _, err := range errs {
			fail("%w", err)
		}
		if len(errs) > 0 || p.paramName() == "" {
			continue
		}
		if seenParams[p.paramKey()] {
			fail("%s is given twice", p.describe())
		}
		seenParams[p.paramKey()] = true
	}

	answers := api
	if own.requestErrors != nil {
		answers.requestErrors = b.bind(name+": "+own.requestErrors.name(), own.requestErrors)
	}
	if own.catchAll != nil {
		answers.catchAll = b.bind(name+": "+own.catchAll.name(), own.catchAll)
	}
	answers.errors = mergeErrors(b, name, api.errors, own.errors)

	if own.statusSet && (own.status < 100 || own.status > 599) {
		fail("Status(%d) isn't an HTTP status", own.status)
	}
	if c != nil && own.status == http.StatusNoContent && c.encMediaType != "" {
		fail("Status(%d) with an encoder whose media type is %q, not \"\"", own.status, c.encMediaType)
	}
	if c != nil {
		readsBody := c.decMediaType != ""
		readsParam := op.path.hasParam() || len(own.params) > 0
		checkConflicts(fail, answers, own, readsParam || readsBody, readsBody)
	}

	if len(b.errs) > before {
		return nil
	}
	status := own.status
	if !own.statusSet {
		status = http.StatusOK
		if c.encMediaType == "" {
			status = http.StatusNoContent
		}
	}
	return &route{
		name:     name,
		method:   op.method,
		path:     op.path,
		status:   status,
		opParams: own.params,
		codecs:   c,
		answers:  answers,
		logger:   logger,
		bodyType: parseMediaType(c.decMediaType),
		meta:     own,
	}
}

// mergeErrors binds the operation's own Errors, and returns them followed
// by the API's, leaving out each of the API's that one of them replaces.
func mergeErrors(b *builder, name string, api []boundError, own []errorAnswer) []boundError {
	var merged []boundError
	replaced := make(map[any]bool)
	for _, e := range own {
		replaced[e.errType()] = true
		if a := b.bind(name+": "+e.name(), e); a != nil {
			merged = append(merged, boundError{answer: a, errType: e.errType()})
		}
	}
	for _, e := range api {
		if !replaced[e.errType] {
			merged = append(merged, e)
		}
	}
	return merged
}

// hasParam reports whether p has a path parameter.
func (p Path) hasParam() bool {
	for _, seg := range p.segs {
		if seg.param != nil {
			return true
		}
	}
	return false
}

// checkAPIConflicts reports two of the API's own Errors at one status, or
// one of them at the catch-all's. Every operation would answer with both,
// so they are reported once, here, rather than for every operation.
func checkAPIConflicts(b *builder, api resolved) {
	byStatus := make(map[int]string)
	if api.catchAll != nil {
		byStatus[api.catchAll.status] = "the catch-all"
	}
	for _, e := range api.errors {
		if other, ok := byStatus[e.status]; ok {
			b.fail("%s and %s both answer with %d", other, e.name, e.status)
			continue
		}
		byStatus[e.status] = e.name
	}
}

// checkConflicts reports two of an operation's answers at one status,
// unless both are the API's own Errors or catch-all, which
// checkAPIConflicts reports.
func checkConflicts(fail func(string, ...any), answers resolved, own parts, canReject400, canReject4xx bool) {
	type claim struct {
		name string
		own  bool
	}
	byStatus := make(map[int]claim)
	claimStatus := func(status int, c claim) {
		other, ok := byStatus[status]
		if !ok {
			byStatus[status] = c
			return
		}
		if c.own || other.own {
			fail("%s and %s both answer with %d", other.name, c.name, status)
		}
	}

	// The request errors' statuses depend on the operation, so a clash
	// with any of them is the operation's.
	if answers.requestErrors != nil {
		if canReject400 {
			claimStatus(http.StatusBadRequest, claim{name: "RequestErrors", own: true})
		}
		if canReject4xx {
			claimStatus(http.StatusRequestEntityTooLarge, claim{name: "RequestErrors", own: true})
			claimStatus(http.StatusUnsupportedMediaType, claim{name: "RequestErrors", own: true})
		}
	}
	if answers.catchAll != nil {
		claimStatus(answers.catchAll.status, claim{name: "the catch-all", own: own.catchAll != nil})
	}
	ownTypes := make(map[any]bool)
	for _, e := range own.errors {
		ownTypes[e.errType()] = true
	}
	for _, e := range answers.errors {
		claimStatus(e.status, claim{name: e.name, own: ownTypes[e.errType]})
	}
}
