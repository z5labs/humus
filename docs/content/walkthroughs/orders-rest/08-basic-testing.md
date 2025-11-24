---
title: Basic Testing
description: Test your API endpoints with Wiremock
weight: 7
type: docs
slug: basic-testing
---

Great! You've implemented both API endpoints. Now let's test them with a real backend service using Wiremock.

## Creating Wiremock Stubs

Wiremock will mock all three backend services (data, restriction, and eligibility) using JSON stub files.

Create the directory structure:

```bash
cd example/rest/orders-walkthrough
mkdir -p wiremock/mappings
```

### Data Service Stubs

Create `wiremock/mappings/data-service.json`:

```json
{
  "mappings": [
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/data/orders",
        "queryParameters": {
          "accountNumber": {
            "equalTo": "ACC-001"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "orders": [
            {
              "order_id": "order-001",
              "account_id": "ACC-001",
              "customer_id": "CUST-001",
              "status": "completed"
            },
            {
              "order_id": "order-002",
              "account_id": "ACC-001",
              "customer_id": "CUST-001",
              "status": "pending"
            }
          ],
          "page_info": {
            "has_next_page": true,
            "end_cursor": "b3JkZXItMDAy"
          }
        }
      }
    },
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/data/orders",
        "queryParameters": {
          "accountNumber": {
            "equalTo": "ACC-001"
          },
          "after": {
            "equalTo": "b3JkZXItMDAy"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "orders": [
            {
              "order_id": "order-003",
              "account_id": "ACC-001",
              "customer_id": "CUST-001",
              "status": "completed"
            }
          ],
          "page_info": {
            "has_next_page": false,
            "end_cursor": ""
          }
        }
      }
    },
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/data/orders",
        "queryParameters": {
          "accountNumber": {
            "equalTo": "ACC-001"
          },
          "status": {
            "equalTo": "completed"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "orders": [
            {
              "order_id": "order-001",
              "account_id": "ACC-001",
              "customer_id": "CUST-001",
              "status": "completed"
            }
          ],
          "page_info": {
            "has_next_page": false,
            "end_cursor": ""
          }
        }
      }
    },
    {
      "request": {
        "method": "POST",
        "urlPathPattern": "/data/order"
      },
      "response": {
        "status": 201,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "order_id": "649cfc69-8323-4c60-8745-c7071506943d"
        }
      }
    }
  ]
}
```

### Restriction Service Stubs

Create `wiremock/mappings/restriction-service.json`:

```json
{
  "mappings": [
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/restrictions",
        "queryParameters": {
          "accountId": {
            "equalTo": "ACC-001"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "restrictions": []
        }
      }
    },
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/restrictions",
        "queryParameters": {
          "accountId": {
            "equalTo": "ACC-FRAUD"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "restrictions": ["FRAUD"]
        }
      }
    },
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/restrictions",
        "queryParameters": {
          "accountId": {
            "equalTo": "ACC-INELIGIBLE"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "restrictions": []
        }
      }
    }
  ]
}
```

### Eligibility Service Stubs

Create `wiremock/mappings/eligibility-service.json`:

```json
{
  "mappings": [
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/eligibility",
        "queryParameters": {
          "accountId": {
            "equalTo": "ACC-001"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "eligible": true
        }
      }
    },
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/eligibility",
        "queryParameters": {
          "accountId": {
            "equalTo": "ACC-FRAUD"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "eligible": true
        }
      }
    },
    {
      "request": {
        "method": "GET",
        "urlPathPattern": "/eligibility",
        "queryParameters": {
          "accountId": {
            "equalTo": "ACC-INELIGIBLE"
          }
        }
      },
      "response": {
        "status": 200,
        "headers": {
          "Content-Type": "application/json"
        },
        "jsonBody": {
          "eligible": false
        }
      }
    }
  ]
}
```

## Starting Wiremock

Now that the stubs are configured, start Wiremock with the following setup.

