---
title: Handler Helpers
description: Type-safe handler creation with built-in serialization
weight: 30
type: docs
---


Humus provides handler helpers that simplify common REST API patterns by combining type-safe request/response handling with automatic serialization and OpenAPI schema generation.

## Overview

The REST package provides five categories of handler helpers:

1. **JSON Handlers** - For endpoints that consume and/or produce JSON
2. **HTML Template Handlers** - For endpoints that render server-side HTML
3. **Form Handlers** - For endpoints that process HTML form submissions
4. **Producer/Consumer Patterns** - For endpoints with only request or only response
5. **Low-level Wrappers** - For building custom serialization formats

## Core Interfaces

All handler helpers build on these core interfaces:

```go
// Handler processes a request and returns a response
type Handler[Req, Resp any] interface {
    Handle(context.Context, *Req) (*Resp, error)
}

// Producer returns a response without consuming a request
type Producer[T any] interface {
    Produce(context.Context) (*T, error)
}

// Consumer consumes a request without returning a response
type Consumer[T any] interface {
    Consume(context.Context, *T) error
}
```

## JSON Handlers

The most common handlers work with JSON payloads. These provide automatic serialization, content-type validation, and OpenAPI schema generation.

### HandleJson - Full Request and Response

Use `HandleJson` when your endpoint consumes and produces JSON:

```go
// Define your handler logic
handler := rest.HandlerFunc[CreateUserRequest, User](
    func(ctx context.Context, req *CreateUserRequest) (*User, error) {
        user := &User{
            ID:    generateID(),
            Name:  req.Name,
            Email: req.Email,
        }
        return user, nil
    },
)

// Wrap with JSON serialization
rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(handler),
)
```

**What happens automatically:**
- Request body parsed as JSON
- Content-Type validation (requires `application/json`)
- Response serialized as JSON with `Content-Type: application/json`
- OpenAPI request/response schemas generated from types

**OpenAPI Output:**
```json
{
  "requestBody": {
    "required": true,
    "content": {
      "application/json": {
        "schema": {
          "$ref": "#/components/schemas/CreateUserRequest"
        }
      }
    }
  },
  "responses": {
    "200": {
      "content": {
        "application/json": {
          "schema": {
            "$ref": "#/components/schemas/User"
          }
        }
      }
    }
  }
}
```

### ProduceJson - GET Endpoints

Use `ProduceJson` for GET endpoints that return data without consuming a request body:

```go
// Define your producer
producer := rest.ProducerFunc[[]User](
    func(ctx context.Context) (*[]User, error) {
        users := getUsersFromDB(ctx)
        return &users, nil
    },
)

// Wrap with JSON serialization
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users"),
    rest.ProduceJson(producer),
)
```

**What happens automatically:**
- No request body parsing
- Response serialized as JSON
- OpenAPI response schema generated

**Accessing Path/Query Parameters:**
```go
producer := rest.ProducerFunc[User](
    func(ctx context.Context) (*User, error) {
        // Extract parameters from context
        userID := rest.PathParamValue(ctx, "id")
        include := rest.QueryParamValue(ctx, "include")

        user := getUserByID(ctx, userID, include)
        return user, nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rest.ProduceJson(producer),
    rest.QueryParam("include"),
)
```

### ConsumeOnlyJson - Webhook Endpoints

Use `ConsumeOnlyJson` for POST/PUT webhooks that process data but don't return content:

```go
// Define your consumer
consumer := rest.ConsumerFunc[WebhookPayload](
    func(ctx context.Context, payload *WebhookPayload) error {
        processWebhook(ctx, payload)
        return nil
    },
)

// Wrap with JSON deserialization
rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks/github"),
    rest.ConsumeOnlyJson(consumer),
)
```

**What happens automatically:**
- Request body parsed as JSON
- Content-Type validation
- Returns `200 OK` with empty body on success
- Returns appropriate error status on failure

**Response behavior:**
```http
POST /webhooks/github HTTP/1.1
Content-Type: application/json

{"event": "push", "repository": "myrepo"}

HTTP/1.1 200 OK
Content-Length: 0
```

## HTML Template Handlers

HTML template handlers enable server-side rendering of HTML pages using Go's `html/template` package. These handlers automatically set the correct content type and provide built-in XSS protection through context-aware HTML escaping.

### ProduceHTML - GET Endpoints

Use `ProduceHTML` for GET endpoints that return server-rendered HTML pages:

