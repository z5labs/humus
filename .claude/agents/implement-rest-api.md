---
name: implement-rest-api
description: Generate a complete Humus REST API from OpenAPI spec, backend specs, sequence diagrams, and data mappings
tools: ["bash", "view", "edit", "create", "glob", "grep", "sql", "task"]
---

# REST API Generator Orchestrator

This agent generates production-ready Humus REST API applications by orchestrating the complete code generation pipeline. It parses input specifications, creates project structure, generates backend service clients, generates endpoint handlers, and validates the final output.

## Inputs

The agent expects the following inputs in the invocation message:

| Input | Required | Description |
|-------|----------|-------------|
| OpenAPI spec | Yes | Path to the OpenAPI 3.x YAML/JSON file defining the API operations |
| Backend specs | No | Directory containing backend service specifications (OpenAPI, gRPC proto, SQL schemas) |
| Sequence diagrams | No | Directory containing sequence diagrams (Mermaid, PlantUML) showing operation flows |
| Data mappings | No | Directory containing request/response mapping specifications |
| Output directory | Yes | Target directory for generated code |
| Module name | Yes | Go module name (e.g., `github.com/myorg/my-service`) |

### Example Invocation

```
/implement-rest-api
OpenAPI spec: specs/api.yaml
Backend specs: specs/backends/
Sequence diagrams: specs/sequences/
Data mappings: specs/mappings/
Output directory: ./my-service
Module name: github.com/myorg/my-service
```

## Process

### Step 1: Parse and Validate Inputs

Read and validate all input files systematically.

#### 1.1 Parse OpenAPI Specification

```bash
# Read the OpenAPI spec file
cat specs/api.yaml
```

Extract from the OpenAPI spec:
- All operations (operationId, method, path, summary, description)
- Request body schemas (for POST/PUT/PATCH)
- Response schemas
- Path parameters and query parameters
- Security requirements

Determine handler type for each operation:
- **producer**: GET/HEAD/OPTIONS with no request body → `bedrockrest.GET` + `WriteJSON`
- **consumer**: POST/PUT/PATCH/DELETE with request body but no response body (204) → `bedrockrest.POST/DELETE[io.Reader]` + `ReadJSON` + `WriteBinary(204, "", ep)`
- **handler**: POST/PUT/PATCH with both request and response bodies → `bedrockrest.POST/PUT/PATCH` + `ReadJSON` + `WriteJSON`
- **delete-no-body**: DELETE with no request or response body (204) → `bedrockrest.DELETE[io.Reader]` + `WriteBinary(204, "", ep)`

#### 1.2 Index Backend Specifications

For each file in the backend specs directory:
1. Detect spec type from file extension or content:
   - `.yaml`/`.json` with `openapi` field → OpenAPI backend
   - `.proto` → gRPC backend
   - `.sql` → SQL schema
2. Extract service name from filename or spec content
3. Store in index for later reference

#### 1.3 Match Sequence Diagrams to Operations

For each sequence diagram file:
1. Parse filename to extract operation reference (e.g., `get-user.mmd` → `getUser`)
2. If filename doesn't match, scan content for `operationId` references
3. Extract backend service calls from the sequence
4. Map diagram to operation

#### 1.4 Match Data Mappings to Operations

For each mapping file:
1. Parse filename or content to identify target operation
2. Store mapping rules for code generation

#### 1.5 Create SQL Tracking Tables

