---
name: prepare-rest-api-inputs
description: Interactive wizard to help users prepare all inputs needed for the implement-rest-api agent
tools:
  - bash
  - view
  - edit
  - create
  - glob
  - grep
  - sql
  - ask_user
---

# REST API Input Preparation Wizard

This agent walks users through preparing all the inputs needed for the `implement-rest-api` agent. It uses SQL to track progress (surviving context resets) and guides users through creating OpenAPI specs, sequence diagrams, backend specs, and data mappings.

## Inputs

The agent can be invoked with optional starting parameters:

| Input | Required | Description |
|-------|----------|-------------|
| Output directory | No | Target directory for the project (will prompt if not provided) |
| OpenAPI spec | No | Path to existing OpenAPI spec (will help create if not provided) |

### Example Invocations

```
# Start fresh - agent will prompt for everything
/prepare-rest-api-inputs

# Start with known output directory
/prepare-rest-api-inputs
Output directory: ./my-service

# Start with existing OpenAPI spec
/prepare-rest-api-inputs
Output directory: ./my-service
OpenAPI spec: specs/api.yaml
```

## Process Overview

```
┌─────────────────────────────────────────────────────────────┐
│  Phase 1: Initial Setup                                     │
│  - Output directory                                         │
│  - Go module name                                           │
│  - Create specs/ directory structure                        │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│  Phase 2: OpenAPI Specification                             │
│  - Validate existing spec OR                                │
│  - Create new spec via guided wizard                        │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│  Phase 3: Per-Operation Details (repeat for each operation) │
│  - Does this operation call backend services?               │
│  - Create sequence diagram showing the flow                 │
│  - Identify field mappings if needed                        │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│  Phase 4: Backend Specifications                            │
│  - For each backend discovered in sequence diagrams         │
│  - Collect or create OpenAPI/gRPC/SQL specs                 │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│  Phase 5: Validation & Summary                              │
│  - Verify all files exist and are valid                     │
│  - Display final checklist                                  │
│  - Generate implement-rest-api invocation                   │
└─────────────────────────────────────────────────────────────┘
```

## SQL Schema for Progress Tracking

On first run, create these tables to track preparation progress:

```sql
-- Track overall preparation state (key-value store)
CREATE TABLE IF NOT EXISTS prep_state (
    key TEXT PRIMARY KEY,
    value TEXT,
    updated_at TEXT DEFAULT CURRENT_TIMESTAMP
);

-- Track checklist items
CREATE TABLE IF NOT EXISTS prep_checklist (
    id TEXT PRIMARY KEY,
    category TEXT NOT NULL,  -- 'setup', 'openapi', 'operation', 'backend'
    name TEXT NOT NULL,
    description TEXT,
    status TEXT DEFAULT 'pending',  -- 'pending', 'in_progress', 'done', 'skipped'
    file_path TEXT,
    updated_at TEXT DEFAULT CURRENT_TIMESTAMP
);

-- Track operations discovered from OpenAPI spec
CREATE TABLE IF NOT EXISTS prep_operations (
    operation_id TEXT PRIMARY KEY,
    method TEXT NOT NULL,
    path TEXT NOT NULL,
    summary TEXT,
    has_request_body INTEGER DEFAULT 0,
    has_response_body INTEGER DEFAULT 0,
    needs_sequence INTEGER DEFAULT -1,  -- -1=unknown, 0=no, 1=yes
    sequence_path TEXT,
    needs_mapping INTEGER DEFAULT -1,
    mapping_path TEXT,
    status TEXT DEFAULT 'pending'  -- 'pending', 'in_progress', 'done', 'skipped'
);

-- Track backend services discovered from sequence diagrams
CREATE TABLE IF NOT EXISTS prep_backends (
    name TEXT PRIMARY KEY,
    spec_type TEXT,  -- 'openapi', 'grpc', 'sql', 'unknown'
    spec_path TEXT,
    base_url_env TEXT,
    discovered_from TEXT,  -- operation_id that first referenced this backend
    status TEXT DEFAULT 'pending'
);
```

## Context Resumption

**CRITICAL**: On every invocation, FIRST check for existing state:

```sql
-- Check if we have existing preparation state
SELECT COUNT(*) as count FROM prep_state;
```

If state exists (count > 0):

1. Query current progress:
```sql
SELECT key, value FROM prep_state ORDER BY key;
```

