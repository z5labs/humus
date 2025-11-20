# 1 Billion Row Challenge - Humus Job Example

This example demonstrates building a job application with Humus to solve the [1 Billion Row Challenge](https://github.com/gunnarmorling/1brc).

## Overview

The job:
1. Reads temperature measurements from MinIO (S3-compatible storage)
2. Parses data in `city;temperature` format
3. Calculates min/mean/max statistics per city
4. Writes formatted results back to MinIO
5. Exports traces, metrics, and logs via OpenTelemetry

## Quick Start

### 1. Start Infrastructure

```bash
podman-compose up -d
```

This starts:
- MinIO (object storage) on port 9000
- OpenTelemetry Collector on port 4317
- Grafana (visualization) on port 3000
- Tempo (traces backend)
- Mimir (metrics backend)
- Loki (logs backend)

### 2. Generate Test Data

```bash
cd tool
go run . -count 1000000
```

This generates measurement data using the real weather stations from the [1BRC repository](https://github.com/gunnarmorling/1brc/blob/main/data/weather_stations.csv) (~413 stations). Temperature values are generated with Gaussian distribution around each station's average temperature.

**For the full 1 billion row challenge:** Use `-count 1000000000` (generates ~13GB file, takes several minutes)

Options:
- `-count`: Number of measurements (default: 1000000000 - 1 billion rows)
- `-workers`: Number of concurrent workers (default: number of CPU cores)
- `-endpoint`: MinIO endpoint (default: localhost:9000)
- `-bucket`: Bucket name (default: onebrc)
- `-key`: Object key (default: measurements.txt)
- `-output`: Generate to file instead of MinIO

### 3. Run the Job

```bash
cd ..
go run .
```

The job will:
- Fetch `measurements.txt` from MinIO
- Process the data
- Upload `results.txt` with statistics

### 4. View Results

**MinIO Console:** http://localhost:9001 (minioadmin/minioadmin)
- Download `results.txt` from the `onebrc` bucket

**Grafana:** http://localhost:3000 (admin/admin)
- **Traces:** Explore → Tempo → Search for service "1brc-job-walkthrough"
- **Metrics:** Explore → Mimir → Query `onebrc_cities_count`
- **Logs:** Explore → Loki → `{service_name="1brc-job-walkthrough"}`

## Configuration

Environment variables (see `config.yaml`):
- `MINIO_ENDPOINT`: MinIO endpoint (default: localhost:9000)
- `MINIO_ACCESS_KEY`: Access key (default: minioadmin)
- `MINIO_SECRET_KEY`: Secret key (default: minioadmin)
- `MINIO_BUCKET`: Bucket name (default: onebrc)
- `ONEBRC_INPUT_KEY`: Input object key (default: measurements.txt)
- `ONEBRC_OUTPUT_KEY`: Output object key (default: results.txt)
- `OTEL_OTLP_TARGET`: OTLP endpoint (default: localhost:4317)
- `OTEL_TRACE_EXPORTER`: Trace exporter type (default: otlp)
- `OTEL_METRIC_EXPORTER`: Metric exporter type (default: otlp)
- `OTEL_LOG_EXPORTER`: Log exporter type (default: otlp)

## Architecture

```
main.go
  └── app/app.go (Config, Init)
      ├── storage/minio.go (S3 client)
      └── onebrc/
          ├── handler.go (orchestration + OTel)
          ├── parser.go (parse city;temp lines)
          └── calculator.go (compute statistics)
```

## Key Features

- **Automatic OTel Integration**: Traces, metrics, logs without manual SDK setup
- **Graceful Shutdown**: Handles OS signals, flushes telemetry
- **Config Templates**: YAML with Go template functions (`env`, `default`)
- **Structured Logging**: Correlated with trace spans
- **Custom Metrics**: `onebrc.cities.count` tracks processed cities

## Output Format

```
Abha=-23.0/18.0/59.2
Abidjan=-16.2/26.0/67.3
Abéché=-10.0/29.4/69.9
Accra=-10.1/26.4/66.4
...
```

Format: One city per line as `city=min/mean/max` (sorted alphabetically)

Uses IEEE 754 "roundTowardPositive" rounding to 1 decimal place.