```go
import "html/template"

// Define your template
tmpl := template.Must(template.New("user").Parse(`
<!DOCTYPE html>
<html>
<head><title>{{.Name}}'s Profile</title></head>
<body>
    <h1>{{.Name}}</h1>
    <p>Email: {{.Email}}</p>
    <p>Joined: {{.CreatedAt}}</p>
</body>
</html>
`))

// Define your producer
producer := rest.ProducerFunc[User](
    func(ctx context.Context) (*User, error) {
        userID := rest.PathParamValue(ctx, "id")
        user := getUserFromDB(ctx, userID)
        return user, nil
    },
)

// Wrap with HTML template rendering
rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rest.ProduceHTML(producer, tmpl),
)
```

**What happens automatically:**
- Response rendered using the provided HTML template
- Content-Type set to `text/html; charset=utf-8`
- XSS protection via automatic HTML escaping
- OpenAPI response schema generated with `text/html` content type

**OpenAPI Output:**
```json
{
  "responses": {
    "200": {
      "content": {
        "text/html": {
          "schema": {
            "type": "string"
          }
        }
      }
    }
  }
}
```

### ReturnHTML - Full Request and Response

Use `ReturnHTML` when your endpoint processes JSON requests but returns HTML responses:

```go
// Define your handler logic
handler := rest.HandlerFunc[SearchRequest, SearchResults](
    func(ctx context.Context, req *SearchRequest) (*SearchResults, error) {
        results := performSearch(ctx, req.Query, req.Filters)
        return results, nil
    },
)

// Define your template
tmpl := template.Must(template.New("results").Parse(`
<div class="search-results">
    <h2>Results for "{{.Query}}"</h2>
    <ul>
    {{range .Items}}
        <li>{{.Title}} - {{.Description}}</li>
    {{end}}
    </ul>
</div>
`))

// Wrap with JSON consumption and HTML rendering
rest.Handle(
    http.MethodPost,
    rest.BasePath("/search"),
    rest.ConsumeJson(rest.ReturnHTML(handler, tmpl)),
)
```

**What happens automatically:**
- Request body parsed as JSON (via `ConsumeJson`)
- Response rendered as HTML using the template
- Content-Type set to `text/html; charset=utf-8`
- HTML entities automatically escaped for security

### Template Data and Path Parameters

Access path and query parameters within your producer:

```go
type PageData struct {
    Title   string
    Content string
    UserID  string
}

tmpl := template.Must(template.New("page").Parse(`
<!DOCTYPE html>
<html>
<head><title>{{.Title}}</title></head>
<body>
    <h1>{{.Title}}</h1>
    <p>User ID: {{.UserID}}</p>
    <div>{{.Content}}</div>
</body>
</html>
`))

producer := rest.ProducerFunc[PageData](
    func(ctx context.Context) (*PageData, error) {
        userID := rest.PathParamValue(ctx, "id")
        format := rest.QueryParamValue(ctx, "format")

        data := &PageData{
            Title:   "User Profile",
            Content: getFormattedContent(ctx, userID, format),
            UserID:  userID,
        }
        return data, nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id").Path("/profile"),
    rest.ProduceHTML(producer, tmpl),
    rest.QueryParam("format"),
)
```

### Nested Data Structures

HTML templates support complex nested data:

```go
type Dashboard struct {
    User    User
    Stats   Statistics
    Recent  []Activity
}

tmpl := template.Must(template.New("dashboard").Parse(`
<!DOCTYPE html>
<html>
<head><title>Dashboard - {{.User.Name}}</title></head>
<body>
    <h1>Welcome, {{.User.Name}}</h1>

    <section class="stats">
        <p>Total Orders: {{.Stats.TotalOrders}}</p>
        <p>Revenue: ${{.Stats.Revenue}}</p>
    </section>

    <section class="activity">
        <h2>Recent Activity</h2>
        <ul>
        {{range .Recent}}
            <li>{{.Timestamp}} - {{.Description}}</li>
        {{end}}
        </ul>
    </section>
</body>
</html>
`))

producer := rest.ProducerFunc[Dashboard](
    func(ctx context.Context) (*Dashboard, error) {
        return getDashboardData(ctx), nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/dashboard"),
    rest.ProduceHTML(producer, tmpl),
)
```

### XSS Protection

HTML templates automatically escape HTML entities to prevent XSS attacks:

```go
type Message struct {
    Content string
}

// Template with user-generated content
tmpl := template.Must(template.New("message").Parse(`
<div class="message">
    <p>{{.Content}}</p>