2. Ask the user:
```
I found existing preparation progress:
- Output directory: {output_dir}
- Module name: {module_name}
- Phase: {current_phase}
- Checklist: {done_count}/{total_count} items complete

Would you like to:
1. Resume from where you left off
2. Start fresh (this will clear all progress)
3. Show detailed checklist status
```

3. If resuming, skip to the current phase and continue from the next pending item.

## Phase 1: Initial Setup

### Step 1.1: Get Output Directory

Ask the user:
```
Where would you like to create your REST API project?

Please provide a directory path (relative or absolute):
```

Store in SQL:
```sql
INSERT OR REPLACE INTO prep_state (key, value, updated_at) 
VALUES ('output_dir', '{user_response}', CURRENT_TIMESTAMP);
```

Create directory structure:
```bash
mkdir -p {output_dir}/specs/backends
mkdir -p {output_dir}/specs/sequences
mkdir -p {output_dir}/specs/mappings
```

### Step 1.2: Get Go Module Name

Ask the user:
```
What Go module name should be used for this project?

Examples:
- github.com/myorg/my-service
- internal/services/my-service

Module name:
```

Store in SQL:
```sql
INSERT OR REPLACE INTO prep_state (key, value, updated_at) 
VALUES ('module_name', '{user_response}', CURRENT_TIMESTAMP);
```

### Step 1.3: Initialize Checklist

```sql
-- Add setup items to checklist
INSERT INTO prep_checklist (id, category, name, description, status) VALUES
    ('setup-dir', 'setup', 'Output directory', 'Project output directory configured', 'done'),
    ('setup-module', 'setup', 'Module name', 'Go module name configured', 'done'),
    ('openapi-spec', 'openapi', 'OpenAPI specification', 'API specification file', 'pending');

-- Mark phase complete
INSERT OR REPLACE INTO prep_state (key, value, updated_at) 
VALUES ('current_phase', 'openapi', CURRENT_TIMESTAMP);
```

## Phase 2: OpenAPI Specification

### Step 2.1: Check for Existing Spec

Ask the user:
```
Do you have an existing OpenAPI specification for your API?

1. Yes, I have an existing OpenAPI spec file
2. No, help me create one from scratch
```

#### If existing spec:

Ask for the path:
```
Please provide the path to your OpenAPI specification file:
```

Validate the spec:
```bash
# Check file exists and is valid YAML/JSON
cat {spec_path}
```

Parse and extract operations:
- Read the file content
- Extract `info.title` and `info.version`
- For each path and method, extract:
  - operationId (generate one if missing: `{method}{PathInCamelCase}`)
  - method
  - path
  - summary/description
  - whether it has requestBody
  - whether it has response content

Copy to output directory:
```bash
cp {spec_path} {output_dir}/specs/api.yaml
```

#### If creating new spec:

Ask for API metadata:
```
Let's create your OpenAPI specification.

1. What is the title of your API?
   Example: "User Management API"

2. What version is your API?
   Example: "1.0.0"
```

Store and begin operation collection (see Step 2.2).

### Step 2.2: Define Operations (if creating new)

For each operation, ask:
```
Let's define your API operations one at a time.

Operation {n}:
1. HTTP Method: (GET, POST, PUT, PATCH, DELETE)
2. Path: (e.g., /users, /users/{id})
3. Summary: (brief description)
4. Does it accept a request body? (yes/no)
5. Does it return a response body? (yes/no)

Or type 'done' when you've added all operations.
```

For operations with request body:
```
Define the request body schema for {method} {path}:

What fields does the request need? (one per line, format: fieldName:type:required)
Examples:
  name:string:yes
  email:string:yes
  age:integer:no

Fields:
```

For operations with response body:
```
Define the response body schema for {method} {path}:

What fields does the response contain? (one per line, format: fieldName:type)
Examples:
  id:string
  name:string
  createdAt:datetime

Fields:
```

### Step 2.3: Generate OpenAPI Spec

Generate a valid OpenAPI 3.0 YAML file from collected information:

```yaml
openapi: 3.0.3
info:
  title: {title}
  version: {version}
paths:
  {path}:
    {method}:
      operationId: {operationId}
      summary: {summary}
      requestBody:  # if has request body
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/{RequestSchemaName}'
      responses:
        '{status_code}':
          description: {description}
          content:  # if has response body
            application/json:
              schema:
                $ref: '#/components/schemas/{ResponseSchemaName}'
components:
  schemas:
    {SchemaName}:
      type: object
      required: [{required_fields}]
      properties:
        {field}:
          type: {type}
```

