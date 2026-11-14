# RabbitMQ

Each guide here moves a RabbitMQ consumer onto
[`queue/rabbitmq`](/humus/services/rabbitmq/). Its handlers become
`queue.Processor`s, its publishing goes through a `rabbitmq.Publisher`, with
publisher confirms, and its topology moves into a definitions file. What it
retried in place goes through a delay queue and a dead-letter exchange, as
the [mailer walkthrough](/humus/walkthroughs/rabbitmq/mailer/)
does.

- [Watermill](/humus/migrating/rabbitmq/watermill/)
