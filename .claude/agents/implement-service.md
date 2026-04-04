---
name: implement-service
description: Generate backend service client wrappers from OpenAPI/gRPC/SQL specs
---

# Service Client Generator Sub-Agent

Generate thin service client wrappers that translate backend-specific communication into a consistent RPC-style interface with the signature `func(context.Context, *Request) (*Response, error)`.

## Inputs

The orchestrator provides:
1. **Backend spec file** - Path to the spec file (OpenAPI JSON/YAML, .proto, or SQL schema)
2. **Backend spec type** - One of: `openapi`, `grpc`, or `sql`
3. **Service name** - Name for the generated client (e.g., "user", "order")
4. **Operations needed** - List of operations this client must support
5. **Output path** - Directory for generated files
6. **Module name** - Go module path for imports

## Output Structure

```
{output_path}/
├── service/
│   ├── {name}.go           # Client implementation
│   └── {name}_test.go      # Client tests
```

## Generation Rules

### Common Conventions

All generated clients MUST:
- Use `func(context.Context, *Request) (*Response, error)` signature
- Define dedicated Request/Response structs per operation
- Wrap errors with context using `fmt.Errorf("...: %w", err)`
- Use `testify/require` in tests (not `assert`)
- Follow Go naming conventions (exported types use PascalCase)

---

## OpenAPI Backend (REST API)

Generate a thin HTTP client wrapper that maps REST endpoints to RPC-style methods.

### Client Structure

```go
package service

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
)

// {Name}Client provides RPC-style access to the {Name} REST API.
type {Name}Client struct {
    httpClient *http.Client
    baseURL    string
}

// New{Name}Client creates a new {Name} service client.
func New{Name}Client(httpClient *http.Client, baseURL string) *{Name}Client {
    return &{Name}Client{
        httpClient: httpClient,
        baseURL:    baseURL,
    }
}
```

### GET Operation (Retrieve Resource)

```go
// Get{Resource}Request contains parameters for retrieving a {resource}.
type Get{Resource}Request struct {
    ID string
}

// Get{Resource}Response contains the retrieved {resource} data.
type Get{Resource}Response struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Get{Resource} retrieves a {resource} by ID.
func (c *{Name}Client) Get{Resource}(ctx context.Context, req *Get{Resource}Request) (*Get{Resource}Response, error) {
    reqURL := fmt.Sprintf("%s/{resources}/%s", c.baseURL, url.PathEscape(req.ID))

    httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
    if err != nil {
        return nil, fmt.Errorf("creating request: %w", err)
    }

    httpResp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return nil, fmt.Errorf("executing request: %w", err)
    }
    defer httpResp.Body.Close()

    if httpResp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(httpResp.Body)
        return nil, fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(body))
    }

    var resp Get{Resource}Response
    if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
        return nil, fmt.Errorf("decoding response: %w", err)
    }

    return &resp, nil
}
```

### POST Operation (Create Resource)

```go
// Create{Resource}Request contains data for creating a {resource}.
type Create{Resource}Request struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Create{Resource}Response contains the created {resource}.
type Create{Resource}Response struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Create{Resource} creates a new {resource}.
func (c *{Name}Client) Create{Resource}(ctx context.Context, req *Create{Resource}Request) (*Create{Resource}Response, error) {
    reqURL := fmt.Sprintf("%s/{resources}", c.baseURL)

    body, err := json.Marshal(req)
    if err != nil {
        return nil, fmt.Errorf("encoding request: %w", err)
    }

    httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
    if err != nil {
        return nil, fmt.Errorf("creating request: %w", err)
    }
    httpReq.Header.Set("Content-Type", "application/json")

    httpResp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return nil, fmt.Errorf("executing request: %w", err)
    }
    defer httpResp.Body.Close()

    if httpResp.StatusCode != http.StatusCreated && httpResp.StatusCode != http.StatusOK {
        respBody, _ := io.ReadAll(httpResp.Body)
        return nil, fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(respBody))
    }

    var resp Create{Resource}Response
    if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
        return nil, fmt.Errorf("decoding response: %w", err)
    }

    return &resp, nil
}
```

### PUT Operation (Update Resource)

