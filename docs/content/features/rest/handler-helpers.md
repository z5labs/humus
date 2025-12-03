---
title: Handler Helpers
description: Quick reference for REST handler helpers
weight: 30
type: docs
---

Humus provides handler helpers that simplify REST API development through type-safe request/response handling, automatic serialization, and OpenAPI schema generation. This quick reference shows how to use each helper function with complete, copy-paste-ready code examples.

## JSON Handlers

### HandleJson

Consume JSON request and produce JSON response - ideal for POST/PUT endpoints with request and response bodies.

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

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

rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(handler),
)
```

### ProduceJson

Produce JSON response without consuming a request body - ideal for GET endpoints.

```go
type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

producer := rest.ProducerFunc[User](
    func(ctx context.Context) (*User, error) {
        // Extract path parameter
        userID := rest.PathParamValue(ctx, "id")

        user := getUserByID(ctx, userID)
        return user, nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rest.ProduceJson(producer),
)
```

### ConsumeOnlyJson

Consume JSON request without producing a response body - ideal for webhook endpoints.

```go
type WebhookPayload struct {
    Event      string `json:"event"`
    Repository string `json:"repository"`
}

consumer := rest.ConsumerFunc[WebhookPayload](
    func(ctx context.Context, payload *WebhookPayload) error {
        processWebhook(ctx, payload)
        return nil
    },
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks/github"),
    rest.ConsumeOnlyJson(consumer),
)
```

### ConsumeJson

Low-level helper that wraps a handler to add JSON request parsing.

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

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

// Wrap handler to add JSON request parsing
jsonHandler := rest.ConsumeJson(handler)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    jsonHandler,
)
```

### ReturnJson

Low-level helper that wraps a handler to add JSON response serialization.

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

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

// Wrap handler to add JSON response serialization
jsonHandler := rest.ReturnJson(handler)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    jsonHandler,
)
```

## Form Handlers

### ConsumeForm

Wrap a handler to consume form-encoded request data.

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

rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.ConsumeForm(handler),
)
```

### ConsumeOnlyForm

Consume form-encoded data without producing a response body - ideal for form webhooks.

```go
type WebhookForm struct {
    Event  string `form:"event"`
    UserID string `form:"user_id"`
    Action string `form:"action"`
}

consumer := rest.ConsumerFunc[WebhookForm](
    func(ctx context.Context, form *WebhookForm) error {
        processWebhookEvent(ctx, form.Event, form.UserID, form.Action)
        return nil
    },
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks/form"),
    rest.ConsumeOnlyForm(consumer),
)
```

### HandleForm

Consume form-encoded request and produce JSON response.

```go
type SearchForm struct {
    Query  string `form:"q"`
    Filter string `form:"filter"`
    Page   int    `form:"page"`
}

type SearchResults struct {
    Results []Result `json:"results"`
    Total   int      `json:"total"`
    Page    int      `json:"page"`
}

handler := rest.HandlerFunc[SearchForm, SearchResults](
    func(ctx context.Context, form *SearchForm) (*SearchResults, error) {
        results := performSearch(ctx, form.Query, form.Filter, form.Page)
        return results, nil
    },
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/search"),
    rest.HandleForm(handler),
)
```

## HTML Template Handlers

### ProduceHTML

Produce HTML response using a template - ideal for GET endpoints rendering pages. Use this when your template for each request is independent of the request data. If you need to specify a response template dynamically, implement your own Handler which returns a `rest.HTMLTemplateResponse` with the `Template` field set based on the request.

```go
import "html/template"

type User struct {
    ID    string
    Name  string
    Email string
}

// Define template
tmpl := template.Must(template.New("user").Parse(`
<!DOCTYPE html>
<html>
<head><title>{{.Name}}'s Profile</title></head>
<body>
    <h1>{{.Name}}</h1>
    <p>Email: {{.Email}}</p>
</body>
</html>
`))

// Create producer
producer := rest.ProducerFunc[User](
    func(ctx context.Context) (*User, error) {
        userID := rest.PathParamValue(ctx, "id")
        user := getUserFromDB(ctx, userID)
        return user, nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rest.ProduceHTML(producer, tmpl),
)
```

### ReturnHTML

Wrap a handler to return HTML responses using a template. Use this when your template for each request is independent of the request data. If you need to specify a response template dynamically, implement your own Handler which returns a `rest.HTMLTemplateResponse` with the `Template` field set based on the request.

