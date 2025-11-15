# Kafka At-Least-Once with Full Observability Stack

This example demonstrates a complete observability setup for the Humus kafka-at-least-once queue processor, showcasing the framework's built-in OpenTelemetry integration with industry-standard backends.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Application Layer                        │
├─────────────────────────────────────────────────────────────┤
│  kafka-at-least-once-app                                    │
│  - Consumes messages from Kafka                              │
│  - Processes with at-least-once semantics                    │
│  - Exports: Traces, Metrics, Logs via OTLP                  │
└─────────────────────────────────────────────────────────────┘
                           ↓ OTLP (gRPC:4317)
┌─────────────────────────────────────────────────────────────┐
│              OpenTelemetry Collector                         │
│  - Receives all telemetry (traces, metrics, logs)           │
│  - Batches and enriches data                                 │
│  - Routes to appropriate backends                            │
└─────────────────────────────────────────────────────────────┘
          ↓ Traces        ↓ Metrics        ↓ Logs
┌──────────────────┐ ┌──────────────────┐ ┌──────────────────┐
│  Grafana Tempo   │ │  Grafana Mimir   │ │  Grafana Loki    │
│  (Traces)        │ │  (Metrics)       │ │  (Logs)          │
│  :3200           │ │  :8080           │ │  :3100           │
└──────────────────┘ └──────────────────┘ └──────────────────┘
          ↑                  ↑                  ↑
          └──────────────────┴──────────────────┘
                             │
                    ┌────────────────┐
                    │    Grafana     │
                    │    :3000       │
                    │                │
                    │  • Trace UI    │
                    │  • Metrics     │
                    │  • Logs        │
                    │  • Correlation │
                    └────────────────┘
```

## Components

### Core Services

- **Kafka**: Message broker (Apache Kafka in KRaft mode)
- **Application**: Queue processor with at-least-once delivery semantics

### Observability Stack

- **OpenTelemetry Collector**: Central telemetry hub that receives OTLP data
- **Grafana Tempo**: Distributed tracing backend for storing and querying traces
- **Grafana Mimir**: Prometheus-compatible metrics storage
- **Grafana Loki**: Log aggregation system
- **Grafana**: Unified visualization interface with data source correlations

## Quick Start

### 1. Start the Observability Stack

```bash
# Start all services (Kafka + observability stack)
podman-compose up -d

# Verify all services are running
podman-compose ps
```

Expected output:
```
NAME                IMAGE                                              STATUS
grafana             docker.io/grafana/grafana:latest                   Up
kafka-at-least-once docker.io/apache/kafka:latest                      Up (healthy)
loki                docker.io/grafana/loki:latest                      Up
mimir               docker.io/grafana/mimir:latest                     Up
otel-collector      docker.io/otel/opentelemetry-collector-contrib:... Up
tempo               docker.io/grafana/tempo:latest                     Up
```

### 2. Access Grafana

Open your browser to: **http://localhost:3000**

The Grafana instance is pre-configured with:
- Anonymous access enabled (no login required)
- All data sources (Tempo, Mimir, Loki) auto-provisioned
- Trace-to-logs and trace-to-metrics correlations enabled

### 3. Prepare Kafka

Create the `orders` topic before running the application:

```bash
# Enter the Kafka container
podman exec -it kafka-at-least-once bash

# Create the topic with 3 partitions
kafka-topics.sh --create \
  --topic orders \
  --partitions 3 \
  --replication-factor 1 \
  --bootstrap-server localhost:9092

# Verify topic creation
kafka-topics.sh --list --bootstrap-server localhost:9092

# Exit the container
exit
```

### 4. Publish Test Messages

Use the Kafka console producer to publish some test messages:

```bash
# Start the console producer
podman exec -it kafka-at-least-once kafka-console-producer.sh \
  --topic orders \
  --bootstrap-server localhost:9092

# Enter messages (JSON format):
{"order_id":"order-1","amount":100.50,"product_id":"prod-123","quantity":2}
{"order_id":"order-2","amount":250.00,"product_id":"prod-456","quantity":5}
{"order_id":"order-3","amount":75.25,"product_id":"prod-789","quantity":1}

