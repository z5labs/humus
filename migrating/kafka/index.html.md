# Kafka

Each guide here moves a Kafka consumer onto
[`queue/kafka`](/humus/services/kafka/). Its handlers become
`queue.Processor`s, its publishing goes through a `kafka.Producer`, and what
it retried in place goes through a retry topic and a dead-letter topic, as
the [order status walkthrough](/humus/walkthroughs/kafka/orderstatus/)
does.

- [Watermill](/humus/migrating/kafka/watermill/)