```go
import "html/template"

type SearchRequest struct {
    Query   string `json:"query"`
    Filters string `json:"filters"`
}

type SearchResults struct {
    Query string
    Items []Item
}

// Define template
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

// Create handler
handler := rest.HandlerFunc[SearchRequest, SearchResults](
    func(ctx context.Context, req *SearchRequest) (*SearchResults, error) {
        results := performSearch(ctx, req.Query, req.Filters)
        return results, nil
    },
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/search"),
    rest.ConsumeJson(rest.ReturnHTML(handler, tmpl)),
)
```

## Core Interfaces

### Handler

Core interface for handlers that consume a request and produce a response.

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Implement Handler interface with a custom struct
type UserHandler struct {
    store UserStore
}

func (h *UserHandler) Handle(ctx context.Context, req *CreateUserRequest) (*User, error) {
    user := &User{
        ID:    generateID(),
        Name:  req.Name,
        Email: req.Email,
    }

    if err := h.store.Create(ctx, user); err != nil {
        return nil, err
    }

    return user, nil
}

// Use with HandleJson
handler := &UserHandler{store: myStore}

rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(handler),
)
```

### Producer

Interface for producers that generate a response without consuming a request.

```go
type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Implement Producer interface with a custom struct
type UserProducer struct {
    store UserStore
}

func (p *UserProducer) Produce(ctx context.Context) (*User, error) {
    userID := rest.PathParamValue(ctx, "id")
    user, err := p.store.Get(ctx, userID)
    if err != nil {
        return nil, err
    }
    return user, nil
}

// Use with ProduceJson
producer := &UserProducer{store: myStore}

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rest.ProduceJson(producer),
)
```

### Consumer

Interface for consumers that consume a request without producing a response.

```go
type WebhookPayload struct {
    Event      string `json:"event"`
    Repository string `json:"repository"`
}

// Implement Consumer interface with a custom struct
type WebhookConsumer struct {
    processor WebhookProcessor
}

func (c *WebhookConsumer) Consume(ctx context.Context, payload *WebhookPayload) error {
    return c.processor.Process(ctx, payload)
}

// Use with ConsumeOnlyJson
consumer := &WebhookConsumer{processor: myProcessor}

rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks/github"),
    rest.ConsumeOnlyJson(consumer),
)
```

## Function Adapters

### HandlerFunc

Function adapter that implements the Handler interface - use for inline handler definitions.

```go
type CreateUserRequest struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Define handler inline using HandlerFunc
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

rest.Handle(
    http.MethodPost,
    rest.BasePath("/users"),
    rest.HandleJson(handler),
)
```

### ProducerFunc

Function adapter that implements the Producer interface - use for inline producer definitions.

```go
type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Define producer inline using ProducerFunc
producer := rest.ProducerFunc[User](
    func(ctx context.Context) (*User, error) {
        userID := rest.PathParamValue(ctx, "id")
        user := getUserByID(ctx, userID)
        return user, nil
    },
)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    rest.ProduceJson(producer),
)
```

### ConsumerFunc

Function adapter that implements the Consumer interface - use for inline consumer definitions.

```go
type WebhookPayload struct {
    Event      string `json:"event"`
    Repository string `json:"repository"`
}

// Define consumer inline using ConsumerFunc
consumer := rest.ConsumerFunc[WebhookPayload](
    func(ctx context.Context, payload *WebhookPayload) error {
        processWebhook(ctx, payload)
        return nil
    },
)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks/github"),
    rest.ConsumeOnlyJson(consumer),
)
```

## Low-Level Building Blocks

### ConsumeNothing

Low-level helper that wraps a Producer to create a handler that ignores the request body.

```go
type User struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Create a producer
producer := rest.ProducerFunc[User](
    func(ctx context.Context) (*User, error) {
        userID := rest.PathParamValue(ctx, "id")
        user := getUserByID(ctx, userID)
        return user, nil
    },
)

// Wrap producer to create a handler that consumes nothing
handler := rest.ConsumeNothing(producer)

rest.Handle(
    http.MethodGet,
    rest.BasePath("/users").Param("id"),
    handler,
)
```

### ProduceNothing

Low-level helper that wraps a Consumer to create a handler that produces no response body.

```go
type WebhookPayload struct {
    Event      string `json:"event"`
    Repository string `json:"repository"`
}

// Create a consumer
consumer := rest.ConsumerFunc[WebhookPayload](
    func(ctx context.Context, payload *WebhookPayload) error {
        processWebhook(ctx, payload)
        return nil
    },
)

// Wrap consumer to create a handler that produces nothing
handler := rest.ProduceNothing(consumer)

rest.Handle(
    http.MethodPost,
    rest.BasePath("/webhooks/github"),
    handler,
)
```