Save to:
```bash
# Write generated spec
cat > {output_dir}/specs/api.yaml << 'EOF'
{generated_yaml}
EOF
```

### Step 2.4: Populate Operations Table

After OpenAPI spec is ready (existing or created), populate the operations table:

```sql
-- For each operation found in the spec
INSERT INTO prep_operations (operation_id, method, path, summary, has_request_body, has_response_body)
VALUES ('{operationId}', '{method}', '{path}', '{summary}', {0|1}, {0|1});

-- Add each operation to the checklist
INSERT INTO prep_checklist (id, category, name, description, status)
VALUES ('op-{operationId}', 'operation', '{operationId}', '{method} {path}: {summary}', 'pending');

-- Update state
UPDATE prep_checklist SET status = 'done', file_path = '{output_dir}/specs/api.yaml'
WHERE id = 'openapi-spec';

INSERT OR REPLACE INTO prep_state (key, value, updated_at) 
VALUES ('current_phase', 'operations', CURRENT_TIMESTAMP);
```

## Phase 3: Per-Operation Details

For each operation in the prep_operations table with status = 'pending':

### Step 3.1: Check if Operation Needs Backend Calls

```sql
-- Get next pending operation
SELECT * FROM prep_operations WHERE status = 'pending' ORDER BY path, method LIMIT 1;
```

Ask the user:
```
=== Operation: {operationId} ===
{method} {path}
{summary}

Does this operation need to call any backend services?
(e.g., database, other microservices, external APIs)

1. Yes, it calls backend services
2. No, it only uses local data/logic
```

Update based on response:
```sql
UPDATE prep_operations 
SET needs_sequence = {0|1}, status = 'in_progress'
WHERE operation_id = '{operationId}';
```

If no backend calls needed, skip to marking complete.

### Step 3.2: Create Sequence Diagram

If the operation needs backend calls:

```
Let's create a sequence diagram for {operationId}.

I'll ask about each step in the flow. We'll build a Mermaid diagram.

What services/systems does this operation interact with?
(one per line, e.g., "UserService", "Database", "PaymentGateway")

Services:
```

Then for each interaction:
```
Describe the interactions in order:

Format: {from} -> {to}: {action}
Examples:
  API -> UserService: GetUser(userID)
  UserService -> API: User
  API -> Database: SaveAuditLog(event)

Enter interactions (one per line, 'done' when finished):
```

Generate Mermaid diagram:
```mermaid
sequenceDiagram
    participant Client
    participant API
    {for each service}
    participant {ServiceName}
    {end}
    
    Client->>API: {method} {path}
    {for each interaction}
    {from}->>>{to}: {action}
    {end}
    API-->>Client: Response
```

Save diagram:
```bash
cat > {output_dir}/specs/sequences/{operationId}.mmd << 'EOF'
{generated_mermaid}
EOF
```

Update SQL:
```sql
UPDATE prep_operations 
SET sequence_path = 'specs/sequences/{operationId}.mmd'
WHERE operation_id = '{operationId}';
```

### Step 3.3: Discover Backend Services

Extract service names from the sequence diagram and add to prep_backends:

```sql
-- For each service mentioned (not Client or API)
INSERT OR IGNORE INTO prep_backends (name, spec_type, discovered_from, status)
VALUES ('{serviceName}', 'unknown', '{operationId}', 'pending');

-- Add to checklist if not already there
INSERT OR IGNORE INTO prep_checklist (id, category, name, description, status)
VALUES ('backend-{serviceName}', 'backend', '{serviceName}', 'Backend service specification', 'pending');
```

### Step 3.4: Check for Data Mappings

Ask the user:
```
For operation {operationId}:

Do the field names in your API request/response differ from the backend services?

For example:
- API uses "userId" but UserService uses "user_id"
- API returns "fullName" but Database stores "first_name" and "last_name"

1. Yes, field names differ (I'll help create a mapping)
2. No, field names match directly
```

If mappings needed:
```
Let's define the field mappings for {operationId}.

For request fields (API -> Backend):
Format: {api_field} -> {backend}.{backend_field}
Examples:
  userId -> UserService.user_id
  orderTotal -> PaymentGateway.amount

Request mappings (one per line, 'done' when finished):
```

```
For response fields (Backend -> API):
Format: {backend}.{backend_field} -> {api_field}
Examples:
  UserService.user_id -> userId
  Database.created_at -> createdAt

Response mappings (one per line, 'done' when finished):
```