</div>
`))

producer := rest.ProducerFunc[Message](
    func(ctx context.Context) (*Message, error) {
        // User input containing script tags
        return &Message{
            Content: `<script>alert('XSS')</script>`,
        }, nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/message"),
    rest.ProduceHTML(producer, tmpl),
)

// Rendered output (safe):
// <div class="message">
//     <p>&lt;script&gt;alert(&#39;XSS&#39;)&lt;/script&gt;</p>
// </div>
```

**Security Note:** The `html/template` package provides automatic context-aware escaping. Use `html/template` (not `text/template`) to ensure XSS protection.

### Template Reuse

Pre-parse and reuse templates across multiple endpoints:

```go
// Parse templates once at initialization
var (
    layoutTmpl = template.Must(template.ParseFiles(
        "templates/layout.html",
        "templates/header.html",
        "templates/footer.html",
    ))

    homeTmpl = template.Must(layoutTmpl.Clone()).Must(
        template.ParseFiles("templates/home.html"),
    )

    aboutTmpl = template.Must(layoutTmpl.Clone()).Must(
        template.ParseFiles("templates/about.html"),
    )
)

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    homeProducer := rest.ProducerFunc[HomeData](getHomeData)
    aboutProducer := rest.ProducerFunc[AboutData](getAboutData)

    api := rest.NewApi(
        "Website",
        "1.0.0",
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/"),
            rest.ProduceHTML(homeProducer, homeTmpl),
        ),
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/about"),
            rest.ProduceHTML(aboutProducer, aboutTmpl),
        ),
    )

    return api, nil
}
```

### Combining JSON and HTML Endpoints

Serve both API and web pages from the same service:

```go
type Product struct {
    ID    string  `json:"id"`
    Name  string  `json:"name"`
    Price float64 `json:"price"`
}

// JSON API endpoint
producer := rest.ProducerFunc[Product](getProduct)
rest.Handle(
    http.MethodGet,
    rest.BasePath("/api/products").Param("id"),
    rest.ProduceJSON(producer),
)

// HTML page endpoint
tmpl := template.Must(template.New("product").Parse(`
<!DOCTYPE html>
<html>
<head><title>{{.Name}}</title></head>
<body>
    <h1>{{.Name}}</h1>
    <p>Price: ${{.Price}}</p>
</body>
</html>
`))

rest.Handle(
    http.MethodGet,
    rest.BasePath("/products").Param("id"),
    rest.ProduceHTML(producer, tmpl),
)
```

### Error Handling

HTML handlers propagate errors like other handlers:

```go
producer := rest.ProducerFunc[PageData](
    func(ctx context.Context) (*PageData, error) {
        userID := rest.PathParamValue(ctx, "id")

        user, err := getUserFromDB(ctx, userID)
        if err != nil {
            // Return appropriate error for HTTP status code
            return nil, rest.NotFoundError{
                Cause: fmt.Errorf("user %s not found", userID),
            }
        }

        return &PageData{User: user}, nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rest.ProduceHTML(producer, tmpl),
)
```

See [Error Handling]({{< ref "error-handling" >}}) for complete error handling patterns.

## Form Handlers

Form handlers enable processing of HTML form submissions using `application/x-www-form-urlencoded` content type. These handlers automatically parse form data into Go structs and provide OpenAPI schema generation for form-based endpoints.

### ConsumeForm - Processing Form Submissions

Use `ConsumeForm` to wrap any handler to consume form-encoded request data:

```go
type CreateUserRequest struct {
    Name  string `form:"name"`
    Email string `form:"email"`
    Age   int    `form:"age"`
}

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
    Age   int    `json:"age"`
}

// Define handler that processes the form data
handler := rest.HandlerFunc[CreateUserRequest, User](
    func(ctx context.Context, req *CreateUserRequest) (*User, error) {
        user := &User{
            ID:    generateID(),
            Name:  req.Name,
            Email: req.Email,
            Age:   req.Age,
        }
        return user, nil
    },
)

// Wrap with form consumption
rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.ConsumeForm(handler),
)
```

**What happens automatically:**
- Form data parsed from request body
- Fields mapped to struct using `form` tags
- Type conversion for basic types (string, int, uint, bool, float)
- OpenAPI request schema generated with `application/x-www-form-urlencoded` content type

### Form Struct Tags

The `form` struct tag specifies the form field name to bind to each struct field:

```go
type ContactForm struct {
    Name    string `form:"name"`      // Binds to form field "name"
    Email   string `form:"email"`     // Binds to form field "email"
    Message string `form:"message"`   // Binds to form field "message"
    NoTag   string                    // Uses lowercase field name "notag"
}
```

