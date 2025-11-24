---
title: Comprehensive Testing
description: Complete end-to-end testing scenarios with full observability
weight: 9
type: docs
slug: comprehensive-testing
---

Now that you have the full observability stack running, let's run comprehensive end-to-end testing scenarios to validate the complete implementation and explore the telemetry data.

## Prerequisites

Before running these tests, ensure:

1. **All containers are running** (from the Infrastructure Setup step):

```bash
podman ps --format "table {{.Names}}\t{{.Status}}"
```

You should see 6 running containers: wiremock, tempo, loki, mimir, otel-collector, and grafana.

2. **API is running** with observability enabled:

```bash
go run .
```

The API should connect to the OTel Collector at localhost:4317.

## Test Scenarios

### Scenario 1: List Orders with Pagination

```bash
# First page
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001" | jq .
```

Expected: Orders list with `has_next_page: true` and `end_cursor`.

```bash
# Next page using cursor
CURSOR=$(curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001" | jq -r '.page_info.end_cursor')
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001&after=$CURSOR" | jq .
```

### Scenario 2: Filter by Status

```bash
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001&status=completed" | jq .
```

Expected: Only orders with status "completed".

### Scenario 3: Successful Order Placement

```bash
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-001"}' | jq .
```

Expected:
```json
{
  "order_id": "uuid-here"
}
```

### Scenario 4: Order Blocked by Restrictions

```bash
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-FRAUD"}'
```

Expected: Error (ACC-FRAUD has fraud restrictions).

### Scenario 5: Order Blocked by Eligibility

```bash
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-INELIGIBLE"}'
```

Expected: Error (ACC-INELIGIBLE is ineligible).

## Observability Validation

After running the test scenarios above, validate that telemetry is being captured:

### Traces in Tempo

1. **Open Grafana**: http://localhost:3000
2. **Go to Explore** (compass icon)
3. **Select Tempo** as data source
4. **Search for recent traces**

You should see:
- Traces for each API request
- Distributed trace spans showing:
  - Main HTTP request span
  - Child spans for service calls (Restriction, Eligibility, Data)
  - Timing information for each span
  - HTTP status codes and attributes

### Logs in Loki

1. **In Grafana, go to Explore**
2. **Select Loki** as data source
3. **Query**: `{job="orders-api"}`

You should see:
- Application logs correlated with trace IDs
- Request/response logs
- Error logs (for failed scenarios)

### Metrics in Mimir

1. **In Grafana, go to Explore**
2. **Select Mimir** as data source
3. **Query examples**:
   - `http_server_request_duration_seconds_bucket` - Request latency distribution
   - `http_server_request_total` - Total request count by status code

You should see:
- HTTP request metrics by endpoint
- Request duration histograms
- Error rates

## Complete Validation Checklist

### Code & Build
- [ ] All code builds: `go build ./...`
- [ ] No lint errors: `go vet ./...`

### Infrastructure
- [ ] All 6 containers running: `podman ps`
- [ ] Wiremock accessible: http://localhost:8080
- [ ] Grafana accessible: http://localhost:3000

### API Functionality
- [ ] API starts without errors
- [ ] Health checks respond: `/health/liveness`, `/health/readiness`
- [ ] OpenAPI spec available: `/openapi.json`

### Endpoint Testing
- [ ] GET /v1/orders returns paginated results
- [ ] Pagination cursors work correctly
- [ ] Status filtering works
- [ ] POST /v1/order creates orders successfully
- [ ] Restrictions block orders correctly
- [ ] Eligibility blocks ineligible accounts

### Observability
- [ ] Traces appear in Tempo for all requests
- [ ] Distributed traces show service call hierarchy
- [ ] Logs appear in Loki with trace IDs
- [ ] Metrics appear in Mimir
- [ ] HTTP request metrics show correct counts
- [ ] Latency histograms are populated

## Cleanup

To stop everything:

```bash
# Stop the API (Ctrl+C in terminal)

# Stop infrastructure
podman-compose down --volumes
```

## Congratulations!

You've built a production-ready REST API with:

- **Two endpoints** with different HTTP methods
- **Service orchestration** with proper error handling
- **Cursor-based pagination** for scalability
- **Full observability** with zero manual instrumentation
- **Auto-generated OpenAPI** documentation

## Next Steps

- Add unit tests for handlers and services
- Implement custom error responses
- Add authentication (JWT)
- Deploy to Kubernetes with OTel Collector sidecar
- Connect to real backend services instead of Wiremock

[Back to Walkthrough Overview →]({{< ref "../orders-rest" >}})
