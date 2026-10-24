# HTTP

An HTTP service serves an `http.Handler` over TLS with `httpserver`, which
[Serving HTTP](/humus/pr-preview/pr-1329/services/http/serving/) covers first. [Serving
an API with OpenAPI](/humus/pr-preview/pr-1329/services/http/openapi/) then builds that
handler from typed operations on typed paths with `openapi/v3`, which writes
the API's OpenAPI document from the same declarations.

The [shoppingcart walkthrough](/humus/pr-preview/pr-1329/walkthroughs/http/shoppingcart/)
builds a production-ready API on both, step by step, and its code is
[`example/http/shoppingcart`](https://github.com/z5labs/humus/tree/main/example/http/shoppingcart).

Next: [Serving HTTP](/humus/pr-preview/pr-1329/services/http/serving/)