**Tag Rules:**
- If `form` tag is present, use the tag value as the form field name
- If `form` tag is absent, use the lowercase field name
- Unexported fields are always skipped

**Supported Field Types:**
- `string` - Direct string value
- `int`, `int8`, `int16`, `int32`, `int64` - Signed integers
- `uint`, `uint8`, `uint16`, `uint32`, `uint64` - Unsigned integers
- `bool` - Boolean values (`true`, `false`, `1`, `0`)
- `float32`, `float64` - Floating-point numbers

### ConsumeOnlyForm - Form Webhooks

Use `ConsumeOnlyForm` for endpoints that process form submissions without returning a response body:

```go
type WebhookForm struct {
    Event    string `form:"event"`
    UserID   string `form:"user_id"`
    Action   string `form:"action"`
}

// Define consumer
consumer := rest.ConsumerFunc[WebhookForm](
    func(ctx context.Context, form *WebhookForm) error {
        processWebhookEvent(ctx, form.Event, form.UserID, form.Action)
        return nil
    },
)

// Wrap with form consumption
rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks/form"),
    rest.ConsumeOnlyForm(consumer),
)
```

**Response behavior:**
```http
POST /webhooks/form HTTP/1.1
Content-Type: application/x-www-form-urlencoded

event=user.created&user_id=123&action=signup

HTTP/1.1 200 OK
Content-Length: 0
```

### HandleForm - Form with JSON Response

Use `HandleForm` for endpoints that consume form data but return JSON responses:

```go
type SearchForm struct {
    Query   string `form:"q"`
    Filter  string `form:"filter"`
    Page    int    `form:"page"`
}

type SearchResults struct {
    Results []Result `json:"results"`
    Total   int      `json:"total"`
    Page    int      `json:"page"`
}

// Define handler
handler := rest.HandlerFunc[SearchForm, SearchResults](
    func(ctx context.Context, form *SearchForm) (*SearchResults, error) {
        results := performSearch(ctx, form.Query, form.Filter, form.Page)
        return results, nil
    },
)

// Wrap with form consumption and JSON response
rest.Handle(
    http.MethodPost,
    rest.BasePath("/search"),
    rest.HandleForm(handler),
)
```

**What happens automatically:**
- Request parsed as `application/x-www-form-urlencoded`
- Response serialized as JSON with `Content-Type: application/json`
- OpenAPI spec includes both form request and JSON response schemas

### Combining Forms with HTML Templates

Form handlers work seamlessly with HTML template responses for HTMX-style applications:

```go
type TodoForm struct {
    Text      string `form:"text"`
    Completed bool   `form:"completed"`
}

type TodoItem struct {
    ID        string
    Text      string
    Completed bool
}

// Template for rendering a single todo item
tmpl := template.Must(template.New("todo-item").Parse(`
<li id="todo-{{.ID}}" class="{{if .Completed}}completed{{end}}">
    <span>{{.Text}}</span>
</li>
`))

// Handler that processes form and returns HTML fragment
handler := rest.HandlerFunc[TodoForm, TodoItem](
    func(ctx context.Context, form *TodoForm) (*TodoItem, error) {
        item := &TodoItem{
            ID:        generateID(),
            Text:      form.Text,
            Completed: form.Completed,
        }
        saveTodo(ctx, item)
        return item, nil
    },
)

// Combine form consumption with HTML template response
rest.Handle(
    http.MethodPost,
    rest.BasePath("/todos"),
    rest.ConsumeForm(rest.ReturnHTML(handler, tmpl)),
)
```

**HTML Form Example:**
```html
<form hx-post="/todos" hx-target="#todo-list" hx-swap="beforeend">
    <input type="text" name="text" placeholder="New todo" required>
    <input type="checkbox" name="completed" value="true">
    <button type="submit">Add</button>
</form>

<ul id="todo-list">
    <!-- New todo items appear here -->
</ul>
```

### Error Handling

Form parsing errors are automatically wrapped as `BadRequestError`:

```go
handler := rest.HandlerFunc[TodoForm, TodoItem](
    func(ctx context.Context, form *TodoForm) (*TodoItem, error) {
        // Validate form data
        if form.Text == "" {
            return nil, rest.BadRequestError{
                Cause: errors.New("text field is required"),
            }
        }

        // Business logic
        return createTodo(ctx, form)
    },
)
```

**Error scenarios:**
- **Invalid percent encoding** - Returns `400 Bad Request` if form data is malformed
- **Type conversion failure** - Returns `400 Bad Request` if a field can't be parsed (e.g., "abc" for an `int` field)
- **Unsupported field types** - Returns `400 Bad Request` if a struct field has an unsupported type

