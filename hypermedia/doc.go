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
// # Building never panics
//
// A path or a parameter isn't checked as it is built, whatever its
// literals, names or constraints, so a path declared as a package-level
// variable never panics while its package initializes. Each method returns
// a copy with one more segment or constraint, and leaves the value it was
// called on unchanged, so two paths built from one prefix, or two
// parameters from one declaration, don't change each other.
package hypermedia