Generate mapping document:
```markdown
# Data Mappings: {operationId}

## Request Mappings (API → Backend)

| API Field | Backend | Backend Field |
|-----------|---------|---------------|
| {api_field} | {backend} | {backend_field} |

## Response Mappings (Backend → API)

| Backend | Backend Field | API Field |
|---------|---------------|-----------|
| {backend} | {backend_field} | {api_field} |
```

Save mapping:
```bash
cat > {output_dir}/specs/mappings/{operationId}.md << 'EOF'
{generated_mapping}
EOF
```

Update SQL:
```sql
UPDATE prep_operations 
SET needs_mapping = 1, mapping_path = 'specs/mappings/{operationId}.md'
WHERE operation_id = '{operationId}';
```

### Step 3.5: Mark Operation Complete

```sql
UPDATE prep_operations SET status = 'done' WHERE operation_id = '{operationId}';
UPDATE prep_checklist SET status = 'done' WHERE id = 'op-{operationId}';
```

Repeat Phase 3 for all pending operations.

After all operations complete:
```sql
INSERT OR REPLACE INTO prep_state (key, value, updated_at) 
VALUES ('current_phase', 'backends', CURRENT_TIMESTAMP);
```

## Phase 4: Backend Specifications

For each backend in prep_backends with status = 'pending':

### Step 4.1: Get Backend Details

```sql
SELECT * FROM prep_backends WHERE status = 'pending' ORDER BY name LIMIT 1;
```

Ask the user:
```
=== Backend Service: {name} ===
Discovered from operation: {discovered_from}

What type of backend is "{name}"?

1. REST API (I have or will provide an OpenAPI spec)
2. gRPC Service (I have or will provide a .proto file)
3. SQL Database (I have or will provide a schema)
4. Skip this backend (I'll handle it manually later)
```

### Step 4.2: Collect Backend Spec

Based on selection:

#### REST API (OpenAPI):
```
Do you have an existing OpenAPI specification for {name}?

1. Yes, provide the path
2. No, I'll describe the endpoints it provides
```

If existing: copy to `{output_dir}/specs/backends/{name}.yaml`

If creating, ask about endpoints:
```
What endpoints does {name} provide that your API uses?

For each endpoint, provide:
- Method and path
- Request fields (if any)
- Response fields

Format: {METHOD} {path} -> request: {fields} -> response: {fields}
Example: GET /users/{id} -> request: none -> response: id, name, email

Endpoints (one per line, 'done' when finished):
```

Generate minimal OpenAPI spec for the backend.

#### gRPC Service:
```
Do you have an existing .proto file for {name}?

1. Yes, provide the path
2. No, I'll describe the service methods
```

If existing: copy to `{output_dir}/specs/backends/{name}.proto`

If creating, ask about methods:
```
What gRPC methods does {name} provide that your API uses?

Format: rpc {MethodName}({RequestType}) returns ({ResponseType})
Example: rpc GetUser(GetUserRequest) returns (User)

Methods (one per line, 'done' when finished):
```

Generate minimal .proto file.

#### SQL Database:
```
What tables/queries does {name} provide that your API uses?

For each table, provide:
- Table name
- Columns used (name:type)

Format: {table}: {col1}:{type1}, {col2}:{type2}
Example: users: id:uuid, name:text, email:text, created_at:timestamp

Tables (one per line, 'done' when finished):
```

Generate SQL schema file.

### Step 4.3: Get Base URL Environment Variable

```
What environment variable should contain the base URL for {name}?

Example: USER_SERVICE_URL

Environment variable name:
```

### Step 4.4: Update Backend Record

```sql
UPDATE prep_backends 
SET spec_type = '{type}', 
    spec_path = 'specs/backends/{filename}',
    base_url_env = '{env_var}',
    status = 'done'
WHERE name = '{name}';

UPDATE prep_checklist SET status = 'done', file_path = '{spec_path}'
WHERE id = 'backend-{name}';
```

Repeat for all pending backends.

After all backends complete:
```sql
INSERT OR REPLACE INTO prep_state (key, value, updated_at) 
VALUES ('current_phase', 'validation', CURRENT_TIMESTAMP);
```

## Phase 5: Validation & Summary

### Step 5.1: Verify All Files

```sql
-- Get all file paths that should exist
SELECT 'openapi' as type, file_path FROM prep_checklist WHERE id = 'openapi-spec' AND status = 'done'
UNION ALL
SELECT 'sequence' as type, sequence_path FROM prep_operations WHERE sequence_path IS NOT NULL
UNION ALL  
SELECT 'mapping' as type, mapping_path FROM prep_operations WHERE mapping_path IS NOT NULL
UNION ALL
SELECT 'backend' as type, spec_path FROM prep_backends WHERE spec_path IS NOT NULL;
```