```sql
-- Track API operations to generate
CREATE TABLE IF NOT EXISTS api_operations (
    operation_id TEXT PRIMARY KEY,
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    summary TEXT,
    description TEXT,
    handler_type TEXT NOT NULL, -- 'producer', 'consumer', 'handler'
    request_schema TEXT,        -- JSON schema as string
    response_schema TEXT,       -- JSON schema as string
    sequence_file TEXT,
    mapping_file TEXT,
    status TEXT DEFAULT 'pending' -- 'pending', 'in_progress', 'done', 'failed'
);

-- Track backend services to generate
CREATE TABLE IF NOT EXISTS backend_services (
    name TEXT PRIMARY KEY,
    spec_file TEXT NOT NULL,
    spec_type TEXT NOT NULL,    -- 'openapi', 'grpc', 'sql'
    base_url_env TEXT,          -- Environment variable for base URL
    status TEXT DEFAULT 'pending'
);

-- Track which operations use which backends
CREATE TABLE IF NOT EXISTS endpoint_backends (
    operation_id TEXT NOT NULL,
    backend_name TEXT NOT NULL,
    PRIMARY KEY (operation_id, backend_name),
    FOREIGN KEY (operation_id) REFERENCES api_operations(operation_id),
    FOREIGN KEY (backend_name) REFERENCES backend_services(name)
);

-- Track generation errors for debugging
CREATE TABLE IF NOT EXISTS generation_errors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type TEXT NOT NULL,  -- 'operation', 'service'
    entity_id TEXT NOT NULL,
    error_message TEXT NOT NULL,
    created_at TEXT DEFAULT CURRENT_TIMESTAMP
);
```

Insert parsed data into tables:

```sql
-- Example: Insert an operation
INSERT INTO api_operations (operation_id, method, path, summary, handler_type, sequence_file)
VALUES ('getUser', 'GET', '/users/{id}', 'Get user by ID', 'producer', 'specs/sequences/get-user.mmd');

-- Example: Insert a backend service
INSERT INTO backend_services (name, spec_file, spec_type, base_url_env)
VALUES ('user-service', 'specs/backends/user-service.yaml', 'openapi', 'USER_SERVICE_URL');

-- Example: Link operation to backend
INSERT INTO endpoint_backends (operation_id, backend_name)
VALUES ('getUser', 'user-service');
```

### Step 2: Create Project Structure

Check if output directory exists and has content:

```bash
# Check if directory exists and has go.mod
if [ -d "./my-service" ] && [ -f "./my-service/go.mod" ]; then
    echo "existing"
else
    echo "new"
fi
```

#### 2.1 New Project Structure

If creating a new project, generate these files:

**go.mod**
```go
module github.com/myorg/my-service

go 1.24.0

require (
    github.com/z5labs/humus v0.x.x
)
```

**main.go**
```go
package main

import (
    "context"
    "log"

    "github.com/myorg/my-service/app"
    "github.com/z5labs/humus/rest"
)

func main() {
    if err := rest.Run(context.Background(), app.Options()...); err != nil {
        log.Fatal(err)
    }
}
```

**app/app.go**
```go
package app

import (
    "github.com/z5labs/humus/rest"
)

// Options returns the REST server options with all registered endpoints.
func Options() []rest.Option {
    return []rest.Option{
        rest.Title("My Service"),
        rest.Version("1.0.0"),
        // Endpoints will be registered here
    }
}
```

**Directory structure:**
```
my-service/
├── go.mod
├── main.go
├── app/
│   └── app.go
├── endpoint/
│   └── .gitkeep
└── service/
    └── .gitkeep
```

#### 2.2 Existing Project

If adding to existing project:
1. Read current `app/app.go` to understand existing registrations
2. Read `go.mod` to understand dependencies
3. Identify existing endpoints and services to avoid conflicts

### Step 3: Generate Backend Service Clients

For each backend service that doesn't already exist:

```sql
-- Find services needing generation
SELECT name, spec_file, spec_type, base_url_env
FROM backend_services
WHERE status = 'pending'
ORDER BY name;
```

#### 3.1 Delegate to implement-service Sub-Agent

For each pending service, invoke the sub-agent:

