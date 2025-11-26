---
title: Building a Basic Job
description: Create a minimal "hello world" job to verify setup
weight: 2
type: docs
---

Let's create the simplest possible job to verify everything works.

## Minimal Configuration

Create `config.yaml`:

```yaml
# Empty config for now - we'll add settings as we need them
```

That's it! No configuration needed yet.

## Simple Handler

Create `onebrc/handler.go`:

```go
package onebrc

import (
	"context"
	"log/slog"
	"os"
)

type Handler struct {
	log *slog.Logger
}

func NewHandler() *Handler {
	return &Handler{
		log: slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
}

func (h *Handler) Handle(ctx context.Context) error {
	h.log.InfoContext(ctx, "Hello from 1BRC job!")
	h.log.InfoContext(ctx, "Job completed successfully")
	return nil
}
```

**Key points:**
- Implements the `job.Handler` interface with a single `Handle(context.Context) error` method
- Uses structured logging with `slog`
- Returns `nil` to indicate success

## Application Initialization

Create `app/app.go`:

```go
package app

import (
	"context"

	"1brc-walkthrough/onebrc"
	"github.com/z5labs/humus/job"
)

type Config struct {
	// Empty for now - we'll add fields as needed
}

func Init(ctx context.Context, cfg Config) (*job.App, error) {
	handler := onebrc.NewHandler()
	return job.NewApp(handler), nil
}
```

**Responsibilities:**
- Define your config structure (empty for now)
- Create and wire up dependencies
- Return a `*job.App` with your handler

## Entry Point

Create `main.go`:

```go
package main

import (
	"bytes"
	_ "embed"

	"1brc-walkthrough/app"
	"github.com/z5labs/humus/job"
)

//go:embed config.yaml
var configBytes []byte

func main() {
	job.Run(bytes.NewReader(configBytes), app.Init)
}
```

**How it works:**
- `//go:embed` embeds config.yaml at compile time
- `job.Run()` parses config, calls `Init`, runs the handler, and handles graceful shutdown

## Run It

```bash
go mod tidy
go run .
```

You should see output like:

```json
{"time":"2024-11-23T22:50:00Z","level":"INFO","msg":"Hello from 1BRC job!"}
{"time":"2024-11-23T22:50:00Z","level":"INFO","msg":"Job completed successfully"}
```

The job runs, logs messages, and exits cleanly. Press Ctrl+C if you want to test graceful shutdown (though it exits immediately anyway).

## Understanding job.Run

When you call `job.Run(configReader, initFunc)`:

1. **Parse config** - Reads YAML and unmarshals into your `Config` struct
2. **Call Init** - Invokes your initialization function with the parsed config
3. **Wrap handler** - Adds middleware for panic recovery and OS signal handling
4. **Execute** - Calls `handler.Handle(ctx)`
5. **Graceful shutdown** - Ensures clean exit

## What's Next

Now that we have a working job, let's implement the core 1BRC algorithm with local file I/O.

[Next: 1BRC Algorithm →]({{< ref "03-1brc-algorithm" >}})
