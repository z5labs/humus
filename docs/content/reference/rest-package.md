---
title: Rest Package
weight: 30
type: docs
---

This page provides a reference for key types and functions in the `rest` package.

## Core Types

### Api

The main API object that combines HTTP routing, OpenAPI spec generation, and health monitoring.

```go
type Api struct {
    // ...
}

func NewApi(title, version string, opts ...ApiOption) *Api
```

### Config

Configuration structure for REST services. Embed this in your custom config:

```go
type Config struct {
    humus.Config `config:",squash"`
    // Your custom fields
}
```

## Handler Types

### Handler

Core interface for handlers that consume a request and produce a response:

```go
type Handler[Req, Resp any] interface {
    Handle(context.Context, *Req) (*Resp, error)
}
```

### HandlerFunc

Function adapter for Handler interface:

```go
type HandlerFunc[Req, Resp any] func(context.Context, *Req) (*Resp, error)
```

### Producer

Interface for producing responses without consuming requests:

```go
type Producer[Resp any] interface {
    Produce(context.Context) (*Resp, error)
}
```

### ProducerFunc

Function adapter for Producer interface:

```go
type ProducerFunc[Resp any] func(context.Context) (*Resp, error)
```

### Consumer

Interface for consuming requests without producing responses:

```go
type Consumer[Req any] interface {
    Consume(context.Context, *Req) error
}
```

### ConsumerFunc

Function adapter for Consumer interface:

```go
type ConsumerFunc[Req any] func(context.Context, *Req) error
```

## Operation Registration

### Handle

Register an operation with the API:

```go
func Handle(method string, path Path, handler http.Handler, opts ...OperationOption) ApiOption
```

Alternatively, create operations separately:

```go
func Operation(method string, path Path, handler http.Handler, opts ...OperationOption) ApiOption
```

### OperationOption

Configure operations with options:

```go
type OperationOption func(*OperationOptions)
```

Common operation options:
- `Intercept(interceptor ServerInterceptor)` - Add request interceptor
- `OnError(handler ErrorHandler)` - Custom error handling
- Parameter validators (Header, QueryParam, PathParam, Cookie)
- Authentication schemes (JWTAuth, APIKey, BasicAuth, etc.)

## Interceptors

### ServerInterceptor

Interface for operation-level request/response interceptors:

```go
type ServerInterceptor interface {
    Intercept(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error
}
```

### ServerInterceptorFunc

Function adapter for ServerInterceptor:

```go
type ServerInterceptorFunc func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error

func (f ServerInterceptorFunc) Intercept(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error
```

### Intercept

Register an interceptor for an operation:

```go
func Intercept(interceptor ServerInterceptor) OperationOption
```

**Example:**

```go
loggingInterceptor := rest.ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
    return func(w http.ResponseWriter, r *http.Request) error {
        log.Printf("Request: %s %s", r.Method, r.URL.Path)
        err := next(w, r)
        log.Printf("Response error: %v", err)
        return err
    }
})

rest.Handle(
    http.MethodGet,
    rest.BasePath("/data"),
    handler,
    rest.Intercept(loggingInterceptor),
)
```

## JSON Handlers

### HandleJson

Handle JSON request and response:

```go
func HandleJson[Req, Resp any](h Handler[Req, Resp]) http.Handler
```

### ProduceJson

Produce JSON response (no request body):

```go
func ProduceJson[Resp any](p Producer[Resp]) http.Handler
```

### ConsumeOnlyJson

Consume JSON request (no response body):

```go
func ConsumeOnlyJson[Req any](c Consumer[Req]) http.Handler
```

### ConsumeJson

Wrap handler to consume JSON request:

```go
func ConsumeJson[Req, Resp any](h Handler[Req, Resp]) Handler[Req, Resp]
```

### ReturnJson

Wrap handler to return JSON response:

```go
func ReturnJson[Req, Resp any](h Handler[Req, Resp]) http.Handler
```

## Form Handlers

### HandleForm

Handle form request and JSON response:

```go
func HandleForm[Req, Resp any](h Handler[Req, Resp]) http.Handler
```

### ConsumeForm

Wrap handler to consume form data:

```go
func ConsumeForm[Req, Resp any](h Handler[Req, Resp]) Handler[Req, Resp]
```

### ConsumeOnlyForm

Consume form request (no response body):

```go
func ConsumeOnlyForm[Req any](c Consumer[Req]) http.Handler
```

## HTML Handlers