```
Use the task tool to spawn an implement-service agent with:

agent_type: "general-purpose"  -- or custom "implement-service" if available
name: "gen-{service-name}"
prompt: |
  Generate a Go HTTP client for the backend service.
  
  **Service Details:**
  - Name: {name}
  - Spec file: {spec_file}
  - Spec type: {spec_type}
  - Base URL env var: {base_url_env}
  
  **Output path:** {output_dir}/service/{name}/
  
  **Requirements:**
  1. Create a client struct with configurable base URL
  2. Implement methods for each operation in the spec
  3. Use proper error handling with wrapped errors
  4. Include OpenTelemetry instrumentation
  5. Follow Humus conventions for HTTP clients
  
  **Spec content:**
  {spec_content}
```

#### 3.2 Track Completion

After each service is generated:

```sql
UPDATE backend_services SET status = 'done' WHERE name = '{name}';
```

On failure:

```sql
UPDATE backend_services SET status = 'failed' WHERE name = '{name}';
INSERT INTO generation_errors (entity_type, entity_id, error_message)
VALUES ('service', '{name}', '{error_message}');
```

### Step 4: Generate Endpoints

For each operation that doesn't already exist:

```sql
-- Find operations ready for generation (all backend dependencies met)
SELECT o.* 
FROM api_operations o
WHERE o.status = 'pending'
AND NOT EXISTS (
    SELECT 1 FROM endpoint_backends eb
    JOIN backend_services bs ON eb.backend_name = bs.name
    WHERE eb.operation_id = o.operation_id
    AND bs.status != 'done'
)
ORDER BY o.path, o.method;
```

#### 4.1 Delegate to implement-endpoint Sub-Agent

For each ready operation, invoke the sub-agent:

```
Use the task tool to spawn an implement-endpoint agent with:

agent_type: "general-purpose"  -- or custom "implement-endpoint" if available
name: "gen-{operation_id}"
prompt: |
  Generate a Humus REST endpoint handler.
  
  **Operation Details:**
  - Operation ID: {operation_id}
  - Method: {method}
  - Path: {path}
  - Summary: {summary}
  - Handler type: {handler_type}
  
  **Request Schema:**
  {request_schema}
  
  **Response Schema:**
  {response_schema}
  
  **Sequence Diagram:**
  {sequence_content}
  
  **Data Mapping:**
  {mapping_content}
  
  **Backend Services Used:**
  {backend_services_list}
  
  **Output path:** {output_dir}/endpoint/{snake_operation_id}/
  
  Where `{snake_operation_id}` is the operationId converted to snake_case (e.g., `listPets` → `list_pets`).
  
  **Requirements:**
  1. Create request/response structs matching schemas
  2. Implement handler following the sequence diagram flow
  3. Use the generated service clients for backend calls
  4. Include proper error handling
  5. Add OpenAPI annotations via struct tags
  6. Write unit tests per implementation file: each `.go` file gets its own `_test.go` with the same base name (e.g., `list_pets.go` → `list_pets_test.go`, `types.go` → `types_test.go`)
  
  **Handler Pattern (bedrock composition — inside-out):**
  All endpoints use the bedrock composition pattern. The sub-agent MUST read
  `instructions/rest-api-generator/code-templates.md` for authoritative templates.
  
  Quick reference:
  - producer (GET, no body): `bedrockrest.GET[Resp](path, fn)` → `WriteJSON[Resp](status, ep)` → `ErrorJSON` → `CatchAll` → `rest.Handle(route)`
  - handler (POST/PUT with body+response): `bedrockrest.POST[Req, Resp](path, fn)` → `ReadJSON[Req](ep)` → `WriteJSON[Resp](status, ep)` → `ErrorJSON` → `CatchAll` → `rest.Handle(route)`
  - consumer/delete (204 no content): `bedrockrest.DELETE[io.Reader](path, fn)` → `WriteBinary(204, "", ep)` → `ErrorJSON` → `CatchAll` → `rest.Handle(route)` — handler returns `bytes.NewReader(nil)` on success
  
  CRITICAL: `req.Body()` is a method call, NOT a field. Use `body := req.Body()` then `&body`.
```

