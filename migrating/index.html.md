# Migrating to humus

These guides are for a service that runs on another Go framework. They aren't
part of the reading order: find your framework under your service's type and
start there.

Every guide:

- maps the framework's concepts to humus's, each linked to the page that
  covers it;
- moves one small service across, in short steps;
- covers the whole process, not just the handlers: `main`, settings, TLS,
  telemetry, health, shutdown and tests;
- runs one set of test cases against the service before and after, so clients
  see no change beyond the ones the guide names.

A guide's code is under `example/<type>/<name>` in the repository, beside the
walkthrough of its service type, and every sample on its page is quoted from
that code.

## The guides

The guides are listed under their service types, in the order
[Service types](/humus/services/) gives them.

### HTTP

- [Echo](/humus/migrating/http/echo/): an Echo v5 service, run on
  `httpserver` with its routes unchanged, then its routes moved onto
  `openapi/v3` a few at a time.
- [Gin](/humus/migrating/http/gin/): a Gin v1 service, run on
  `httpserver` with its routes unchanged, then its routes moved onto
  `openapi/v3` a few at a time.
