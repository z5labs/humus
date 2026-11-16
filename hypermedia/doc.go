// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package hypermedia serves the pages, fragments and actions of a
// hypermedia-driven UI. It knows no client library: the headers,
// attributes, events and status codes a library reads are the end user's.
//
// # Paths and links
//
// A [Path] is a Go value, not a pattern string, built from [Root] one
// segment at a time. The same value routes a request and builds the links
// to it, so every href, form action and Location comes from the path that
// serves it:
//
//	var (
//		ContactID = hypermedia.PathParam[ID]("contactId").Minimum(1)
//		List      = hypermedia.Root().Lit("contacts")     // /contacts
//		One       = List.Param(ContactID)                 // /contacts/{contactId}
//		Edit      = One.Lit("edit")                       // /contacts/{contactId}/edit
//		Asset     = hypermedia.PathParam[string]("file")
//		Static    = hypermedia.Root().Lit("static").Rest(Asset) // /static/{file...}
//	)
//
// [Path.URL] builds a link, with one [Arg] for each of the path's
// parameters, in any order. Each Arg has its parameter's type, so a value
// of the wrong type doesn't compile:
//
//	href := Edit.URL(hypermedia.Arg(ContactID, c.ID)).String() // /contacts/7/edit
//
// A missing, repeated or extra Arg panics, naming the path and the
// parameter, so the first test that renders the link finds it. A value
// isn't checked against its parameter's constraints: a link out of bounds
// is built, and leads to the 404 page.
//
// Each value is escaped as one path segment, so a "/" in it is "%2F". A
// [Path.Rest] value is the exception: it keeps its slashes, each segment
// between them is escaped, and it may be empty.
//
// [URL.With] adds a query parameter to a link, escaped:
//
//	Page = hypermedia.QueryParam[int]("page")
//	href := List.URL().With(Page, 2).String() // /contacts?page=2
//
// # Routes and the Site
//
// A [Route] is one method on one [Path], and its [Handler]. [NewSite]
// serves routes on an [http.ServeMux], and checks every path and route
// first, returning one error that names every mistake:
//
//	site, err := hypermedia.NewSite(s, errorPage, []hypermedia.Route{
//		hypermedia.NewRoute(http.MethodGet, List, list),
//		hypermedia.NewRoute(http.MethodGet, Edit, edit),
//		hypermedia.NewRoute(http.MethodPut, One, update),
//		hypermedia.NewRoute("QUERY", List, search),
//	})
//
// A GET route also serves HEAD, and a method may be any token, such as
// QUERY. A request reaches its route's handler with its path values
// already parsed as their types and checked against their constraints, and
// [ParamFrom] returns them. [QueryFrom] reads a query parameter, with the
// same QueryParameter that [URL.With] writes in a link.
//
// A browser never sees ServeMux's plain-text 404 or 405. A path no route
// serves, or whose value doesn't parse or breaks a constraint, gets the
// end user's [ErrorPage] with a [RequestError] of Status 404, as no such
// page exists: /contacts/0 under Minimum(1) is a 404. A method its path
// doesn't have gets the ErrorPage with a *RequestError of Status 405, and
// Allow already set on w. Allow lists the methods of the routes that match
// the path and whose values parse, so beside /contacts/{contactId}, which
// takes an ID, /contacts/changes allows only its own methods.
//
// The route names the request's span, such as "PUT /contacts/{contactId}",
// whatever handlers above the Site pass on a copy of the request.
//
// # Failures
//
// A handler's w has net/http's semantics: each WriteHeader, Write and
// Flush goes straight to the response, and nothing is held. So any
// template engine, a library's own helpers, and [http.ServeFileFS] write
// as they would on net/http, and a body with no Content-Type is sniffed.
// The Site watches only whether the response has started.
//
// Before it starts, a handler's error or panic gets the ErrorPage, without
// the headers and cookies the handler set, and with those set above the
// Site. After it starts, the response is the handler's: an error ends it
// as written, and a panic aborts it. A handler whose render can fail
// halfway renders into a buffer first. A panic with [http.ErrAbortHandler]
// passes through, so a handler can abort a stream on purpose.
//
// Every error but a [RequestError] is recorded, whatever status the page
// writes, and every panic but http.ErrAbortHandler: as an exception event
// on the request's span, its status set to Error, and one ERROR record in
// the request's context, through the logger
// github.com/z5labs/humus/hypermedia. The Site adds no instrument or
// attribute of its own.
//
// # Forms
//
// A [FormField] is a field of a form, declared once as a Go value, with
// its constraints, as a path parameter is:
//
//	var (
//		Name  = hypermedia.Field[string]("name").Required().MaxLength(100)
//		Email = hypermedia.Field[string]("email").Required().Pattern(emailRE)
//		Age   = hypermedia.Field[int]("age").Minimum(18)
//		VIP   = hypermedia.Field[bool]("vip")
//	)
//
// [ReadForm] reads a request's body as a form, urlencoded or multipart,
// whatever its method, so a QUERY's body is read as a POST's is. A form
// already parsed, as by method-override middleware, is read from
// r.PostForm. Only the body is read: a value in the URL's query isn't a
// form field. It checks every field, so a form with several problems
// reports each one:
//
//	form, err := hypermedia.ReadForm(r, Name, Email, Age, VIP)
//	if err != nil {
//		return err
//	}
//	if !form.Valid() {
//		w.WriteHeader(http.StatusUnprocessableEntity)
//		return editForm.Execute(w, form)
//	}
//	c := Contact{Name: hypermedia.FieldFrom(form, Name), Age: hypermedia.FieldFrom(form, Age)}
//
// A field's value is parsed as its type: a string, bool, int, int64 or
// float64 type, named types included. A bool is true when the client sent
// it, as a checked checkbox sends "on", and false when it didn't. A field
// sent empty is one not sent, as HTML's required treats an empty control.
//
// A field with a problem has a [FieldError], from [Form.Problem], naming
// the [Rule] it broke and the constraint's value as its Limit, and
// [Form.Raw] returns what the client sent, so the form is re-rendered as
// it was. The rules, and their Limits, are:
//   - [Repeated], a field sent more than once, with no Limit;
//   - [Missing], a Required field not sent, or sent empty, with no Limit;
//   - [Malformed], a value that doesn't parse as its type, with no Limit;
//   - [NotInEnum], with the Enum's values;
//   - [PatternMismatch], with the Pattern;
//   - [TooShort] and [TooLong], with the MinLength or MaxLength;
//   - [TooSmall] and [TooLarge], with the Minimum or Maximum.
//
// A value breaks the first of them it fails, in that order, and an
// optional field not sent breaks none. The messages are the end user's:
// a FieldError's text is for a log, not for a page.
//
// A multipart body's file parts are skipped, and no part of the body is
// written to disk, so an upload is never spilled outside humus.Host. An
// end user who takes uploads streams them with r.MultipartReader(). The
// body's limit is the end user's, set with [http.MaxBytesHandler]: a body
// over it is a [RequestError] of Status 413, and a body that isn't a form
// is one of 415, so the Site answers both with the ErrorPage, and records
// neither. A form with problems is an answer, not a failure, so ReadForm
// records nothing either.
//
// # Building never panics
//
// A path, a parameter or a form field isn't checked as it is built,
// whatever its literals, names or constraints, so one declared as a
// package-level variable never panics while its package initializes.
// NewSite checks paths and parameters, and ReadForm checks its fields,
// returning an error that names a constraint that doesn't fit its type.
// Each method returns a copy with one more segment or constraint, and
// leaves the value it was called on unchanged, so two paths built from one
// prefix, or two parameters or fields from one declaration, don't change
// each other.
package hypermedia