#### 4.2 Track Completion

After each endpoint is generated:

```sql
UPDATE api_operations SET status = 'done' WHERE operation_id = '{operation_id}';
```

On failure:

```sql
UPDATE api_operations SET status = 'failed' WHERE operation_id = '{operation_id}';
INSERT INTO generation_errors (entity_type, entity_id, error_message)
VALUES ('operation', '{operation_id}', '{error_message}');
```

### Step 5: Update app.go

After all endpoints are generated, update the application registration.

#### 5.1 Read Current State

```bash
cat {output_dir}/app/app.go
```

#### 5.2 Generate Updated app.go

Collect all generated endpoints:

```sql
SELECT operation_id, method, path, handler_type
FROM api_operations
WHERE status = 'done'
ORDER BY path, method;
```

Update `app/app.go` with:
1. Import statements for all endpoint packages
2. Import statements for all service packages
3. Service client initialization
4. Endpoint registration calls using `rest.Handle()`

**Example updated app.go:**
```go
package app

import (
    "net/http"

    "github.com/z5labs/humus/rest"
    
    "github.com/myorg/my-service/endpoint/getuser"
    "github.com/myorg/my-service/endpoint/createuser"
    "github.com/myorg/my-service/service/userservice"
)

// Options returns the REST server options with all registered endpoints.
func Options() []rest.Option {
    // Initialize service clients
    userSvc := userservice.New()
    
    return []rest.Option{
        rest.Title("My Service"),
        rest.Version("1.0.0"),
        
        // GET /users/{id} - Get user by ID
        getuser.Route(userSvc),
        
        // POST /users - Create user
        createuser.Route(userSvc),
    }
}
```

### Step 6: Validation

Verify the generated code compiles and passes tests.

#### 6.1 Resolve Dependencies

```bash
cd {output_dir} && go mod tidy
```

#### 6.2 Build Verification

```bash
cd {output_dir} && go build ./...
```

#### 6.3 Test Verification

```bash
cd {output_dir} && go test -race ./...
```

#### 6.4 Lint Verification (Optional)

```bash
cd {output_dir} && golangci-lint run 2>/dev/null || echo "Linter not available"
```

#### 6.5 Report Results

Generate a summary report:

```sql
-- Summary statistics
SELECT 
    'Operations' as entity,
    COUNT(*) as total,
    SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END) as completed,
    SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) as failed
FROM api_operations
UNION ALL
SELECT 
    'Services' as entity,
    COUNT(*) as total,
    SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END) as completed,
    SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) as failed
FROM backend_services;

-- List any errors
SELECT entity_type, entity_id, error_message, created_at
FROM generation_errors
ORDER BY created_at;
```

## Sub-Agent Delegation

### implement-service Agent

Generates backend service client code.

**Invocation:**
```
task(
    agent_type: "general-purpose",
    name: "impl-svc-{name}",
    description: "Generate {name} service client",
    prompt: "Generate a Go HTTP client for {name}..."
)
```

**Expected Output:**
- `service/{snake_name}/{snake_name}.go` - Client implementation
- `service/{snake_name}/{snake_name}_test.go` - Unit tests matching the implementation file
- `service/{snake_name}/types.go` - Request/response types (if complex)
- `service/{snake_name}/types_test.go` - Tests for types (if types.go exists)

Where `{snake_name}` is the service name converted to snake_case (e.g., `userService` → `user_service`).

### implement-endpoint Agent

Generates endpoint handler code.

**Invocation:**
```
task(
    agent_type: "general-purpose", 
    name: "impl-ep-{operation_id}",
    description: "Generate {operation_id} endpoint",
    prompt: "Generate a Humus REST endpoint handler..."
)
```

**Expected Output:**
- `endpoint/{snake_id}/{snake_id}.go` - Handler implementation
- `endpoint/{snake_id}/{snake_id}_test.go` - Unit tests matching the implementation file
- `endpoint/{snake_id}/types.go` - Request/response types (if separate from handler file)
- `endpoint/{snake_id}/types_test.go` - Tests for types (if types.go exists)

