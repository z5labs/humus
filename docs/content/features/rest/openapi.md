---
title: OpenAPI
description: Working with generated specs
weight: 70
type: docs
---


Humus automatically generates OpenAPI 3.0 specifications for all REST APIs, providing comprehensive API documentation with zero manual effort.

## Overview

Every Humus REST API includes:

- **Automatic OpenAPI 3.0 schema generation** from Go types
- **Live specification endpoint** at `GET /openapi.json`
- **Request/response schemas** via reflection
- **Parameter validation rules** in the specification
- **Security scheme documentation** for authentication
- **No manual annotation required** - schemas generated from code

This enables seamless integration with tools like Swagger UI, Postman, ReDoc, and API client generators.

## Accessing the OpenAPI Specification

Every API created with `rest.NewApi()` automatically serves its OpenAPI specification:

```bash
# Get the full OpenAPI spec
curl http://localhost:8080/openapi.json

# Pretty-print with jq
curl http://localhost:8080/openapi.json | jq
```

### Basic Example

```go
package main

import (
    "context"
    "net/http"

    "github.com/z5labs/humus/rest"
)

type CreateUserRequest struct {
    Email    string `json:"email"`
    Name     string `json:"name"`
    Age      int    `json:"age,omitempty"`
}

type UserResponse struct {
    ID       string `json:"id"`
    Email    string `json:"email"`
    Name     string `json:"name"`
    Age      int    `json:"age,omitempty"`
    Created  string `json:"created"`
}

func Init(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
    handler := rest.HandlerFunc[CreateUserRequest, UserResponse](
        func(ctx context.Context, req *CreateUserRequest) (*UserResponse, error) {
            // Implementation
            return &UserResponse{
                ID:      "user-123",
                Email:   req.Email,
                Name:    req.Name,
                Age:     req.Age,
                Created: "2025-01-15T10:30:00Z",
            }, nil
        },
    )

    api := rest.NewApi(
        "User Management API",
        "1.0.0",
        rest.Handle(
            http.MethodPost,
            rest.BasePath("/users"),
            rest.HandleJson(handler),
        ),
    )

    return api, nil
}
```

**Generated OpenAPI Schema:**

```json
{
  "openapi": "3.0",
  "info": {
    "title": "User Management API",
    "version": "1.0.0"
  },
  "paths": {
    "/users": {
      "post": {
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "properties": {
                  "email": {"type": "string"},
                  "name": {"type": "string"},
                  "age": {"type": "integer"}
                },
                "required": ["email", "name"]
              }
            }
          }
        },
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "id": {"type": "string"},
                    "email": {"type": "string"},
                    "name": {"type": "string"},
                    "age": {"type": "integer"},
                    "created": {"type": "string"}
                  },
                  "required": ["id", "email", "name", "created"]
                }
              }
            }
          }
        }
      }
    }
  }
}
```

## Schema Generation