### Form Validation Patterns

Validate form data in your handler before processing:

```go
type RegistrationForm struct {
    Username string `form:"username"`
    Email    string `form:"email"`
    Age      int    `form:"age"`
}

handler := rest.HandlerFunc[RegistrationForm, User](
    func(ctx context.Context, form *RegistrationForm) (*User, error) {
        // Validation logic
        if len(form.Username) < 3 {
            return nil, rest.BadRequestError{
                Cause: errors.New("username must be at least 3 characters"),
            }
        }

        if !strings.Contains(form.Email, "@") {
            return nil, rest.BadRequestError{
                Cause: errors.New("invalid email format"),
            }
        }

        if form.Age < 18 {
            return nil, rest.BadRequestError{
                Cause: errors.New("must be 18 or older"),
            }
        }

        // Process valid form
        return createUser(ctx, form)
    },
)
```

### OpenAPI Schema Generation

Form handlers automatically generate OpenAPI schemas:

```go
type ProductForm struct {
    Name     string  `form:"name"`
    Price    float64 `form:"price"`
    Quantity int     `form:"quantity"`
}
```

**Generated OpenAPI:**
```json
{
  "requestBody": {
    "required": true,
    "content": {
      "application/x-www-form-urlencoded": {
        "schema": {
          "type": "object",
          "properties": {
            "name": {
              "type": "string"
            },
            "price": {
              "type": "number"
            },
            "quantity": {
              "type": "integer"
            }
          }
        }
      }
    }
  }
}
```

### Complete HTMX Example

Building a complete HTMX todo list application:

```go
type TodoForm struct {
    Text string `form:"text"`
}

type TodoList struct {
    Items []TodoItem
}

type TodoItem struct {
    ID   string
    Text string
}

// Template for the full todo list page
pageTmpl := template.Must(template.New("page").Parse(`
<!DOCTYPE html>
<html>
<head>
    <title>Todo List</title>
    <script src="https://unpkg.com/htmx.org@1.9.10"></script>
</head>
<body>
    <h1>My Todos</h1>

    <form hx-post="/todos" hx-target="#todo-list" hx-swap="beforeend">
        <input type="text" name="text" placeholder="New todo" required>
        <button type="submit">Add</button>
    </form>

    <ul id="todo-list">
        {{range .Items}}
        <li id="todo-{{.ID}}">
            {{.Text}}
            <button hx-delete="/todos/{{.ID}}" hx-target="#todo-{{.ID}}" hx-swap="outerHTML">
                Delete
            </button>
        </li>
        {{end}}
    </ul>
</body>
</html>
`))

// Template for a single todo item
itemTmpl := template.Must(template.New("item").Parse(`
<li id="todo-{{.ID}}">
    {{.Text}}
    <button hx-delete="/todos/{{.ID}}" hx-target="#todo-{{.ID}}" hx-swap="outerHTML">
        Delete
    </button>
</li>
`))

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    store := NewTodoStore()

    // GET / - Show full page
    pageProducer := rest.ProducerFunc[TodoList](
        func(ctx context.Context) (*TodoList, error) {
            items := store.List(ctx)
            return &TodoList{Items: items}, nil
        },
    )

    // POST /todos - Add new todo (returns HTML fragment)
    addHandler := rest.HandlerFunc[TodoForm, TodoItem](
        func(ctx context.Context, form *TodoForm) (*TodoItem, error) {
            item := &TodoItem{
                ID:   generateID(),
                Text: form.Text,
            }
            store.Add(ctx, item)
            return item, nil
        },
    )

    // DELETE /todos/{id} - Delete todo (returns empty response)
    deleteConsumer := rest.ConsumerFunc[struct{}](
        func(ctx context.Context, _ *struct{}) error {
            id := rest.PathParamValue(ctx, "id")
            return store.Delete(ctx, id)
        },
    )

    api := rest.NewApi(
        "Todo List",
        "1.0.0",
        rest.Handle(
            http.MethodGet,
            rest.BasePath("/"),
            rest.ProduceHTML(pageProducer, pageTmpl),
        ),
        rest.Handle(
            http.MethodPost,
            rest.BasePath("/todos"),
            rest.ConsumeForm(rest.ReturnHTML(addHandler, itemTmpl)),
        ),
        rest.Handle(
            http.MethodDelete,
            rest.BasePath("/todos").Param("id"),
            rest.ConsumeOnlyJson(deleteConsumer),
        ),
    )

    return api, nil
}
```

