---
title: Running and Monitoring
description: Execute the instrumented job and view telemetry in Grafana
weight: 8
type: docs
---

Now let's run your instrumented job and explore the telemetry in Grafana!

## Run the Instrumented Job

Make sure the infrastructure is still running:

```bash
podman-compose ps
```

All 6 services should be `Up`. Now run your job:

```bash
go run .
```

You should now see structured logs with trace IDs:

```json
{"time":"...","level":"INFO","msg":"starting 1BRC processing","trace_id":"abc123...","bucket":"onebrc","input_key":"measurements.txt"}
{"time":"...","level":"INFO","msg":"1BRC processing completed successfully","trace_id":"abc123...","cities_processed":15}
```

Notice the `trace_id` field - this links logs to traces!

## View Results in MinIO

1. Open http://localhost:9001
2. Login with minioadmin/minioadmin
3. Browse the `onebrc` bucket
4. Download `results.txt`

Expected format:

```
Abha=-23.0/18.0/59.2
Beijing=-19.5/16.3/49.8
Cairo=-18.2/17.9/48.5
...
```

## Explore Telemetry in Grafana

### Accessing Grafana

Open http://localhost:3000 (anonymous admin access, no login required)

### View Traces

1. Click **Explore** (compass icon in left sidebar)
2. Select **Tempo** datasource (dropdown at top)
3. Click **Search** tab
4. Filter by:
   - **Service Name:** `1brc-job-walkthrough`
   - **Span Name:** `handle`
5. Click **Run Query**
6. Click on a trace to see the waterfall view

**What you'll see:**

```
handle (2.5s total)
├── parse (2.3s)
├── calculate (0.15s)
└── write_results (0.05s)
```

This shows exactly where time is spent:
- Most time in parsing (reading/aggregating data)
- Quick calculation (sorting and formatting)
- Fast write (uploading to S3)

### View Metrics

1. **Explore** → Select **Mimir** datasource
2. In the query builder, enter: `onebrc_cities_count`
   - Note: The metric is defined as `onebrc.cities.count` but exporters convert dots to underscores
3. Click **Run Query**

You should see a datapoint showing how many cities were processed (e.g., 15 for the test dataset).

**Add dimensions:**
```promql
onebrc_cities_count{bucket="onebrc"}
```

This shows the metric filtered by bucket name.

### View Logs

1. **Explore** → Select **Loki** datasource
2. Query: `{service_name="1brc-job-walkthrough"}`
3. See all structured logs from your job

**Filter by log level:**
```logql
{service_name="1brc-job-walkthrough"} | json | level="INFO"
```

**Search for specific messages:**
```logql
{service_name="1brc-job-walkthrough"} |= "completed successfully"
```

### Trace-to-Logs Correlation

This is the killer feature of integrated observability:

1. In a trace span, click the **Logs** tab
2. See all logs emitted during that span
3. Automatically filtered by trace ID!

Or go the other way:

1. In Loki logs, find a log line
2. Click the **Tempo** button next to the `trace_id` field
3. Jump directly to the correlated trace!

This bi-directional linking makes debugging much easier - you can see the full context around any log message.

## Run With Larger Dataset

Try processing more data:

```bash
cd tool
go run . -count 1000000  # 1 million measurements
cd ..
go run .
```

Now check the traces again:
- Parse time will be significantly longer
- You can see the performance impact clearly
- Metrics show the larger city count

## Understanding Span Duration

The trace waterfall shows:
- **Total duration:** End-to-end job execution
- **Parse time:** Reading and aggregating line-by-line
- **Calculate time:** Computing statistics and sorting cities alphabetically
- **Write time:** Uploading formatted results to S3

## Performance Analysis

Use traces to identify bottlenecks:

**Is parsing slow?**
- Consider optimizing the parsing loop
- Try concurrent parsing with goroutines
- Use faster parsing libraries

**Is writing slow?**
- Check network latency to MinIO
- Consider compression
- Use buffered writes

**Are spans missing?**
- Add more instrumentation points
- Instrument the parser at a finer granularity
- Add spans for bucket operations

## Clean Up

When you're done exploring:

```bash
podman-compose down

# Or remove all data including volumes
podman-compose down -v
```

## What You Learned

- ✅ Building a production job with Humus
- ✅ Starting with minimal setup before adding observability
- ✅ Config embedding and YAML templates
- ✅ Streaming large files with MinIO
- ✅ Retrofitting OpenTelemetry traces, metrics, logs
- ✅ Viewing distributed traces in Grafana
- ✅ Trace-to-logs correlation for debugging
- ✅ Using metrics to track business KPIs

## Key Takeaways

**Development Flow:**
1. Build and test with minimal infrastructure first
2. Verify business logic works correctly
3. Add observability stack later
4. Instrument code progressively

**Observability Value:**
- Traces show where time is spent (performance)
- Metrics track business outcomes (cities processed)
- Logs provide detailed context (errors, events)
- Correlation enables fast debugging

## Next Steps

Explore more Humus features:
- [REST Services]({{< ref "/features/rest" >}}) - Build HTTP APIs with OpenAPI
- [gRPC Services]({{< ref "/features/grpc" >}}) - Build gRPC microservices
- [Queue Processing]({{< ref "/features/queue" >}}) - Process Kafka messages
- [Advanced OTel Integration]({{< ref "/advanced/otel-integration" >}}) - Custom instrumentation patterns
