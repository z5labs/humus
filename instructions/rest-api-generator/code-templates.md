# REST API Generator - Code Templates

This document provides exact code templates for generating Humus REST API applications. All templates are copy-paste ready with clear placeholder markers.

**Placeholder Conventions:**
- `{ModuleName}` - Go module path (e.g., `github.com/myorg/my-service`)
- `{ServiceName}` - Service name in kebab-case (e.g., `user-service`)
- `{ServiceNameCamel}` - Service name in CamelCase (e.g., `UserService`)
- `{EndpointName}` - Endpoint name in CamelCase (e.g., `GetUser`, `CreateOrder`)
- `{endpointName}` - Endpoint name in camelCase (e.g., `getUser`, `createOrder`)
- `{ResourceName}` - Resource name in CamelCase (e.g., `User`, `Order`)
- `{resourceName}` - Resource name in camelCase (e.g., `user`, `order`)

---

## 1. Project Structure Files

### 1.1 main.go Template

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"

	bedrockconfig "github.com/z5labs/bedrock/config"
	"github.com/z5labs/humus/rest"

	"{ModuleName}/endpoint"
)

func main() {
	rest.Run(
		context.Background(),
		rest.Title("{ServiceNameCamel} API"),
		rest.Version("1.0.0"),
		rest.Port(bedrockconfig.Default(
			8443,
			bedrockconfig.IntFromString(bedrockconfig.Env("{SERVICE_NAME_UPPER}_PORT")),
		)),
		// Register endpoints
		endpoint.{EndpointName}(),
	)
}
```

**Alternative with embedded config (for complex configurations):**

```go
package main

import (
	"context"
	_ "embed"
	"log"

	"gopkg.in/yaml.v3"

	bedrockconfig "github.com/z5labs/bedrock/config"
	"github.com/z5labs/humus/rest"

	"{ModuleName}/endpoint"
)

//go:embed config.yaml
var configBytes []byte