### Best Practices

**1. Use appropriate struct tags:**
```go
// Good - explicit form field names
type Form struct {
    UserName string `form:"username"`
    Email    string `form:"email"`
}

// Avoid - relying on lowercase field names
type Form struct {
    UserName string  // Will bind to "username" (lowercase)
    Email    string  // Will bind to "email"
}
```

**2. Validate early:**
```go
// Good - validate immediately in handler
func(ctx context.Context, form *Form) (*Response, error) {
    if form.Email == "" {
        return nil, rest.BadRequestError{...}
    }
    // Process valid form
}

// Avoid - processing invalid data
func(ctx context.Context, form *Form) (*Response, error) {
    result := process(form)  // May fail later
    validate(result)         // Too late
}
```

**3. Use the right helper for your use case:**
```go
// Form-only endpoint (no response body)
rest.ConsumeOnlyForm(consumer)

// Form with JSON response (API endpoint)
rest.HandleForm(handler)

// Form with HTML response (HTMX fragment)
rest.ConsumeForm(rest.ReturnHTML(handler, tmpl))
```

## Function Adapters

Handler helpers provide function adapters for inline handler definitions:

### HandlerFunc

Convert a function to a `Handler`:

```go
handler := rest.HandlerFunc[Request, Response](
    func(ctx context.Context, req *Request) (*Response, error) {
        // Process request and return response
        return &Response{}, nil
    },
)
```

### ProducerFunc

Convert a function to a `Producer`:

```go
producer := rest.ProducerFunc[Response](
    func(ctx context.Context) (*Response, error) {
        // Generate and return response
        return &Response{}, nil
    },
)
```

### ConsumerFunc

Convert a function to a `Consumer`:

```go
consumer := rest.ConsumerFunc[Request](
    func(ctx context.Context, req *Request) error {
        // Process request
        return nil
    },
)
```

## Composition Patterns

Handler helpers are designed to compose together, allowing you to build complex handlers from simple pieces.

### Adding Custom Middleware

Wrap handlers with additional behavior:

```go
// Base handler
baseHandler := rest.HandlerFunc[Request, Response](businessLogic)

// Add validation layer
validatingHandler := rest.HandlerFunc[Request, Response](
    func(ctx context.Context, req *Request) (*Response, error) {
        if err := validateRequest(req); err != nil {
            return nil, err
        }
        return baseHandler.Handle(ctx, req)
    },
)

// Wrap with JSON serialization
rest.Handle(
    http.MethodPost,
    rest.BasePath("/api"),
    rest.HandleJson(validatingHandler),
)
```

### Transforming Responses

Chain transformations before serialization:

```go
// Handler returns internal type
handler := rest.HandlerFunc[Request, InternalResponse](getInternalData)

// Transform to API response
transformer := rest.HandlerFunc[Request, ApiResponse](
    func(ctx context.Context, req *Request) (*ApiResponse, error) {
        internal, err := handler.Handle(ctx, req)
        if err != nil {
            return nil, err
        }
        return toApiResponse(internal), nil
    },
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api"),
    rest.HandleJson(transformer),
)
```

## Low-Level Building Blocks

For custom serialization formats, use the underlying wrappers directly.

### ConsumeJson - Custom Request Deserialization

Wrap any handler to consume JSON requests:

```go
handler := rest.HandlerFunc[MyRequest, MyResponse](businessLogic)

// Add JSON request deserialization
jsonHandler := rest.ConsumeJson(handler)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api"),
    jsonHandler,
)
```

### ReturnJson - Custom Response Serialization

Wrap any handler to return JSON responses:

```go
handler := rest.HandlerFunc[MyRequest, MyResponse](businessLogic)

// Add JSON response serialization
jsonHandler := rest.ReturnJson(handler)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/api"),
    jsonHandler,
)
```

### ConsumeNothing and ProduceNothing

Build handlers without request or response bodies:

```go
// Producer - generates response without request body
producer := rest.ProducerFunc[Response](generateData)
handler := rest.ConsumeNothing(producer)

// Consumer - processes request without response body
consumer := rest.ConsumerFunc[Request](processData)
handler := rest.ProduceNothing(consumer)
```

## Advanced Patterns

### Conditional Response Types

Return different response types based on business logic:

