---
title: Walkthroughs
description: Step-by-step guides for building production-ready applications
weight: 10
type: docs
---

Complete, hands-on tutorials that guide you through building real-world applications with Humus.

## Available Walkthroughs

### [Orders REST API]({{< ref "orders-rest" >}})

Build a production-ready REST API with service orchestration, cursor-based pagination, and full observability.

**What You'll Build:**
- GET /v1/orders endpoint with pagination
- POST /v1/order endpoint with service orchestration (Restriction → Eligibility → Data)
- Complete observability stack (Tempo, Loki, Mimir, Grafana)
- Mock backend services with Wiremock

**Time Estimate:** 45-60 minutes

**Prerequisites:**
- Go 1.24+
- Podman or Docker
- Basic familiarity with REST APIs

[Start the Orders REST Walkthrough →]({{< ref "orders-rest" >}})

---

## About Walkthroughs

Each walkthrough is designed to:
- **Be Complete** - Every step results in runnable code
- **Follow Best Practices** - Use production-ready patterns
- **Include Observability** - Full OpenTelemetry integration from the start
- **Match Code to Docs** - Source code and documentation are always in sync

The source code for each walkthrough is available in the `example/` directory of the repository.