Where `{snake_id}` is the operationId converted to snake_case (e.g., `listPets` → `list_pets`).

**Test file convention:** Each implementation file must have a corresponding test file with the same base name and a `_test.go` suffix. Do NOT consolidate tests from multiple implementation files into a single test file.

## Error Handling

### Input Validation Errors

If required inputs are missing or invalid:
1. Report the specific error to the user
2. List what's missing or malformed
3. Stop processing

### Backend Generation Failures

If a service client fails to generate:
1. Log error to `generation_errors` table
2. Mark service as `failed`
3. Continue with other services
4. Skip endpoints that depend on failed services
5. Report in final summary

### Endpoint Generation Failures

If an endpoint handler fails to generate:
1. Log error to `generation_errors` table
2. Mark operation as `failed`
3. Continue with other endpoints
4. Report in final summary

### Compilation Failures

If final build fails:
1. Report the build errors
2. Suggest fixes based on error messages
3. Do NOT mark operations as successful if build fails

### Partial Completion

The agent supports resuming from partial completion:
1. Check SQL tables for existing state
2. Skip already-completed items
3. Retry failed items if requested
4. Generate only missing components

## Example

### Complete Invocation

```
/implement-rest-api
OpenAPI spec: specs/petstore.yaml
Backend specs: specs/backends/
Sequence diagrams: specs/sequences/
Data mappings: specs/mappings/
Output directory: ./petstore-api
Module name: github.com/myorg/petstore-api
```

### Sample OpenAPI Spec (specs/petstore.yaml)

```yaml
openapi: 3.0.3
info:
  title: Petstore API
  version: 1.0.0
paths:
  /pets:
    get:
      operationId: listPets
      summary: List all pets
      responses:
        '200':
          description: A list of pets
          content:
            application/json:
              schema:
                type: array
                items:
                  $ref: '#/components/schemas/Pet'
    post:
      operationId: createPet
      summary: Create a pet
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CreatePetRequest'
      responses:
        '201':
          description: Created pet
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Pet'
  /pets/{petId}:
    get:
      operationId: getPet
      summary: Get a pet by ID
      parameters:
        - name: petId
          in: path
          required: true
          schema:
            type: string
      responses:
        '200':
          description: A pet
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Pet'
components:
  schemas:
    Pet:
      type: object
      properties:
        id:
          type: string
        name:
          type: string
        status:
          type: string
          enum: [available, pending, sold]
    CreatePetRequest:
      type: object
      required: [name]
      properties:
        name:
          type: string
        status:
          type: string
```

### Expected Output Structure

```
petstore-api/
├── go.mod
├── go.sum
├── main.go
├── config.yaml
├── app/
│   └── app.go
├── endpoint/
│   ├── list_pets/
│   │   ├── list_pets.go
│   │   └── list_pets_test.go
│   ├── create_pet/
│   │   ├── create_pet.go
│   │   └── create_pet_test.go
│   └── get_pet/
│       ├── get_pet.go
│       └── get_pet_test.go
└── service/
    └── (backend service clients if any)
```

### Expected app.go Output

```go
package app

import (
    "github.com/z5labs/humus/rest"

    "github.com/myorg/petstore-api/endpoint/create_pet"
    "github.com/myorg/petstore-api/endpoint/get_pet"
    "github.com/myorg/petstore-api/endpoint/list_pets"
)

// Options returns the REST server options with all registered endpoints.
func Options() []rest.Option {
    return []rest.Option{
        rest.Title("Petstore API"),
        rest.Version("1.0.0"),

        // GET /pets - List all pets
        list_pets.Route(),

        // POST /pets - Create a pet
        create_pet.Route(),

        // GET /pets/{petId} - Get a pet by ID
        get_pet.Route(),
    }
}
```

