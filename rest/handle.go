// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"net/http"

	"github.com/swaggest/openapi-go/openapi3"
	"github.com/z5labs/humus"
	"github.com/z5labs/sdk-go/try"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Handler extends [http.Handler] with OpenAPI schema information.
// Implementations define how to handle HTTP requests and provide metadata
// for generating OpenAPI specifications.
//
// See the rest/rpc subpackage for handler implementations.
type Handler interface {
	http.Handler

	RequestBody() openapi3.RequestBodyOrRef
	Responses() openapi3.Responses
}

type securityScheme struct {
	name   string
	scheme openapi3.SecurityScheme
}

// OperationOptions holds configuration for an HTTP operation registered with [Handle].
// This includes security schemes, parameter definitions, request transformations,
// and error handling.
type OperationOptions struct {
	securityScheme *securityScheme
	parameters     []openapi3.ParameterOrRef
	transforms     []func(*http.Request) (*http.Request, error)
	errHandler     ErrorHandler
}

// OperationOption configures an operation created by [Handle].
// Common implementations include parameter validators ([Header], [QueryParam], etc.)
// and [OnError] for custom error handling.
type OperationOption func(*OperationOptions)

// OnError configures a custom [ErrorHandler] for an operation.
// If not specified, operations use a default error handler that logs errors
// and returns appropriate HTTP status codes.
//
// Example:
//
//	customErrorHandler := rest.ErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, err error) {
//	    log.Printf("Error: %v", err)
//	    w.WriteHeader(http.StatusInternalServerError)
//	    json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
//	})
//	rest.Handle(http.MethodGet, rest.BasePath("/users"), handler, rest.OnError(customErrorHandler))
func OnError(eh ErrorHandler) OperationOption {
	return func(oo *OperationOptions) {
		oo.errHandler = eh
	}
}

type operationHandler struct {
	tracer     trace.Tracer
	errHandler ErrorHandler
	transforms []func(*http.Request) (*http.Request, error)
	inner      http.Handler
}

// Handle registers an HTTP operation (endpoint) with an [Api].
//
// It creates an [ApiOption] that configures both the HTTP routing and the
// OpenAPI specification for the operation. The operation is registered with
// the specified HTTP method and path, and handles requests using the provided handler.
//
// Parameters:
//   - method: HTTP method (e.g., http.MethodGet, http.MethodPost)
//   - path: URL path created with [BasePath] and path building methods
//   - h: Handler that processes requests and provides OpenAPI schema info
//   - opts: Optional configuration (parameters, auth, error handling)
//
// Example:
//
//	getUser := rest.Handle(
//	    http.MethodGet,
//	    rest.BasePath("/users").Param("id"),
//	    userHandler,
//	    rest.QueryParam("format", rest.Required()),
//	    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt")),
//	)
//	api := rest.NewApi("User API", "v1.0.0", getUser)
func Handle(method string, path Path, h Handler, opts ...OperationOption) ApiOption {
	return apiOptionFunc(func(ao *ApiOptions) {
		oo := &OperationOptions{
			errHandler: defaultErrorHandler(humus.LogHandler("rest")),
		}
		for _, opt := range opts {
			opt(oo)
		}

		reqBody := h.RequestBody()
		responses := h.Responses()

		var op openapi3.Operation
		op.RequestBody = &reqBody
		op.Responses = responses

		endpoint := path.String()

		err := ao.def.AddOperation(method, endpoint, op)
		if err != nil {
			panic(err)
		}

		if oo.securityScheme != nil {
			ao.def.ComponentsEns().SecuritySchemesEns().WithMapOfSecuritySchemeOrRefValuesItem(
				oo.securityScheme.name,
				openapi3.SecuritySchemeOrRef{
					SecurityScheme: &oo.securityScheme.scheme,
				},
			)

			op.WithSecurity(map[string][]string{
				oo.securityScheme.name: {}, // todo: add support for populating this
			})
		}

		ao.mux.Method(method, endpoint, otelhttp.WithRouteTag(endpoint, &operationHandler{
			tracer:     otel.Tracer("rest"),
			errHandler: oo.errHandler,
			transforms: oo.transforms,
			inner:      h,
		}))
	})
}

// ServeHTTP implements [http.Handler] for operation handlers.
// It applies request transformations (parameter validation, auth checks),
// delegates to the inner handler, and handles any errors via the configured
// error handler. All operations are traced with OpenTelemetry.
func (o *operationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	spanCtx, span := o.tracer.Start(r.Context(), "operationHandler.ServeHTTP")
	defer span.End()

	var err error
	defer func() {
		if err == nil {
			return
		}

		o.errHandler.OnError(spanCtx, w, err)
	}()
	defer try.Recover(&err)

	for _, transform := range o.transforms {
		r, err = transform(r)
		if err != nil {
			return
		}
	}

	o.inner.ServeHTTP(w, r)
}