### ProduceHTML

Produce HTML response using a template:

```go
func ProduceHTML[Resp any](p Producer[Resp], tmpl *template.Template) http.Handler
```

### ReturnHTML

Wrap handler to return HTML response:

```go
func ReturnHTML[Req, Resp any](h Handler[Req, Resp], tmpl *template.Template) http.Handler
```

## Path Building

### BasePath

Create a new path builder:

```go
func BasePath(segment string) Path
```

### Path Methods

```go
type Path interface {
    Segment(name string) Path  // Add static segment
    Param(name string) Path    // Add path parameter
    String() string            // Get path string
}
```

**Example:**

```go
path := rest.BasePath("/api").Segment("v1").Segment("users").Param("id")
// Results in: /api/v1/users/{id}
```

## Parameters

### Header

Define and validate a header parameter:

```go
func Header(name string, opts ...ParameterOption) OperationOption
```

### QueryParam

Define and validate a query parameter:

```go
func QueryParam(name string, opts ...ParameterOption) OperationOption
```

### Cookie

Define and validate a cookie parameter:

```go
func Cookie(name string, opts ...ParameterOption) OperationOption
```

### Parameter Options

```go
func Required() ParameterOption
func Regex(pattern *regexp.Regexp) ParameterOption
func APIKey(name string) ParameterOption
func BasicAuth(name string) ParameterOption
func JWTAuth(name string, verifier JWTVerifier) ParameterOption
func OAuth2(name string) ParameterOption
func OpenIDConnect(name, discoveryURL string) ParameterOption
```

## Context Value Extraction

### HeaderValue

Extract header values from context:

```go
func HeaderValue(ctx context.Context, name string) []string
```

### QueryParamValue

Extract query parameter values from context:

```go
func QueryParamValue(ctx context.Context, name string) []string
```

### PathParamValue

Extract path parameter value from context:

```go
func PathParamValue(ctx context.Context, name string) string
```

### CookieValue

Extract cookie values from context:

```go
func CookieValue(ctx context.Context, name string) []*http.Cookie
```

## Error Handling

### ErrorHandler

Interface for custom error handling:

```go
type ErrorHandler interface {
    OnError(ctx context.Context, w http.ResponseWriter, err error)
}
```

### ErrorHandlerFunc

Function adapter for ErrorHandler:

```go
type ErrorHandlerFunc func(ctx context.Context, w http.ResponseWriter, err error)
```

### OnError

Configure custom error handler for operation:

```go
func OnError(handler ErrorHandler) OperationOption
```

### ProblemDetailsErrorHandler

RFC 7807 Problem Details error handler:

```go
func NewProblemDetailsErrorHandler(opts ...ProblemDetailsOption) ErrorHandler
```

**Options:**

```go
func WithDefaultType(uri string) ProblemDetailsOption
```

### Framework Errors

```go
type BadRequestError struct {
    Message string
}

type UnauthorizedError struct {
    Message string
}

type InvalidContentTypeError struct {
    ContentType string
}

type InvalidJWTError struct {
    Message string
}

type MissingRequiredParameterError struct {
    Name string
}

type InvalidParameterValueError struct {
    Name  string
    Value string
}
```

### ProblemDetail

RFC 7807 Problem Details structure:

```go
type ProblemDetail struct {
    Type     string `json:"type"`
    Title    string `json:"title"`
    Status   int    `json:"status"`
    Detail   string `json:"detail"`
    Instance string `json:"instance,omitempty"`
}
```

Embed in custom errors for structured error responses with extension fields.

## Empty Types

### EmptyRequest

Type for handlers with no request body:

```go
type EmptyRequest struct{}
```

### EmptyResponse

Type for handlers with no response body:

```go
type EmptyResponse struct{}
```

## Runner

### Run

Convenience function to run a REST service:

```go
func Run[T Configer](source bedrockcfg.Source, init func(context.Context, T) (*Api, error)) error
```

### YamlSource

Create a YAML config source:

```go
func YamlSource(path string) bedrockcfg.Source
```

## See Also

- [Features: REST Services]({{< ref "/features/rest" >}}) - Complete REST documentation
- [Interceptors]({{< ref "/features/rest/interceptors" >}}) - Operation-level processing
- [Error Handling]({{< ref "/features/rest/error-handling" >}}) - Custom error responses
- [pkg.go.dev](https://pkg.go.dev/github.com/z5labs/humus/rest) - Complete API reference