### Expected Endpoint Implementation (endpoint/get_pet/get_pet.go)

```go
package get_pet

import (
	"context"
	"log/slog"
	"net/http"

	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus"
	"github.com/z5labs/humus/rest"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Response represents the pet returned by this endpoint.
type Response struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// PetError represents an error response.
type PetError struct {
	Message string `json:"message"`
}

func (e PetError) Error() string { return e.Message }

type getPetHandler struct {
	tracer trace.Tracer
	log    *slog.Logger
	// Add dependencies here (e.g., database, service clients)
}

func (h *getPetHandler) handle(ctx context.Context, id string) (Response, error) {
	ctx, span := h.tracer.Start(ctx, "GetPet")
	defer span.End()

	h.log.InfoContext(ctx, "getting pet", slog.String("id", id))

	// Fetch pet from data source
	return Response{
		ID:     id,
		Name:   "Fluffy",
		Status: "available",
	}, nil
}

// Route returns the REST route option for this endpoint.
func Route() rest.Option {
	h := &getPetHandler{
		tracer: otel.Tracer("petstore/endpoint"),
		log:    humus.Logger("petstore/endpoint"),
	}

	petID := bedrockrest.PathParam[string]("petId", bedrockrest.Required())

	ep := bedrockrest.GET[Response]("/pets/{petId}", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) (Response, error) {
		id := bedrockrest.ParamFrom(req, petID)
		return h.handle(ctx, id)
	})
	ep = petID.Read(ep)
	ep = bedrockrest.WriteJSON[Response](http.StatusOK, ep)
	ep = bedrockrest.ErrorJSON[PetError](http.StatusNotFound, ep)
	route := bedrockrest.CatchAll(http.StatusInternalServerError, func(err error) PetError {
		return PetError{Message: err.Error()}
	}, ep)

	return rest.Handle(route)
}
```

### Progress Output

During execution, the agent reports progress:

```
📋 Parsing OpenAPI spec: specs/petstore.yaml
   Found 3 operations: listPets, createPet, getPet

📁 Creating project structure at ./petstore-api
   ✓ go.mod
   ✓ main.go
   ✓ config.yaml
   ✓ app/app.go

🔧 Generating endpoints...
   [1/3] listPets (GET /pets) - producer
         ✓ endpoint/list_pets/list_pets.go
         ✓ endpoint/list_pets/list_pets_test.go
   [2/3] createPet (POST /pets) - handler
         ✓ endpoint/create_pet/create_pet.go
         ✓ endpoint/create_pet/create_pet_test.go
   [3/3] getPet (GET /pets/{petId}) - producer
         ✓ endpoint/get_pet/get_pet.go
         ✓ endpoint/get_pet/get_pet_test.go

📝 Updating app/app.go with endpoint registrations

✅ Validation
   ✓ go mod tidy
   ✓ go build ./...
   ✓ go test -race ./...

🎉 Generation complete!
   Operations: 3/3 successful
   Services: 0/0 successful
```

## Resumability

The agent supports resuming interrupted generation:

1. **Check for existing state:**
   ```sql
   SELECT COUNT(*) FROM api_operations WHERE status = 'pending';
   ```

2. **Resume from last checkpoint:**
   - Skip completed services and endpoints
   - Retry failed items
   - Continue with pending items

3. **Force regeneration:**
   If user requests regeneration of specific items:
   ```sql
   UPDATE api_operations SET status = 'pending' WHERE operation_id = 'getUser';
   ```

## Notes

- All generated code follows Humus conventions exactly
- Use `require` (not `assert`) in tests per project conventions
- Follow Go naming: `ErrFoo` for errors, `camelCase` for unexported
- Embed OpenAPI metadata in struct tags for automatic schema generation
- Each endpoint is self-contained with its own types and tests
- File naming and test file conventions are enforced by the `implement-endpoint` sub-agent
