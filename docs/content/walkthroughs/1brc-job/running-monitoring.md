---
title: Running and Monitoring
description: Execute the job and view telemetry in Grafana
weight: 7
type: docs
---

## Generate Test Data

First, generate temperature measurements:

```bash
cd example/job/1brc-walkthrough/tool

# Generate 1M rows (quick test)
go run . -count 1000000

# Or generate 1B rows (full challenge, takes several minutes)
go run . -count 1000000000
```

The tool will:
1. Fetch weather stations from the 1BRC repository
2. Generate data with concurrent workers
3. Upload directly to MinIO

## Run the Job

```bash
cd example/job/1brc-walkthrough
go run .
```

You should see logs like:
```
{"time":"...","level":"INFO","msg":"starting 1BRC processing","bucket":"onebrc","input_key":"measurements.txt"}
{"time":"...","level":"INFO","msg":"1BRC processing completed successfully","cities_processed":413}
```

## View Results in MinIO

1. Open http://localhost:9001
2. Login with minioadmin/minioadmin
3. Browse the `onebrc` bucket
4. Download `results.txt`

Expected format:
```
Abha=-23.0/18.0/59.2
Abidjan=-16.2/26.0/67.3
...
```

## View Telemetry in Grafana

### Accessing Grafana

Open http://localhost:3000 (anonymous admin access)

### View Traces

1. Click **Explore** (compass icon)
2. Select **Tempo** datasource
3. Click **Search**
4. Filter by:
   - Service Name: `1brc-job-walkthrough`
   - Span Name: `handle`
5. Click a trace to see the waterfall view

**What you'll see:**
```
handle (2m45s)
├── parse (2m30s)
├── calculate (10s)
└── write_results (5s)
```

### View Metrics

1. **Explore** → Select **Mimir**
2. Query: `onebrc_cities_count` (the metric is defined as `onebrc.cities.count` but exporters convert dots to underscores)
3. You should see a datapoint showing 413 cities processed

### View Logs

1. **Explore** → Select **Loki**
2. Query: `{service_name="1brc-job-walkthrough"}`
3. See structured logs with trace IDs

**Correlation:**
- Click a log line
- Click **Tempo** button next to `trace_id` field
- Jump directly to the correlated trace!

## Trace-to-Logs Navigation

In a trace span:
1. Click the **Logs** tab
2. See all logs emitted during that span
3. Filter by log level or message

## Understanding Span Duration

The trace shows:
- **Total duration:** End-to-end job execution
- **Parse time:** Reading and aggregating data
- **Calculate time:** Computing statistics and sorting
- **Write time:** Uploading results to S3

## Performance Analysis

Use traces to identify bottlenecks:
- Is parsing slow? Consider optimizing the loop
- Is writing slow? Check network or use buffering
- Are spans missing? Add more instrumentation

## Clean Up

Stop the infrastructure:
```bash
podman-compose down

# Or remove all data
podman-compose down -v
```

## What You Learned

- ✅ Building a production job with Humus
- ✅ Config embedding and YAML templates
- ✅ Streaming large files with MinIO
- ✅ Adding OpenTelemetry traces, metrics, logs
- ✅ Viewing distributed traces in Grafana
- ✅ Trace-to-logs correlation

## Next Steps

Explore more Humus features:
- [REST Services]({{< ref "/features/rest" >}})
- [gRPC Services]({{< ref "/features/grpc" >}})
- [Queue Processing]({{< ref "/features/queue" >}})
