---
title: Project Setup
description: Create the directory structure and understand the project layout
weight: 1
type: docs
---

Let's start by creating the project structure for our 1BRC job.

## Directory Structure

Create the following directory structure:

```bash
mkdir -p 1brc-walkthrough/{app,service,onebrc,tool}
cd 1brc-walkthrough
```

The final structure will be:

```
1brc-walkthrough/
├── main.go                  # Entry point
├── config.yaml              # Configuration
├── go.mod                   # Module definition
├── app/
│   └── app.go              # Job initialization and config
├── onebrc/
│   ├── handler.go          # Job orchestration
│   ├── parser.go           # Parse "city;temp" format
│   └── calculator.go       # Compute statistics
└── tool/
    └── main.go             # Generate test data
```

## Initialize Go Module

Create `go.mod`:

```go
module 1brc-walkthrough

go 1.24.0

require github.com/z5labs/humus v0.20.2
```

## Package Organization

Each package has a specific responsibility:

- **app/** - Job initialization and configuration
- **onebrc/** - Core business logic: orchestration, parsing, and calculation
- **tool/** - Standalone utility to generate test data

We'll start simple with local file I/O, then add cloud storage integration later.

## What's Next

In the next section, we'll build a basic "hello world" job to verify everything works.

[Next: Building a Basic Job →]({{< ref "02-basic-job" >}})
