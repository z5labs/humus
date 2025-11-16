---
title: Quick Start
description: Build your first REST API
weight: 10
type: docs
---


This guide walks you through building a complete REST API with CRUD operations.

## Prerequisites

- Go 1.21 or later
- Humus installed (`go get github.com/z5labs/humus`)

## Project Setup

```bash
mkdir todo-api
cd todo-api
go mod init todo-api
go get github.com/z5labs/humus
```

## Configuration

Create `config.yaml`:

```yaml
rest:
  port: 8080

otel:
  service:
    name: todo-api
  sdk:
    disabled: true  # Disable for this example
```

## Define Your Model

Create `main.go`:

```go
package main

import (
    "context"
    "fmt"
    "net/http"
    "sync"

    "github.com/z5labs/humus/rest"
    "github.com/z5labs/humus/rest/rpc"
)

type Todo struct {
    ID        string `json:"id"`
    Title     string `json:"title"`
    Completed bool   `json:"completed"`
}

// In-memory store
type TodoStore struct {
    mu    sync.RWMutex
    todos map[string]Todo
}

func NewTodoStore() *TodoStore {
    return &TodoStore{
        todos: make(map[string]Todo),
    }
}

func (s *TodoStore) Create(todo Todo) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.todos[todo.ID] = todo
}

func (s *TodoStore) Get(id string) (Todo, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    todo, ok := s.todos[id]
    return todo, ok
}

func (s *TodoStore) List() []Todo {
    s.mu.RLock()
    defer s.mu.RUnlock()

    todos := make([]Todo, 0, len(s.todos))
    for _, todo := range s.todos {
        todos = append(todos, todo)
    }
    return todos
}

func (s *TodoStore) Update(todo Todo) bool {
    s.mu.Lock()
    defer s.mu.Unlock()

    if _, exists := s.todos[todo.ID]; !exists {
        return false
    }

    s.todos[todo.ID] = todo
    return true
}

func (s *TodoStore) Delete(id string) bool {
    s.mu.Lock()
    defer s.mu.Unlock()

    if _, exists := s.todos[id]; !exists {
        return false
    }

    delete(s.todos, id)
    return true
}
```

## Configuration Struct

```go
type Config struct {
    rest.Config `config:",squash"`
}
```

## Main Function

```go
func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}
```

## Initialize API

```go
func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    api := rest.NewApi("Todo API", "1.0.0")

    store := NewTodoStore()

    // Register handlers
    registerHandlers(api, store)

    return api, nil
}

func registerHandlers(api *rest.Api, store *TodoStore) {
    // Create todo
    createHandler := rpc.NewOperation(
        rpc.ConsumeJson(
            rpc.ReturnJson(
                rpc.Handle(func(ctx context.Context, req Todo) (Todo, error) {
                    if req.ID == "" {
                        req.ID = fmt.Sprintf("todo-%d", len(store.todos)+1)
                    }
                    store.Create(req)
                    return req, nil
                }),
            ),
        ),
    )
    rest.Handle(http.MethodPost, rest.BasePath("/todos"), createHandler)

    // List todos
    listHandler := rpc.NewOperation(
        rpc.ReturnJson(
            rpc.Handle(func(ctx context.Context, _ any) ([]Todo, error) {
                return store.List(), nil
            }),
        ),
    )
    rest.Handle(http.MethodGet, rest.BasePath("/todos"), listHandler)

    // Get todo
    getHandler := rpc.NewOperation(
        rpc.ReturnJson(
            rpc.Handle(func(ctx context.Context, req PathParams) (Todo, error) {
                todo, ok := store.Get(req.ID)
                if !ok {
                    return Todo{}, fmt.Errorf("todo not found")
                }
                return todo, nil
            }),
        ),
    )
    rest.Handle(http.MethodGet, rest.BasePath("/todos").Param("id"), getHandler)

    // Update todo
    updateHandler := rpc.NewOperation(
        rpc.ConsumeJson(
            rpc.ReturnJson(
                rpc.Handle(func(ctx context.Context, req UpdateRequest) (Todo, error) {
                    req.Todo.ID = req.ID
                    if !store.Update(req.Todo) {
                        return Todo{}, fmt.Errorf("todo not found")
                    }
                    return req.Todo, nil
                }),
            ),
        ),
    )
    rest.Handle(http.MethodPut, rest.BasePath("/todos").Param("id"), updateHandler)

    // Delete todo
    deleteHandler := rpc.NewOperation(
        rpc.Handle(func(ctx context.Context, req PathParams) (string, error) {
            if !store.Delete(req.ID) {
                return "", fmt.Errorf("todo not found")
            }
            return "deleted", nil
        }),
    )
    rest.Handle(http.MethodDelete, rest.BasePath("/todos").Param("id"), deleteHandler)
}

type PathParams struct {
    ID string `path:"id"`
}

type UpdateRequest struct {
    ID   string `path:"id"`
    Todo Todo   `json:",inline"`
}
```