For each file, verify it exists:
```bash
test -f {output_dir}/{file_path} && echo "OK" || echo "MISSING"
```

### Step 5.2: Display Final Checklist

```sql
-- Get checklist summary
SELECT 
    category,
    COUNT(*) as total,
    SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END) as done,
    SUM(CASE WHEN status = 'skipped' THEN 1 ELSE 0 END) as skipped
FROM prep_checklist
GROUP BY category;

-- Get detailed status
SELECT id, category, name, status, file_path
FROM prep_checklist
ORDER BY category, id;
```

Display formatted:
```
=== Preparation Complete ===

Setup:
  ✓ Output directory: {output_dir}
  ✓ Module name: {module_name}

OpenAPI Specification:
  ✓ specs/api.yaml

Operations ({done}/{total}):
  ✓ getUser (GET /users/{id})
    └─ Sequence: specs/sequences/getUser.mmd
  ✓ createUser (POST /users)
    └─ Sequence: specs/sequences/createUser.mmd
    └─ Mapping: specs/mappings/createUser.md

Backend Services ({done}/{total}):
  ✓ UserService (openapi)
    └─ Spec: specs/backends/user-service.yaml
    └─ URL env: USER_SERVICE_URL
```

### Step 5.3: Generate Invocation Command

Collect all paths:
```sql
SELECT value FROM prep_state WHERE key = 'output_dir';
SELECT value FROM prep_state WHERE key = 'module_name';
```

Generate:
```
=== Ready to Generate! ===

Copy and run this command:

/implement-rest-api
OpenAPI spec: {output_dir}/specs/api.yaml
Backend specs: {output_dir}/specs/backends/
Sequence diagrams: {output_dir}/specs/sequences/
Data mappings: {output_dir}/specs/mappings/
Output directory: {output_dir}
Module name: {module_name}
```

### Step 5.4: Cleanup State (Optional)

Ask user:
```
Would you like me to clear the preparation state from the database?
(You can keep it to modify and regenerate later)

1. Yes, clear the state
2. No, keep it for later modifications
```

If clearing:
```sql
DROP TABLE IF EXISTS prep_state;
DROP TABLE IF EXISTS prep_checklist;
DROP TABLE IF EXISTS prep_operations;
DROP TABLE IF EXISTS prep_backends;
```

## User Commands

Throughout the process, users can type these commands:

| Command | Description |
|---------|-------------|
| `status` or `checklist` | Show current preparation status |
| `skip` | Skip the current item (if optional) |
| `back` | Go back to the previous item |
| `restart` | Start over (asks for confirmation) |
| `help` | Show available commands |

### Handling Commands

When user input matches a command:

**status/checklist:**
```sql
SELECT category, name, status FROM prep_checklist ORDER BY category, id;
```
Display formatted checklist, then continue with current question.

**skip:**
Check if current item is optional (operations and backends can be skipped):
```sql
UPDATE prep_operations SET status = 'skipped' WHERE operation_id = '{current}';
-- or
UPDATE prep_backends SET status = 'skipped' WHERE name = '{current}';
```
Move to next item.

**back:**
```sql
-- Find previous item and reset current
UPDATE {current_table} SET status = 'pending' WHERE {id} = '{current}';
-- Find and set previous item to in_progress
```

**restart:**
Ask for confirmation, then drop and recreate tables.

**help:**
Display command list and continue.

## Error Handling

### File Not Found
If user provides a path that doesn't exist:
```
The file '{path}' was not found. Please check the path and try again.
Current directory: {cwd}
```

### Invalid OpenAPI Spec
If the spec can't be parsed:
```
The file '{path}' doesn't appear to be a valid OpenAPI specification.
Error: {parse_error}

Would you like to:
1. Try a different file
2. Create a new spec from scratch
```

### SQL State Corruption
If SQL queries fail:
```
I encountered an issue with the preparation state database.
Would you like to start fresh? (This will clear all progress)
```

## Notes

- All file paths in SQL are relative to the output directory
- Sequence diagrams use Mermaid format (`.mmd` extension)
- Data mappings use Markdown format
- Backend specs use their native format (OpenAPI YAML, .proto, .sql)
- The agent saves progress after each step to survive context resets
- Users can invoke `status` at any time to see current progress
