---
title: Running & Testing
description: Complete end-to-end testing scenarios
weight: 9
type: docs
slug: running-testing
---

Let's run through comprehensive testing scenarios to validate the complete implementation.

## Starting the Stack

1. **Start infrastructure:**

```bash
cd example/rest/orders-walkthrough
podman-compose up -d
```

2. **Verify all containers:**

```bash
podman ps --format "table {{.Names}}\t{{.Status}}"
```

You should see 6 running containers.

3. **Start the API:**

```bash
go run .
```

The API starts on port 8090.

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

## OpenAPI Validation

Check the auto-generated OpenAPI specification:

```bash
curl -s http://localhost:8090/openapi.json | jq '.info'
```

Verify endpoints:

```bash
curl -s http://localhost:8090/openapi.json | jq '.paths | keys'
```

Expected: `["/v1/order", "/v1/orders"]`

## Health Checks

```bash
# Liveness probe
curl -s http://localhost:8090/health/liveness

# Readiness probe
curl -s http://localhost:8090/health/readiness
```

## Observability Validation

After running test scenarios:

1. **Open Grafana**: http://localhost:3000
2. **Check Tempo**: Should see traces for all API calls
3. **Check Loki**: Should see correlated logs
4. **Check Mimir**: Should see HTTP metrics

## Validation Checklist

- [ ] All code builds: `go build ./...`
- [ ] Infrastructure starts: `podman-compose up -d`
- [ ] API starts without errors
- [ ] GET /v1/orders returns paginated results
- [ ] POST /v1/order creates orders
- [ ] Restrictions block orders correctly
- [ ] Eligibility blocks orders correctly
- [ ] Traces appear in Tempo
- [ ] Logs appear in Loki
- [ ] Metrics appear in Mimir
- [ ] OpenAPI spec is correct

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
