# The topology

The mailer declares nothing. Its topology is
`deploy/rabbitmq/definitions.json`, a
[definitions file](https://www.rabbitmq.com/docs/definitions) the broker's
team imports. It holds no users and no passwords.

## The exchanges

Four exchanges, each durable:

```json
"exchanges": [
  {
    "name": "mail",
    "vhost": "/",
    "type": "direct",
    "durable": true,
    "auto_delete": false,
    "internal": false,
    "arguments": {}
  },
  {
    "name": "mail.retry",
    "vhost": "/",
    "type": "direct",
    "durable": true,
    "auto_delete": false,
    "internal": false,
    "arguments": {}
  },
  {
    "name": "mail.dead",
    "vhost": "/",
    "type": "fanout",
    "durable": true,
    "auto_delete": false,
    "internal": false,
    "arguments": {}
  },
  {
    "name": "mail.events",
    "vhost": "/",
    "type": "topic",
    "durable": true,
    "auto_delete": false,
    "internal": false,
    "arguments": {}
  }
],
```

## The queues

Three quorum queues. The queue type is their only argument:

```json
"queues": [
  {
    "name": "mail.send",
    "vhost": "/",
    "durable": true,
    "auto_delete": false,
    "arguments": {
      "x-queue-type": "quorum"
    }
  },
  {
    "name": "mail.send.retry",
    "vhost": "/",
    "durable": true,
    "auto_delete": false,
    "arguments": {
      "x-queue-type": "quorum"
    }
  },
  {
    "name": "mail.send.dead",
    "vhost": "/",
    "durable": true,
    "auto_delete": false,
    "arguments": {
      "x-queue-type": "quorum"
    }
  }
],
```

## The bindings

Requests and parked requests by `mail.send`, and every dead letter:

```json
"bindings": [
  {
    "source": "mail",
    "vhost": "/",
    "destination": "mail.send",
    "destination_type": "queue",
    "routing_key": "mail.send",
    "arguments": {}
  },
  {
    "source": "mail.retry",
    "vhost": "/",
    "destination": "mail.send.retry",
    "destination_type": "queue",
    "routing_key": "mail.send",
    "arguments": {}
  },
  {
    "source": "mail.dead",
    "vhost": "/",
    "destination": "mail.send.dead",
    "destination_type": "queue",
    "routing_key": "",
    "arguments": {}
  }
],
```

## The policies

The delays and limits are policies, since RabbitMQ changes a policy in place
but a queue's arguments only by deleting the queue:

```json
"policies": [
  {
    "name": "mail.send",
    "vhost": "/",
    "pattern": "^mail\\.send$",
    "apply-to": "queues",
    "priority": 0,
    "definition": {
      "delivery-limit": 5,
      "dead-letter-exchange": "mail.dead",
      "dead-letter-strategy": "at-least-once",
      "overflow": "reject-publish"
    }
  },
  {
    "name": "mail.send.retry",
    "vhost": "/",
    "pattern": "^mail\\.send\\.retry$",
    "apply-to": "queues",
    "priority": 0,
    "definition": {
      "message-ttl": 300000,
      "dead-letter-exchange": "mail",
      "dead-letter-routing-key": "mail.send",
      "dead-letter-strategy": "at-least-once",
      "overflow": "reject-publish"
    }
  }
]
```

- `mail.send` dead-letters a message to `mail.dead` after 5 deliveries.
- `mail.send.retry` holds a parked request for 5 minutes, then
  dead-letters it back to `mail` by `mail.send`.
- At-least-once dead-lettering needs `overflow` `reject-publish`.

[Retries and dead letters](/humus/pr-preview/pr-1185/walkthroughs/mailer/retries/)
shows how the mailer uses them.

## Importing it

Into a running broker:

```sh
rabbitmqctl import_definitions definitions.json
```

The kind overlay imports a second file after it, with policies of a higher
priority, so a test waits 1s for a parked request and 2 deliveries for the
delivery limit:

```json
{
  "name": "kind.mail.send",
  "vhost": "/",
  "pattern": "^mail\\.send$",
  "apply-to": "queues",
  "priority": 1,
  "definition": {
    "delivery-limit": 2,
    "dead-letter-exchange": "mail.dead",
    "dead-letter-strategy": "at-least-once",
    "overflow": "reject-publish"
  }
},
{
  "name": "kind.mail.send.retry",
  "vhost": "/",
  "pattern": "^mail\\.send\\.retry$",
  "apply-to": "queues",
  "priority": 1,
  "definition": {
    "message-ttl": 1000,
    "dead-letter-exchange": "mail",
    "dead-letter-routing-key": "mail.send",
    "dead-letter-strategy": "at-least-once",
    "overflow": "reject-publish"
  }
}
```

## The logins

Create each login with a password read from a file, then grant it its
configure, write and read permissions, in that order:

```sh
rabbitmqctl add_user mailer <mailer-password
rabbitmqctl set_permissions --vhost / mailer '^$' '^mail\.(retry|dead|events)$' '^mail\.send$'
```

| Permission | The mailer's | Because |
|---|---|---|
| configure | none | it declares nothing |
| write | `mail.retry`, `mail.dead` and `mail.events` | it parks, dead-letters and publishes events |
| read | `mail.send` | it consumes its work queue only |

A service that asks for emails needs to write `mail` and nothing else, as
the kind overlay's `mailrequest` login does:

```sh
rabbitmqctl add_user mailrequest <mailrequest-password
rabbitmqctl set_permissions --vhost / mailrequest '^$' '^mail$' '^$'
```