## Complete Code

Put it all together in `main.go`:

```go
package main

import (
    "context"
    "fmt"
    "net/http"
    "sync"

    "github.com/z5labs/humus/rest"
    "github.com/z5labs/humus/rest/rpc"
)

type Todo struct {
    ID        string `json:"id"`
    Title     string `json:"title"`
    Completed bool   `json:"completed"`
}

type TodoStore struct {
    mu    sync.RWMutex
    todos map[string]Todo
}

func NewTodoStore() *TodoStore {
    return &TodoStore{
        todos: make(map[string]Todo),
    }
}

func (s *TodoStore) Create(todo Todo) {
    s.mu.Lock()
    defer s.mu.Unlock()
    s.todos[todo.ID] = todo
}

func (s *TodoStore) Get(id string) (Todo, bool) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    todo, ok := s.todos[id]
    return todo, ok
}

func (s *TodoStore) List() []Todo {
    s.mu.RLock()
    defer s.mu.RUnlock()

    todos := make([]Todo, 0, len(s.todos))
    for _, todo := range s.todos {
        todos = append(todos, todo)
    }
    return todos
}

func (s *TodoStore) Update(todo Todo) bool {
    s.mu.Lock()
    defer s.mu.Unlock()

    if _, exists := s.todos[todo.ID]; !exists {
        return false
    }

    s.todos[todo.ID] = todo
    return true
}

func (s *TodoStore) Delete(id string) bool {
    s.mu.Lock()
    defer s.mu.Unlock()

    if _, exists := s.todos[id]; !exists {
        return false
    }

    delete(s.todos, id)
    return true
}

type Config struct {
    rest.Config `config:",squash"`
}

func main() {
    rest.Run(rest.YamlSource("config.yaml"), Init)
}

func Init(ctx context.Context, cfg Config) (*rest.Api, error) {
    api := rest.NewApi("Todo API", "1.0.0")

    store := NewTodoStore()
    registerHandlers(api, store)

    return api, nil
}

func registerHandlers(api *rest.Api, store *TodoStore) {
    createHandler := rpc.NewOperation(
        rpc.ConsumeJson(
            rpc.ReturnJson(
                rpc.Handle(func(ctx context.Context, req Todo) (Todo, error) {
                    if req.ID == "" {
                        req.ID = fmt.Sprintf("todo-%d", len(store.todos)+1)
                    }
                    store.Create(req)
                    return req, nil
                }),
            ),
        ),
    )
    rest.Handle(http.MethodPost, rest.BasePath("/todos"), createHandler)

    listHandler := rpc.NewOperation(
        rpc.ReturnJson(
            rpc.Handle(func(ctx context.Context, _ any) ([]Todo, error) {
                return store.List(), nil
            }),
        ),
    )
    rest.Handle(http.MethodGet, rest.BasePath("/todos"), listHandler)

    getHandler := rpc.NewOperation(
        rpc.ReturnJson(
            rpc.Handle(func(ctx context.Context, req PathParams) (Todo, error) {
                todo, ok := store.Get(req.ID)
                if !ok {
                    return Todo{}, fmt.Errorf("todo not found")
                }
                return todo, nil
            }),
        ),
    )
    rest.Handle(http.MethodGet, rest.BasePath("/todos").Param("id"), getHandler)

    updateHandler := rpc.NewOperation(
        rpc.ConsumeJson(
            rpc.ReturnJson(
                rpc.Handle(func(ctx context.Context, req UpdateRequest) (Todo, error) {
                    req.Todo.ID = req.ID
                    if !store.Update(req.Todo) {
                        return Todo{}, fmt.Errorf("todo not found")
                    }
                    return req.Todo, nil
                }),
            ),
        ),
    )
    rest.Handle(http.MethodPut, rest.BasePath("/todos").Param("id"), updateHandler)

    deleteHandler := rpc.NewOperation(
        rpc.Handle(func(ctx context.Context, req PathParams) (string, error) {
            if !store.Delete(req.ID) {
                return "", fmt.Errorf("todo not found")
            }
            return "deleted", nil
        }),
    )
    rest.Handle(http.MethodDelete, rest.BasePath("/todos").Param("id"), deleteHandler)
}

type PathParams struct {
    ID string `path:"id"`
}

type UpdateRequest struct {
    ID   string `path:"id"`
    Todo Todo   `json:",inline"`
}
```