type config struct {
	HTTP struct {
		Port int `yaml:"port"`
	} `yaml:"http"`
	OpenAPI struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"openapi"`
}

func main() {
	var cfg config
	if err := yaml.Unmarshal(configBytes, &cfg); err != nil {
		log.Fatal(err)
	}

	rest.Run(
		context.Background(),
		rest.Title(cfg.OpenAPI.Title),
		rest.Version(cfg.OpenAPI.Version),
		rest.Port(bedrockconfig.ReaderOf(cfg.HTTP.Port)),
		// Register endpoints
		endpoint.{EndpointName}(),
	)
}
```

### 1.2 config.yaml Template

```yaml
# {ServiceName} Configuration
# Environment variables can be used with Go templates

openapi:
  title: {{env "OPENAPI_TITLE" | default "{ServiceNameCamel} API"}}
  version: {{env "OPENAPI_VERSION" | default "1.0.0"}}

http:
  port: {{env "{SERVICE_NAME_UPPER}_PORT" | default "8443"}}

# Service-specific configuration
database:
  url: {{env "DATABASE_URL" | default "postgres://localhost:5432/{service_name}"}}
  max_connections: {{env "DATABASE_MAX_CONNECTIONS" | default "10"}}

# Backend service URLs
services:
  user_service:
    base_url: {{env "USER_SERVICE_URL" | default "http://localhost:8081"}}
  order_service:
    base_url: {{env "ORDER_SERVICE_URL" | default "http://localhost:8082"}}

# Observability
otel:
  service_name: {{env "OTEL_SERVICE_NAME" | default "{service-name}"}}
  exporter:
    endpoint: {{env "OTEL_EXPORTER_OTLP_ENDPOINT" | default ""}}
```

### 1.3 go.mod Template

```
module {ModuleName}

go 1.24

require (
	github.com/z5labs/bedrock latest
	github.com/z5labs/humus latest
	go.opentelemetry.io/otel latest
	gopkg.in/yaml.v3 v3.0.1
)
```

---

## 2. Endpoint Handler Templates

### 2.1 Producer Template (GET - no request body)

Use this for endpoints that return data without accepting a request body.

**File: `endpoint/{get_resource}.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"log/slog"
	"net/http"

	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"{ModuleName}/service"
)

// {EndpointName}Response represents the response for the {EndpointName} endpoint.
type {EndpointName}Response struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// {EndpointName}Error represents an error response.
type {EndpointName}Error struct {
	Message string `json:"message"`
}

func (e {EndpointName}Error) Error() string { return e.Message }

type {endpointName}Handler struct {
	tracer trace.Tracer
	log    *slog.Logger
	client *service.{ResourceName}Client
}

func {EndpointName}(client *service.{ResourceName}Client) rest.Option {
	h := &{endpointName}Handler{
		tracer: otel.Tracer("{service-name}/endpoint"),
		log:    humus.Logger("{service-name}/endpoint"),
		client: client,
	}

	// Define path parameter
	{resourceName}ID := bedrockrest.PathParam[string]("id", bedrockrest.Required())

	// Build endpoint
	ep := bedrockrest.GET("/{resources}/{id}", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) ({EndpointName}Response, error) {
		id := bedrockrest.ParamFrom(req, {resourceName}ID)
		return h.handle(ctx, id)
	})
	ep = {resourceName}ID.Read(ep)
	ep = bedrockrest.WriteJSON[{EndpointName}Response](http.StatusOK, ep)
	ep = bedrockrest.ErrorJSON[{EndpointName}Error](http.StatusNotFound, ep)
	route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) {EndpointName}Error {
		return {EndpointName}Error{Message: err.Error()}
	}, ep)

	return rest.Handle(route)
}

func (h *{endpointName}Handler) handle(ctx context.Context, id string) ({EndpointName}Response, error) {
	ctx, span := h.tracer.Start(ctx, "{EndpointName}")
	defer span.End()

	h.log.InfoContext(ctx, "fetching {resourceName}", slog.String("id", id))

	// Call backend service
	resp, err := h.client.Get{ResourceName}(ctx, &service.Get{ResourceName}Request{ID: id})
	if err != nil {
		h.log.ErrorContext(ctx, "failed to fetch {resourceName}", slog.String("error", err.Error()))
		return {EndpointName}Response{}, {EndpointName}Error{Message: "resource not found"}
	}

	// Map response
	return {EndpointName}Response{
		ID:        resp.ID,
		Name:      resp.Name,
		CreatedAt: resp.CreatedAt,
	}, nil
}
```

### 2.2 Consumer Template (POST - no response body)

Use this for webhooks or endpoints that accept data but don't return a body.

**File: `endpoint/{webhook_resource}.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"log/slog"
	"net/http"

	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"{ModuleName}/service"
)

// {EndpointName}Request represents the request body for the {EndpointName} endpoint.
type {EndpointName}Request struct {
	EventType string         `json:"event_type"`
	Data      map[string]any `json:"data"`
	Timestamp string         `json:"timestamp"`
}

// {EndpointName}Error represents an error response.
type {EndpointName}Error struct {
	Message string `json:"message"`
}

func (e {EndpointName}Error) Error() string { return e.Message }

// emptyResponse is used when no response body is needed.
type emptyResponse struct{}

type {endpointName}Handler struct {
	tracer    trace.Tracer
	log       *slog.Logger
	processor *service.EventProcessor
}

func {EndpointName}(processor *service.EventProcessor) rest.Option {
	h := &{endpointName}Handler{
		tracer:    otel.Tracer("{service-name}/endpoint"),
		log:       humus.Logger("{service-name}/endpoint"),
		processor: processor,
	}

	// Build endpoint
	ep := bedrockrest.POST[{EndpointName}Request, emptyResponse]("/webhooks/{resource}", func(ctx context.Context, req bedrockrest.Request[{EndpointName}Request]) (emptyResponse, error) {
		return h.handle(ctx, &req.Body)
	})
	ep = bedrockrest.ReadJSON[{EndpointName}Request](ep)
	ep = bedrockrest.ErrorJSON[{EndpointName}Error](http.StatusBadRequest, ep)
	route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) {EndpointName}Error {
		return {EndpointName}Error{Message: err.Error()}
	}, ep)

	return rest.Handle(route)
}

func (h *{endpointName}Handler) handle(ctx context.Context, req *{EndpointName}Request) (emptyResponse, error) {
	ctx, span := h.tracer.Start(ctx, "{EndpointName}")
	defer span.End()

	h.log.InfoContext(ctx, "processing webhook",
		slog.String("event_type", req.EventType),
		slog.String("timestamp", req.Timestamp),
	)

	// Process the event
	err := h.processor.Process(ctx, &service.ProcessEventRequest{
		EventType: req.EventType,
		Data:      req.Data,
	})
	if err != nil {
		h.log.ErrorContext(ctx, "failed to process webhook", slog.String("error", err.Error()))
		return emptyResponse{}, {EndpointName}Error{Message: "failed to process event"}
	}

	return emptyResponse{}, nil
}
```

### 2.3 Handler Template (POST/PUT - request and response)

Use this for endpoints that accept a request body and return a response.

**File: `endpoint/{create_resource}.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"log/slog"
	"net/http"

	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"{ModuleName}/service"
)

// {EndpointName}Request represents the request body for the {EndpointName} endpoint.
type {EndpointName}Request struct {
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// {EndpointName}Response represents the response for the {EndpointName} endpoint.
type {EndpointName}Response struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
}

// {EndpointName}Error represents an error response.
type {EndpointName}Error struct {
	Message string `json:"message"`
}

func (e {EndpointName}Error) Error() string { return e.Message }

type {endpointName}Handler struct {
	tracer trace.Tracer
	log    *slog.Logger
	client *service.{ResourceName}Client
}

func {EndpointName}(client *service.{ResourceName}Client) rest.Option {
	h := &{endpointName}Handler{
		tracer: otel.Tracer("{service-name}/endpoint"),
		log:    humus.Logger("{service-name}/endpoint"),
		client: client,
	}

	// Build endpoint
	ep := bedrockrest.POST[{EndpointName}Request, {EndpointName}Response]("/{resources}", func(ctx context.Context, req bedrockrest.Request[{EndpointName}Request]) ({EndpointName}Response, error) {
		return h.handle(ctx, &req.Body)
	})
	ep = bedrockrest.ReadJSON[{EndpointName}Request](ep)
	ep = bedrockrest.WriteJSON[{EndpointName}Response](http.StatusCreated, ep)
	ep = bedrockrest.ErrorJSON[{EndpointName}Error](http.StatusBadRequest, ep)
	route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) {EndpointName}Error {
		return {EndpointName}Error{Message: err.Error()}
	}, ep)

	return rest.Handle(route)
}

func (h *{endpointName}Handler) handle(ctx context.Context, req *{EndpointName}Request) ({EndpointName}Response, error) {
	ctx, span := h.tracer.Start(ctx, "{EndpointName}")
	defer span.End()

	h.log.InfoContext(ctx, "creating {resourceName}",
		slog.String("name", req.Name),
		slog.String("email", req.Email),
	)

	// Validate request
	if req.Name == "" {
		return {EndpointName}Response{}, {EndpointName}Error{Message: "name is required"}
	}
	if req.Email == "" {
		return {EndpointName}Response{}, {EndpointName}Error{Message: "email is required"}
	}

	// Call backend service
	resp, err := h.client.Create{ResourceName}(ctx, &service.Create{ResourceName}Request{
		Name:        req.Name,
		Email:       req.Email,
		Description: req.Description,
		Tags:        req.Tags,
	})
	if err != nil {
		h.log.ErrorContext(ctx, "failed to create {resourceName}", slog.String("error", err.Error()))
		return {EndpointName}Response{}, {EndpointName}Error{Message: "failed to create resource"}
	}

	// Map response
	return {EndpointName}Response{
		ID:        resp.ID,
		Name:      resp.Name,
		Email:     resp.Email,
		CreatedAt: resp.CreatedAt,
	}, nil
}
```

### 2.4 PUT/Update Handler Template

**File: `endpoint/{update_resource}.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"log/slog"
	"net/http"

	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"{ModuleName}/service"
)

// {EndpointName}Request represents the request body for the {EndpointName} endpoint.
type {EndpointName}Request struct {
	Name        *string  `json:"name,omitempty"`
	Email       *string  `json:"email,omitempty"`
	Description *string  `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// {EndpointName}Response represents the response for the {EndpointName} endpoint.
type {EndpointName}Response struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	UpdatedAt string `json:"updated_at"`
}

// {EndpointName}Error represents an error response.
type {EndpointName}Error struct {
	Message string `json:"message"`
}

func (e {EndpointName}Error) Error() string { return e.Message }

type {endpointName}Handler struct {
	tracer trace.Tracer
	log    *slog.Logger
	client *service.{ResourceName}Client
}

