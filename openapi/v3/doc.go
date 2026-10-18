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
// bounds, a parameter given twice to one operation, and a zero parameter.
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
// # Errors
//
// Every error has an answer the end user chose, written by an encoder the
// end user named:
//
//   - A RequestError is answered by [RequestErrors], with its status, and
//     the handler isn't called.
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
//     400 when the operation reads a parameter or a body, and at 413 and 415
//     when it reads a body; and the catch-all at its status.
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
// GET /carts/{cartId}. The package never reads or sets OpenTelemetry's
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
package openapi
