# HTTP

An HTTP service serves an `http.Handler` over TLS with `httpserver`, which
[Serving HTTP](/humus/pr-preview/pr-1628/services/http/serving/) covers first. [Serving
an API with OpenAPI](/humus/pr-preview/pr-1628/services/http/openapi/) then builds that
handler from typed operations on typed paths with `openapi/v3`, which writes
the API's OpenAPI document from the same declarations. [Serving a hypermedia
UI](/humus/pr-preview/pr-1628/services/http/hypermedia/) builds the handler from
pages, fragments and actions on typed paths with `hypermedia` instead, and
writes their
event streams with `sse`, with the code of
[`example/http/inlineedit`](https://github.com/z5labs/humus/tree/main/example/http/inlineedit).

The [shoppingcart walkthrough](/humus/pr-preview/pr-1628/walkthroughs/http/shoppingcart/)
builds a production-ready API on both, step by step, and its code is
[`example/http/shoppingcart`](https://github.com/z5labs/humus/tree/main/example/http/shoppingcart).
The [polls walkthrough](/humus/pr-preview/pr-1628/walkthroughs/http/polls/) builds a
production-ready hypermedia UI, with live results on every replica, and its
code is
[`example/http/polls`](https://github.com/z5labs/humus/tree/main/example/http/polls).

A service on Echo, Gin or Fiber moves to both a few routes at a time, as
[Migrating from Echo](/humus/pr-preview/pr-1628/migrating/http/echo/),
[Migrating from Gin](/humus/pr-preview/pr-1628/migrating/http/gin/) and
[Migrating from Fiber](/humus/pr-preview/pr-1628/migrating/http/fiber/) show, and an
API on Huma a few operations at a time, as
[Migrating from Huma](/humus/pr-preview/pr-1628/migrating/http/huma/) shows.

Next: [Serving HTTP](/humus/pr-preview/pr-1628/services/http/serving/)