func {EndpointName}(client *service.{ResourceName}Client) rest.Option {
	h := &{endpointName}Handler{
		tracer: otel.Tracer("{service-name}/endpoint"),
		log:    humus.Logger("{service-name}/endpoint"),
		client: client,
	}

	// Define path parameter
	{resourceName}ID := bedrockrest.PathParam[string]("id", bedrockrest.Required())

	// Build endpoint
	ep := bedrockrest.PUT[{EndpointName}Request, {EndpointName}Response]("/{resources}/{id}", func(ctx context.Context, req bedrockrest.Request[{EndpointName}Request]) ({EndpointName}Response, error) {
		id := bedrockrest.ParamFrom(req, {resourceName}ID)
		return h.handle(ctx, id, &req.Body)
	})
	ep = {resourceName}ID.Read(ep)
	ep = bedrockrest.ReadJSON[{EndpointName}Request](ep)
	ep = bedrockrest.WriteJSON[{EndpointName}Response](http.StatusOK, ep)
	ep = bedrockrest.ErrorJSON[{EndpointName}Error](http.StatusBadRequest, ep)
	ep = bedrockrest.ErrorJSON[{EndpointName}Error](http.StatusNotFound, ep)
	route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) {EndpointName}Error {
		return {EndpointName}Error{Message: err.Error()}
	}, ep)

	return rest.Handle(route)
}

func (h *{endpointName}Handler) handle(ctx context.Context, id string, req *{EndpointName}Request) ({EndpointName}Response, error) {
	ctx, span := h.tracer.Start(ctx, "{EndpointName}")
	defer span.End()

	h.log.InfoContext(ctx, "updating {resourceName}", slog.String("id", id))

	// Call backend service
	resp, err := h.client.Update{ResourceName}(ctx, &service.Update{ResourceName}Request{
		ID:          id,
		Name:        req.Name,
		Email:       req.Email,
		Description: req.Description,
		Tags:        req.Tags,
	})
	if err != nil {
		h.log.ErrorContext(ctx, "failed to update {resourceName}", slog.String("error", err.Error()))
		return {EndpointName}Response{}, {EndpointName}Error{Message: "failed to update resource"}
	}

	// Map response
	return {EndpointName}Response{
		ID:        resp.ID,
		Name:      resp.Name,
		Email:     resp.Email,
		UpdatedAt: resp.UpdatedAt,
	}, nil
}
```

### 2.5 DELETE Handler Template

**File: `endpoint/{delete_resource}.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"log/slog"
	"net/http"

	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"{ModuleName}/service"
)

// {EndpointName}Error represents an error response.
type {EndpointName}Error struct {
	Message string `json:"message"`
}

func (e {EndpointName}Error) Error() string { return e.Message }

type {endpointName}Handler struct {
	tracer trace.Tracer
	log    *slog.Logger
	client *service.{ResourceName}Client
}

func {EndpointName}(client *service.{ResourceName}Client) rest.Option {
	h := &{endpointName}Handler{
		tracer: otel.Tracer("{service-name}/endpoint"),
		log:    humus.Logger("{service-name}/endpoint"),
		client: client,
	}

	// Define path parameter
	{resourceName}ID := bedrockrest.PathParam[string]("id", bedrockrest.Required())

	// Build endpoint - DELETE returns empty body on success
	ep := bedrockrest.DELETE[emptyResponse]("/{resources}/{id}", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) (emptyResponse, error) {
		id := bedrockrest.ParamFrom(req, {resourceName}ID)
		return h.handle(ctx, id)
	})
	ep = {resourceName}ID.Read(ep)
	ep = bedrockrest.ErrorJSON[{EndpointName}Error](http.StatusNotFound, ep)
	route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) {EndpointName}Error {
		return {EndpointName}Error{Message: err.Error()}
	}, ep)

	return rest.Handle(route)
}

func (h *{endpointName}Handler) handle(ctx context.Context, id string) (emptyResponse, error) {
	ctx, span := h.tracer.Start(ctx, "{EndpointName}")
	defer span.End()

	h.log.InfoContext(ctx, "deleting {resourceName}", slog.String("id", id))

	// Call backend service
	err := h.client.Delete{ResourceName}(ctx, &service.Delete{ResourceName}Request{ID: id})
	if err != nil {
		h.log.ErrorContext(ctx, "failed to delete {resourceName}", slog.String("error", err.Error()))
		return emptyResponse{}, {EndpointName}Error{Message: "resource not found"}
	}

	return emptyResponse{}, nil
}
```

### 2.6 List Handler Template (GET with query parameters)

**File: `endpoint/{list_resources}.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"log/slog"
	"net/http"

	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"{ModuleName}/service"
)

// {EndpointName}Response represents the response for the {EndpointName} endpoint.
type {EndpointName}Response struct {
	Items      []{ResourceName}Item `json:"items"`
	TotalCount int                  `json:"total_count"`
	Page       int                  `json:"page"`
	PageSize   int                  `json:"page_size"`
}

// {ResourceName}Item represents a single item in the list response.
type {ResourceName}Item struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// {EndpointName}Error represents an error response.
type {EndpointName}Error struct {
	Message string `json:"message"`
}

func (e {EndpointName}Error) Error() string { return e.Message }

type {endpointName}Handler struct {
	tracer trace.Tracer
	log    *slog.Logger
	client *service.{ResourceName}Client
}

func {EndpointName}(client *service.{ResourceName}Client) rest.Option {
	h := &{endpointName}Handler{
		tracer: otel.Tracer("{service-name}/endpoint"),
		log:    humus.Logger("{service-name}/endpoint"),
		client: client,
	}

	// Define query parameters
	pageParam := bedrockrest.QueryParam[int]("page", bedrockrest.DefaultValue(1))
	pageSizeParam := bedrockrest.QueryParam[int]("page_size", bedrockrest.DefaultValue(20))
	filterParam := bedrockrest.QueryParam[string]("filter", bedrockrest.Optional())

	// Build endpoint
	ep := bedrockrest.GET("/{resources}", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) ({EndpointName}Response, error) {
		page := bedrockrest.ParamFrom(req, pageParam)
		pageSize := bedrockrest.ParamFrom(req, pageSizeParam)
		filter := bedrockrest.ParamFrom(req, filterParam)
		return h.handle(ctx, page, pageSize, filter)
	})
	ep = pageParam.Read(ep)
	ep = pageSizeParam.Read(ep)
	ep = filterParam.Read(ep)
	ep = bedrockrest.WriteJSON[{EndpointName}Response](http.StatusOK, ep)
	route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) {EndpointName}Error {
		return {EndpointName}Error{Message: err.Error()}
	}, ep)

	return rest.Handle(route)
}

