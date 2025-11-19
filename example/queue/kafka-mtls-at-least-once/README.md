# Kafka mTLS At-Least-Once Queue Example

This example demonstrates how to build a secure Kafka queue processor with mutual TLS (mTLS) authentication using the Humus framework.

## Features

- **mTLS Authentication**: Secure connections to Kafka brokers using client certificates
- **At-Least-Once Delivery**: Messages are processed before being committed, ensuring no message loss
- **Idempotent Processing**: Handles duplicate messages gracefully
- **OpenTelemetry Integration**: Automatic tracing, metrics, and logging
- **Flexible Certificate Configuration**: Supports both file paths and environment variables

## Configuration

The application uses YAML configuration with Go template support for environment variables:

```yaml
kafka:
  brokers:
    - {{env "KAFKA_BROKERS" | default "localhost:9093"}}
  topic: {{env "KAFKA_TOPIC" | default "orders"}}
  group_id: {{env "KAFKA_GROUP_ID" | default "order-processor-mtls"}}
  tls:
    cert_file: {{env "KAFKA_CLIENT_CERT_FILE" | default "./certs/client-cert.pem"}}
    key_file: {{env "KAFKA_CLIENT_KEY_FILE" | default "./certs/client-key.pem"}}
    ca_file: {{env "KAFKA_CA_CERT_FILE" | default "./certs/ca-cert.pem"}}
```

### TLS Configuration Options

- **cert_file**: Path to client certificate (PEM format)
- **key_file**: Path to client private key (PEM format)
- **ca_file**: Path to CA certificate for broker verification (PEM format)
- **server_name** (optional): Server name for SNI
- **min_version** (optional): Minimum TLS version (12 = TLS 1.2, 13 = TLS 1.3)

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `KAFKA_BROKERS` | Kafka broker addresses | `localhost:9093` |
| `KAFKA_TOPIC` | Topic to consume from | `orders` |
| `KAFKA_GROUP_ID` | Consumer group ID | `order-processor-mtls` |
| `KAFKA_CLIENT_CERT_FILE` | Client certificate path | `./certs/client-cert.pem` |
| `KAFKA_CLIENT_KEY_FILE` | Client key path | `./certs/client-key.pem` |
| `KAFKA_CA_CERT_FILE` | CA certificate path | `./certs/ca-cert.pem` |

## Running the Example

### Prerequisites

1. **Kafka with mTLS enabled** running on port 9093
2. **TLS certificates** (see "Generating Test Certificates" below)
3. **Go 1.24+** installed

### Generating Test Certificates

For testing, you can generate self-signed certificates:

```bash
# Create certs directory
mkdir -p certs

# Generate CA
openssl req -new -x509 -days 365 -keyout certs/ca-key.pem -out certs/ca-cert.pem \
  -subj "/CN=Test CA" -nodes

# Generate server certificate (for Kafka broker)
openssl req -new -keyout certs/server-key.pem -out certs/server-req.pem \
  -subj "/CN=kafka" -nodes
openssl x509 -req -in certs/server-req.pem -CA certs/ca-cert.pem \
  -CAkey certs/ca-key.pem -CAcreateserial -out certs/server-cert.pem -days 365

# Generate client certificate
openssl req -new -keyout certs/client-key.pem -out certs/client-req.pem \
  -subj "/CN=kafka-client" -nodes
openssl x509 -req -in certs/client-req.pem -CA certs/ca-cert.pem \
  -CAkey certs/ca-key.pem -CAcreateserial -out certs/client-cert.pem -days 365
```

### Running with Docker Compose

A complete setup with Kafka, Zookeeper, and mTLS is available in the repository examples.

### Running the Application

```bash
# Build the application
go build -o kafka-mtls-processor

# Run with default configuration
./kafka-mtls-processor

# Run with custom environment variables
export KAFKA_BROKERS="kafka.example.com:9093"
export KAFKA_CLIENT_CERT_FILE="/path/to/client.pem"
export KAFKA_CLIENT_KEY_FILE="/path/to/key.pem"
export KAFKA_CA_CERT_FILE="/path/to/ca.pem"
./kafka-mtls-processor
```

