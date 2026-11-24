# Kafka

Each guide here moves a Kafka consumer onto
[`queue/kafka`](/humus/pr-preview/pr-1628/services/kafka/). Its handlers become
`queue.Processor`s, its publishing goes through a `kafka.Producer`, and what
it retried in place goes through a retry topic and a dead-letter topic, as
the [order status walkthrough](/humus/pr-preview/pr-1628/walkthroughs/kafka/orderstatus/)
does.

- [Watermill](/humus/pr-preview/pr-1628/migrating/kafka/watermill/)