# Press Ctrl+C to exit
```

### 5. Run the Application with Observability

Set the required environment variables and run the application:

```bash
# Set OpenTelemetry configuration
export OTEL_SERVICE_NAME="kafka-at-least-once-example"
export OTEL_SERVICE_VERSION="1.0.0"
export OTEL_TRACE_EXPORTER="otlp"
export OTEL_METRIC_EXPORTER="otlp"
export OTEL_LOG_EXPORTER="otlp"
export OTEL_OTLP_TARGET="localhost:4317"
export OTEL_SAMPLING="1.0"  # 100% sampling for development

# Set Kafka configuration
export KAFKA_BROKERS="localhost:9092"
export KAFKA_TOPIC="orders"
export KAFKA_GROUP_ID="order-processor"

# Run the application
go run .
```

Alternatively, use the pre-configured `config-with-otel.yaml`:

```bash
# Export only the service name
export OTEL_SERVICE_NAME="kafka-at-least-once-example"

# Run with the enhanced config
go run . -config config-with-otel.yaml
```

## Exploring Observability Data

### Traces in Tempo

1. Open Grafana: http://localhost:3000
2. Go to **Explore** (compass icon in left sidebar)
3. Select **Tempo** from the data source dropdown
4. Choose **Search** tab
5. Set filters:
   - **Service Name**: `kafka-at-least-once-example`
   - **Span Name**: (leave empty to see all)
6. Click **Run query**

**What to look for:**
- End-to-end traces showing: Kafka consume → process → acknowledge
- Partition-level processing with goroutine-per-partition model
- Span attributes showing order details (order_id, amount, etc.)
- Error traces if processing fails

**Example TraceQL query:**
```traceql
{.service.name="kafka-at-least-once-example" && .kafka.topic="orders"}
```

### Metrics in Mimir

1. In Grafana **Explore**, select **Mimir** data source
2. Try these PromQL queries:

**Consumer lag (if franz-go exposes this):**
```promql
kafka_consumer_lag{service_name="kafka-at-least-once-example"}
```

**Request rate from trace metrics:**
```promql
rate(traces_spanmetrics_calls_total{service_name="kafka-at-least-once-example"}[5m])
```

**Go runtime metrics:**
```promql
# Goroutines
go_goroutines{service_name="kafka-at-least-once-example"}