Humus uses reflection via [github.com/swaggest/jsonschema-go](https://github.com/swaggest/jsonschema-go) to automatically generate JSON schemas from Go types.

### Basic Types

Go types map to JSON schema types:

```go
type Product struct {
    Name        string  `json:"name"`           // "type": "string"
    Price       float64 `json:"price"`          // "type": "number"
    Quantity    int     `json:"quantity"`       // "type": "integer"
    Available   bool    `json:"available"`      // "type": "boolean"
    Tags        []string `json:"tags"`          // "type": "array", "items": {"type": "string"}
}
```

### Optional Fields

Use `omitempty` to mark fields as optional:

```go
type User struct {
    ID    string `json:"id"`              // Required
    Email string `json:"email"`           // Required
    Phone string `json:"phone,omitempty"` // Optional
}
```

**Generated Schema:**

```json
{
  "type": "object",
  "properties": {
    "id": {"type": "string"},
    "email": {"type": "string"},
    "phone": {"type": "string"}
  },
  "required": ["id", "email"]
}
```

### Nested Objects

Nested structs are automatically expanded:

```go
type Address struct {
    Street  string `json:"street"`
    City    string `json:"city"`
    Country string `json:"country"`
}

type Customer struct {
    Name    string  `json:"name"`
    Address Address `json:"address"`
}
```

**Generated Schema:**

```json
{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "address": {
      "type": "object",
      "properties": {
        "street": {"type": "string"},
        "city": {"type": "string"},
        "country": {"type": "string"}
      },
      "required": ["street", "city", "country"]
    }
  },
  "required": ["name", "address"]
}
```

### Arrays and Slices

```go
type Catalog struct {
    Products  []Product           `json:"products"`
    Categories []string           `json:"categories"`
    Tags      map[string]string  `json:"tags"`
}
```

**Generated Schema:**

```json
{
  "type": "object",
  "properties": {
    "products": {
      "type": "array",
      "items": {
        "$ref": "#/components/schemas/Product"
      }
    },
    "categories": {
      "type": "array",
      "items": {"type": "string"}
    },
    "tags": {
      "type": "object",
      "additionalProperties": {"type": "string"}
    }
  }
}
```

### Pointers

Pointer types become optional in the schema:

```go
type UpdateRequest struct {
    Name  *string `json:"name,omitempty"`   // Optional, can be null
    Email *string `json:"email,omitempty"`  // Optional, can be null
}
```

### Enumerations

Use type aliases or constants for enumerations:

```go
type Status string

const (
    StatusPending   Status = "pending"
    StatusActive    Status = "active"
    StatusCompleted Status = "completed"
)

type Order struct {
    ID     string `json:"id"`
    Status Status `json:"status"`
}
```

**Note:** Basic Go enums don't automatically generate `enum` constraints in the schema. For strict validation, implement custom schema methods or use validation in your handler.

### Time and Date

Use `time.Time` for timestamps:

```go
import "time"

type Event struct {
    Name      string    `json:"name"`
    StartTime time.Time `json:"start_time"`
    EndTime   time.Time `json:"end_time,omitempty"`
}
```

**Generated Schema:**

```json
{
  "type": "object",
  "properties": {
    "name": {"type": "string"},
    "start_time": {"type": "string", "format": "date-time"},
    "end_time": {"type": "string", "format": "date-time"}
  },
  "required": ["name", "start_time"]
}
```

## Path Parameters

Path parameters are automatically included in the OpenAPI spec:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id").Path("orders").Param("orderId"),
    getOrderHandler,
)
```

**Generated Operation:**

```json
{
  "paths": {
    "/users/{id}/orders/{orderId}": {
      "get": {
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "schema": {"type": "string"}
          },
          {
            "name": "orderId",
            "in": "path",
            "required": true,
            "schema": {"type": "string"}
          }
        ]
      }
    }
  }
}
```

## Query Parameters

Query parameters defined with `rest.QueryParam()` appear in the spec:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/search"),
    searchHandler,
    rest.QueryParam("q", rest.Required()),
    rest.QueryParam("limit", rest.Regex(regexp.MustCompile(`^\d+$`))),
    rest.QueryParam("offset"),
)
```

**Generated Parameters:**

```json
{
  "parameters": [
    {
      "name": "q",
      "in": "query",
      "required": true,
      "schema": {"type": "string"}
    },
    {
      "name": "limit",
      "in": "query",
      "required": false,
      "schema": {
        "type": "string",
        "pattern": "^\\d+$"
      }
    },
    {
      "name": "offset",
      "in": "query",
      "required": false,
      "schema": {"type": "string"}
    }
  ]
}
```

## Headers

Header parameters are documented in the specification:

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/data"),
    handler,
    rest.Header("X-API-Key", rest.Required()),
    rest.Header("X-Request-ID"),
)
```

**Generated Parameters:**

```json
{
  "parameters": [
    {
      "name": "X-API-Key",
      "in": "header",
      "required": true,
      "schema": {"type": "string"}
    },
    {
      "name": "X-Request-ID",
      "in": "header",
      "required": false,
      "schema": {"type": "string"}
    }
  ]
}
```

## Security Schemes

Authentication schemes are automatically documented. See [Authentication]({{< ref "authentication" >}}) for details.

### JWT Authentication

```go
rest.Handle(
    http.MethodPost,
    rest.BasePath("/orders"),
    createOrderHandler,
    rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier)),
)
```

**Generated Security Scheme:**

```json
{
  "components": {
    "securitySchemes": {
      "jwt": {
        "type": "http",
        "scheme": "bearer",
        "bearerFormat": "JWT"
      }
    }
  },
  "paths": {
    "/orders": {
      "post": {
        "security": [{"jwt": []}]
      }
    }
  }
}
```

### API Key Authentication

```go
rest.Handle(
    http.MethodGet,
    rest.BasePath("/data"),
    handler,
    rest.Header("X-API-Key", rest.Required(), rest.APIKey("api-key")),
)
```

**Generated Security Scheme:**

```json
{
  "components": {
    "securitySchemes": {
      "api-key": {
        "type": "apiKey",
        "in": "header",
        "name": "X-API-Key"
      }
    }
  },
  "paths": {
    "/data": {
      "get": {
        "security": [{"api-key": []}]
      }
    }
  }
}
```

## Integration with Tools

### Swagger UI

Serve Swagger UI to visualize your API:

```go
import (
    "net/http"
    "github.com/z5labs/humus/rest"
)

