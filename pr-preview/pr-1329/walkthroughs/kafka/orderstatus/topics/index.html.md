# The topics

The processor creates no topic. The cluster's owner creates the topics and
their ACLs before it starts. The kind overlay does it with a Job that runs
`deploy/kind/kafka/topics.sh`.

## Logging in as the admin

Kafka's tools read the admin's login from a properties file. Over mTLS, it
is a certificate and key in one PEM file, and the cluster's CA:

```properties
security.protocol=SSL
ssl.keystore.type=PEM
ssl.keystore.location=/etc/kafka-admin/tls/tls.pem
ssl.truststore.type=PEM
ssl.truststore.location=/etc/kafka-admin/tls/ca.crt
```

`kafka` runs one of the tools with it:

```sh
kafka() {
  "/opt/kafka/bin/$1" --bootstrap-server kafka:9093 --command-config /etc/kafka-admin/config/admin.properties "${@:2}"
}
```

## Creating the topics

Four topics, twelve partitions each. kind has one broker, so one replica:

```sh
for topic in orders.events orders.status orders.dlq orders.retry; do
  kafka kafka-topics.sh --create --if-not-exists --topic "$topic" --partitions 12 --replication-factor 1
done
```

## Keys

Every producer keys a record by the order's ID, so an order's events are on
one partition, processed one at a time, in order. The processor keeps the
key on every record it produces:

```go
records = append(records, kafka.Record{
	Topic: p.settings.StatusTopic,
	Key:   m.Key,
	Value: encodeChange(e.OrderID, c),
})
```

## Partitions

A replica processes each of its partitions at once, so twelve partitions are
room for up to twelve replicas. The Deployment runs three:

```yaml
replicas: 3
```

`orders.retry` has twelve too, so its records spread across the replicas as
`orders.events`' do.

Never add partitions to `orders.events`: that moves an order's later events
to another partition, where they can be projected at the same time as its
earlier ones. A topic that needs more partitions is a new topic.

## Retention

| Topic | Keeps a record |
|---|---|
| `orders.events` | longer than the processor can be down or behind, so no event expires before it is projected |
| `orders.retry` | the same, and longer than `RETRY_MAX_DELAY` |
| `orders.status` | as long as its consumers need |
| `orders.dlq` | as long as you need to handle a dead letter |

Set it with `--config retention.ms=<ms>` on `kafka-topics.sh --create`. The
kind overlay keeps the broker's default, 7 days.

## The ACLs

The processor's login is its certificate, `CN=orderstatus`. Grant it its
group, and only the topics it reads and writes:

```sh
acl() {
  kafka kafka-acls.sh --add --allow-principal "User:CN=$1" "${@:2}"
}

acl orderstatus --operation Read --group orderstatus
acl orderstatus --operation Read --topic orders.events --topic orders.retry
acl orderstatus --operation Write --topic orders.status --topic orders.retry --topic orders.dlq
```

| Operation | The processor's | Because |
|---|---|---|
| Read | the group `orderstatus` | it consumes as that group |
| Read | `orders.events` and `orders.retry` | it consumes both |
| Write | `orders.status`, `orders.retry` and `orders.dlq` | it produces changes, retries and dead letters |

A service that writes orders' events needs Write on `orders.events` and
nothing else.

Next: [The projection](/humus/pr-preview/pr-1329/walkthroughs/kafka/orderstatus/projection/)
