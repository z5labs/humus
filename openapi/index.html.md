# Serving an API with OpenAPI

The [`openapi/v3`](https://pkg.go.dev/github.com/z5labs/humus/openapi/v3)
package serves an API of typed operations, and its OpenAPI document. The end
user's `Init` declares each operation once, on a typed path, with a typed
handler and the codecs of its bodies, builds the API with `openapi.NewAPI`,
and hands it to `httpserver.NewService`, which serves it over TLS as
[Serving HTTP](/humus/http/) describes. The same declarations serve
the requests and write the document, so the two can't disagree.

The import path names the major version of OpenAPI the package follows, and
the package's name is `openapi`. Two packages beside it are opt-in, and
`openapi` uses neither:
[`jsoncodec`](https://pkg.go.dev/github.com/z5labs/humus/openapi/v3/jsoncodec)
reads and writes JSON bodies, and
[`problem`](https://pkg.go.dev/github.com/z5labs/humus/openapi/v3/problem)
answers errors with RFC 9457 problem details. The design behind everything
on this page is in
[Proposal: OpenAPI endpoint DSL](https://github.com/z5labs/humus/discussions/760),
as its revisions
[1](https://github.com/z5labs/humus/discussions/760#discussioncomment-19010521),
[2](https://github.com/z5labs/humus/discussions/760#discussioncomment-19010578),
[3](https://github.com/z5labs/humus/discussions/760#discussioncomment-19010983),
[4](https://github.com/z5labs/humus/discussions/760#discussioncomment-19011032)
and [5](https://github.com/z5labs/humus/discussions/760#discussioncomment-19011110)
amend it.

## An API

This `Init` serves an API of orders. It sources the server's `Settings`,
builds the API with `NewAPI`, and returns the Service that
`httpserver.NewService` builds from both:

```go
// Init is the end user's Init: it sources the server's Settings, builds the
// orders API with NewAPI, and serves it with httpserver.NewService.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	api, err := openapi.NewAPI(s,
		openapi.Info{Title: "Orders", Version: "1.0.0"},
		openapi.Unmatched{
			NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
			MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
		},
		openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
		openapi.CatchAll(http.StatusInternalServerError, serverError, jsoncodec.Problem),
		append(NewOrders().Operations(),
			openapi.Document(openapi.Root().Lit("openapi.json")),
		),
	)
	if err != nil {
		return nil, err // every mistake in the declarations, named
	}

	svc, err := httpserver.NewService(s, settings, api)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}
```

`readSettings` sources `ADDR` and a PEM file named by `TLS_PEM_FILE`, as
[Serving HTTP](/humus/http/#a-service) shows; the variable names are
the service's own. The service's `main` is one call to `httpserver.Main`,
which runs the App with the drain and the grace period a server behind a
load balancer needs:

```go
httpserver.Main(Init)
```

`NewAPI` takes, in order:

- the Setup, whose `LoggerProvider` the API logs through;
- `openapi.Info`, the document's `info`, whose `Title` and `Version` are
  required. `Version` is the API's, not humus's or OpenAPI's;
- `openapi.Unmatched`, the end user's answers to a request no operation
  matches;
- `openapi.RequestErrors`, the answer to a request an operation rejects
  before its handler runs;
- `openapi.CatchAll`, the answer to every error nothing else answers;
- the operations, the document's included;
- any number of `openapi.APIOption`s, given to every operation before its
  own parts. An `openapi.Error` is one.

The answers are arguments rather than options, so an API can't be built
without them, and none has a default: `openapi` refers to no format, such as
JSON, and writes no answer of its own. [Errors](#errors) describes them.

`NewAPI` calls each codec function, and each codec's `Schema`, once, checks
every declaration, and returns one error that names every mistake it found:
a missing `Title` or `Version`, a nil handler, codec or answer, a path or a
parameter it can't serve, two operations at one method and path, two
operations with one `OperationID`, two answers at one status on one
operation, and the rest its doc lists. `Init` returns that error, and the App
exits 1 with it, before it listens. `NewAPI` never panics over a mistake,
so a path or a parameter declared as a package-level variable can't fail its
package's initialization, and no mistake is left to fail every request.

An `*openapi.API` is an `http.Handler`. A service that also serves something
the API doesn't describe, such as a static page, mounts the API next to it
on a mux of its own, with `mux.Handle("/", api)`, and hands the mux to
`NewService`. What is mounted next to the API isn't in its document.

## Paths

A path is built from the root, one segment at a time, from literals and
path parameters:

```go
// The paths, built from the root.
var (
	orders = openapi.Root().Lit("orders") // /orders
	order  = orders.Param(orderID)        // /orders/{orderId}
	lines  = order.Lit("lines")           // /orders/{orderId}/lines
)
```

Each method returns a new `Path`, so a prefix is a value that several paths
and operations share. `Root()` is `/`, and so is the zero `Path`.

There are no path templates. A template is a string with a syntax: braces,
wildcards such as `{name...}`, an anchor such as `{$}`, a trailing slash, a
host, and a router's own extensions. Each of them is something to parse, to
reject, and to keep in step with the document, and a typo in one is found at
the first request, or never. A typed path has none of them to get wrong:

- The OpenAPI path and the router's pattern are both written from the
  segments, such as `/orders/{orderId}/lines`, and the end user writes
  neither.
- A literal is made only of letters, digits, `-`, `.`, `_` and `~`, and isn't
  empty, `.` or `..`, so it never needs escaping, in the document or on the
  wire.
- A path parameter is an `openapi.PathParameter[T]`, a type of its own, so a
  query or header parameter given to `Param` doesn't compile. Its name is an
  ASCII Go identifier, and appears once in its path.
- A path declares its path parameters, and an operation reads every
  parameter of its path, so a path and its operation can't disagree.

`NewAPI` checks every path, and rejects two operations at one method and
path, and two paths that differ only in their parameters' names, such as
`/orders/{id}` and `/orders/{orderId}`, which OpenAPI forbids. The API
redirects nothing: `/orders/` and `//orders` are paths no operation serves.
A path value is unescaped before it is decoded, so `/files/a%2Fb` gives the
handler `a/b`.

## Parameters

A parameter is declared once, usually as a package-level value, with
`openapi.PathParam`, `openapi.QueryParam` or `openapi.HeaderParam`, and its
constraints. Each method returns a copy with one more part, so a declaration
can be shared, and a variant made from it:

```go
// orderID is the path parameter of the operations on one order.
var orderID = openapi.PathParam[string]("orderId").
	Description("The order's identifier.").
	Pattern(orderIDPattern)

// byStatus and pageSize are listOrders' query parameters.
var byStatus = openapi.QueryParam[OrderStatus]("status").
	Description("List only the orders with this status.").
	Enum(Open, Cancelled).
	Default(Open)

var pageSize = openapi.QueryParam[int]("limit").
	Description("The most orders to list.").
	Minimum(1).
	Maximum(100).
	Default(20)

// idempotencyKey is createOrder's header parameter.
var idempotencyKey = openapi.HeaderParam[string]("Idempotency-Key").
	Description("Sent again with the same key, the request creates no second order.").
	MaxLength(64)
```

`T` is a string, bool, int, int64 or float64 type, such as `OrderStatus`, a
string type of the end user's own. A path parameter belongs to its path. A
query or header parameter is read by each operation that gives it as a part,
with `openapi.Query` or `openapi.Header`, and the handler reads every
parameter with `openapi.ParamFrom`, as its own `T`:

```go
func (o *Orders) listOrders(_ context.Context, req openapi.Request[openapi.EmptyBody]) ([]Order, error) {
	status := openapi.ParamFrom(req, byStatus) // Open when the request has no status
	limit := openapi.ParamFrom(req, pageSize)  // 1 to 100, or 20
	o.mu.Lock()
	defer o.mu.Unlock()
	listed := []Order{}
	for _, ord := range o.orders {
		if ord.Status == status && len(listed) < limit {
			listed = append(listed, *ord)
		}
	}
	return listed, nil
}
```

`ParamFrom` panics, naming the parameter and the operation, for a parameter
the operation doesn't read. That is a bug in the handler, and the panic is
answered as [Panics](#panics) describes, rather than handing the handler a
zero value that looks like an answer.

A path parameter is always required. A query or header parameter is
optional unless it is `Required()`, and an absent one is its `Default`, or
`T`'s zero value. A query parameter that appears more than once is
rejected, and a header's lines are read as their values joined by commas, as
HTTP reads them. A header parameter's name is matched whatever its case.

The constraints are `Description`, `Example`, `Enum`, `Pattern`,
`MinLength`, `MaxLength`, `Minimum` and `Maximum`, plus `Required` and
`Default` on a query or header parameter. The types check where a parameter
goes and the values of its constraints, so these don't compile:

- a value of another type in `Enum`, `Default`, `Example`, `Minimum` or
  `Maximum`, which take the parameter's own `T`;
- `Required` or `Default` on a path parameter, which has neither method;
- a path parameter given to `Query`, and a query parameter given to
  `Path.Param`;
- a `T` that isn't one of the five.

`NewAPI` reports the rest: `Pattern`, `MinLength` or `MaxLength` on a type
that isn't a string, `Minimum` or `Maximum` on one that isn't a number, a
`Default` or an `Example` that breaks the parameter's own `Enum`, `Pattern`
or bounds, a float64 value that is NaN or infinite, which the document can't
hold, a parameter given twice to one operation, and a zero parameter.

`Pattern` takes a compiled `*regexp.Regexp`, so a bad expression is a
`regexp.MustCompile` panic where it is declared, never a 400 on every
request. The document lists it as it is written, and OpenAPI reads a pattern
as an ECMA-262 regular expression, so write it in the syntax RE2 and
ECMA-262 share: no flags such as `(?i)`, no `\A` or `\z`, and no POSIX
classes.

A value that doesn't parse as `T` or breaks a constraint, an empty path
parameter, a missing required parameter and a repeated query parameter are
each a `*openapi.RequestError` with status 400, whose `Param` names the
parameter. The handler isn't called, and `RequestErrors` answers it.

## Operations

`openapi.Get`, `Post`, `Put`, `Patch` and `Delete` each take a path, a typed
handler, a decoder for the request body (`Post`, `Put` and `Patch`), an
encoder for the response, and the operation's parts:

```go
// Operations returns the orders API's operations.
func (o *Orders) Operations() []openapi.Operation {
	return []openapi.Operation{
		openapi.Get(orders, o.listOrders, jsoncodec.Encoder,
			openapi.OperationID("listOrders"),
			openapi.Summary("List the orders"),
			openapi.Query(byStatus),
			openapi.Query(pageSize),
		),
		openapi.Post(orders, o.createOrder, openapi.EmptyDecoder, jsoncodec.Encoder,
			openapi.OperationID("createOrder"),
			openapi.Summary("Create an order"),
			openapi.Status(http.StatusCreated),
			openapi.Header(idempotencyKey),
		),
		openapi.Get(order, o.getOrder, jsoncodec.Encoder,
			openapi.OperationID("getOrder"),
			openapi.Summary("Get an order"),
			orderNotFound,
		),
		openapi.Delete(order, o.cancelOrder, openapi.EmptyEncoder,
			openapi.OperationID("cancelOrder"),
			openapi.Summary("Cancel an order"),
			orderNotFound,
		),
		openapi.Post(lines, o.addLine, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("addLine"),
			openapi.Summary("Add a line to an order"),
			openapi.Status(http.StatusCreated),
			orderNotFound,
			invalidLine,
		),
	}
}
```

A handler is a `func(context.Context, openapi.Request[B]) (Resp, error)`.
`req.Body()` is the decoded body, of type `B`, and `openapi.EmptyBody` for an
operation that reads none: `Get` and `Delete` read no body, and
`openapi.EmptyDecoder` reads none for a `Post`. The handler's response is
written by the encoder, and `openapi.EmptyEncoder` writes none. The context
is the request's.

The parts are:

- the operation's metadata in the document: `openapi.Summary`,
  `Description`, `OperationID`, `Tags` and `Deprecated()`;
- the query and header parameters it reads: `openapi.Query` and
  `openapi.Header`;
- its success status: `openapi.Status`. Without it, the status is 204 when
  the encoder's media type is `""`, as `EmptyEncoder`'s is, and 200
  otherwise;
- the answers to its errors: `openapi.Error`, and an `openapi.CatchAll` or
  `openapi.RequestErrors` of its own, which replaces `NewAPI`'s for this
  operation.

A part that several operations share is a value, such as `orderNotFound`
above. `Deprecated()` marks the operation in the document; the API still
serves it.

A request is served in this order:

1. Each path parameter is decoded as its type, then each query and header
   parameter the operation reads. A failure is a `RequestError` with status
   400.
2. When the decoder's media type isn't `""`, a request whose `Content-Type`
   names another media type, parameters aside, or none at all, is a
   `RequestError` with status 415.
3. The decoder reads the body. Its error is a `RequestError` with status 400,
   or 413 when it wraps `*http.MaxBytesError`, as `http.MaxBytesHandler`'s
   reader returns.
4. The handler is called with the decoded `Request`.
5. The encoder's `Encode` runs, before the status is written, so a value
   that can't be encoded is answered by the catch-all, never as a 200 with a
   broken body.
6. The status is written, with `Content-Type` set to the encoder's media
   type, unless that is `""`.
7. The `io.WriterTo` that `Encode` returned writes the body.

## Codecs

Every body names its codec, and nothing is assumed. humus doesn't know what
an API's bodies are, so it doesn't guess: an operation without its codecs
doesn't compile, and the codecs on each operation say what it reads and
writes. The cost is that a JSON-only API names `jsoncodec` on every
operation.

An `openapi.Decoder[T]` reads request bodies of type `T` in one media type,
and an `openapi.Encoder[T]` writes them. Each has three methods:

- **`MediaType() string`**, such as `application/xml`. A decoder's is checked
  against the request's `Content-Type`, and an encoder's is the response's.
  `""` is no body: no `Content-Type` is checked or written, and the document
  lists no content.
- **`Schema() (openapi.Schema, error)`**, the body's JSON Schema, as JSON,
  which the document lists under the media type. `NewAPI` calls it once, and
  returns its error, so a codec that can't handle `T` says so there, rather
  than on every request. A nil `Schema` lists the media type alone, as for
  raw bytes.
- **`Decode(r *http.Request) (T, error)`**, or
  **`Encode(v T) (io.WriterTo, error)`**. `Decode`'s error is answered with a
  400, or a 413. `Encode` runs before the status is written, and its error is
  answered by the catch-all; the `WriterTo` it returns runs after.

`Decoder` and `Encoder` are separate interfaces. With one interface for both
directions, two generic codecs swapped between the body and the response
compiled, and a codec that only writes, given for a request body, failed
every request with a 400. With two, both are compile errors.

### jsoncodec

`jsoncodec.Decoder` and `jsoncodec.Encoder` are `T`'s codecs as
`application/json`, and `jsoncodec.Problem` is `T`'s encoder as
`application/problem+json`, for RFC 9457 problem details. They handle the
details that are easy to get wrong:

- It decodes one JSON value. An empty body, malformed JSON, a value of the
  wrong type and data after the value are errors, which the API answers with
  a 400. Unknown fields are ignored.
- An error from `http.MaxBytesHandler`'s reader stays wrapped, so the API
  answers it with a 413.
- Its `Schema` reflects `T`'s json tags with `openapi.SchemaOf`, and rejects
  a `T` that JSON can't decode into or encode, such as a channel, a function
  or an `io.Reader`, so the mistake is a `NewAPI` error rather than every
  request's.
- It encodes into a buffer, so a value that can't be encoded is answered by
  the catch-all, and the response has a `Content-Length`.

`jsoncodec` is opt-in. It uses only `openapi`'s exported API, as a third
party's codec would, and is named on each operation like one.

### Writing a codec

A codec of one's own implements the three methods. This XML codec reads and
writes `T` as `application/xml`, with a schema reflected from `T`'s xml tags:

```go
// xmlCodec reads and writes T as XML, as application/xml: a second format,
// whose schema SchemaOf reflects by T's xml tags.
type xmlCodec[T any] struct{}

// xmlDecoder is T's decoder as application/xml.
func xmlDecoder[T any]() openapi.Decoder[T] { return xmlCodec[T]{} }

// xmlEncoder is T's encoder as application/xml.
func xmlEncoder[T any]() openapi.Encoder[T] { return xmlCodec[T]{} }

func (xmlCodec[T]) MediaType() string { return "application/xml" }

func (xmlCodec[T]) Schema() (openapi.Schema, error) { return openapi.SchemaOf[T]("xml") }

func (xmlCodec[T]) Decode(r *http.Request) (T, error) {
	var v T
	err := xml.NewDecoder(r.Body).Decode(&v)
	return v, err
}

func (xmlCodec[T]) Encode(v T) (io.WriterTo, error) {
	b, err := xml.Marshal(v)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}
```

It is given like any other codec, uninstantiated:
`openapi.Put(p, h, xmlDecoder, xmlEncoder)`. One operation can read one
format and write another, such as `jsoncodec.Decoder` with `xmlEncoder`. An
operation's body has one codec, though, and there is no content negotiation
by `Content-Type` or `Accept`: a body in a second media type is a second
operation, at a path of its own.

Whether a codec buffers or streams is the codec's to say. A decoder that
buffers reads the whole body in `Decode`, as the XML codec does. One that
streams returns a value that reads the body as the handler reads it, such
as the request's body itself. This decoder hands an upload's handler the
body unread, as an `io.Reader`:

```go
// upload is the decoder of a body that is streamed: Decode hands the
// handler the request's body, unread, as an io.Reader.
type upload struct{}

func uploadDecoder() openapi.Decoder[io.Reader] { return upload{} }

func (upload) MediaType() string { return "application/octet-stream" }

func (upload) Schema() (openapi.Schema, error) { return nil, nil }

func (upload) Decode(r *http.Request) (io.Reader, error) { return r.Body, nil }
```

The handler copies the body to where it is kept as the client sends it:

```go
// putFile stores the request's body as it reads it: the decoder handed it
// over unread.
func (f Files) putFile(ctx context.Context, req openapi.Request[io.Reader]) (openapi.EmptyBody, error) {
	name := openapi.ParamFrom(req, fileName)
	return openapi.EmptyBody{}, f.Objects.Put(ctx, name, req.Body())
}
```

The body is read after `Decode` has returned, so a body that fails partway,
or runs past `http.MaxBytesHandler`'s limit, fails in the handler, and its
error is the handler's, answered by an `Error` or the catch-all, not by
`RequestErrors`.

An encoder that buffers returns its encoded bytes, such as a
`*bytes.Reader`. One that streams returns a `WriterTo` that writes as it
goes. The `WriterTo` runs on the request's goroutine, with the request's
context still live, so it may do more work once it has written the body.
This download copies an object from a store such as S3 to the client, then
records that it was downloaded:

```go
// download is what getFile returns: what to copy, and what to do once it
// has been copied.
type download struct {
	body io.ReadCloser // the object's body
	done func() error  // runs after the last byte is written
}

func (f Files) getFile(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (download, error) {
	name := openapi.ParamFrom(req, fileName)
	body, err := f.Objects.Get(ctx, name)
	if err != nil {
		return download{}, err // no status yet: answered by an Error or the catch-all
	}
	return download{body: body, done: func() error {
		return f.Objects.MarkDownloaded(ctx, name) // ctx is still the request's
	}}, nil
}

func downloadEncoder() openapi.Encoder[download] { return downloadCodec{} }

// downloadCodec writes a download as application/octet-stream. Its WriterTo,
// the download itself, has no Len, so the response is chunked, and a
// failure after the last byte is seen by the client.
type downloadCodec struct{}

func (downloadCodec) MediaType() string { return "application/octet-stream" }

func (downloadCodec) Schema() (openapi.Schema, error) { return nil, nil }

func (downloadCodec) Encode(d download) (io.WriterTo, error) { return d, nil }

func (d download) WriteTo(w io.Writer) (int64, error) {
	defer func() { _ = d.body.Close() }()
	n, err := io.Copy(w, d.body)
	if err != nil {
		return n, err // the object failed partway: the response is cut short
	}
	return n, d.done() // a failed update: the same
}
```

The handler's context is captured in the value it returns, and is still the
request's when `WriteTo` runs. The `WriterTo` is handed a writer that
flushes each write, so each part reaches the client as it is written. Both
operations are declared like any other:

```go
		openapi.Put(file, f.putFile, uploadDecoder, openapi.EmptyEncoder,
			openapi.OperationID("putFile"),
			openapi.Summary("Upload a file"),
		),
		openapi.Get(file, f.getFile, downloadEncoder,
			openapi.OperationID("getFile"),
			openapi.Summary("Download a file"),
		),
```

An error the `WriterTo` returns comes after the status, so it can't change
the response; [A failure after the status](#a-failure-after-the-status)
describes what it does instead.

### Why codecs are function arguments

An operation takes each codec as a function that returns it, such as
`jsoncodec.Encoder`, rather than as a codec value. So a generic codec is
passed uninstantiated, and the compiler infers its type argument from the
handler: `openapi.Get(order, o.getOrder, jsoncodec.Encoder)` writes an
`Order` because `getOrder` returns one, and the end user never writes
`jsoncodec.Encoder[Order]`. A codec that doesn't fit the handler's types
doesn't compile, and the compiler's message names both types.

Codecs are taken only as function arguments, never as a struct's fields.
Go 1.27.0's compiler has a bug: given a generic function from another
package, such as `jsoncodec.Encoder`, uninstantiated, as a field of a struct
literal, it stops with an internal compiler error rather than inferring the
type argument. The same function given as an argument compiles.
An `openapi.Operation` is therefore built by a call, `openapi.Get` and the
rest, rather than as a struct literal of handler and codecs, and a part that
takes an encoder, such as `openapi.Error`, takes it as an argument too. An
end user who keeps codecs in a struct of their own, to build operations
from, instantiates them there, as `jsoncodec.Encoder[Order]`.

## Errors

Every error has an answer the end user chose, written by an encoder the end
user named:

- **A rejected request** is a `*openapi.RequestError`, answered by
  `RequestErrors`, with the `RequestError`'s status, and the handler isn't
  called.
- **The handler's error** is answered by the first `openapi.Error[E]` whose
  `E` matches it, with `errors.As`: the operation's own first, then the
  API's. An operation's `Error` for an `E` replaces the API's.
- **Anything else** is answered by the catch-all, the operation's
  `CatchAll` or `NewAPI`'s, with its status and the error itself if it
  matches `E`, or `wrap(err)` if not.
- **A request no operation matches** is answered by `Unmatched`.

### Error

`openapi.Error[E](status, enc)` answers an error that matches `E` with
`status` and `E`, written by `enc`. Given to `NewAPI`, it answers on every
operation, and every operation's document lists it. An error that only some
operations return isn't API-wide: it is a shared value, given to those
operations, so the document lists it where it can happen:

```go
// NotFound is the answer for an order that doesn't exist: a 404, as an RFC
// 9457 problem detail.
type NotFound struct{ problem.Details }

// Invalid is the answer for a body that decodes, but that the API won't
// accept, such as a quantity below 1: a 422. It isn't a 400, which is
// RequestErrors' answer for a body that doesn't decode.
type Invalid struct{ problem.Details }

// Parts that several operations share.
var (
	// orderNotFound answers a NotFound, on the operations that address an
	// order.
	orderNotFound = openapi.Error[NotFound](http.StatusNotFound, jsoncodec.Problem)

	// invalidLine answers an Invalid, on the operation that reads a line.
	invalidLine = openapi.Error[Invalid](http.StatusUnprocessableEntity, jsoncodec.Problem)
)
```

The handler returns a `NotFound`, wrapped or not, and the API answers it:

```go
// find returns the order whose ID is id, or a NotFound naming id.
func (o *Orders) find(id string) (*Order, error) {
	ord, ok := o.byID[id]
	if !ok {
		return nil, NotFound{problem.Details{
			Type:   "about:blank",
			Title:  http.StatusText(http.StatusNotFound),
			Status: http.StatusNotFound,
			Detail: "There is no order " + id + ".",
		}}
	}
	return ord, nil
}
```

An answer from an `Error` isn't logged: it is an answer the end user
planned.

### CatchAll

`openapi.CatchAll(status, wrap, enc)` answers an error that no `Error`
matched. `NewAPI` requires one, and an operation's own replaces it. An error
the catch-all answers is one the end user didn't plan for, so the API
records it, as [Telemetry](#telemetry) describes, and `wrap` decides what
the client sees. It should not put the error's text in the body, which may
hold a database's message or a secret; the record has it:

```go
// serverError is the catch-all's wrap. Its answer is fixed, and never
// carries err's text, which the API records instead.
func serverError(err error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
		Detail: "The server failed to answer the request.",
	}
}
```

The catch-all also answers an `Encode` that fails, and a panic, which
[Panics](#panics) describes. When the catch-all's own encoder fails, its
status is sent alone.

### RequestErrors

`openapi.RequestErrors(wrap, enc)` answers every `*openapi.RequestError`
with its `Status` and `wrap`'s value. `NewAPI` requires one, and an
operation's own replaces it. A `RequestError` has:

- `Status`: 400 for a parameter or a body that doesn't decode, 413 for a
  body over `http.MaxBytesHandler`'s limit, 415 for a `Content-Type` the
  operation's decoder doesn't read, or none, and 401 and 403 for a request
  that its [Security](#security) refused;
- `Param`: the parameter that doesn't decode, for a 400 about one, and `""`
  otherwise;
- `Err`: why, the parameter's error or the decoder's, or
  `auth.ErrUnauthenticated` for a 401 and `auth.ErrForbidden` for a 403,
  never a verifier's own error.

The document lists `RequestErrors`' answer at 400 on an operation that reads
a parameter or a body, at 413 and 415 on one that reads a body, at 401 on one
with requirements, and at 403 on one whose requirements name a scope. Its
answers aren't logged: a request the API rejects is the client's mistake.

### Unmatched

`openapi.Unmatched` holds two `http.Handler`s, and `NewAPI` requires both:

- `NotFound` answers a request whose path no operation serves.
- `MethodNotAllowed` answers a request whose path an operation serves, but
  not with the request's method. The API sets the `Allow` header to the
  path's methods before it calls the handler.

The package writes nothing of its own for either. `Unmatched` is a struct,
so its two handlers are named and can't be swapped; a nil one is a `NewAPI`
error. Their answers aren't in the document, and aren't logged. They are the
end user's own handlers, so a panic in one isn't recovered by the API.

### Problem details

The `problem` package answers errors with RFC 9457 problem details, in any
encoding the end user names. It is opt-in, and `openapi` never uses it:

- **`problem.Details`** is RFC 9457's problem detail, with `json` and `xml`
  tags that name its members in both serializations the RFC defines. Its
  `Error` method makes it an error, so a type that embeds it, such as
  `NotFound` above, is an error of its own that `openapi.Error` matches.
- **`problem.FromRequestError`** is a wrap for `RequestErrors`: type
  `about:blank`, the status's name as the title, and the `RequestError`'s
  error as the detail.
- **`problem.Handler(status, enc)`** is a handler for `Unmatched`, which
  answers with `status` and an `about:blank` problem detail, keeping a 405's
  `Allow`.

Each takes any encoder of `problem.Details`: `jsoncodec.Problem` writes
`application/problem+json`, and an XML codec of the end user's own could
write `application/problem+xml`.

## Security

`openapi.Security(reqs...)` is what a request must have to be served: one of
its requirements, which the [`auth`](https://pkg.go.dev/github.com/z5labs/humus/auth)
package declares, such as `auth.Require(bearer)` or
`auth.RequireScopes(bearer, "carts:write")`. Given to `NewAPI`, it is every
operation's, and the document's. Given to an operation or to `Document`, it
replaces the API's, and `openapi.Security(auth.Disabled())` makes what it is
given to public. An API without `Security` authenticates nothing.

An operation with requirements authenticates the request before it decodes a
parameter or reads the body. A request no requirement authenticated, and one
that lacked a scope, are `RequestError`s at 401 and 403, which
`RequestErrors` answers after the API has set `WWW-Authenticate`. A
verifier's error that doesn't wrap `auth.ErrUnauthenticated` is the server's,
and is answered as a handler's error is, by the first `Error` that matches
it or by the catch-all. The document lists every scheme under
`components.securitySchemes`, and each operation's requirements under its
`security`.

[Authentication and authorization](/humus/auth/) covers schemes and
their principals, requirements and scopes, `auth.Disabled()` and
`auth.Anonymous()`, the answers and challenges, verifying JWTs with
`auth/jwt`, answering a `*jwt.KeySetError`, and testing an API that requires
tokens.

## Panics

A panic of the end user's code, on the goroutine serving an operation's
request, is answered as the document says, so the API keeps its contract
even when that code has a bug. The API recovers a panic of the decoder's
`Decode`, of the handler, of `ParamFrom`, of the encoder's `Encode`, and of
the `Len` of the `WriterTo` it returns, which all run before the status is
written. The operation's catch-all answers it, with its status and `wrap` of
an error that doesn't carry the panic's value, so the value never reaches
the client, and the response is the one the document lists for the
catch-all. The API records the panic, with its stack, and keeps serving
every other request.

Recovering has a cost. Whatever the panicking code left half-changed, such
as a map it was halfway through writing, or a lock it took without a
deferred unlock, stays that way for the requests that follow. Nothing is
rolled back. A recovered panic is a bug to fix, not an answer to rely on,
and the `operation panicked` record is there so it gets fixed.

These aren't recovered by the API:

- **A panic of the `WriterTo`'s `WriteTo`** comes after the status, so it
  can't be answered. The API records it as it records any other panic, and
  cuts the response short, as a `WriterTo`'s error does.
- **A panic with `http.ErrAbortHandler`**, which the end user's code uses to
  abort a response on purpose, passes through to `httpserver`, which aborts
  the response without recording it.
- **A panic while answering**, in a `wrap` or in the encoder of an `Error`,
  a `CatchAll` or `RequestErrors`, which would panic again if it answered its
  own panic, and **a panic in `Unmatched`'s handlers**, pass through to
  `httpserver`, which records it and aborts the response, as
  [Serving HTTP](/humus/http/#panics) describes.
- **A panic on a goroutine the end user's code starts** can't be recovered
  by anything but that goroutine, and crashes the process, as it would
  anywhere in Go.

## A failure after the status

The status is written before the first byte of the body, as HTTP requires,
so an error from the `WriterTo` comes once the status, and whatever part of
the body was written, have already gone out. The API can't change the
response, so it aborts it: on HTTP/2 the stream is reset, and on HTTP/1.1 the
connection is closed before the final chunk. The client sees a failed
response rather than one that looks complete. The error is the server's,
unless the client went away, and the API records it as `response failed`.

Whether a failure is seen depends on `Content-Length`. When the `WriterTo`
has a `Len() int` method, as a `*bytes.Reader` does, the API sets
`Content-Length` to it, so `Len` must be the number of bytes `WriteTo`
writes: a body longer than its `Len` fails, and one shorter is cut short.
Otherwise the response is chunked over HTTP/1.1, and has no length on
HTTP/2. A response with a `Content-Length` that fails after its last byte
has the length it was promised, and looks complete to the client. So a
`WriterTo` that does work after the body, as the download above does, leaves
`Len` out, so that a failure is seen.

A status that depends on that work needs the body copied elsewhere first.
`Encode` runs before the status: it copies the object to a temporary file,
say, does the work, then returns a `WriterTo` over the copy, and an error
along the way is answered by the catch-all, with a status of its own.

There are no trailers. A failure after the status can only cut the response
short.

## The document

`openapi.Document(path)` returns the operation that serves the API's OpenAPI
document, at `path`, with GET, as `application/json`. An API whose
operations include no `Document` serves no document, and a `Document` whose
path has a parameter, or a second `Document`, is a `NewAPI` error. `NewAPI`
builds the document from the same declarations that serve the API's
requests, and encodes it once, so every request for it gets the same bytes.

The document is OpenAPI 3.2.0, the latest minor version of OpenAPI 3. The
import path names the major version, and within it the package follows the
latest minor version, since OpenAPI's minor versions add to the one before
rather than break it. When OpenAPI publishes a new 3.x, humus moves the
document to it in a release of its own, whose notes say so. The end user's
code doesn't change: the served document states the new version, and a
golden file of it changes with it. The document's `info` is
`openapi.Info`'s. Each operation is listed at its path, under its method,
with:

- its metadata: `Summary`, `Description`, `OperationID`, `Tags` and
  `Deprecated`;
- its parameters, the path's and those it gives to `Query` and `Header`,
  with their constraints;
- its request body, required, under the decoder's media type, with the
  decoder's `Schema`;
- its responses: the success status, under the encoder's media type with
  its `Schema`; each `Error`, the operation's own and the API's, with its
  encoder's; `RequestErrors`' answer at 400, 413 and 415 where the operation
  can answer with them; and the catch-all at its status.

What each body is listed as is its codec's to say:

| Codec | Listed as |
|---|---|
| `jsoncodec.Decoder`, `jsoncodec.Encoder` | `application/json`, with `T`'s schema, reflected from its json tags |
| `jsoncodec.Problem` | `application/problem+json`, with `T`'s schema, reflected from its json tags |
| a codec whose `Schema` is nil, such as `upload` and `downloadCodec` above | its media type alone, with no schema |
| a codec whose media type is `""`, such as `EmptyDecoder` | no request body, or a response with no content |
| a codec of one's own, such as `xmlCodec` | its media type, with the schema it returns, such as `openapi.SchemaOf[T]("xml")` |

Schemas are inline, since one Go type can have a different schema under
each codec. `openapi.SchemaOf[T](tag)` reflects `T`'s schema from its fields,
naming each property by `tag`, for a codec whose bodies are a Go type's
fields; it rejects a `T` that holds itself, which an inline schema can't. A
codec adds what its format needs, such as OpenAPI's `xml` object, to the
schema it returns. The document doesn't describe itself, the requests
`Unmatched` answers, or anything mounted next to the API.

## Telemetry

The API adds no spans and no metrics of its own: `httpserver` gives every
request a server span and its metrics, as
[Serving HTTP](/humus/http/#telemetry) describes, and the API names
the span after the operation's route, such as `GET /orders/{orderId}`. A 404
and a 405 have no route, so their spans are named after the method alone.
The package never reads or sets OpenTelemetry's globals. It logs through the
Setup's `LoggerProvider`, under the logger name
`github.com/z5labs/humus/openapi/v3`, and emits three records, each one
`ERROR`, in the request's context, so each carries the request span's trace
and span IDs:

| Record | When | Attributes |
|---|---|---|
| `request failed` | the catch-all answered an error | `http.request.method`, `http.route`, `error.type`, `exception.message` |
| `operation panicked` | the API recovered a panic of the end user's code | `http.request.method`, `http.route`, `exception.type`, `exception.message`, `exception.stacktrace` |
| `response failed` | a `WriterTo` failed after the status was written | `http.request.method`, `http.route`, `error.type`, `exception.message` |

Each is also an exception event on the request's span, and `operation
panicked` and `response failed` set the span's status to Error. When the
request's context is done because the client went away, `request failed`
and `response failed` go on the span only: the failure is the client's
leaving. `operation panicked` is emitted even then, because a panic is a bug
whatever the client did. `httpserver` records no `handler panicked` for a
panic the API recovered, or for a response the API aborted. Answers from an
`Error`, from `RequestErrors` and from `Unmatched` aren't logged.

## Testing

An API is tested whole, as the service that serves it, inside
`testing/synctest`, with `httpservertest`, as
[Serving HTTP](/humus/http/#testing) describes for any HTTP
service: `httpservertest.Start` runs the end user's `Init` on
`httpserver.Main`'s defaults, on a humustest Host, and
`httpservertest.NewClient` speaks HTTP/2 to it, trusting a CA that
[`humustest.NewCA`](/humus/testing/#tls) generates for the test.
The requests go through the API exactly as production's do: routing,
parameters, codecs, answers and the document.

The shoppingcart's `app_test.go` does it this way. Its `start` helper runs
the service's `Init`, with every variable it reads, on a new Host, beside
the test's identity provider, which serves a JSON Web Key Set from a handler
of the test's own and signs the tokens the test sends. A `newFixture` helper
makes the Host, its files and the variables:

```go
func start(t *testing.T, init func(context.Context, *humus.Setup) ([]humus.Service, error)) *service {
	t.Helper()
	f := newFixture(t)

	provider := newIdP(t)
	stopIdP := provider.serve(t, f.h.Host(), idpHost, f.idpCert)
	<-f.h.Listening(idpHost)

	p, status := httpservertest.Start(t, init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(f.env),
	)
	select {
	case <-f.h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("the cart exited %d: %v", st.Code(), st.Cause())
	}
	return &service{
		h:       f.h,
		ca:      f.ca,
		client:  httpservertest.NewClient(f.h, f.ca.Pool()),
		p:       p,
		status:  status,
		idp:     provider,
		stopIdP: stopIdP,
		token:   provider.token(t, shopper, endpoint.ScopeWrite),
	}
}
```

Postgres can't run on humustest's Host, so most tests swap an in-memory
store in for it, and a catalog of fixed prices in for the catalog's client:
the service's `Init` calls an unexported `initWith` with the functions that
open its store and its catalog, and the tests pass ones that open a map.
Each test then sends requests and checks the answers, and stops the service
with SIGTERM, checking that it exits 0:

```go
func TestInit_answers_unmatched_requests_with_a_problem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))

		isProblem(t, s.do(t, http.MethodGet, "/orders", "", ""), http.StatusNotFound)

		r := s.do(t, http.MethodPut, "/carts", "", "")
		isProblem(t, r, http.StatusMethodNotAllowed)
		require.Equal(t, http.MethodPost, r.header.Get("Allow"))

		s.stop(t)
	})
}
```

The document is tested the same way. The test fetches it from the running
service, compares it with a golden file, so a change to any declaration
shows up as a change to the document in review, and validates it against the
OpenAPI Initiative's 3.2 schema, which `openapi/v3` keeps in its testdata,
so nothing is read from the network:

```go
	t.Run("as the golden file", func(t *testing.T) {
		var indented bytes.Buffer
		require.NoError(t, json.Indent(&indented, doc, "", "  "))
		indented.WriteByte('\n')
		want, err := os.ReadFile(filepath.Join("testdata", "openapi.json"))
		require.NoError(t, err)
		require.Equal(t, string(want), indented.String())
	})

	t.Run("valid against OpenAPI 3.2", func(t *testing.T) {
		require.NoError(t, validate(doc))
	})
```

A handler is a plain function, so the business rules behind one can be
unit-tested on their own too, with a store of the test's own. A handler
that reads parameters is tested through the API, since only the API decodes
them: a `Request` the test builds itself has none, and `ParamFrom` panics on
it.

## The shoppingcart

[`example/shoppingcart`](https://github.com/z5labs/humus/tree/main/example/shoppingcart)
is humus's reference service: an API for shopping carts, on Postgres, built
on `openapi/v3`, `jsoncodec` and `problem`, and served by `httpserver`. It
is a module of its own, so pgx, uuid and
testcontainers stay out of the core module's dependencies, and it is built
and tested against the humus in the same commit. It has three packages:

- **`endpoint`**, the API: six typed operations on carts and their items,
  their paths and parameters, the `NotFound` and `Invalid` answers, and the
  `Store` interface they are served on.
- **`database`**, the `Store` on a `pgxpool.Pool`, and the schema.
- **`app`**, the `Init`. It sources its settings with `config.Struct`,
  opens its pool through the Host and closes it with `OnStop`, builds the API
  with `NewAPI`, and serves it with `httpserver.NewService`.

Every operation requires a JWT bearer token, which `auth/jwt`'s `Verifier`
checks against the identity provider's key set, fetched by a
`jwt.RemoteKeySet` with an `httpclient` client for the key set's origin.
Changing a cart requires the scope `carts:write` too, which the token's
claims, a type of the service's own, report through `auth.Scoped`. The
document is public, served with `Document(path,
openapi.Security(auth.Disabled()))`. A `*jwt.KeySetError`, a key set
that is down before any key was fetched, is answered with a 503
`Unavailable` by an `openapi.Error`, as a catalog or a database that is
down is. A cart belongs to the subject of the token that created it:
a token with no `sub` is a 401, and another subject's token gets a 404 for
the cart. [Authentication and authorization](/humus/auth/)
describes each piece.

Its tests run the whole service as [Testing](#testing) shows, its golden
document is `app/testdata/openapi.json`, and a test behind the
`testcontainers` build tag runs it once against Postgres in a container, on
the OS Host, beside the test's identity provider, in a CI job of its own.

### pgx's ambient leaks

pgx dials through the Host: the shoppingcart's `Init` sets the pool's
`DialFunc` to the Host's `Dial`, and its `LookupFunc` to one that returns
the host unresolved, so the Host's `Dial` resolves the name too. Everything
else pgx reads, it reads from the real process, whatever Host the App runs
on, as [Ambient leaks](/humus/lifecycle/#ambient-leaks) lists:

- `pgxpool.ParseConfig`, which parses the URL in the file
  `DATABASE_URL_FILE` names, fills in whatever the URL leaves out from the
  real `PG*` variables, such as `PGHOST`, `PGUSER`, `PGPASSWORD`,
  `PGSSLMODE` and `PGSERVICE`, not from the Process's environment.
- With no password in the URL or in `PGPASSWORD`, it reads the password from
  the real `~/.pgpass`, or the file `PGPASSFILE` names. A `PGSERVICE`, or a
  service in the URL, is read from the real `pg_service.conf`.
- The files `sslcert`, `sslkey` and `sslrootcert` name, and
  `~/.postgresql`'s defaults for them, are read from the real filesystem,
  not the Host's.
- A URL with no host connects to a Unix socket, in `/tmp` or the like,
  through the Host's `Dial`, which may not reach it.

So the URL sets every field the service uses, host, port, user, password,
database and sslmode, and nothing is left for pgx to find. The file holds:

```text
postgres://carts:secret@db.internal:5432/carts?sslmode=verify-full
```

The tests do the same, with a password generated for each test, so what a
test runs doesn't depend on the environment or the home directory of the
machine it runs on.

## Migration from `rest.Run`

`openapi/v3` replaces the REST half of `rest.Run` from before the reset,
and bedrock's REST DSL beneath it. Its server half is `httpserver`, as
[Serving HTTP](/humus/http/#migration-from-restrun) describes.

| Before the reset | With `openapi/v3` |
|---|---|
| `rest.Run(ctx, rest.Title(…), rest.Version(…), rest.Handle(route))` | `httpserver.Main(Init)`, with `Init` calling `openapi.NewAPI(s, info, unmatched, requestErrors, catchAll, ops)` and `httpserver.NewService` |
| `bedrockrest.Build(opts...)`, a `bedrock.Builder[http.Handler]` | `openapi.NewAPI`, an `*openapi.API` and an error |
| `Title`, `Version`, `APIDescription`; defaults `"API"` and `"0.0.0"` | `openapi.Info`; `Title` and `Version` required |
| `SpecPath`, default `/openapi.json` | `openapi.Document(path)`, an operation; no document unless the operations include one |
| an OpenAPI 3.0.3 document | an OpenAPI 3.2.0 document, from `github.com/z5labs/humus/openapi/v3`; following the latest 3.x |
| `rest.GET("/carts/{cartId}", h)` … `DELETE`, a path template | `openapi.Get(cart, h, enc, parts...)` … `Delete`, with `cart := openapi.Root().Lit("carts").Param(cartID)` |
| `cartID.Read(ep)` for a path parameter | nothing: the path declares it |
| `p.Read(ep)` for a query or header parameter | `openapi.Query(p)` or `openapi.Header(p)`, a part |
| `Summary(s, ep)`, `EndpointDescription`, `Tags`, `OperationID`, `MarkDeprecated` | `openapi.Summary(s)`, `Description`, `Tags`, `OperationID`, `Deprecated()`, as parts |
| `PathParam[T any](name, opts...)`, an unsupported `T` failing every request with 400 | `openapi.PathParam[T ParamValue](name)`, a compile error, with constraints as methods |
| `Pattern(string)`, a bad expression failing every request | `.Pattern(*regexp.Regexp)` |
| `ParamFrom` returns the zero value for a parameter the endpoint didn't read | it panics, naming the parameter |
| chi, `chi.URLParam` | chi inside the API, read with `openapi.ParamFrom`; spans named after the route |
| `ReadJSON[T]` | `jsoncodec.Decoder`, given to the operation; plus 415, 413 and trailing data |
| `WriteJSON[Resp](status)`, written with the encoder's error ignored | `jsoncodec.Encoder`, and `openapi.Status`; encoded before the status is written |
| `ReadBinary(ct)`, `WriteBinary(status, ct)` | a decoder or encoder of `io.Reader` in that media type, the end user's own |
| `ReadFormFile`, `ReadFormField` | dropped |
| a 204 with `WriteBinary(204, "", …)` and an empty reader | an `openapi.EmptyBody` response, with `openapi.EmptyEncoder`: 204 by default |
| `ErrorJSON[E](status, ep)` on every route | `openapi.Error[E](status, jsoncodec.Problem)`, or `jsoncodec.Encoder` for `application/json`, given to an operation or to `NewAPI` |
| `CatchAll[E](status, wrap, ep)` on every route | `openapi.CatchAll(status, wrap, jsoncodec.Problem)`, given to `NewAPI`, and to an operation that overrides it |
| a bad parameter or body: `{"param", "error"}` and a 400 outside the document | a `*openapi.RequestError`, answered by `RequestErrors`, such as `RequestErrors(problem.FromRequestError, jsoncodec.Problem)`, and a documented 400, 413 or 415 |
| chi's plain-text 404 and 405 | the end user's `openapi.Unmatched` handlers, such as `problem.Handler`, with `Allow` on the 405 |
| the example's own problem type | `problem.Details`, embedded |
| an error the catch-all answered: lost | an ERROR record and a span event |
| a panic: bedrock's `RecoverPanics` around the runtime | the catch-all, recorded |
| `otelhttp.NewHandler(h, "rest")` on the globals, spans named `rest` | `httpserver`'s instrumentation |
| `example/rest/shoppingcart` in the root module; `os.Getenv("DATABASE_URL")` and `pgxpool.New` in `main` | the `example/shoppingcart` module; `Init` sources the URL, the pool dials through the `Host`, and `OnStop` closes it |
| the example's `specs/` (a hand-written `api.yaml`, sequence diagrams) | dropped; the served document is the golden file |