func (h *{endpointName}Handler) handle(ctx context.Context, page, pageSize int, filter string) ({EndpointName}Response, error) {
	ctx, span := h.tracer.Start(ctx, "{EndpointName}")
	defer span.End()

	h.log.InfoContext(ctx, "listing {resources}",
		slog.Int("page", page),
		slog.Int("page_size", pageSize),
		slog.String("filter", filter),
	)

	// Call backend service
	resp, err := h.client.List{ResourceName}s(ctx, &service.List{ResourceName}sRequest{
		Page:     page,
		PageSize: pageSize,
		Filter:   filter,
	})
	if err != nil {
		h.log.ErrorContext(ctx, "failed to list {resources}", slog.String("error", err.Error()))
		return {EndpointName}Response{}, {EndpointName}Error{Message: "failed to list resources"}
	}

	// Map response
	items := make([]{ResourceName}Item, len(resp.Items))
	for i, item := range resp.Items {
		items[i] = {ResourceName}Item{
			ID:        item.ID,
			Name:      item.Name,
			CreatedAt: item.CreatedAt,
		}
	}

	return {EndpointName}Response{
		Items:      items,
		TotalCount: resp.TotalCount,
		Page:       page,
		PageSize:   pageSize,
	}, nil
}
```

---

## 3. Endpoint Test Templates

### 3.1 Producer Test Template (GET endpoint)

**File: `endpoint/{get_resource}_test.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"{ModuleName}/service"
)

// Mock{ResourceName}Client implements service.{ResourceName}ClientInterface for testing.
type mock{ResourceName}ClientFor{EndpointName} struct {
	get{ResourceName}Func func(ctx context.Context, req *service.Get{ResourceName}Request) (*service.Get{ResourceName}Response, error)
}

func (m *mock{ResourceName}ClientFor{EndpointName}) Get{ResourceName}(ctx context.Context, req *service.Get{ResourceName}Request) (*service.Get{ResourceName}Response, error) {
	return m.get{ResourceName}Func(ctx, req)
}

func Test{EndpointName}(t *testing.T) {
	tests := []struct {
		name           string
		id             string
		mockResponse   *service.Get{ResourceName}Response
		mockError      error
		expectedResult {EndpointName}Response
		expectedError  error
	}{
		{
			name: "successfully fetches {resourceName}",
			id:   "123",
			mockResponse: &service.Get{ResourceName}Response{
				ID:        "123",
				Name:      "Test {ResourceName}",
				CreatedAt: "2024-01-01T00:00:00Z",
			},
			mockError: nil,
			expectedResult: {EndpointName}Response{
				ID:        "123",
				Name:      "Test {ResourceName}",
				CreatedAt: "2024-01-01T00:00:00Z",
			},
			expectedError: nil,
		},
		{
			name:           "{resourceName} not found",
			id:             "not-found",
			mockResponse:   nil,
			mockError:      errors.New("not found"),
			expectedResult: {EndpointName}Response{},
			expectedError:  {EndpointName}Error{Message: "resource not found"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mock{ResourceName}ClientFor{EndpointName}{
				get{ResourceName}Func: func(ctx context.Context, req *service.Get{ResourceName}Request) (*service.Get{ResourceName}Response, error) {
					require.Equal(t, tt.id, req.ID)
					return tt.mockResponse, tt.mockError
				},
			}

			h := &{endpointName}Handler{
				client: mockClient,
			}

			result, err := h.handle(context.Background(), tt.id)

			if tt.expectedError != nil {
				require.Error(t, err)
				require.Equal(t, tt.expectedError, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedResult, result)
			}
		})
	}
}
```

### 3.2 Handler Test Template (POST/PUT endpoint)

**File: `endpoint/{create_resource}_test.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"{ModuleName}/service"
)

// Mock{ResourceName}Client implements service.{ResourceName}ClientInterface for testing.
type mock{ResourceName}ClientFor{EndpointName} struct {
	create{ResourceName}Func func(ctx context.Context, req *service.Create{ResourceName}Request) (*service.Create{ResourceName}Response, error)
}

func (m *mock{ResourceName}ClientFor{EndpointName}) Create{ResourceName}(ctx context.Context, req *service.Create{ResourceName}Request) (*service.Create{ResourceName}Response, error) {
	return m.create{ResourceName}Func(ctx, req)
}

func Test{EndpointName}(t *testing.T) {
	tests := []struct {
		name           string
		request        *{EndpointName}Request
		mockResponse   *service.Create{ResourceName}Response
		mockError      error
		expectedResult {EndpointName}Response
		expectedError  error
	}{
		{
			name: "successfully creates {resourceName}",
			request: &{EndpointName}Request{
				Name:  "Test {ResourceName}",
				Email: "test@example.com",
			},
			mockResponse: &service.Create{ResourceName}Response{
				ID:        "new-123",
				Name:      "Test {ResourceName}",
				Email:     "test@example.com",
				CreatedAt: "2024-01-01T00:00:00Z",
			},
			mockError: nil,
			expectedResult: {EndpointName}Response{
				ID:        "new-123",
				Name:      "Test {ResourceName}",
				Email:     "test@example.com",
				CreatedAt: "2024-01-01T00:00:00Z",
			},
			expectedError: nil,
		},
		{
			name: "validation error - missing name",
			request: &{EndpointName}Request{
				Name:  "",
				Email: "test@example.com",
			},
			mockResponse:   nil,
			mockError:      nil,
			expectedResult: {EndpointName}Response{},
			expectedError:  {EndpointName}Error{Message: "name is required"},
		},
		{
			name: "validation error - missing email",
			request: &{EndpointName}Request{
				Name:  "Test {ResourceName}",
				Email: "",
			},
			mockResponse:   nil,
			mockError:      nil,
			expectedResult: {EndpointName}Response{},
			expectedError:  {EndpointName}Error{Message: "email is required"},
		},
		{
			name: "backend service error",
			request: &{EndpointName}Request{
				Name:  "Test {ResourceName}",
				Email: "test@example.com",
			},
			mockResponse:   nil,
			mockError:      errors.New("database error"),
			expectedResult: {EndpointName}Response{},
			expectedError:  {EndpointName}Error{Message: "failed to create resource"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &mock{ResourceName}ClientFor{EndpointName}{
				create{ResourceName}Func: func(ctx context.Context, req *service.Create{ResourceName}Request) (*service.Create{ResourceName}Response, error) {
					return tt.mockResponse, tt.mockError
				},
			}

			h := &{endpointName}Handler{
				client: mockClient,
			}

			result, err := h.handle(context.Background(), tt.request)

			if tt.expectedError != nil {
				require.Error(t, err)
				require.Equal(t, tt.expectedError, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedResult, result)
			}
		})
	}
}
```

---

## 4. Service Client Templates

### 4.1 REST Backend Client

**File: `service/{resource}.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// {ResourceName}Client handles communication with the {ResourceName} backend service.
type {ResourceName}Client struct {
	httpClient *http.Client
	baseURL    string
}