## Architecture

### At-Least-Once Semantics

This example uses `kafka.AtLeastOnce()` which:

1. **Consumes** messages from Kafka
2. **Processes** each message through the business logic
3. **Commits** offsets only after successful processing

If processing fails, the message is **not** committed and will be redelivered. This means your processor must be **idempotent**.

### Idempotent Processing

The `OrderProcessor` tracks processed order IDs to handle duplicates:

```go
func (p *OrderProcessor) Process(ctx context.Context, msg *OrderMessage) error {
    // Check if already processed
    if p.processed[msg.OrderID] {
        return nil // Skip duplicate
    }
    
    // Process order...
    
    // Mark as processed
    p.processed[msg.OrderID] = true
    return nil
}
```

In production, you would check a database instead of in-memory map.

### Middleware Pattern

The example uses a middleware pattern to separate:
- **Decoding**: Converting Kafka bytes to typed messages (`DecodingProcessor`)
- **Business Logic**: Processing orders (`OrderProcessor`)

This keeps concerns separated and makes testing easier.

## Security Best Practices

1. **Certificate Permissions**: Keep private keys secure with `chmod 600`
2. **TLS Version**: Use TLS 1.2 or higher (configured by default)
3. **Certificate Rotation**: Implement a process for rotating certificates before expiry
4. **Kubernetes Secrets**: Use volume mounts for certificates in production
5. **Environment Variables**: Never hardcode certificates in configuration files

## Kubernetes Deployment

For Kubernetes deployments, mount certificates as secrets:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: kafka-mtls-certs
type: Opaque
data:
  client-cert.pem: <base64-encoded-cert>
  client-key.pem: <base64-encoded-key>
  ca-cert.pem: <base64-encoded-ca>
---
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
      - name: processor
        env:
        - name: KAFKA_CLIENT_CERT_FILE
          value: /etc/kafka/certs/client-cert.pem
        - name: KAFKA_CLIENT_KEY_FILE
          value: /etc/kafka/certs/client-key.pem
        - name: KAFKA_CA_CERT_FILE
          value: /etc/kafka/certs/ca-cert.pem
        volumeMounts:
        - name: kafka-certs
          mountPath: /etc/kafka/certs
          readOnly: true
      volumes:
      - name: kafka-certs
        secret:
          secretName: kafka-mtls-certs
```

## Monitoring

The application includes automatic OpenTelemetry instrumentation:

- **Traces**: Request traces for each message processed
- **Metrics**: Consumer group lag, processing duration, error rates
- **Logs**: Structured logs with trace correlation

Configure exporters via environment variables or the YAML configuration.

## Troubleshooting

### Connection Refused

- Verify Kafka is running on the configured port
- Check firewall rules allow connections to port 9093

### TLS Handshake Errors

- Verify certificate paths are correct
- Check certificate validity: `openssl x509 -in cert.pem -text -noout`
- Ensure CA certificate matches the broker's certificate
- Verify ServerName matches broker hostname if using SNI

### Authentication Failures

- Verify client certificate is signed by the CA that Kafka trusts
- Check Kafka broker TLS configuration requires client certificates
- Ensure certificate has not expired

### Permission Denied Reading Certificates

- Check file permissions: `chmod 600 certs/*.pem`
- Verify the application has read access to certificate files

## Related Examples

- `kafka-at-least-once`: Same pattern without TLS
- `kafka-at-most-once`: Fast processing with potential message loss

## Learn More

- [Humus Documentation](https://github.com/z5labs/humus)
- [Kafka TLS Configuration](https://kafka.apache.org/documentation/#security_ssl)
- [franz-go Documentation](https://pkg.go.dev/github.com/twmb/franz-go)
