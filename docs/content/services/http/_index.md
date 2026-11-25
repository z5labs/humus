---
title: HTTP
linkTitle: HTTP
description: >
  Serving HTTP with the httpserver package, and HTTP/3 beside it with
  http3server, then serving an API of typed
  operations, with its OpenAPI document, with the openapi/v3 package, and a
  hypermedia UI with the hypermedia and sse packages.
type: docs
weight: 10
---

An HTTP service serves an `http.Handler` over TLS with `httpserver`, which
[Serving HTTP]({{< relref "/services/http/serving" >}}) covers first, and
[Serving HTTP/3]({{< relref "/services/http/serving-http3" >}}) serves the
same handler over HTTP/3 beside it with the http3 module's `http3server`. [Serving
an API with OpenAPI]({{< relref "/services/http/openapi" >}}) then builds that
handler from typed operations on typed paths with `openapi/v3`, which writes
the API's OpenAPI document from the same declarations. [Serving a hypermedia
UI]({{< relref "/services/http/hypermedia" >}}) builds the handler from
pages, fragments and actions on typed paths with `hypermedia` instead, and
writes their
event streams with `sse`, with the code of
[`example/http/inlineedit`](https://github.com/z5labs/humus/tree/main/example/http/inlineedit).

The [shoppingcart walkthrough]({{< relref "/walkthroughs/http/shoppingcart" >}})
builds a production-ready API on both, step by step, and its code is
[`example/http/shoppingcart`](https://github.com/z5labs/humus/tree/main/example/http/shoppingcart).
The [polls walkthrough]({{< relref "/walkthroughs/http/polls" >}}) builds a
production-ready hypermedia UI, with live results on every replica, and its
code is
[`example/http/polls`](https://github.com/z5labs/humus/tree/main/example/http/polls).

A service on Echo, Gin or Fiber moves to both a few routes at a time, as
[Migrating from Echo]({{< relref "/migrating/http/echo" >}}),
[Migrating from Gin]({{< relref "/migrating/http/gin" >}}) and
[Migrating from Fiber]({{< relref "/migrating/http/fiber" >}}) show, and an
API on Huma a few operations at a time, as
[Migrating from Huma]({{< relref "/migrating/http/huma" >}}) shows.
