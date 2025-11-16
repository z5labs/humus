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
    "github.com/z5labs/humus/job"
)

type Config struct {
    humus.Config `config:",squash"`

    Database struct {
        Host string `config:"host"`
        Name string `config:"name"`
    } `config:"database"`
}

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
    job.Run(job.YamlSource("config.yaml"), Init)
}

func Init(ctx context.Context, cfg Config) (job.Handler, error) {
    return &MigrationJob{
        dbHost: cfg.Database.Host,
        dbName: cfg.Database.Name,
    }, nil
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

1. **Configuration Loading** - Config file is parsed
2. **Initialization** - `Init` function creates the handler
3. **Execution** - `Handle` method is called
4. **Exit**:
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

Minimal config for jobs:

```yaml
otel:
  service:
    name: my-job
  sdk:
    disabled: false  # Enable for production

# Add your job-specific config
database:
  host: localhost
  port: 5432
```

## What You'll Learn

This section covers:

- [Quick Start]({{< ref "quick-start" >}}) - Build your first job
- [Job Handler]({{< ref "job-handler" >}}) - Implementing the interface
- [Use Cases]({{< ref "use-cases" >}}) - Common patterns and examples

## Next Steps

Start with the [Quick Start Guide]({{< ref "quick-start" >}}) to build your first job service.