func Init(ctx context.Context, cfg rest.Config) (*rest.Api, error) {
    api := rest.NewApi(
        "My API",
        "1.0.0",
        // Your operations...
    )

    // Serve Swagger UI (manual setup)
    http.Handle("/docs/", http.StripPrefix("/docs/", http.FileServer(http.Dir("./swagger-ui"))))

    return api, nil
}
```

Configure Swagger UI to point to `/openapi.json`:

```html
<!-- swagger-ui/index.html -->
<!DOCTYPE html>
<html>
<head>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@latest/swagger-ui.css">
</head>
<body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@latest/swagger-ui-bundle.js"></script>
    <script>
        SwaggerUIBundle({
            url: "/openapi.json",
            dom_id: '#swagger-ui'
        });
    </script>
</body>
</html>
```

Access at: `http://localhost:8080/docs/`

### Postman

Import the OpenAPI spec into Postman:

1. Open Postman
2. Click **Import**
3. Select **Link** and enter `http://localhost:8080/openapi.json`
4. Postman generates a complete API collection with all endpoints

### ReDoc

Serve ReDoc for clean API documentation:

```html
<!DOCTYPE html>
<html>
<head>
    <title>API Documentation</title>
</head>
<body>
    <redoc spec-url="/openapi.json"></redoc>
    <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
</body>
</html>
```

### OpenAPI Generator

Generate client libraries in any language:

```bash
# Generate TypeScript client
openapi-generator-cli generate \
    -i http://localhost:8080/openapi.json \
    -g typescript-fetch \
    -o ./generated/typescript-client

# Generate Python client
openapi-generator-cli generate \
    -i http://localhost:8080/openapi.json \
    -g python \
    -o ./generated/python-client

# Generate Go client
openapi-generator-cli generate \
    -i http://localhost:8080/openapi.json \
    -g go \
    -o ./generated/go-client
```

## Complete Example

```go
package main

import (
    "context"
    "net/http"
    "time"

    "github.com/z5labs/humus/rest"
)

// Request/Response Types
type CreateBookRequest struct {
    Title       string   `json:"title"`
    Author      string   `json:"author"`
    ISBN        string   `json:"isbn,omitempty"`
    PublishDate string   `json:"publish_date,omitempty"`
    Tags        []string `json:"tags,omitempty"`
}

type Book struct {
    ID          string    `json:"id"`
    Title       string    `json:"title"`
    Author      string    `json:"author"`
    ISBN        string    `json:"isbn,omitempty"`
    PublishDate string    `json:"publish_date,omitempty"`
    Tags        []string  `json:"tags,omitempty"`
    CreatedAt   time.Time `json:"created_at"`
}

type BookList struct {
    Books      []Book `json:"books"`
    TotalCount int    `json:"total_count"`
    Page       int    `json:"page"`
}

// Handlers
func createBookHandler(ctx context.Context, req *CreateBookRequest) (*Book, error) {
    return &Book{
        ID:          "book-123",
        Title:       req.Title,
        Author:      req.Author,
        ISBN:        req.ISBN,
        PublishDate: req.PublishDate,
        Tags:        req.Tags,
        CreatedAt:   time.Now(),
    }, nil
}

func listBooksHandler(ctx context.Context) (*BookList, error) {
    page := rest.QueryParamValue(ctx, "page")
    // Use page parameter...

    return &BookList{
        Books: []Book{
            {
                ID:        "book-1",
                Title:     "Go Programming",
                Author:    "John Doe",
                CreatedAt: time.Now(),
            },
        },
        TotalCount: 1,
        Page:       1,
    }, nil
}

func getBookHandler(ctx context.Context) (*Book, error) {
    bookID := rest.PathParamValue(ctx, "id")
    // Fetch book by ID...

    return &Book{
        ID:        bookID,
        Title:     "Go Programming",
        Author:    "John Doe",
        CreatedAt: time.Now(),
    }, nil
}

type Config struct {
    rest.Config `config:",squash"`
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    api := rest.NewApi(
        "Bookstore API",
        "1.0.0",
        // Create a book
        rest.Handle(
            http.MethodPost,
            rest.BasePath("/books"),
            rest.HandleJson(rest.HandlerFunc[CreateBookRequest, Book](createBookHandler)),
        ),
        // List books with pagination
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/books"),
            rest.ProduceJson(rest.ProducerFunc[BookList](listBooksHandler)),
            rest.QueryParam("page"),
            rest.QueryParam("limit"),
        ),
        // Get a specific book
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/books").Param("id"),
            rest.ProduceJson(rest.ProducerFunc[Book](getBookHandler)),
        ),
    )

    return api, nil
}

func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}
```