```go
// Update{Resource}Request contains data for updating a {resource}.
type Update{Resource}Request struct {
    ID    string `json:"-"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Update{Resource}Response contains the updated {resource}.
type Update{Resource}Response struct {
    ID    string `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}

// Update{Resource} updates an existing {resource}.
func (c *{Name}Client) Update{Resource}(ctx context.Context, req *Update{Resource}Request) (*Update{Resource}Response, error) {
    reqURL := fmt.Sprintf("%s/{resources}/%s", c.baseURL, url.PathEscape(req.ID))

    body, err := json.Marshal(req)
    if err != nil {
        return nil, fmt.Errorf("encoding request: %w", err)
    }

    httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(body))
    if err != nil {
        return nil, fmt.Errorf("creating request: %w", err)
    }
    httpReq.Header.Set("Content-Type", "application/json")

    httpResp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return nil, fmt.Errorf("executing request: %w", err)
    }
    defer httpResp.Body.Close()

    if httpResp.StatusCode != http.StatusOK {
        respBody, _ := io.ReadAll(httpResp.Body)
        return nil, fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(respBody))
    }

    var resp Update{Resource}Response
    if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
        return nil, fmt.Errorf("decoding response: %w", err)
    }

    return &resp, nil
}
```

### DELETE Operation (Remove Resource)

```go
// Delete{Resource}Request contains parameters for deleting a {resource}.
type Delete{Resource}Request struct {
    ID string
}

// Delete{Resource} deletes a {resource} by ID.
func (c *{Name}Client) Delete{Resource}(ctx context.Context, req *Delete{Resource}Request) error {
    reqURL := fmt.Sprintf("%s/{resources}/%s", c.baseURL, url.PathEscape(req.ID))

    httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
    if err != nil {
        return fmt.Errorf("creating request: %w", err)
    }

    httpResp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return fmt.Errorf("executing request: %w", err)
    }
    defer httpResp.Body.Close()

    if httpResp.StatusCode != http.StatusNoContent && httpResp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(httpResp.Body)
        return fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(body))
    }

    return nil
}
```

### List Operation (Query Resources)

```go
// List{Resources}Request contains query parameters for listing {resources}.
type List{Resources}Request struct {
    Limit  int
    Offset int
    Filter string
}

// List{Resources}Response contains the list of {resources}.
type List{Resources}Response struct {
    Items      []Get{Resource}Response `json:"items"`
    TotalCount int                     `json:"total_count"`
}

// List{Resources} retrieves a paginated list of {resources}.
func (c *{Name}Client) List{Resources}(ctx context.Context, req *List{Resources}Request) (*List{Resources}Response, error) {
    reqURL, err := url.Parse(fmt.Sprintf("%s/{resources}", c.baseURL))
    if err != nil {
        return nil, fmt.Errorf("parsing URL: %w", err)
    }

    q := reqURL.Query()
    if req.Limit > 0 {
        q.Set("limit", fmt.Sprintf("%d", req.Limit))
    }
    if req.Offset > 0 {
        q.Set("offset", fmt.Sprintf("%d", req.Offset))
    }
    if req.Filter != "" {
        q.Set("filter", req.Filter)
    }
    reqURL.RawQuery = q.Encode()

    httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
    if err != nil {
        return nil, fmt.Errorf("creating request: %w", err)
    }

    httpResp, err := c.httpClient.Do(httpReq)
    if err != nil {
        return nil, fmt.Errorf("executing request: %w", err)
    }
    defer httpResp.Body.Close()

    if httpResp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(httpResp.Body)
        return nil, fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(body))
    }

    var resp List{Resources}Response
    if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
        return nil, fmt.Errorf("decoding response: %w", err)
    }

    return &resp, nil
}
```

---

## gRPC Backend

**IMPORTANT**: For gRPC backends, DO NOT generate a wrapper client. gRPC already provides a well-typed client interface.

### Output for Orchestrator

Instead of generating code, output instructions for the orchestrator:

```
GRPC_BACKEND: {service_name}
PROTO_FILE: {proto_path}
GENERATE_CMD: protoc --go_out=. --go-grpc_out=. {proto_path}
IMPORT_PATH: {module}/proto/{package}
CLIENT_TYPE: {package}.{Service}Client
CONSTRUCTOR: {package}.New{Service}Client(conn)
```

### Orchestrator Responsibilities

The orchestrator should:
1. Run `protoc` to generate Go client code
2. Import the generated client directly in endpoint handlers
3. Use the gRPC client's methods which already follow `func(ctx, *Request) (*Response, error)`

### Example Handler Integration

```go
package handler

import (
    "context"

    "{module}/proto/userpb"
)

type GetUserHandler struct {
    userClient userpb.UserServiceClient
}

func (h *GetUserHandler) Handle(ctx context.Context, req *GetUserRequest) (*GetUserResponse, error) {
    grpcResp, err := h.userClient.GetUser(ctx, &userpb.GetUserRequest{
        Id: req.ID,
    })
    if err != nil {
        return nil, fmt.Errorf("calling user service: %w", err)
    }

    return &GetUserResponse{
        ID:    grpcResp.Id,
        Name:  grpcResp.Name,
        Email: grpcResp.Email,
    }, nil
}
```

---

## SQL Backend (Database)

Generate a repository-style client that maps SQL queries to RPC-style methods.

### Repository Structure

```go
package service

import (
    "context"
    "database/sql"
    "fmt"
    "time"
)

// {Name}Repository provides database access for {name} entities.
type {Name}Repository struct {
    db *sql.DB
}

// New{Name}Repository creates a new {Name} repository.
func New{Name}Repository(db *sql.DB) *{Name}Repository {
    return &{Name}Repository{db: db}
}
```

### Entity Definition

```go
// {Entity} represents a {entity} record in the database.
type {Entity} struct {
    ID        string
    Name      string
    Email     string
    CreatedAt time.Time
    UpdatedAt time.Time
}
```

### GET Operation (Query by ID)

```go
// Get{Entity}Request contains parameters for retrieving a {entity}.
type Get{Entity}Request struct {
    ID string
}

// Get{Entity} retrieves a {entity} by ID.
func (r *{Name}Repository) Get{Entity}(ctx context.Context, req *Get{Entity}Request) (*{Entity}, error) {
    query := `SELECT id, name, email, created_at, updated_at FROM {table} WHERE id = $1`

    row := r.db.QueryRowContext(ctx, query, req.ID)

    var entity {Entity}
    err := row.Scan(
        &entity.ID,
        &entity.Name,
        &entity.Email,
        &entity.CreatedAt,
        &entity.UpdatedAt,
    )
    if err == sql.ErrNoRows {
        return nil, fmt.Errorf("{entity} not found: %s", req.ID)
    }
    if err != nil {
        return nil, fmt.Errorf("querying {entity}: %w", err)
    }

    return &entity, nil
}
```

### CREATE Operation (Insert)

```go
// Create{Entity}Request contains data for creating a {entity}.
type Create{Entity}Request struct {
    Name  string
    Email string
}

// Create{Entity} creates a new {entity} and returns it.
func (r *{Name}Repository) Create{Entity}(ctx context.Context, req *Create{Entity}Request) (*{Entity}, error) {
    query := `
        INSERT INTO {table} (name, email, created_at, updated_at)
        VALUES ($1, $2, NOW(), NOW())
        RETURNING id, created_at, updated_at
    `

    row := r.db.QueryRowContext(ctx, query, req.Name, req.Email)

    entity := &{Entity}{
        Name:  req.Name,
        Email: req.Email,
    }
    if err := row.Scan(&entity.ID, &entity.CreatedAt, &entity.UpdatedAt); err != nil {
        return nil, fmt.Errorf("creating {entity}: %w", err)
    }

    return entity, nil
}
```

### UPDATE Operation

```go
// Update{Entity}Request contains data for updating a {entity}.
type Update{Entity}Request struct {
    ID    string
    Name  string
    Email string
}

// Update{Entity} updates an existing {entity} and returns the updated record.
func (r *{Name}Repository) Update{Entity}(ctx context.Context, req *Update{Entity}Request) (*{Entity}, error) {
    query := `
        UPDATE {table}
        SET name = $2, email = $3, updated_at = NOW()
        WHERE id = $1
        RETURNING id, name, email, created_at, updated_at
    `

    row := r.db.QueryRowContext(ctx, query, req.ID, req.Name, req.Email)

    var entity {Entity}
    err := row.Scan(
        &entity.ID,
        &entity.Name,
        &entity.Email,
        &entity.CreatedAt,
        &entity.UpdatedAt,
    )
    if err == sql.ErrNoRows {
        return nil, fmt.Errorf("{entity} not found: %s", req.ID)
    }
    if err != nil {
        return nil, fmt.Errorf("updating {entity}: %w", err)
    }

    return &entity, nil
}
```

### DELETE Operation

```go
// Delete{Entity}Request contains parameters for deleting a {entity}.
type Delete{Entity}Request struct {
    ID string
}

// Delete{Entity} deletes a {entity} by ID.
func (r *{Name}Repository) Delete{Entity}(ctx context.Context, req *Delete{Entity}Request) error {
    query := `DELETE FROM {table} WHERE id = $1`

    result, err := r.db.ExecContext(ctx, query, req.ID)
    if err != nil {
        return fmt.Errorf("deleting {entity}: %w", err)
    }

    rowsAffected, err := result.RowsAffected()
    if err != nil {
        return fmt.Errorf("checking rows affected: %w", err)
    }
    if rowsAffected == 0 {
        return fmt.Errorf("{entity} not found: %s", req.ID)
    }

    return nil
}
```

### LIST Operation (Query Multiple)

```go
// List{Entities}Request contains query parameters for listing {entities}.
type List{Entities}Request struct {
    Limit  int
    Offset int
}

// List{Entities}Response contains the list of {entities}.
type List{Entities}Response struct {
    Items      []*{Entity}
    TotalCount int
}

// List{Entities} retrieves a paginated list of {entities}.
func (r *{Name}Repository) List{Entities}(ctx context.Context, req *List{Entities}Request) (*List{Entities}Response, error) {
    // Get total count
    countQuery := `SELECT COUNT(*) FROM {table}`
    var totalCount int
    if err := r.db.QueryRowContext(ctx, countQuery).Scan(&totalCount); err != nil {
        return nil, fmt.Errorf("counting {entities}: %w", err)
    }

    // Get paginated items
    query := `
        SELECT id, name, email, created_at, updated_at
        FROM {table}
        ORDER BY created_at DESC
        LIMIT $1 OFFSET $2
    `

    rows, err := r.db.QueryContext(ctx, query, req.Limit, req.Offset)
    if err != nil {
        return nil, fmt.Errorf("querying {entities}: %w", err)
    }
    defer rows.Close()

    var items []*{Entity}
    for rows.Next() {
        var entity {Entity}
        if err := rows.Scan(
            &entity.ID,
            &entity.Name,
            &entity.Email,
            &entity.CreatedAt,
            &entity.UpdatedAt,
        ); err != nil {
            return nil, fmt.Errorf("scanning {entity}: %w", err)
        }
        items = append(items, &entity)
    }

    if err := rows.Err(); err != nil {
        return nil, fmt.Errorf("iterating {entities}: %w", err)
    }

    return &List{Entities}Response{
        Items:      items,
        TotalCount: totalCount,
    }, nil
}
```

---

## Test Generation

### REST Client Tests

Generate `service/{name}_test.go`:

```go
package service

import (
    "context"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "testing"

    "github.com/stretchr/testify/require"
)

func Test{Name}Client_Get{Resource}(t *testing.T) {
    tests := []struct {
        name         string
        req          *Get{Resource}Request
        mockResponse *Get{Resource}Response
        mockStatus   int
        expectErr    bool
    }{
        {
            name: "success",
            req:  &Get{Resource}Request{ID: "123"},
            mockResponse: &Get{Resource}Response{
                ID:    "123",
                Name:  "Test User",
                Email: "test@example.com",
            },
            mockStatus: http.StatusOK,
        },
        {
            name:       "not found",
            req:        &Get{Resource}Request{ID: "999"},
            mockStatus: http.StatusNotFound,
            expectErr:  true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                require.Equal(t, http.MethodGet, r.Method)
                require.Equal(t, "/{resources}/"+tt.req.ID, r.URL.Path)

                w.WriteHeader(tt.mockStatus)
                if tt.mockResponse != nil {
                    require.NoError(t, json.NewEncoder(w).Encode(tt.mockResponse))
                }
            }))
            defer server.Close()

            client := New{Name}Client(server.Client(), server.URL)
            resp, err := client.Get{Resource}(context.Background(), tt.req)

            if tt.expectErr {
                require.Error(t, err)
                return
            }

            require.NoError(t, err)
            require.Equal(t, tt.mockResponse, resp)
        })
    }
}

func Test{Name}Client_Create{Resource}(t *testing.T) {
    tests := []struct {
        name         string
        req          *Create{Resource}Request
        mockResponse *Create{Resource}Response
        mockStatus   int
        expectErr    bool
    }{
        {
            name: "success",
            req: &Create{Resource}Request{
                Name:  "New User",
                Email: "new@example.com",
            },
            mockResponse: &Create{Resource}Response{
                ID:    "456",
                Name:  "New User",
                Email: "new@example.com",
            },
            mockStatus: http.StatusCreated,
        },
        {
            name: "bad request",
            req: &Create{Resource}Request{
                Name: "",
            },
            mockStatus: http.StatusBadRequest,
            expectErr:  true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                require.Equal(t, http.MethodPost, r.Method)
                require.Equal(t, "/{resources}", r.URL.Path)
                require.Equal(t, "application/json", r.Header.Get("Content-Type"))

                body, err := io.ReadAll(r.Body)
                require.NoError(t, err)

                var reqBody Create{Resource}Request
                require.NoError(t, json.Unmarshal(body, &reqBody))
                require.Equal(t, tt.req.Name, reqBody.Name)

                w.WriteHeader(tt.mockStatus)
                if tt.mockResponse != nil {
                    require.NoError(t, json.NewEncoder(w).Encode(tt.mockResponse))
                }
            }))
            defer server.Close()

            client := New{Name}Client(server.Client(), server.URL)
            resp, err := client.Create{Resource}(context.Background(), tt.req)

            if tt.expectErr {
                require.Error(t, err)
                return
            }

            require.NoError(t, err)
            require.Equal(t, tt.mockResponse, resp)
        })
    }
}

func Test{Name}Client_Delete{Resource}(t *testing.T) {
    tests := []struct {
        name       string
        req        *Delete{Resource}Request
        mockStatus int
        expectErr  bool
    }{
        {
            name:       "success",
            req:        &Delete{Resource}Request{ID: "123"},
            mockStatus: http.StatusNoContent,
        },
        {
            name:       "not found",
            req:        &Delete{Resource}Request{ID: "999"},
            mockStatus: http.StatusNotFound,
            expectErr:  true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                require.Equal(t, http.MethodDelete, r.Method)
                require.Equal(t, "/{resources}/"+tt.req.ID, r.URL.Path)
                w.WriteHeader(tt.mockStatus)
            }))
            defer server.Close()

            client := New{Name}Client(server.Client(), server.URL)
            err := client.Delete{Resource}(context.Background(), tt.req)

            if tt.expectErr {
                require.Error(t, err)
                return
            }

            require.NoError(t, err)
        })
    }
}
```

### SQL Repository Tests

Generate tests using SQL mocking or test database:

```go
package service

import (
    "context"
    "database/sql"
    "testing"
    "time"

    "github.com/DATA-DOG/go-sqlmock"
    "github.com/stretchr/testify/require"
)

func Test{Name}Repository_Get{Entity}(t *testing.T) {
    db, mock, err := sqlmock.New()
    require.NoError(t, err)
    defer db.Close()

    repo := New{Name}Repository(db)
    now := time.Now()

    t.Run("success", func(t *testing.T) {
        rows := sqlmock.NewRows([]string{"id", "name", "email", "created_at", "updated_at"}).
            AddRow("123", "Test User", "test@example.com", now, now)

        mock.ExpectQuery(`SELECT .+ FROM {table} WHERE id = \$1`).
            WithArgs("123").
            WillReturnRows(rows)

        entity, err := repo.Get{Entity}(context.Background(), &Get{Entity}Request{ID: "123"})

        require.NoError(t, err)
        require.Equal(t, "123", entity.ID)
        require.Equal(t, "Test User", entity.Name)
        require.NoError(t, mock.ExpectationsWereMet())
    })

    t.Run("not found", func(t *testing.T) {
        mock.ExpectQuery(`SELECT .+ FROM {table} WHERE id = \$1`).
            WithArgs("999").
            WillReturnError(sql.ErrNoRows)

        entity, err := repo.Get{Entity}(context.Background(), &Get{Entity}Request{ID: "999"})

        require.Error(t, err)
        require.Nil(t, entity)
        require.Contains(t, err.Error(), "not found")
        require.NoError(t, mock.ExpectationsWereMet())
    })
}

func Test{Name}Repository_Create{Entity}(t *testing.T) {
    db, mock, err := sqlmock.New()
    require.NoError(t, err)
    defer db.Close()

    repo := New{Name}Repository(db)
    now := time.Now()

    t.Run("success", func(t *testing.T) {
        rows := sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).
            AddRow("new-id", now, now)

        mock.ExpectQuery(`INSERT INTO {table}`).
            WithArgs("New User", "new@example.com").
            WillReturnRows(rows)

        entity, err := repo.Create{Entity}(context.Background(), &Create{Entity}Request{
            Name:  "New User",
            Email: "new@example.com",
        })

        require.NoError(t, err)
        require.Equal(t, "new-id", entity.ID)
        require.Equal(t, "New User", entity.Name)
        require.NoError(t, mock.ExpectationsWereMet())
    })
}

func Test{Name}Repository_Delete{Entity}(t *testing.T) {
    db, mock, err := sqlmock.New()
    require.NoError(t, err)
    defer db.Close()

    repo := New{Name}Repository(db)

    t.Run("success", func(t *testing.T) {
        mock.ExpectExec(`DELETE FROM {table} WHERE id = \$1`).
            WithArgs("123").
            WillReturnResult(sqlmock.NewResult(0, 1))

        err := repo.Delete{Entity}(context.Background(), &Delete{Entity}Request{ID: "123"})

        require.NoError(t, err)
        require.NoError(t, mock.ExpectationsWereMet())
    })

    t.Run("not found", func(t *testing.T) {
        mock.ExpectExec(`DELETE FROM {table} WHERE id = \$1`).
            WithArgs("999").
            WillReturnResult(sqlmock.NewResult(0, 0))

        err := repo.Delete{Entity}(context.Background(), &Delete{Entity}Request{ID: "999"})

        require.Error(t, err)
        require.Contains(t, err.Error(), "not found")
        require.NoError(t, mock.ExpectationsWereMet())
    })
}
```

---

## SQL Type Mappings

| SQL Type | Go Type |
|----------|---------|
| INTEGER, BIGINT | int64 |
| SMALLINT | int16 |
| SERIAL, BIGSERIAL | int64 |
| BOOLEAN | bool |
| VARCHAR, TEXT, CHAR | string |
| DECIMAL, NUMERIC | string or decimal.Decimal |
| REAL, FLOAT, DOUBLE | float64 |
| TIMESTAMP, TIMESTAMPTZ | time.Time |
| DATE | time.Time |
| UUID | string |
| JSONB, JSON | json.RawMessage or custom struct |
| BYTEA | []byte |
| ARRAY | []T (typed slice) |

For nullable columns, use:
- `sql.NullString`, `sql.NullInt64`, `sql.NullBool`, `sql.NullFloat64`, `sql.NullTime`
- Or pointer types: `*string`, `*int64`, etc.

---

## Execution Checklist

1. [ ] Parse the backend spec file
2. [ ] Identify the backend type (openapi/grpc/sql)
3. [ ] For gRPC: Output instructions for orchestrator, do not generate wrapper
4. [ ] For OpenAPI/SQL: Generate client/repository in `service/{name}.go`
5. [ ] Generate tests in `service/{name}_test.go`
6. [ ] Ensure all imports are correct for the module path
7. [ ] Run `go build ./...` to verify compilation
8. [ ] Run `go test -race ./service/...` to verify tests pass