## Run the Service

```bash
go run main.go
```

## Test the API

```bash
# Create a todo
curl -X POST http://localhost:8080/todos \
  -H "Content-Type: application/json" \
  -d '{"title": "Learn Humus", "completed": false}'

# List all todos
curl http://localhost:8080/todos

# Get a specific todo
curl http://localhost:8080/todos/todo-1

# Update a todo
curl -X PUT http://localhost:8080/todos/todo-1 \
  -H "Content-Type: application/json" \
  -d '{"title": "Learn Humus", "completed": true}'

# Delete a todo
curl -X DELETE http://localhost:8080/todos/todo-1

# View OpenAPI spec
curl http://localhost:8080/openapi.json
```

## What's Happening

1. **rest.Run()** loads config and calls Init
2. **rest.NewApi()** creates the API with name and version
3. **rpc.NewOperation()** wraps handlers with type-safe serialization
4. **rest.Handle()** registers handlers at specific paths
5. **Automatic instrumentation** traces all requests
6. **OpenAPI generation** creates `/openapi.json` from your types

## Securing Your API (Optional)

Add JWT authentication to protect write operations:

### 1. Create a Simple JWT Verifier

```go
import (
    "context"
    "fmt"
)

type SimpleJWTVerifier struct{}

func (v *SimpleJWTVerifier) Verify(ctx context.Context, token string) (context.Context, error) {
    // In production, verify the JWT signature and claims
    // For this example, we just accept any non-empty token
    if token == "" {
        return nil, fmt.Errorf("empty token")
    }

    // Extract user info (in production, parse from JWT claims)
    userID := "user-from-token"
    return context.WithValue(ctx, "user_id", userID), nil
}
```

### 2. Protect Create/Update/Delete Operations

```go
func registerHandlers(api *rest.Api, store *TodoStore) {
    verifier := &SimpleJWTVerifier{}

    // Public endpoint - no auth required
    listHandler := rpc.NewOperation(
        rpc.ReturnJson(
            rpc.Handle(func(ctx context.Context, _ any) ([]Todo, error) {
                return store.List(), nil
            }),
        ),
    )
    rest.Handle(http.MethodGet, rest.BasePath("/todos"), listHandler)

    // Protected endpoint - JWT required
    createHandler := rpc.NewOperation(
        rpc.ConsumeJson(
            rpc.ReturnJson(
                rpc.Handle(func(ctx context.Context, req Todo) (Todo, error) {
                    if req.ID == "" {
                        req.ID = fmt.Sprintf("todo-%d", len(store.todos)+1)
                    }
                    store.Create(req)
                    return req, nil
                }),
            ),
        ),
    )
    rest.Handle(
        http.MethodPost,
        rest.BasePath("/todos"),
        createHandler,
        rest.Header("Authorization", rest.Required(), rest.JWTAuth("jwt", verifier)),
    )

    // Other endpoints...
}
```

### 3. Test with Authentication

```bash
# Fails - no Authorization header
curl -X POST http://localhost:8080/todos \
  -H "Content-Type: application/json" \
  -d '{"title": "Protected todo"}'
# Returns: 401 Unauthorized

# Success - with Bearer token
curl -X POST http://localhost:8080/todos \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer my-token" \
  -d '{"title": "Protected todo"}'
# Returns: 200 OK
```

For production JWT implementation with proper signature verification, see [Authentication]({{< ref "authentication" >}}).

## Next Steps

- Learn about [Authentication]({{< ref "authentication" >}}) for production-ready JWT verification
- Explore [REST API]({{< ref "rest-api" >}}) for advanced API configuration
- Read [RPC Pattern]({{< ref "rpc-pattern" >}}) for type-safe handlers
- See [Routing]({{< ref "routing" >}}) for path parameters and validation
- Understand [Error Handling]({{< ref "error-handling" >}}) for custom error responses