```go
handler := rest.HandlerFunc[Request, Response](
    func(ctx context.Context, req *Request) (*Response, error) {
        // Return different status codes via custom error types
        if !isAuthorized(ctx) {
            return nil, rest.UnauthorizedError{
                Cause: errors.New("invalid credentials"),
            }
        }

        if !exists(req.ID) {
            return nil, rest.NotFoundError{
                Cause: errors.New("resource not found"),
            }
        }

        return &Response{Data: getData(req.ID)}, nil
    },
)
```

See [Error Handling]({{< ref "error-handling" >}}) for complete error handling patterns.

### Streaming Responses

For streaming responses, implement custom `TypedResponse`:

```go
type StreamingResponse struct {
    data chan []byte
}

func (sr *StreamingResponse) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
    w.Header().Set("Content-Type", "application/x-ndjson")
    w.WriteHeader(http.StatusOK)

    for data := range sr.data {
        if _, err := w.Write(data); err != nil {
            return err
        }
        if f, ok := w.(http.Flusher); ok {
            f.Flush()
        }
    }
    return nil
}

func (sr *StreamingResponse) Spec() (int, openapi3.ResponseOrRef, error) {
    // Define OpenAPI spec for streaming response
    return http.StatusOK, openapi3.ResponseOrRef{}, nil
}
```

### Custom Content Types

Implement handlers for other content types:

```go
// XML request type
type XMLRequest[T any] struct {
    inner T
}

func (xr *XMLRequest[T]) ReadRequest(ctx context.Context, r *http.Request) error {
    contentType := r.Header.Get("Content-Type")
    if contentType != "application/xml" {
        return rest.BadRequestError{
            Cause: rest.InvalidContentTypeError{
                ContentType: contentType,
            },
        }
    }

    dec := xml.NewDecoder(r.Body)
    return dec.Decode(&xr.inner)
}

func (xr *XMLRequest[T]) Spec() (openapi3.RequestBodyOrRef, error) {
    // Define OpenAPI spec for XML request
    return openapi3.RequestBodyOrRef{}, nil
}
```

## Complete Example

Putting it all together in a CRUD API:

```go
type UserStore interface {
    Create(ctx context.Context, user User) error
    Get(ctx context.Context, id string) (*User, error)
    List(ctx context.Context) ([]User, error)
    Update(ctx context.Context, user User) error
    Delete(ctx context.Context, id string) error
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    store := NewUserStore()

    // POST /users - Create user
    createHandler := rest.HandlerFunc[CreateUserRequest, User](
        func(ctx context.Context, req *CreateUserRequest) (*User, error) {
            user := User{
                ID:    generateID(),
                Name:  req.Name,
                Email: req.Email,
            }
            if err := store.Create(ctx, user); err != nil {
                return nil, err
            }
            return &user, nil
        },
    )

    // GET /users - List all users
    listProducer := rest.ProducerFunc[[]User](
        func(ctx context.Context) (*[]User, error) {
            users, err := store.List(ctx)
            return &users, err
        },
    )

    // GET /users/{id} - Get single user
    getProducer := rest.ProducerFunc[User](
        func(ctx context.Context) (*User, error) {
            id := rest.PathParamValue(ctx, "id")
            return store.Get(ctx, id)
        },
    )

    // PUT /users/{id} - Update user
    updateHandler := rest.HandlerFunc[UpdateUserRequest, User](
        func(ctx context.Context, req *UpdateUserRequest) (*User, error) {
            id := rest.PathParamValue(ctx, "id")
            user := User{
                ID:    id,
                Name:  req.Name,
                Email: req.Email,
            }
            if err := store.Update(ctx, user); err != nil {
                return nil, err
            }
            return &user, nil
        },
    )

    // DELETE /users/{id} - Delete user
    deleteConsumer := rest.ConsumerFunc[struct{}](
        func(ctx context.Context, _ *struct{}) error {
            id := rest.PathParamValue(ctx, "id")
            return store.Delete(ctx, id)
        },
    )

    api := rest.NewApi(
        "User API",
        "1.0.0",
        rest.Handle(http.MethodPost, rest.BasePath("/users"), rest.HandleJson(createHandler)),
        rest.Handle(http.MethodGet, rest.BasePath("/users"), rest.ProduceJson(listProducer)),
        rest.Handle(http.MethodGet, rest.BasePath("/users").Param("id"), rest.ProduceJson(getProducer)),
        rest.Handle(http.MethodPut, rest.BasePath("/users").Param("id"), rest.HandleJson(updateHandler)),
        rest.Handle(http.MethodDelete, rest.BasePath("/users").Param("id"), rest.ConsumeOnlyJson(deleteConsumer)),
    )

    return api, nil
}
```

## Best Practices

