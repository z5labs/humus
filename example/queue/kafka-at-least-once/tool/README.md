# Kafka Message Publisher Tool

A simple command-line tool to publish random order messages to Kafka for testing the at-least-once queue consumer.

## Features

- Publishes randomly generated `OrderMessage` objects to Kafka
- Configurable number of messages and publish interval
- Generates realistic test data with:
  - Unique order IDs (timestamp-based)
  - Random amounts ($10.00 to $1000.00)
  - Random product IDs from a predefined list
  - Random quantities (1 to 10 items)

## Usage

### Build the tool

```bash
cd /home/carson/github.com/z5labs/humus/example/queue/kafka-at-least-once/tool
go build -o publisher .
```

### Run with default configuration

```bash
./publisher
```

This will publish 10 messages with a 1-second interval between each message.

### Run with custom configuration

Edit `tool/config.yaml`:

```yaml
kafka:
  brokers:
    - localhost:9092
  topic: orders

publisher:
  count: 50        # Publish 50 messages
  interval: 500    # 500ms between messages
```

### Run with command-line flags

```bash
# Publish 100 messages with 100ms interval
./publisher -count 100 -interval 100

# Use a different config file
./publisher -config /path/to/config.yaml

# Override config file settings
./publisher -config tool/config.yaml -count 5 -interval 2000
```

## Command-line Flags

- `-config` - Path to configuration file (default: `tool/config.yaml`)
- `-count` - Number of messages to publish (overrides config file)
- `-interval` - Interval between messages in milliseconds (overrides config file)

## Example Output

```
2025/11/10 12:34:56 Publishing 10 messages to topic 'orders' with 1000ms interval
2025/11/10 12:34:56 [1/10] Published order: ORDER-1731249296123456789 (Amount: $234.56, Product: PROD-003, Qty: 5)
2025/11/10 12:34:57 [2/10] Published order: ORDER-1731249297234567890 (Amount: $789.12, Product: PROD-007, Qty: 2)
...
2025/11/10 12:35:05 Successfully published 10/10 messages
```

## Message Format

The tool publishes JSON messages with the following structure:

```json
{
  "order_id": "ORDER-1731249296123456789",
  "amount": 234.56,
  "product_id": "PROD-003",
  "quantity": 5
}
```

## Testing the At-Least-Once Consumer

1. Start Kafka using the podman-compose setup:
   ```bash
   cd /home/carson/github.com/z5labs/humus/example/queue/kafka-at-least-once
   podman-compose up -d
   ```

2. Start the consumer application:
   ```bash
   go run .
   ```

3. In another terminal, run the publisher:
   ```bash
   cd tool
   go build -o publisher .
   ./publisher -count 20 -interval 500
   ```

4. Watch the consumer process the messages with idempotent handling for any duplicates.
