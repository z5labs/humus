// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package openapi serves an API of typed operations, and its OpenAPI
// document, following the latest minor version of OpenAPI 3: 3.2.0.
//
// The end user's Init builds the API with [NewAPI], from typed operations
// on typed paths, and hands it to httpserver.NewService, which serves it:
//
//	api, err := openapi.NewAPI(s,
//		openapi.Info{Title: "Carts", Version: "1.0.0"},
//		openapi.Unmatched{NotFound: notFound, MethodNotAllowed: methodNotAllowed},
//		openapi.RequestErrors(rejected, problemEncoder),
//		openapi.CatchAll(http.StatusInternalServerError, internalError, problemEncoder),
//		[]openapi.Operation{
//			openapi.Post(openapi.Root().Lit("carts").Param(cartID).Lit("items"), c.addCartItem, itemDecoder, itemEncoder,
//				openapi.OperationID("addCartItem"),
//				openapi.Summary("Add an item to a shopping cart"),
//				openapi.Status(http.StatusCreated),
//			),
//			openapi.Document(openapi.Root().Lit("openapi.json")),
//		},
//		openapi.Error[NotFound](http.StatusNotFound, problemEncoder),
//	)
//	if err != nil {
//		return nil, err
//	}
//	svc, err := httpserver.NewService(s, settings, api)
//
// The import path, github.com/z5labs/humus/openapi/v3, names the major
// version of OpenAPI the package follows. The package's name is openapi.
//
// # Paths
//
// A [Path] is built from the root one segment at a time, from literals and
// path parameters, so there is no template to parse:
//
//	cartID := openapi.PathParam[string]("cartId")
//	cart := openapi.Root().Lit("carts").Param(cartID) // /carts/{cartId}
//
// Each method returns a new Path, so a prefix is a value that several
// operations share. A path declares its path parameters, and an operation
// reads every parameter of its path. Its handler reads one with
// [ParamFrom], which panics, naming the parameter and the operation, for a
// parameter the operation doesn't read.
//
// NewAPI checks every path, and reports a mistake as an error, never as a
// panic, so a path declared as a package-level variable can't fail its
// package's initialization.
//
// # Parameters
//
// A parameter is declared once, usually as a package-level value, with
// [PathParam], [QueryParam] or [HeaderParam] and its constraints. Each
// method returns a copy with one more part, so a declaration can be
// shared:
//
//	var include = openapi.QueryParam[Include]("include").
//		Description("What the cart includes").
//		Enum(IncludeItems, IncludeCount).
//		Default(IncludeItems)
//
//	openapi.Get(cart, c.getCart, cartEncoder, openapi.Query(include))
//
// A path parameter belongs to its path. A query or header parameter is
// read by each operation that gives it as a part, with [Query] or
// [Header]. A path parameter is always required. A query or header
// parameter is optional unless it is Required, and an absent one is its
// Default, or T's zero value. A query parameter that appears more than
// once is rejected, and a header's lines are read as their values joined
// by commas, as HTTP reads them.
//
// The types check where a parameter goes and the values of its
// constraints, so these don't compile:
//
//   - a value of another type in Enum, Default, Example, Minimum or
//     Maximum, which take the parameter's own T;
//   - Required or Default on a path parameter, which has neither method;
//   - a path parameter given to Query, and a query parameter given to
//     Path.Param;
//   - a T that isn't a string, bool, int, int64 or float64 type.
//
// NewAPI reports the rest: Pattern, MinLength or MaxLength on a type that
// isn't a string, Minimum or Maximum on a type that isn't a number, a
// Default or an Example that breaks the parameter's own Enum, Pattern or
// bounds, a float64 value that is NaN or infinite, which the document
// can't hold, a parameter given twice to one operation, and a zero
// parameter.
//
// # Operations and their codecs
//
// [Get], [Post], [Put], [Patch] and [Delete] take a path, a typed handler, a
// decoder for the request body (Post, Put and Patch), an encoder for the
// response, and the operation's parts. Every body names its codec, and
// nothing is assumed: the package refers to no format, such as JSON, and
// has no default. An operation without its codecs doesn't compile.
//
// A [Decoder] reads a request body, and an [Encoder] writes one. Codecs are
// passed as functions, so a generic codec function is passed
// uninstantiated, and the compiler infers its type argument from the
// handler. [EmptyDecoder] and [EmptyEncoder] read and write no body.
//
// A request is served in this order:
//
//   - When the operation has requirements, the request is authenticated
//     against them; see Security.
//   - Each path parameter is decoded as its type, then each query and
//     header parameter the operation reads. A path parameter that is
//     empty, a missing required parameter, a repeated query parameter,
//     and a value that doesn't parse or breaks a constraint, is a
//     [RequestError] with status 400, whose Param is the parameter's name.
//   - When the decoder's media type isn't "", a request whose Content-Type
//     names another media type, parameters aside, or none at all, is a
//     RequestError with status 415.
//   - The decoder reads the body. Its error is a RequestError with status
//     400, or 413 when it wraps *http.MaxBytesError, as
//     http.MaxBytesHandler's does.
//   - The handler is called with the decoded [Request].
//   - The encoder's Encode runs before the status is written, so a value
//     that can't be encoded is answered by the catch-all, never as a 200
//     with a broken body.
//   - The status is written: [Status]'s, or 204 when the encoder's media
//     type is "", and 200 otherwise. Content-Type is the encoder's media
//     type, and isn't set when that is "".
//   - The WriterTo Encode returned writes the body.
//
// A panic of a verifier, the decoder, the handler or Encode is answered by
// the catch-all; see Panics.
//
// # Streaming
//
// Whether a codec buffers or streams is the codec's to say. A decoder that
// buffers reads the whole body in Decode, and one that streams returns a
// value that reads the body as the handler reads it, such as the
// request's body itself, as an io.Reader. An encoder that buffers returns
// its encoded bytes, such as a *bytes.Reader, and one that streams returns
// a WriterTo that writes as it goes.
//
// The WriterTo is handed a writer that flushes each write, so each part
// reaches the client as it is written. It runs on the request's goroutine,
// with the request's context still live, so it may do more work once it
// has written the body, such as recording that a download completed, with
// the context the handler captured in the value it returned. When the
// WriterTo has a Len() int method, as a *bytes.Reader does, the API sets
// Content-Length to it, so Len must be the number of bytes WriteTo writes:
// a body longer than its Len fails, and one shorter is cut short.
// Otherwise the response is chunked over HTTP/1.1.
//
// An error from the WriterTo cuts the response short. The status, and
// whatever part of the body was written, have already gone out, so the API
// aborts the response: on HTTP/2 the stream is reset, and on HTTP/1.1 the
// connection is closed before the final chunk. The client sees a failed
// response rather than one that looks complete. A response with a
// Content-Length that fails after its last byte has the length it was
// promised, and looks complete, so a WriterTo that does work after the
// body should leave Len out, so that a failure is seen.
//
// The status is written before the first byte of the body, as HTTP
// requires. So a status that depends on that work needs the body copied
// elsewhere first: Encode copies it, to a temporary file, say, does the
// work, then returns a WriterTo over the copy.
//
// # Errors
//
// Every error has an answer the end user chose, written by an encoder the
// end user named:
//
//   - A RequestError is answered by [RequestErrors], with its status, and
//     the handler isn't called. A request refused by [Security] is one, at
//     401 or 403.
//   - The handler's error is answered by the first [Error] whose E matches
//     it, with errors.As: the operation's own first, then the API's. An
//     operation's Error for an E replaces the API's.
//   - Otherwise the catch-all answers, the operation's [CatchAll] or
//     NewAPI's, with its status and the error itself if it matches E, or
//     wrap(err) if not.
//
// NewAPI requires RequestErrors and a catch-all as arguments, so an API
// can't be built without them. An Error given to NewAPI answers on every
// operation.
//
// A request no operation matches is answered by the end user's
// [Unmatched]: a path no operation serves by NotFound, and a method its
// path doesn't serve by MethodNotAllowed, after the API has set Allow to
// the path's methods. The package writes nothing of its own for either,
// and redirects nothing.
//
// # Security
//
// [Security] is what a request must have to be served: one of its
// requirements, which the auth package declares. A requirement is made of
// schemes, such as auth.Bearer, each with a verify function that turns a
// credential into the end user's own principal, which a handler reads back
// with the scheme's From:
//
//	bearer := auth.Bearer("bearer", tokens.Verify).Format("JWT")
//
//	api, err := openapi.NewAPI(s, info, unmatched, requestErrors, catchAll,
//		[]openapi.Operation{
//			openapi.Get(cart, c.getCart, cartEncoder),
//			openapi.Delete(cart, c.deleteCart, openapi.EmptyEncoder,
//				openapi.Security(auth.RequireScopes(bearer, "carts:write")),
//			),
//			openapi.Get(openapi.Root().Lit("status"), c.status, statusEncoder,
//				openapi.Security(auth.Disabled()),
//			),
//			openapi.Document(openapi.Root().Lit("openapi.json"),
//				openapi.Security(auth.Disabled()),
//			),
//		},
//		openapi.Security(auth.RequireScopes(bearer, "carts:read")),
//	)
//
// Given to NewAPI, Security is every operation's, and the document's.
// Given to an operation or to [Document], it replaces the API's, whatever
// the API's was. Security(auth.Disabled()) makes what it is given to
// public: no credential is read, and From of every scheme returns false.
// An API without Security authenticates nothing. Security() with no
// requirement is a mistake, as are auth.Disabled() beside another
// requirement or inside an auth.All, and every mistake auth.NewHandler
// reports in a requirement, which NewAPI reports in its one error.
//
// An operation with requirements authenticates the request before
// anything else: before it decodes a parameter, checks the Content-Type
// or reads the body. So a client that isn't authenticated gets a 401,
// never a 400 that says which parameter it got wrong, and its body is
// never read. A request that Unmatched answers matched no operation, and
// isn't authenticated.
//
// A request is answered by what its credentials meet:
//
//   - A request no requirement authenticated is a RequestError with Status
//     401 and Err auth.ErrUnauthenticated, and one that a scheme
//     authenticated, but without a scope its requirement needs, is a
//     RequestError with Status 403 and Err auth.ErrForbidden.
//     RequestErrors answers both, and isn't logged: a refusal is the
//     client's mistake. Err is the sentinel, never a verifier's error, so
//     no verifier's reason reaches the client. The API sets
//     WWW-Authenticate before RequestErrors' encoder runs, as it sets
//     Allow before MethodNotAllowed: a Bearer or Basic challenge for a
//     401, and Bearer's insufficient_scope for a 403.
//   - A verifier's error that doesn't wrap auth.ErrUnauthenticated, such
//     as a directory that is down, is the server's, never a 401. It is
//     answered as a handler's error is: by the first [Error] that matches
//     it, such as an Error at 503, which clients retry, else by the
//     catch-all, which records it as "request failed".
//   - A verifier's panic is answered by the catch-all, and recorded as
//     "operation panicked"; see Panics.
//
// An Error or a catch-all at 401 on an operation that can answer 401, and
// at 403 on one whose requirements name a scope, is a NewAPI error, as one
// at RequestErrors' other statuses is.
//
// The document is secured like the API: a request for it must meet the
// API's Security, unless Document is given its own. A team that keeps an
// internal API's document open for its tooling says so in one line,
// Document(p, openapi.Security(auth.Disabled())), where a review sees it.
// Document takes Security and no other option.
//
// # Panics
//
// A panic of the end user's code, on the goroutine serving an operation's
// request, is answered as the document says, so the API keeps its contract even when
// that code has a bug. The API recovers a panic of a scheme's verify
// function, of the decoder's Decode,
// of the handler, of the encoder's Encode, and of the Len of the WriterTo
// it returns, which all run before the status is written. The operation's catch-all answers it, with its status
// and wrap of an error that doesn't carry the panic's value, so the value
// never reaches the client, and the response is the one the document lists
// for the catch-all. The API keeps serving every other request.
//
// A panic of the WriterTo's WriteTo comes after the status, so it can't be
// answered: it cuts the response short, as a WriterTo's error does. A
// panic on a goroutine the end user's code starts can't be recovered by
// the API at all, and crashes the process, as it would anywhere in Go.
//
// Recovering has a cost: whatever the panicking code left half-changed,
// such as a map it was halfway through writing, or a lock it took without
// a deferred unlock, stays that way for the requests that follow. Nothing
// is rolled back.
//
// These pass through, unrecovered, to httpserver, which aborts the
// response, and records every one but http.ErrAbortHandler:
//
//   - a panic with http.ErrAbortHandler, which the end user's code uses to
//     abort a response on purpose, and which httpserver passes through
//     without recording;
//   - a panic while answering: in a wrap, or in the encoder of an [Error],
//     a CatchAll or RequestErrors, which would panic again if it answered
//     its own panic;
//   - a panic in Unmatched's handlers, which are the end user's own
//     http.Handlers.
//
// # The document
//
// [Document] returns the operation that serves the API's OpenAPI document,
// at its path, with GET, as application/json. An API whose operations
// include no Document serves no document. NewAPI builds the document from
// the same declarations that serve the API's requests, and encodes it once,
// so every request for it gets the same bytes. Its openapi is 3.2.0, and
// its info is [Info]'s.
//
// Each operation is listed at its path, under its method, with:
//
//   - its metadata: [Summary], [Description], [OperationID], [Tags] and
//     [Deprecated];
//   - its parameters, the path's and those it gives to Query and Header,
//     with their constraints;
//   - its request body, required, under the decoder's media type, with the
//     decoder's Schema. A nil Schema lists the media type alone, and a
//     decoder whose media type is "", such as EmptyDecoder, lists no
//     request body;
//   - its responses: the success status, under the encoder's media type
//     with its Schema, or with no content when that is ""; each Error, the
//     operation's own and the API's, with its encoder's; RequestErrors' E at
//     400 when the operation reads a parameter or a body, at 413 and 415
//     when it reads a body, at 401 when it has requirements, and at 403
//     when one of them names a scope; and the catch-all at its status;
//   - its own Security, when it was given one, under its security: [] for
//     Security(auth.Disabled()).
//
// The Security given to NewAPI is the document's top-level security, and
// components.securitySchemes holds every scheme of every requirement,
// keyed by its name: Bearer and Basic as http schemes, with Bearer's
// Format as its bearerFormat, an API key as an apiKey in its header,
// query parameter or cookie, MutualTLS as mutualTLS, OAuth2 as oauth2 with
// its flows, and OpenIDConnect as openIdConnect with its discovery URL,
// each with its Description. A requirement is an object from each of its
// schemes' names to its scopes, and auth.Anonymous() is {}.
//
// Schemas are inline, since one Go type can have a different schema under
// each codec. A codec whose bodies are a Go type's fields writes its Schema
// with [SchemaOf], named by its own struct tag. The document doesn't
// describe itself, the requests Unmatched answers, or anything mounted next
// to the API.
//
// # Telemetry
//
// The API adds no spans or metrics: httpserver gives every request a
// server span, named after the operation's route, such as
// GET /carts/{cartId}, and records http.server.request.duration with that
// route as its http.route. That holds under a ServeMux, and behind handlers
// that pass the API a copy of the request, such as http.MaxBytesHandler and
// http.TimeoutHandler. The package never reads or sets OpenTelemetry's
// globals. It logs through the Setup's LoggerProvider, under the logger
// name github.com/z5labs/humus/openapi/v3.
//
// An error the catch-all answers is one the end user didn't plan for. The
// API records it as an exception event on the request's span, and as one
// ERROR record, "request failed", in the request's context, with
// http.request.method, http.route, error.type and exception.message. When
// the request's context is done because the client went away, the error
// goes on the span only. Answers from an Error or from RequestErrors aren't
// logged.
//
// A panic the API recovers is recorded as an exception event on the
// request's span, with exception.type, exception.message and
// exception.stacktrace, the stack of the panic, and the span's status is
// set to Error. The API emits one ERROR record, "operation panicked", in
// the request's context, with http.request.method, http.route and the
// same exception attributes, even when the client went away: a panic is a
// bug whatever the client did. httpserver records no "handler panicked"
// for it, since the panic never reaches httpserver.
//
// An error from a WriterTo, once the status is written, is the server's
// too. The API records it as an exception event on the request's span,
// sets the span's status to Error, and emits one ERROR record, "response
// failed", in the request's context, with the same attributes. When the
// client went away, it goes on the span only. The client went away when
// the request's context is done, or when a write to the writer the
// WriterTo is handed failed because the connection did: over HTTP/2, such
// a write can fail before the server cancels the request's context. A
// write the server refuses, a body for a status that has none or more
// bytes than the WriterTo's Len, is the server's failure. Then the API
// panics with http.ErrAbortHandler, which
// httpserver and net/http pass through without logging, so httpserver
// records no "handler panicked" for it.
package openapi