// New{ResourceName}Client creates a new {ResourceName}Client.
func New{ResourceName}Client(httpClient *http.Client, baseURL string) *{ResourceName}Client {
	return &{ResourceName}Client{
		httpClient: httpClient,
		baseURL:    baseURL,
	}
}

// --- Get{ResourceName} ---

// Get{ResourceName}Request represents the request to get a {resourceName}.
type Get{ResourceName}Request struct {
	ID string
}

// Get{ResourceName}Response represents the response from getting a {resourceName}.
type Get{ResourceName}Response struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
}

// Get{ResourceName} retrieves a {resourceName} by ID.
func (c *{ResourceName}Client) Get{ResourceName}(ctx context.Context, req *Get{ResourceName}Request) (*Get{ResourceName}Response, error) {
	reqURL := fmt.Sprintf("%s/{resources}/%s", c.baseURL, url.PathEscape(req.ID))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("{resourceName} not found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result Get{ResourceName}Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// --- Create{ResourceName} ---

// Create{ResourceName}Request represents the request to create a {resourceName}.
type Create{ResourceName}Request struct {
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// Create{ResourceName}Response represents the response from creating a {resourceName}.
type Create{ResourceName}Response struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
}

// Create{ResourceName} creates a new {resourceName}.
func (c *{ResourceName}Client) Create{ResourceName}(ctx context.Context, req *Create{ResourceName}Request) (*Create{ResourceName}Response, error) {
	reqURL := fmt.Sprintf("%s/{resources}", c.baseURL)

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result Create{ResourceName}Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// --- Update{ResourceName} ---

// Update{ResourceName}Request represents the request to update a {resourceName}.
type Update{ResourceName}Request struct {
	ID          string   `json:"-"`
	Name        *string  `json:"name,omitempty"`
	Email       *string  `json:"email,omitempty"`
	Description *string  `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// Update{ResourceName}Response represents the response from updating a {resourceName}.
type Update{ResourceName}Response struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	UpdatedAt string `json:"updated_at"`
}

// Update{ResourceName} updates an existing {resourceName}.
func (c *{ResourceName}Client) Update{ResourceName}(ctx context.Context, req *Update{ResourceName}Request) (*Update{ResourceName}Response, error) {
	reqURL := fmt.Sprintf("%s/{resources}/%s", c.baseURL, url.PathEscape(req.ID))

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("{resourceName} not found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result Update{ResourceName}Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}

// --- Delete{ResourceName} ---

// Delete{ResourceName}Request represents the request to delete a {resourceName}.
type Delete{ResourceName}Request struct {
	ID string
}

// Delete{ResourceName} deletes a {resourceName} by ID.
func (c *{ResourceName}Client) Delete{ResourceName}(ctx context.Context, req *Delete{ResourceName}Request) error {
	reqURL := fmt.Sprintf("%s/{resources}/%s", c.baseURL, url.PathEscape(req.ID))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("{resourceName} not found")
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

// --- List{ResourceName}s ---

// List{ResourceName}sRequest represents the request to list {resourceName}s.
type List{ResourceName}sRequest struct {
	Page     int
	PageSize int
	Filter   string
}

// List{ResourceName}sResponse represents the response from listing {resourceName}s.
type List{ResourceName}sResponse struct {
	Items      []*{ResourceName}ListItem `json:"items"`
	TotalCount int                       `json:"total_count"`
}

// {ResourceName}ListItem represents an item in a list response.
type {ResourceName}ListItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// List{ResourceName}s retrieves a paginated list of {resourceName}s.
func (c *{ResourceName}Client) List{ResourceName}s(ctx context.Context, req *List{ResourceName}sRequest) (*List{ResourceName}sResponse, error) {
	reqURL, err := url.Parse(fmt.Sprintf("%s/{resources}", c.baseURL))
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}

	q := reqURL.Query()
	q.Set("page", fmt.Sprintf("%d", req.Page))
	q.Set("page_size", fmt.Sprintf("%d", req.PageSize))
	if req.Filter != "" {
		q.Set("filter", req.Filter)
	}
	reqURL.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var result List{ResourceName}sResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &result, nil
}
```

### 4.2 gRPC Backend Client Note

For gRPC backends, use the generated client directly from the protobuf definitions. Only create a wrapper when:

1. You need to add retry logic
2. You need to transform request/response types
3. You want to abstract the gRPC details from endpoint handlers

**Example wrapper (when needed):**

```go
package service

import (
	"context"

	pb "{ModuleName}/proto/userpb"
)

// {ResourceName}GRPCClient wraps the generated gRPC client.
type {ResourceName}GRPCClient struct {
	client pb.{ResourceName}ServiceClient
}

// New{ResourceName}GRPCClient creates a new gRPC client wrapper.
func New{ResourceName}GRPCClient(client pb.{ResourceName}ServiceClient) *{ResourceName}GRPCClient {
	return &{ResourceName}GRPCClient{client: client}
}

// Get{ResourceName} retrieves a {resourceName} via gRPC.
func (c *{ResourceName}GRPCClient) Get{ResourceName}(ctx context.Context, req *Get{ResourceName}Request) (*Get{ResourceName}Response, error) {
	resp, err := c.client.Get{ResourceName}(ctx, &pb.Get{ResourceName}Request{
		Id: req.ID,
	})
	if err != nil {
		return nil, err
	}
	return &Get{ResourceName}Response{
		ID:        resp.Id,
		Name:      resp.Name,
		Email:     resp.Email,
		CreatedAt: resp.CreatedAt.AsTime().Format("2006-01-02T15:04:05Z"),
	}, nil
}
```

### 4.3 SQL/Database Repository

**File: `service/{resource}_repository.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Err{ResourceName}NotFound is returned when a {resourceName} is not found.
var Err{ResourceName}NotFound = errors.New("{resourceName} not found")

// {ResourceName}Repository handles database operations for {resourceName}s.
type {ResourceName}Repository struct {
	db *sql.DB

	// Prepared statements for better performance
	getStmt    *sql.Stmt
	createStmt *sql.Stmt
	updateStmt *sql.Stmt
	deleteStmt *sql.Stmt
	listStmt   *sql.Stmt
}

// New{ResourceName}Repository creates a new repository with prepared statements.
func New{ResourceName}Repository(db *sql.DB) (*{ResourceName}Repository, error) {
	getStmt, err := db.Prepare("SELECT id, name, email, created_at, updated_at FROM {resources} WHERE id = $1")
	if err != nil {
		return nil, fmt.Errorf("failed to prepare get statement: %w", err)
	}

	createStmt, err := db.Prepare("INSERT INTO {resources} (id, name, email, description, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6)")
	if err != nil {
		return nil, fmt.Errorf("failed to prepare create statement: %w", err)
	}

	updateStmt, err := db.Prepare("UPDATE {resources} SET name = $1, email = $2, updated_at = $3 WHERE id = $4")
	if err != nil {
		return nil, fmt.Errorf("failed to prepare update statement: %w", err)
	}

	deleteStmt, err := db.Prepare("DELETE FROM {resources} WHERE id = $1")
	if err != nil {
		return nil, fmt.Errorf("failed to prepare delete statement: %w", err)
	}

	listStmt, err := db.Prepare("SELECT id, name, created_at FROM {resources} ORDER BY created_at DESC LIMIT $1 OFFSET $2")
	if err != nil {
		return nil, fmt.Errorf("failed to prepare list statement: %w", err)
	}

	return &{ResourceName}Repository{
		db:         db,
		getStmt:    getStmt,
		createStmt: createStmt,
		updateStmt: updateStmt,
		deleteStmt: deleteStmt,
		listStmt:   listStmt,
	}, nil
}

// Close closes all prepared statements.
func (r *{ResourceName}Repository) Close() error {
	var errs []error
	if err := r.getStmt.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := r.createStmt.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := r.updateStmt.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := r.deleteStmt.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := r.listStmt.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("failed to close statements: %v", errs)
	}
	return nil
}

// {ResourceName}Entity represents a {resourceName} in the database.
type {ResourceName}Entity struct {
	ID          string
	Name        string
	Email       string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Get retrieves a {resourceName} by ID.
func (r *{ResourceName}Repository) Get(ctx context.Context, id string) (*{ResourceName}Entity, error) {
	var entity {ResourceName}Entity
	err := r.getStmt.QueryRowContext(ctx, id).Scan(
		&entity.ID,
		&entity.Name,
		&entity.Email,
		&entity.CreatedAt,
		&entity.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Err{ResourceName}NotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query {resourceName}: %w", err)
	}
	return &entity, nil
}

// Create inserts a new {resourceName}.
func (r *{ResourceName}Repository) Create(ctx context.Context, entity *{ResourceName}Entity) error {
	now := time.Now()
	entity.CreatedAt = now
	entity.UpdatedAt = now

	_, err := r.createStmt.ExecContext(ctx,
		entity.ID,
		entity.Name,
		entity.Email,
		entity.Description,
		entity.CreatedAt,
		entity.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert {resourceName}: %w", err)
	}
	return nil
}

// Update modifies an existing {resourceName}.
func (r *{ResourceName}Repository) Update(ctx context.Context, entity *{ResourceName}Entity) error {
	entity.UpdatedAt = time.Now()

	result, err := r.updateStmt.ExecContext(ctx,
		entity.Name,
		entity.Email,
		entity.UpdatedAt,
		entity.ID,
	)
	if err != nil {
		return fmt.Errorf("failed to update {resourceName}: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return Err{ResourceName}NotFound
	}
	return nil
}

// Delete removes a {resourceName} by ID.
func (r *{ResourceName}Repository) Delete(ctx context.Context, id string) error {
	result, err := r.deleteStmt.ExecContext(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to delete {resourceName}: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rows == 0 {
		return Err{ResourceName}NotFound
	}
	return nil
}

// List retrieves a paginated list of {resourceName}s.
func (r *{ResourceName}Repository) List(ctx context.Context, limit, offset int) ([]*{ResourceName}Entity, error) {
	rows, err := r.listStmt.QueryContext(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to query {resourceName}s: %w", err)
	}
	defer rows.Close()

	var entities []*{ResourceName}Entity
	for rows.Next() {
		var entity {ResourceName}Entity
		if err := rows.Scan(&entity.ID, &entity.Name, &entity.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		entities = append(entities, &entity)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	return entities, nil
}

// --- Transaction Pattern ---

// WithTx executes a function within a database transaction.
func (r *{ResourceName}Repository) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("failed to rollback: %w (original error: %v)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}
```

---

## 5. Service Test Templates

### 5.1 REST Client Test with httptest

**File: `service/{resource}_test.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test{ResourceName}Client_Get{ResourceName}(t *testing.T) {
	tests := []struct {
		name           string
		id             string
		serverResponse *Get{ResourceName}Response
		serverStatus   int
		expectError    bool
		errorContains  string
	}{
		{
			name: "success",
			id:   "123",
			serverResponse: &Get{ResourceName}Response{
				ID:        "123",
				Name:      "Test {ResourceName}",
				Email:     "test@example.com",
				CreatedAt: "2024-01-01T00:00:00Z",
			},
			serverStatus: http.StatusOK,
			expectError:  false,
		},
		{
			name:          "not found",
			id:            "not-found",
			serverStatus:  http.StatusNotFound,
			expectError:   true,
			errorContains: "not found",
		},
		{
			name:          "server error",
			id:            "123",
			serverStatus:  http.StatusInternalServerError,
			expectError:   true,
			errorContains: "unexpected status code",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/{resources}/"+tt.id, r.URL.Path)

				w.WriteHeader(tt.serverStatus)
				if tt.serverResponse != nil {
					json.NewEncoder(w).Encode(tt.serverResponse)
				}
			}))
			defer server.Close()

			client := New{ResourceName}Client(server.Client(), server.URL)

			resp, err := client.Get{ResourceName}(context.Background(), &Get{ResourceName}Request{ID: tt.id})

			if tt.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errorContains)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.serverResponse.ID, resp.ID)
				require.Equal(t, tt.serverResponse.Name, resp.Name)
			}
		})
	}
}

func Test{ResourceName}Client_Create{ResourceName}(t *testing.T) {
	tests := []struct {
		name           string
		request        *Create{ResourceName}Request
		serverResponse *Create{ResourceName}Response
		serverStatus   int
		expectError    bool
	}{
		{
			name: "success",
			request: &Create{ResourceName}Request{
				Name:  "New {ResourceName}",
				Email: "new@example.com",
			},
			serverResponse: &Create{ResourceName}Response{
				ID:        "new-123",
				Name:      "New {ResourceName}",
				Email:     "new@example.com",
				CreatedAt: "2024-01-01T00:00:00Z",
			},
			serverStatus: http.StatusCreated,
			expectError:  false,
		},
		{
			name: "server error",
			request: &Create{ResourceName}Request{
				Name:  "New {ResourceName}",
				Email: "new@example.com",
			},
			serverStatus: http.StatusInternalServerError,
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/{resources}", r.URL.Path)

				var body Create{ResourceName}Request
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, tt.request.Name, body.Name)
				require.Equal(t, tt.request.Email, body.Email)

				w.WriteHeader(tt.serverStatus)
				if tt.serverResponse != nil {
					json.NewEncoder(w).Encode(tt.serverResponse)
				}
			}))
			defer server.Close()

			client := New{ResourceName}Client(server.Client(), server.URL)

			resp, err := client.Create{ResourceName}(context.Background(), tt.request)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.serverResponse.ID, resp.ID)
			}
		})
	}
}
```

### 5.2 Repository Test with Test Database

**File: `service/{resource}_repository_test.go`**

```go
// Copyright (c) {Year} {Organization} and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package service

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	_ "github.com/mattn/go-sqlite3" // SQLite driver for testing
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)

	// Create table
	_, err = db.Exec(`
		CREATE TABLE {resources} (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT NOT NULL,
			description TEXT,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)
	`)
	require.NoError(t, err)

	t.Cleanup(func() {
		db.Close()
	})

	return db
}

func Test{ResourceName}Repository_CRUD(t *testing.T) {
	db := setupTestDB(t)
	repo, err := New{ResourceName}Repository(db)
	require.NoError(t, err)
	defer repo.Close()

	ctx := context.Background()

	// Test Create
	t.Run("create", func(t *testing.T) {
		entity := &{ResourceName}Entity{
			ID:          "test-123",
			Name:        "Test {ResourceName}",
			Email:       "test@example.com",
			Description: "A test {resourceName}",
		}

		err := repo.Create(ctx, entity)
		require.NoError(t, err)
		require.False(t, entity.CreatedAt.IsZero())
	})

	// Test Get
	t.Run("get", func(t *testing.T) {
		entity, err := repo.Get(ctx, "test-123")
		require.NoError(t, err)
		require.Equal(t, "Test {ResourceName}", entity.Name)
		require.Equal(t, "test@example.com", entity.Email)
	})

	// Test Get Not Found
	t.Run("get not found", func(t *testing.T) {
		_, err := repo.Get(ctx, "not-found")
		require.ErrorIs(t, err, Err{ResourceName}NotFound)
	})

	// Test Update
	t.Run("update", func(t *testing.T) {
		entity := &{ResourceName}Entity{
			ID:    "test-123",
			Name:  "Updated {ResourceName}",
			Email: "updated@example.com",
		}

		err := repo.Update(ctx, entity)
		require.NoError(t, err)

		updated, err := repo.Get(ctx, "test-123")
		require.NoError(t, err)
		require.Equal(t, "Updated {ResourceName}", updated.Name)
	})

	// Test Delete
	t.Run("delete", func(t *testing.T) {
		err := repo.Delete(ctx, "test-123")
		require.NoError(t, err)

		_, err = repo.Get(ctx, "test-123")
		require.ErrorIs(t, err, Err{ResourceName}NotFound)
	})
}

func Test{ResourceName}Repository_List(t *testing.T) {
	db := setupTestDB(t)
	repo, err := New{ResourceName}Repository(db)
	require.NoError(t, err)
	defer repo.Close()

	ctx := context.Background()

	// Create test data
	for i := 0; i < 5; i++ {
		entity := &{ResourceName}Entity{
			ID:    fmt.Sprintf("test-%d", i),
			Name:  fmt.Sprintf("Test {ResourceName} %d", i),
			Email: fmt.Sprintf("test%d@example.com", i),
		}
		require.NoError(t, repo.Create(ctx, entity))
	}

	// Test pagination
	t.Run("first page", func(t *testing.T) {
		entities, err := repo.List(ctx, 2, 0)
		require.NoError(t, err)
		require.Len(t, entities, 2)
	})

	t.Run("second page", func(t *testing.T) {
		entities, err := repo.List(ctx, 2, 2)
		require.NoError(t, err)
		require.Len(t, entities, 2)
	})
}
```

---

## 6. Path Building Reference

### Static Paths

```go
// Simple path
bedrockrest.GET("/users", handler)

// Multi-segment path
bedrockrest.GET("/api/v1/users", handler)
```

### Path Parameters

```go
// Single parameter
userID := bedrockrest.PathParam[string]("id", bedrockrest.Required())
ep := bedrockrest.GET("/users/{id}", handler)
ep = userID.Read(ep)

// Multiple parameters
userID := bedrockrest.PathParam[string]("user_id", bedrockrest.Required())
orderID := bedrockrest.PathParam[string]("order_id", bedrockrest.Required())
ep := bedrockrest.GET("/users/{user_id}/orders/{order_id}", handler)
ep = userID.Read(ep)
ep = orderID.Read(ep)
```

### Query Parameters

```go
// Required query param
nameParam := bedrockrest.QueryParam[string]("name", bedrockrest.Required())

// Optional with default
pageParam := bedrockrest.QueryParam[int]("page", bedrockrest.DefaultValue(1))

// Optional without default
filterParam := bedrockrest.QueryParam[string]("filter", bedrockrest.Optional())

// With validation
limitParam := bedrockrest.QueryParam[int]("limit",
    bedrockrest.DefaultValue(20),
    bedrockrest.Minimum(1),
    bedrockrest.Maximum(100),
)

// Enum values
statusParam := bedrockrest.QueryParam[string]("status",
    bedrockrest.Optional(),
    bedrockrest.Enum("active", "inactive", "pending"),
)
```

### Header Parameters

```go
// Required header
authHeader := bedrockrest.HeaderParam[string]("Authorization", bedrockrest.Required())

// Optional header
traceHeader := bedrockrest.HeaderParam[string]("X-Trace-ID", bedrockrest.Optional())
```

### Complete Example

```go
func GetUserOrder() rest.Option {
    userID := bedrockrest.PathParam[string]("user_id", bedrockrest.Required())
    orderID := bedrockrest.PathParam[string]("order_id", bedrockrest.Required())
    includeItems := bedrockrest.QueryParam[bool]("include_items", bedrockrest.DefaultValue(false))
    authHeader := bedrockrest.HeaderParam[string]("Authorization", bedrockrest.Required())

    ep := bedrockrest.GET("/users/{user_id}/orders/{order_id}", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) (OrderResponse, error) {
        uid := bedrockrest.ParamFrom(req, userID)
        oid := bedrockrest.ParamFrom(req, orderID)
        withItems := bedrockrest.ParamFrom(req, includeItems)
        auth := bedrockrest.ParamFrom(req, authHeader)
        // Use parameters...
        return OrderResponse{}, nil
    })
    ep = userID.Read(ep)
    ep = orderID.Read(ep)
    ep = includeItems.Read(ep)
    ep = authHeader.Read(ep)
    ep = bedrockrest.WriteJSON[OrderResponse](http.StatusOK, ep)
    route := bedrockrest.CatchAll(http.StatusInternalServerError, wrapError, ep)

    return rest.Handle(route)
}
```

---

## 7. Data Mapping Code Patterns

### Simple Field Mapping

Direct assignment when field names and types match:

```go
// Source: service.GetUserResponse
// Target: endpoint.GetUserResponse

response := GetUserResponse{
    ID:        serviceResp.ID,
    Name:      serviceResp.Name,
    Email:     serviceResp.Email,
    CreatedAt: serviceResp.CreatedAt,
}
```

### Nested Field Mapping

Access nested fields in source to populate flat target:

```go
// Source: service.OrderResponse with nested Address
// Target: endpoint.OrderResponse with flat fields

response := OrderResponse{
    OrderID:       serviceResp.ID,
    CustomerName:  serviceResp.Customer.Name,
    ShippingCity:  serviceResp.Address.City,
    ShippingState: serviceResp.Address.State,
    ShippingZip:   serviceResp.Address.ZipCode,
}
```

### Type Conversion

Convert between different types:

```go
import "strconv"

// String to int
response := Response{
    Count: strconv.Atoi(serviceResp.CountStr), // Handle error in production
}

// Int to string
response := Response{
    ID: strconv.Itoa(serviceResp.IntID),
}

// Time formatting
response := Response{
    CreatedAt: serviceResp.CreatedAt.Format(time.RFC3339),
}

// Time parsing
createdAt, _ := time.Parse(time.RFC3339, req.CreatedAt)
```

### Conditional Mapping (Optional Fields)

Handle pointer fields and optional values:

```go
// Map optional field (pointer)
if serviceResp.Description != nil {
    response.Description = *serviceResp.Description
}

// Map with default value
if serviceResp.Status != nil {
    response.Status = *serviceResp.Status
} else {
    response.Status = "unknown"
}

// Reverse: create pointer for optional field in request
var description *string
if req.Description != "" {
    description = &req.Description
}
serviceReq := &service.CreateRequest{
    Description: description,
}
```

### Slice/Array Mapping

Transform arrays/slices:

```go
// Map slice of items
items := make([]ResponseItem, len(serviceResp.Items))
for i, item := range serviceResp.Items {
    items[i] = ResponseItem{
        ID:   item.ID,
        Name: item.Name,
    }
}
response := Response{
    Items: items,
}

// Filter while mapping
var activeItems []ResponseItem
for _, item := range serviceResp.Items {
    if item.Status == "active" {
        activeItems = append(activeItems, ResponseItem{
            ID:   item.ID,
            Name: item.Name,
        })
    }
}
```

### Enum/Constant Mapping

Map between different enum representations:

```go
// Map string status to custom type
var status OrderStatus
switch serviceResp.Status {
case "pending":
    status = OrderStatusPending
case "completed":
    status = OrderStatusCompleted
case "cancelled":
    status = OrderStatusCancelled
default:
    status = OrderStatusUnknown
}
response := Response{Status: status}
```

### Map/Dictionary Mapping

Transform maps:

```go
// Map with transformation
metadata := make(map[string]string)
for k, v := range serviceResp.Metadata {
    metadata[k] = fmt.Sprintf("%v", v) // Convert any to string
}
response := Response{Metadata: metadata}
```

### Complete Mapping Function Example

```go
// mapServiceResponseToEndpointResponse transforms the service layer response
// to the endpoint response format.
func mapServiceResponseToEndpointResponse(svc *service.GetUserResponse) GetUserResponse {
    resp := GetUserResponse{
        ID:        svc.ID,
        Name:      svc.Name,
        Email:     svc.Email,
        CreatedAt: svc.CreatedAt.Format(time.RFC3339),
    }

    // Handle optional fields
    if svc.Bio != nil {
        resp.Bio = *svc.Bio
    }

    // Map nested address
    if svc.Address != nil {
        resp.Address = &AddressResponse{
            Street:  svc.Address.Street,
            City:    svc.Address.City,
            Country: svc.Address.Country,
        }
    }

    // Map roles slice
    resp.Roles = make([]string, len(svc.Roles))
    for i, role := range svc.Roles {
        resp.Roles[i] = role.Name
    }

    return resp
}
```

---

## Quick Reference Card

| Pattern | Template |
|---------|----------|
| GET (Producer) | `bedrockrest.GET[Resp](path, handler)` → `WriteJSON` |
| POST (Handler) | `bedrockrest.POST[Req, Resp](path, handler)` → `ReadJSON` → `WriteJSON` |
| PUT (Handler) | `bedrockrest.PUT[Req, Resp](path, handler)` → `ReadJSON` → `WriteJSON` |
| DELETE | `bedrockrest.DELETE[Resp](path, handler)` |
| Path Param | `bedrockrest.PathParam[T]("name", opts...)` |
| Query Param | `bedrockrest.QueryParam[T]("name", opts...)` |
| Header Param | `bedrockrest.HeaderParam[T]("name", opts...)` |
| Read Param | `bedrockrest.ParamFrom(req, param)` |
| Error Response | `bedrockrest.ErrorJSON[E](status, ep)` |
| Catch All | `bedrockrest.CatchAll(status, wrapFn, ep)` |
| Register Route | `rest.Handle(route)` |

---

## Checklist for Generated Code

Before finalizing generated code, verify:

- [ ] All imports are included and correct
- [ ] Package name matches directory
- [ ] Error types implement `error` interface
- [ ] Handler struct has `tracer` and `log` fields
- [ ] Tracer and Logger use correct service name
- [ ] Path parameters use `bedrockrest.PathParam` with `Required()`
- [ ] Query parameters have appropriate defaults or `Optional()`
- [ ] All parameters are `.Read()` into the endpoint chain
- [ ] Error handling returns typed errors for proper status codes
- [ ] `CatchAll` wraps all unhandled errors
- [ ] Service client methods return proper error types
- [ ] Tests use `require` not `assert`
- [ ] Tests are table-driven where appropriate
- [ ] Resource cleanup uses lifecycle hooks or defer
