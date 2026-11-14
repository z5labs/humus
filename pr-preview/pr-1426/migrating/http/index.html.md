# HTTP

Each guide here moves an HTTP service in the same steps. It runs the service
on [`httpserver`](/humus/pr-preview/pr-1426/services/http/serving/) first, its routes
and handlers unchanged. Then it moves the routes onto
[`openapi/v3`](/humus/pr-preview/pr-1426/services/http/openapi/) a few at a time,
while the framework serves the rest, until the service no longer requires
the framework.

- [Echo](/humus/pr-preview/pr-1426/migrating/http/echo/)