### 1. Choose the Right Helper

Match the helper to your endpoint's behavior:

```go
// GET endpoints returning JSON - ProduceJson
rest.ProduceJson(producer)

// GET endpoints returning HTML pages - ProduceHTML
rest.ProduceHTML(producer, tmpl)

// Webhooks - ConsumeOnlyJson
rest.ConsumeOnlyJson(consumer)

// Full CRUD operations - HandleJson
rest.HandleJson(handler)

// Mixed JSON request / HTML response - ReturnHTML
rest.ConsumeJson(rest.ReturnHTML(handler, tmpl))
```

### 2. Keep Handlers Focused

Each handler should have a single responsibility:

```go
// Good - focused handler
handler := rest.HandlerFunc[CreateUserRequest, User](createUser)

// Avoid - handler doing too much
handler := rest.HandlerFunc[Request, Response](
    func(ctx context.Context, req *Request) (*Response, error) {
        validate(req)      // Should be middleware
        log(req)           // Should be middleware
        transform(req)     // Should be separate transformer
        return process(req), nil
    },
)
```

### 3. Use Type Parameters Effectively

Define clear request/response types:

```go
// Good - explicit types
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type UserResponse struct {
    ID        string    `json:"id"`
    Name      string    `json:"name"`
    Email     string    `json:"email"`
    CreatedAt time.Time `json:"created_at"`
}

// Avoid - using map[string]interface{}
handler := rest.HandlerFunc[map[string]interface{}, map[string]interface{}](...)
```

### 4. Leverage Function Adapters

Use function adapters for inline handlers:

```go
// Good - inline with adapter
rest.ProduceJson(rest.ProducerFunc[Response](
    func(ctx context.Context) (*Response, error) {
        return &Response{}, nil
    },
))

// Verbose - defining separate type
type MyProducer struct{}
func (p *MyProducer) Produce(ctx context.Context) (*Response, error) {
    return &Response{}, nil
}
rest.ProduceJson(&MyProducer{})
```

### 5. Document with JSON Tags

Use JSON tags to control serialization and OpenAPI schema generation:

```go
type User struct {
    ID        string    `json:"id"`                    // Required field
    Name      string    `json:"name"`                  // Required field
    Email     string    `json:"email,omitempty"`       // Optional field
    Internal  string    `json:"-"`                     // Excluded from JSON
    CreatedAt time.Time `json:"created_at"`            // Snake case in JSON
}
```

## Performance Considerations

### Request Body Size

JSON handlers buffer the entire request body in memory. For large uploads, consider:

```go
// Set max request size at the HTTP server level
config := rest.Config{
    HTTP: rest.HTTPConfig{
        MaxRequestBodySize: 10 * 1024 * 1024, // 10 MB
    },
}
```

### Response Streaming

For large responses, use custom streaming responses instead of buffering:

```go
// Avoid - buffers entire response
type LargeResponse struct {
    Data []LargeItem `json:"data"` // Could be GBs
}

// Prefer - streams data
type StreamingHandler struct{}

func (h *StreamingHandler) Handle(ctx context.Context, req *Request) (*StreamingResponse, error) {
    return &StreamingResponse{
        data: generateDataStream(ctx),
    }, nil
}
```

## Troubleshooting

### Content-Type Errors

If you see `400 Bad Request` with "invalid content type":

```json
{
  "error": "invalid content type: text/plain"
}
```

Ensure your client sends `Content-Type: application/json`:

```bash
curl -X POST http://localhost:8080/api \
  -H "Content-Type: application/json" \
  -d '{"key": "value"}'
```

### JSON Parsing Errors

If you see JSON unmarshal errors, verify your request structure matches the type:

```go
// Handler expects
type Request struct {
    Name string `json:"name"`
}

// Client must send
{"name": "value"}

// Not
{"Name": "value"}  // Wrong - Go field name instead of JSON tag
```

### Empty Response Body

`ConsumeOnlyJson` returns an empty response body by design:

```go
// This is correct behavior
rest.ConsumeOnlyJson(consumer)
// Returns: 200 OK with empty body

// To return data, use HandleJson instead
rest.HandleJson(handler)
// Returns: 200 OK with JSON response
```

## Next Steps

- Learn about [Error Handling]({{< ref "error-handling" >}}) for custom error responses
- Explore [Routing]({{< ref "routing" >}}) for path parameters and query strings
- See [OpenAPI]({{< ref "openapi" >}}) for customizing generated schemas
- Read [Authentication]({{< ref "authentication" >}}) for securing endpoints