Create a minimal `podman-compose.yaml` (or use the existing one):

```yaml
services:
  wiremock:
    image: docker.io/wiremock/wiremock:3.10.0
    ports:
      - "8080:8080"
    volumes:
      - ./wiremock:/home/wiremock:z
    command: --verbose
```

The `wiremock/` directory contains JSON stub files that define mock responses for all backend service endpoints.

Start Wiremock:

```bash
cd example/rest/orders-walkthrough
podman-compose up -d
```

Verify it's running:

```bash
podman ps
```

You should see the wiremock container running on port 8080.

## Starting the API

In a separate terminal:

```bash
cd example/rest/orders-walkthrough
go run .
```

The API starts on port 8090 and connects to Wiremock at http://localhost:8080.

## Testing GET /v1/orders

List orders for an account:

```bash
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001" | jq .
```

Expected response:

```json
{
  "orders": [
    {
      "order_id": "order-001",
      "account_id": "ACC-001",
      "customer_id": "CUST-001",
      "status": "completed"
    },
    {
      "order_id": "order-002",
      "account_id": "ACC-001",
      "customer_id": "CUST-001",
      "status": "pending"
    }
  ],
  "page_info": {
    "has_next_page": true,
    "end_cursor": "b3JkZXItMDAy"
  }
}
```

Test pagination using the cursor:

```bash
CURSOR=$(curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001" | jq -r '.page_info.end_cursor')
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001&after=$CURSOR" | jq .
```

Filter by status:

```bash
curl -s "http://localhost:8090/v1/orders?accountNumber=ACC-001&status=completed" | jq .
```

## Testing POST /v1/order

### Successful Order Placement

```bash
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-001"}' | jq .
```

Expected response:

```json
{
  "order_id": "649cfc69-8323-4c60-8745-c7071506943d"
}
```

### Order Blocked by Restrictions

Test with an account that has fraud restrictions:

```bash
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-FRAUD"}' | jq .
```

This returns an error because ACC-FRAUD has fraud restrictions in the Wiremock stub.

### Order Blocked by Eligibility

Test with an ineligible account:

```bash
curl -s -X POST http://localhost:8090/v1/order \
  -H "Content-Type: application/json" \
  -d '{"customer_id":"CUST-001","account_id":"ACC-INELIGIBLE"}' | jq .
```

This returns an error because ACC-INELIGIBLE is not eligible in the Wiremock stub.

## Verifying OpenAPI Schema

Check the auto-generated OpenAPI specification:

```bash
curl -s http://localhost:8090/openapi.json | jq '.paths | keys'
```

Expected output:

```json
[
  "/v1/order",
  "/v1/orders"
]
```

View the POST endpoint schema:

```bash
curl -s http://localhost:8090/openapi.json | jq '.paths["/v1/order"].post'
```

## Health Checks

Test the built-in health endpoints:

```bash
# Liveness probe
curl -s http://localhost:8090/health/liveness

# Readiness probe
curl -s http://localhost:8090/health/readiness
```

## Quick Validation Checklist

- [ ] Wiremock container is running
- [ ] API starts without errors
- [ ] GET /v1/orders returns paginated results
- [ ] Pagination cursor works correctly
- [ ] Status filtering works
- [ ] POST /v1/order creates orders successfully
- [ ] Restrictions block orders correctly
- [ ] Eligibility checks block ineligible accounts
- [ ] OpenAPI spec includes both endpoints
- [ ] Health checks respond

## Stopping Services

When you're done testing:

```bash
# Stop the API (Ctrl+C in terminal)

# Stop Wiremock
podman-compose down
```

## What's Next

Your API is working! Now let's add the full observability stack (Grafana, Tempo, Loki, Mimir) to see traces, logs, and metrics.

[Next: Infrastructure Setup →]({{< ref "/walkthroughs/orders-rest/09-infrastructure" >}})
