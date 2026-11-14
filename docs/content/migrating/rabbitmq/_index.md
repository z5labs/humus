---
title: RabbitMQ
linkTitle: RabbitMQ
description: >
  Guides that move a RabbitMQ consumer from another Go framework to humus:
  its handlers become queue.Processors on queue/rabbitmq, its publishing goes
  through a rabbitmq.Publisher with publisher confirms, its topology moves
  into a definitions file, and its retries become a delay queue and a
  dead-letter exchange.
type: docs
weight: 50
---

Each guide here moves a RabbitMQ consumer onto
[`queue/rabbitmq`]({{< relref "/services/rabbitmq" >}}). Its handlers become
`queue.Processor`s, its publishing goes through a `rabbitmq.Publisher`, with
publisher confirms, and its topology moves into a definitions file. What it
retried in place goes through a delay queue and a dead-letter exchange, as
the [mailer walkthrough]({{< relref "/walkthroughs/rabbitmq/mailer" >}})
does.

- [Watermill]({{< relref "/migrating/rabbitmq/watermill" >}})