**Access the specification:**

```bash
# Start the service
go run main.go

# Get the OpenAPI spec
curl http://localhost:8080/openapi.json | jq

# The spec includes:
# - All three endpoints (/books POST, GET, /books/{id} GET)
# - Request schemas (CreateBookRequest)
# - Response schemas (Book, BookList)
# - Path parameters ({id})
# - Query parameters (page, limit)
# - Proper HTTP methods and status codes
```

## Best Practices

### 1. Use Descriptive Type Names

Type names appear in the OpenAPI schema:

```go
// Good - clear and descriptive
type CreateUserRequest struct { ... }
type UserResponse struct { ... }

// Avoid - vague names
type Request struct { ... }
type Response struct { ... }
```

### 2. Add JSON Tags

Always use `json` tags for consistent field naming:

```go
type User struct {
    UserID    string `json:"user_id"`     // snake_case in JSON
    FirstName string `json:"first_name"`  // consistent naming
    LastName  string `json:"last_name"`
}
```

### 3. Document with Comments

While Humus doesn't currently extract comments into the OpenAPI spec, they help developers:

```go
// User represents a registered user in the system
type User struct {
    // Unique identifier for the user
    ID string `json:"id"`

    // User's email address (must be unique)
    Email string `json:"email"`
}
```

### 4. Use Separate Request/Response Types

Don't reuse types for both requests and responses:

```go
// Good - separate types
type CreateUserRequest struct {
    Email string `json:"email"`
    Name  string `json:"name"`
}

type UserResponse struct {
    ID       string    `json:"id"`
    Email    string    `json:"email"`
    Name     string    `json:"name"`
    Created  time.Time `json:"created"`
}

// Avoid - single type for both
type User struct {
    ID      string    `json:"id,omitempty"`  // Confusing: required in response, not in request
    Email   string    `json:"email"`
    Name    string    `json:"name"`
    Created time.Time `json:"created,omitempty"`
}
```

### 5. Version Your API

Include version in the API title or base path:

```go
api := rest.NewApi(
    "Bookstore API",
    "2.0.0",  // Semantic versioning
    // ...
)

// Or use versioned base paths
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/v2/books"),
    handler,
)
```

### 6. Validate the Generated Spec

Use OpenAPI validators to ensure correctness:

```bash
# Using openapi-generator-cli
openapi-generator-cli validate -i http://localhost:8080/openapi.json

# Using Spectral (advanced linting)
spectral lint http://localhost:8080/openapi.json
```

### 7. Cache the Spec for Performance

For high-traffic APIs, consider caching the generated spec:

```go
// Cache the spec at startup
var cachedSpec []byte

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    api := rest.NewApi("My API", "1.0.0", ...)

    // Pre-generate and cache the spec
    resp := httptest.NewRecorder()
    req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
    api.ServeHTTP(resp, req)
    cachedSpec = resp.Body.Bytes()

    return api, nil
}
```

## Limitations

### No Description Fields

Currently, Humus does not extract Go comments into OpenAPI `description` fields. Type and field descriptions must be added manually if needed.

### Limited Validation Constraints

While parameter validators (regex, required) appear in the spec, complex field-level validations (min/max length, numeric ranges) are not automatically reflected. Implement these in your handlers.

### No Response Status Code Customization

Response types currently default to `200 OK`. Custom status codes require implementing the `TypedResponse` interface directly.

## Next Steps

- Learn about [Authentication]({{< ref "authentication" >}}) to add security schemes
- Explore [Error Handling]({{< ref "error-handling" >}}) for error response schemas
- Read [Routing]({{< ref "routing" >}}) for path and parameter configuration
- See the [OpenAPI 3.0 Specification](https://spec.openapis.org/oas/v3.0.0) for complete schema reference
- Check [pkg.go.dev](https://pkg.go.dev/github.com/z5labs/humus/rest) for API reference
