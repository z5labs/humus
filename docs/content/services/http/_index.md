---
title: HTTP
linkTitle: HTTP
description: >
  Serving HTTP with the httpserver package, then serving an API of typed
  operations, with its OpenAPI document, with the openapi/v3 package.
type: docs
weight: 10
---

An HTTP service serves an `http.Handler` over TLS with `httpserver`, which
[Serving HTTP]({{< relref "/services/http/serving" >}}) covers first. [Serving
an API with OpenAPI]({{< relref "/services/http/openapi" >}}) then builds that
handler from typed operations on typed paths with `openapi/v3`, which writes
the API's OpenAPI document from the same declarations.

The [shoppingcart walkthrough]({{< relref "/walkthroughs/http/shoppingcart" >}})
builds a production-ready API on both, step by step, and its code is
[`example/http/shoppingcart`](https://github.com/z5labs/humus/tree/main/example/http/shoppingcart).
