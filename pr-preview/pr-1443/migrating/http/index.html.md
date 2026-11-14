# HTTP

Each guide here moves an HTTP service in the same steps. It runs the service
on [`httpserver`](/humus/pr-preview/pr-1443/services/http/serving/) first, its routes
and handlers unchanged. Then it moves the routes onto
[`openapi/v3`](/humus/pr-preview/pr-1443/services/http/openapi/) a few at a time,
while the framework serves the rest, until the service no longer requires
the framework.

- [Echo](/humus/pr-preview/pr-1443/migrating/http/echo/)
- [Gin](/humus/pr-preview/pr-1443/migrating/http/gin/)
- [Fiber](/humus/pr-preview/pr-1443/migrating/http/fiber/)
- [Huma](/humus/pr-preview/pr-1443/migrating/http/huma/)
