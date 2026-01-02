---
title: Job Services
description: Building batch processors and one-off tasks
weight: 30
type: docs
---


Humus Job services provide a framework for building one-off executors, batch processors, database migrations, and scheduled tasks with the same observability and lifecycle management as long-running services.

## Overview

Job services in Humus are built on:

- **Simple Handler Interface** - Single `Handle(context.Context) error` method
- **Full Observability** - Same OpenTelemetry support as REST/gRPC
- **Lifecycle Management** - Graceful shutdown and context cancellation
- **Exit Code Handling** - Proper success/failure signaling

## Quick Start

```go
package main

import (
    "context"
    "fmt"

    "github.com/z5labs/humus"
    "github.com/z5labs/humus/app"
    "github.com/z5labs/humus/config"
    "github.com/z5labs/humus/otel"
    
    "go.opentelemetry.io/otel/metric"
    "go.opentelemetry.io/otel/trace"
)

type MigrationJob struct {
    dbHost string
    dbName string
}

func (j *MigrationJob) Handle(ctx context.Context) error {
    log := humus.Logger("migration")

    log.InfoContext(ctx, "starting migration",
        "host", j.dbHost,
        "database", j.dbName,
    )

    // Run your migration logic
    if err := runMigrations(ctx, j.dbHost, j.dbName); err != nil {
        log.ErrorContext(ctx, "migration failed", "error", err)
        return err
    }

    log.InfoContext(ctx, "migration completed successfully")
    return nil
}

func main() {
    // Read configuration from environment
    dbHost := config.MustOr(context.Background(), "localhost", config.Env("DB_HOST"))
    dbName := config.MustOr(context.Background(), "mydb", config.Env("DB_NAME"))

    // Create job handler
    handler := &MigrationJob{
        dbHost: dbHost,
        dbName: dbName,
    }

    // Build job application
    jobBuilder := app.BuilderFunc[app.JobApp](func(ctx context.Context) (app.JobApp, error) {
        return app.JobApp{Handler: handler}, nil
    })

    // Configure OpenTelemetry (disabled for simplicity)
    sdk := otel.SDK{
        TracerProvider: config.ReaderFunc[trace.TracerProvider](func(ctx context.Context) (config.Value[trace.TracerProvider], error) {
            return config.Value[trace.TracerProvider]{}, nil
        }),
        MeterProvider: config.ReaderFunc[metric.MeterProvider](func(ctx context.Context) (config.Value[metric.MeterProvider], error) {
            return config.Value[metric.MeterProvider]{}, nil
        }),
    }

    otelBuilder := otel.Build(sdk, jobBuilder)

    _ = app.Run(context.Background(), otelBuilder)
}

func runMigrations(ctx context.Context, host, name string) error {
    // Your migration logic here
    return nil
}
```

## The Handler Interface

```go
type Handler interface {
    Handle(context.Context) error
}
```

That's it! Just implement one method.

## Lifecycle

Jobs have a simple lifecycle:

1. **Building** - Application builder creates the job handler
2. **Execution** - `Handle` method is called with context
3. **Exit**:
   - Returns `nil` → Exit code 0 (success)
   - Returns `error` → Exit code 1 (failure)
   - Receives SIGTERM/SIGINT → Context cancelled, job should return

## Use Cases

### Database Migrations

```go
type MigrationJob struct {
    db *sql.DB
}

func (j *MigrationJob) Handle(ctx context.Context) error {
    migrations := []string{
        "CREATE TABLE users...",
        "CREATE INDEX idx_users_email...",
    }

    for i, migration := range migrations {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            log.InfoContext(ctx, "running migration", "step", i+1)
            if _, err := j.db.ExecContext(ctx, migration); err != nil {
                return fmt.Errorf("migration %d failed: %w", i+1, err)
            }
        }
    }

    return nil
}
```

### Batch Processing

```go
type BatchProcessor struct {
    source Source
    dest   Destination
}

func (j *BatchProcessor) Handle(ctx context.Context) error {
    items, err := j.source.FetchAll(ctx)
    if err != nil {
        return err
    }

    for i, item := range items {
        select {
        case <-ctx.Done():
            log.InfoContext(ctx, "shutdown requested", "processed", i)
            return ctx.Err()
        default:
            if err := j.dest.Write(ctx, item); err != nil {
                return fmt.Errorf("failed at item %d: %w", i, err)
            }
        }
    }

    return nil
}
```

### Data Import

```go
type ImportJob struct {
    filePath string
    db       *Database
}

func (j *ImportJob) Handle(ctx context.Context) error {
    file, err := os.Open(j.filePath)
    if err != nil {
        return err
    }
    defer file.Close()

    scanner := bufio.NewScanner(file)
    lineNum := 0

    for scanner.Scan() {
        lineNum++

        select {
        case <-ctx.Done():
            log.InfoContext(ctx, "import cancelled", "lines_processed", lineNum)
            return ctx.Err()
        default:
            if err := j.db.Insert(ctx, scanner.Text()); err != nil {
                return fmt.Errorf("line %d: %w", lineNum, err)
            }
        }
    }

    return scanner.Err()
}
```

### Scheduled Task (with external scheduler)

Jobs are designed to run once. Use an external scheduler (cron, Kubernetes CronJob, etc.) to run them periodically:

```yaml
# Kubernetes CronJob example
apiVersion: batch/v1
kind: CronJob
metadata:
  name: daily-report
spec:
  schedule: "0 2 * * *"  # 2 AM daily
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: report-job
            image: my-report-job:latest
          restartPolicy: OnFailure
```

## Context Handling

Always respect context cancellation:

```go
func (j *MyJob) Handle(ctx context.Context) error {
    for i := 0; i < 1000; i++ {
        // Check for shutdown before each iteration
        select {
        case <-ctx.Done():
            log.InfoContext(ctx, "graceful shutdown", "progress", i)
            return ctx.Err()
        default:
            processItem(ctx, i)
        }
    }
    return nil
}
```

## Error Handling

Return errors for failures:

```go
func (j *MyJob) Handle(ctx context.Context) error {
    if err := validateInput(); err != nil {
        return fmt.Errorf("validation failed: %w", err)
    }

    if err := processData(ctx); err != nil {
        return fmt.Errorf("processing failed: %w", err)
    }

    return nil  // Success
}
```

The job framework will:
- Log the error
- Exit with code 1
- Ensure proper cleanup

## Configuration

Jobs read configuration from environment variables:

```go
func main() {
    // Read config from environment
    dbURL := config.MustOr(context.Background(), 
        "postgres://localhost/mydb", 
        config.Env("DATABASE_URL"),
    )
    
    batchSize := config.Map(
        config.Default("100", config.Env("BATCH_SIZE")),
        func(ctx context.Context, s string) (int, error) {
            return strconv.Atoi(s)
        },
    )
    
    // Use config to build your job...
}
```

**Environment Variables:**
- `OTEL_SERVICE_NAME` - Service name for telemetry
- `OTEL_TRACES_SAMPLER_RATIO` - Trace sampling (0.0-1.0)
- Add your job-specific variables

## What You'll Learn

This section covers:

- [Quick Start]({{< ref "quick-start" >}}) - Build your first job
- [Job Handler]({{< ref "job-handler" >}}) - Implementing the interface
- [Use Cases]({{< ref "use-cases" >}}) - Common patterns and examples

## Next Steps

Start with the [Quick Start Guide]({{< ref "quick-start" >}}) to build your first job service.