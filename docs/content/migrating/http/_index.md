---
title: HTTP
linkTitle: HTTP
description: >
  Guides that move an HTTP service from another Go framework to humus: run
  it on httpserver as it is, then move its routes onto openapi/v3 a few at a
  time, until it no longer requires the framework.
type: docs
weight: 10
---

Each guide here moves an HTTP service in the same steps. It runs the service
on [`httpserver`]({{< relref "/services/http/serving" >}}) first, its routes
and handlers unchanged. Then it moves the routes onto
[`openapi/v3`]({{< relref "/services/http/openapi" >}}) a few at a time,
while the framework serves the rest, until the service no longer requires
the framework.

- [Echo]({{< relref "/migrating/http/echo" >}})
- [Gin]({{< relref "/migrating/http/gin" >}})
- [Fiber]({{< relref "/migrating/http/fiber" >}})
- [Huma]({{< relref "/migrating/http/huma" >}})