# Memory usage
go_memstats_alloc_bytes{service_name="kafka-at-least-once-example"}
```

### Logs in Loki

1. In Grafana **Explore**, select **Loki** data source
2. Use LogQL queries:

**All logs from the application:**
```logql
{service_name="kafka-at-least-once-example"}
```

**Only order processing logs:**
```logql
{service_name="kafka-at-least-once-example"} |= "processing order"
```

**Logs at ERROR level:**
```logql
{service_name="kafka-at-least-once-example"} | json | level="ERROR"
```

**Logs for a specific trace (click "View Trace" link in log line):**
```logql
{service_name="kafka-at-least-once-example"} | json | trace_id="<your-trace-id>"
```

## Trace-to-Logs Correlation

Grafana is configured to enable seamless navigation between traces and logs:

1. Open a trace in Tempo
2. Click on any span
3. In the span details panel, click **Logs for this span**
4. Grafana will automatically query Loki for logs within the span's time range with the matching trace ID

This works because:
- The application includes trace context in all log records
- Loki is configured with derived fields to extract trace IDs from logs
- Tempo is configured to link to Loki for related logs

## Configuration Files

### Application Configuration

- **`config.yaml`**: Basic Kafka configuration (minimal, no OTel)
- **`config-with-otel.yaml`**: Full configuration with OpenTelemetry enabled

### Observability Configuration

- **`otel-collector-config.yaml`**: OTel Collector pipeline configuration
- **`tempo-config.yaml`**: Tempo trace storage configuration
- **`mimir-config.yaml`**: Mimir metrics storage configuration
- **`loki-config.yaml`**: Loki log aggregation configuration
- **`grafana-datasources.yaml`**: Grafana data source auto-provisioning

### Infrastructure

- **`podman-compose.yaml`**: Complete stack definition with all services

## Key Features Demonstrated

### 1. At-Least-Once Delivery Tracing

The trace clearly shows the order of operations:
1. **Consume** span: Fetch message from Kafka partition
2. **Process** span: Execute business logic (order processing)
3. **Acknowledge** span: Commit offset to Kafka

If processing fails, you'll see the acknowledge span is missing, and the message will be redelivered.

### 2. Goroutine-per-Partition Concurrency

The runtime uses a goroutine for each Kafka partition:
- Trace view shows parallel processing across partitions
- Service map visualizes the partition-level parallelism
- Each partition has independent traces

### 3. Structured Logging with Context

All logs include:
- **Trace ID**: Links logs to traces
- **Span ID**: Links logs to specific operations
- **Service metadata**: service.name, service.version
- **Business context**: order_id, amount, product_id, etc.

### 4. Metrics from Traces (Exemplars)

Tempo's metrics generator creates RED metrics from traces:
- **Rate**: Requests per second
- **Errors**: Error rate
- **Duration**: Latency percentiles

These metrics include exemplars (sample trace IDs), enabling drill-down from metrics to traces.

## Troubleshooting

### Application not sending telemetry

**Check OTel Collector logs:**
```bash
podman logs otel-collector
```

Look for "Serving OTLP receiver on..." messages.

**Verify environment variables:**
```bash
echo $OTEL_SERVICE_NAME
echo $OTEL_TRACE_EXPORTER
echo $OTEL_OTLP_TARGET
```

### No traces in Tempo

**Check Tempo logs:**
```bash
podman logs tempo
```

**Verify OTel Collector is forwarding:**
```bash
# Check collector metrics
curl http://localhost:8888/metrics | grep otelcol_exporter_sent_spans
```

### No logs in Loki

**Check Loki logs:**
```bash
podman logs loki
```

**Verify logs are being exported:**
```bash
# Check collector metrics
curl http://localhost:8888/metrics | grep otelcol_exporter_sent_log_records
```

### Grafana data source errors

**Restart Grafana to reload data sources:**
```bash
podman-compose restart grafana
```

**Manually test data source connectivity:**
In Grafana:
1. Go to **Configuration** → **Data Sources**
2. Click on a data source (e.g., Tempo)
3. Click **Save & Test**

## Production Considerations

This setup is designed for **development and demonstration**. For production:

### Security

- [ ] Enable TLS for all components
- [ ] Add authentication (Grafana, OTel Collector)
- [ ] Use secrets management for credentials
- [ ] Restrict network access

### Scalability

- [ ] Run Tempo in distributed mode (compactor, ingester, querier)
- [ ] Run Mimir in distributed mode
- [ ] Run Loki in scalable mode with object storage
- [ ] Use remote/cloud storage instead of local filesystem

### Retention

- [ ] Configure data retention policies (Tempo, Mimir, Loki)
- [ ] Set up compaction schedules
- [ ] Implement archival strategies

### Sampling

- [ ] Reduce trace sampling ratio (e.g., 0.1 = 10%)
- [ ] Implement tail-based sampling for errors
- [ ] Use adaptive sampling based on traffic

### High Availability

- [ ] Run multiple replicas of each component
- [ ] Use shared storage or object stores
- [ ] Configure load balancing

## Cleanup

Stop and remove all containers and volumes:

```bash
# Stop all services
podman-compose down

# Remove volumes (deletes all stored data)
podman-compose down -v
```

## Resources

- [Humus Framework Documentation](../../CLAUDE.md)
- [OpenTelemetry Documentation](https://opentelemetry.io/docs/)
- [Grafana Tempo Documentation](https://grafana.com/docs/tempo/)
- [Grafana Mimir Documentation](https://grafana.com/docs/mimir/)
- [Grafana Loki Documentation](https://grafana.com/docs/loki/)
- [OpenTelemetry Collector Documentation](https://opentelemetry.io/docs/collector/)

## What's Next?

After exploring this example, consider:

1. **Create custom dashboards** in Grafana for your use case
2. **Set up alerts** based on metrics and trace patterns
3. **Explore service graphs** to visualize system architecture
4. **Build recording rules** in Mimir for aggregated metrics
5. **Create log-based alerts** in Loki for error patterns
6. **Implement trace-based alerting** for latency SLOs

This example serves as a reference for building observable queue-based applications with the Humus framework.
