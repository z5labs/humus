---
title: Project Setup
description: Create the directory structure and initialize the Go module
weight: 1
type: docs
slug: project-setup
---

Let's start by creating the project structure for our orders REST API.

## Directory Structure

Create the following directory structure:

```bash
mkdir -p rest-orders-walkthrough/{app,endpoint,service,wiremock/mappings}
cd rest-orders-walkthrough
```

The final structure will be:

```
rest-orders-walkthrough/
├── main.go                    # Entry point
├── config.yaml                # Configuration with OTel settings
├── go.mod                     # Module definition
├── app/
│   └── app.go                 # API initialization and config
├── endpoint/
│   ├── model.go               # Domain types (Order, pagination)
│   ├── interfaces.go          # Service interfaces
│   ├── list_orders.go         # GET /v1/orders
│   └── place_order.go         # POST /v1/order
├── service/
│   ├── data.go                # Data service client
│   ├── restriction.go         # Restriction service client
│   └── eligibility.go         # Eligibility service client
├── podman-compose.yaml        # Infrastructure stack
└── wiremock/
    └── mappings/              # Wiremock stub definitions
```

## Initialize Go Module

Create `go.mod`:

```go
module rest-orders-walkthrough

go 1.24.0

require github.com/z5labs/humus v0.16.0
```

## Package Organization

Each package has a specific responsibility:

- **endpoint/** - Domain types, service interfaces, and REST endpoint handlers
- **service/** - Service HTTP client implementations (depend on endpoint types)
- **app/** - Application initialization and configuration

This follows idiomatic Go where **consumers define interfaces** - the endpoint package defines what it needs from services, and service implementations import endpoint types to satisfy those interfaces.

## What's Next

In the next section, we'll create a minimal running API to verify everything is set up correctly.

[Next: Minimal Running App →]({{< ref "/walkthroughs/orders-rest/02-minimal-app" >}})
