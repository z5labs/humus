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
// # Building never panics
//
// A path or a parameter isn't checked as it is built, whatever its
// literals, names or constraints, so a path declared as a package-level
// variable never panics while its package initializes. Each method returns
// a copy with one more segment or constraint, and leaves the value it was
// called on unchanged, so two paths built from one prefix, or two
// parameters from one declaration, don't change each other.
package hypermedia
