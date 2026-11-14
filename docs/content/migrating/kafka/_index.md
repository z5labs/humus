---
title: Kafka
linkTitle: Kafka
description: >
  Guides that move a Kafka consumer from another Go framework to humus: its
  handlers become queue.Processors on queue/kafka, its publishing goes
  through a kafka.Producer, and its retries become a retry topic and a
  dead-letter topic.
type: docs
weight: 40
---

Each guide here moves a Kafka consumer onto
[`queue/kafka`]({{< relref "/services/kafka" >}}). Its handlers become
`queue.Processor`s, its publishing goes through a `kafka.Producer`, and what
it retried in place goes through a retry topic and a dead-letter topic, as
the [order status walkthrough]({{< relref "/walkthroughs/kafka/orderstatus" >}})
does.

- [Watermill]({{< relref "/migrating/kafka/watermill" >}})
