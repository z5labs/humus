---
description: 'Patterns and best practices for job executors using Humus'
applyTo: '**/*.go'
---

# Humus Framework - Job Instructions

This file provides patterns and best practices specific to one-off job executors using Humus. Use this file alongside `humus-common.instructions.md` for complete guidance.

## Project Structure

```
my-job/
├── main.go
├── config.yaml
├── app/
│   └── app.go          # Init function
├── processor/
│   └── processor.go    # Job logic
├── go.mod
└── go.sum
```

## Job Service Patterns

### Entry Point

**main.go:**
```go
package main

import (
    "bytes"
    _ "embed"
    "github.com/z5labs/humus/job"
    "my-job/app"
)

//go:embed config.yaml
var configBytes []byte

func main() {
    job.Run(bytes.NewReader(configBytes), app.Init)
}
```

### Init Function

**app/app.go:**
```go
package app

import (
    "context"
    "my-job/processor"
    "github.com/z5labs/humus"
    "github.com/z5labs/humus/job"
)

type Config struct {
    humus.Config `config:",squash"`
    
    // Job-specific configuration
    Database struct {
        URL string `config:"url"`
    } `config:"database"`
}

func Init(ctx context.Context, cfg Config) (job.Handler, error) {
    proc := processor.New(cfg.Database.URL)
    return proc, nil
}
```

### Job Handler Implementation

**processor/processor.go:**
```go
package processor

import (
    "context"
    "database/sql"
)

type Processor struct {
    db *sql.DB
}

func New(dbURL string) (*Processor, error) {
    db, err := sql.Open("postgres", dbURL)
    if err != nil {
        return nil, err
    }
    
    return &Processor{db: db}, nil
}

func (p *Processor) Handle(ctx context.Context) error {
    // Perform the job logic
    rows, err := p.db.QueryContext(ctx, "SELECT id, name FROM users WHERE active = false")
    if err != nil {
        return err
    }
    defer rows.Close()
    
    // Process rows...
    for rows.Next() {
        var id, name string
        if err := rows.Scan(&id, &name); err != nil {
            return err
        }
        // Do something with the data
    }
    
    return rows.Err()
}
```

## Job-Specific Best Practices

### DO ✅

1. **Implement job.Handler interface** - just one method: `Handle(ctx context.Context) error`
2. **Use context for cancellation** - respect context cancellation signals
3. **Return errors for failures** - don't panic, return errors
4. **Use lifecycle hooks for cleanup** - close database connections, files, etc.
5. **Keep jobs idempotent when possible** - safe to re-run

### DON'T ❌

1. **Don't run jobs in infinite loops** - jobs should execute once and exit
2. **Don't ignore context cancellation** - check ctx.Done() for long-running operations
3. **Don't panic** - return errors instead
4. **Don't leave resources open** - use lifecycle hooks or defer for cleanup
5. **Don't hardcode configuration** - use the Config struct

## Job with Resource Cleanup

```go
func Init(ctx context.Context, cfg Config) (job.Handler, error) {
    db, err := sql.Open("postgres", cfg.Database.URL)
    if err != nil {
        return nil, err
    }
    
    // Register cleanup
    lc, _ := lifecycle.FromContext(ctx)
    lc.OnPostRun(lifecycle.HookFunc(func(ctx context.Context) error {
        return db.Close()
    }))
    
    proc := processor.New(db)
    return proc, nil
}
```

## Long-Running Jobs with Context

```go
func (p *Processor) Handle(ctx context.Context) error {
    items, err := p.fetchItems(ctx)
    if err != nil {
        return err
    }
    
    for _, item := range items {
        // Check for cancellation
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
        
        if err := p.processItem(ctx, item); err != nil {
            return err
        }
    }
    
    return nil
}
```

## Example Use Cases

- **Data migration**: One-time database migrations or data transformations
- **Batch processing**: Process a batch of records and exit
- **Cleanup tasks**: Delete old data, archive records, etc.
- **Report generation**: Generate and send reports
- **Scheduled tasks**: Run via cron or scheduler (one execution per invocation)

## Additional Resources

- **Job Documentation**: https://z5labs.dev/humus/features/job/
- **Common patterns**: See `humus-common.instructions.md`
