# Migrating from Watermill

This guide moves a small [Watermill](https://watermill.io) v1 service on
RabbitMQ to humus's `queue/rabbitmq`. Its Router's handler becomes a
`queue.Processor`, and its publishing goes through a `rabbitmq.Publisher`,
with publisher confirms. Its process, settings, telemetry, probes and
shutdown become humus's.

Watermill's AMQP backend,
[watermill-amqp](https://github.com/ThreeDotsLabs/watermill-amqp), is built
on amqp091-go, as `queue/rabbitmq` is, so the client stays and the code
around it changes. The guide's steps go from the Watermill service to the
humus service, rather than each leaving a service that runs.

## Watermill and humus

| Watermill | humus |
|---|---|
| `message.Router` and its `Run` | `humus.Main`, and an `Init` that returns the Service from `rabbitmq.NewService`: [A consumer](/humus/services/rabbitmq/#a-consumer) |
| `plugin.SignalsHandler`, and the Router's `CloseTimeout` | none: `humus.Main` stops the App on SIGTERM, and the consumer drains within a 20s grace period: [Shutdown](/humus/services/rabbitmq/#shutdown) |
| `amqp.Subscriber`, and its `Consume.Qos` | `rabbitmq.Settings`, with its `Queue` and `Prefetch`: [Settings](/humus/services/rabbitmq/#settings) |
| `ConnectionConfig.AmqpURI` | `Settings.Connection`, which `fromURI` fills from the same URI: [An AMQP URI](/humus/services/rabbitmq/#an-amqp-uri) |
| the connection's reconnect | none: the Service and the `Publisher` connect again on their own: [Connection loss](/humus/services/rabbitmq/#connection-loss) |
| `amqp.NewDurablePubSubConfig` and its `TopologyBuilder`, which declare the exchanges, queues and bindings | a definitions file the broker's team imports, since humus declares nothing: [Topology](/humus/services/rabbitmq/#topology) |
| the handler, a `message.HandlerFunc` | a `queue.Processor`, whose `Process` gets a `rabbitmq.Message`: [The contract](/humus/services/rabbitmq/#the-contract) |
| `amqp.DefaultMarshaler`, and a message's payload and metadata | the message's body and headers, in `rabbitmq.Message` and `rabbitmq.Publishing`: [The contract](/humus/services/rabbitmq/#the-contract) |
| a message's UUID, from `watermill.NewUUID` | none: an order is known by its correlation ID |
| `amqp.Publisher`, and the messages the handler returns | a `rabbitmq.Publisher`, from `rabbitmq.NewPublisher`, which the `Processor` calls before it returns, and which waits for the broker's confirm: [Publishing](/humus/services/rabbitmq/#publishing) |
| `middleware.Retry` | a delay queue, `orders_invoicer.retry`, for what may pass later, which hands it back after its `message-ttl`: [Retries and dead letters](/humus/walkthroughs/rabbitmq/mailer/retries/) |
| `middleware.PoisonQueue` | the `orders.poison` exchange, for what never will pass, and for what still fails after `RETRY_ATTEMPTS`: [Dead letters](/humus/services/rabbitmq/#dead-letters) |
| `middleware.Recoverer` | none: a panicking `Processor` crashes the process, and the broker delivers its order again after the restart. The `Processor` returns an error instead: [Failures](/humus/services/rabbitmq/#failures) |
| `middleware.CorrelationID` | none: the `Processor` copies the `correlation_id` header to the invoice. Each published message's span continues the trace of the order it came from: [The send span](/humus/services/rabbitmq/#the-send-span) |
| `watermill.NewStdLogger` | the Setup's `LoggerProvider`: [The logs](/humus/services/rabbitmq/#the-logs) |
| no traces or metrics | a span for each message processed and published, with metrics: [Telemetry](/humus/services/rabbitmq/#telemetry) |
| no probes | `httphealth.NewService`, ready once the consumer consumes: [Readiness](/humus/services/rabbitmq/#readiness) |
| a test against a broker | `humustest.Start`, against RabbitMQ in a container: [Testing](/humus/services/rabbitmq/#testing) |

## The Watermill service

The guide's code is under
[`example/rabbitmq/watermill`](https://github.com/z5labs/humus/tree/main/example/rabbitmq/watermill),
a module for each service:

```text
example/rabbitmq/watermill/
├── cases/    the black-box test cases both services pass
├── start/    the Watermill service the guide starts from
└── humus/    the humus service it ends with, and its definitions file
```

The service consumes orders from the `orders` exchange and publishes an
invoice for each to the `invoices` exchange. Its Router is built as
[Watermill's documentation](https://watermill.io/docs/messages-router/)
builds one, with middleware:

```go
func newRouter(subscriber message.Subscriber, publisher message.Publisher, logger watermill.LoggerAdapter) (*message.Router, error) {
	router, err := message.NewRouter(message.RouterConfig{}, logger)
	if err != nil {
		return nil, err
	}

	poisonQueue, err := middleware.PoisonQueue(publisher, poisonTopic)
	if err != nil {
		return nil, err
	}

	router.AddMiddleware(
		middleware.CorrelationID,
		poisonQueue,
		middleware.Retry{
			MaxRetries:      3,
			InitialInterval: 100 * time.Millisecond,
			MaxInterval:     time.Second,
			Multiplier:      2,
			Logger:          logger,
		}.Middleware,
		middleware.Recoverer,
	)

	router.AddHandler("invoicer", ordersTopic, subscriber, invoicesTopic, publisher, issueInvoice)
	return router, nil
}
```

Its handler reads an order, and returns the invoice for the Router to
publish:

```go
func issueInvoice(msg *message.Message) ([]*message.Message, error) {
	var o order
	if err := json.Unmarshal(msg.Payload, &o); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("the order has no id")
	}

	payload, err := json.Marshal(invoice{OrderID: o.ID, Customer: o.Customer, Amount: o.Total})
	if err != nil {
		return nil, err
	}
	return []*message.Message{message.NewMessage(watermill.NewUUID(), payload)}, nil
}
```

Its config is
[watermill-amqp's](https://watermill.io/pubsubs/amqp/) durable pub/sub
config. Each topic is a fanout exchange, and the subscriber declares a queue
of its own, `orders_invoicer`, and binds it to `orders`:

```go
func newConfig(amqpURI string) amqp.Config {
	return amqp.NewDurablePubSubConfig(amqpURI, amqp.GenerateQueueNameTopicNameWithSuffix(consumerGroup))
}
```

`main` runs the Router until SIGINT or SIGTERM:

```go
func main() {
	logger := watermill.NewStdLogger(false, false)
	config := newConfig(os.Getenv("AMQP_URI"))

	subscriber, err := amqp.NewSubscriber(config, logger)
	if err != nil {
		panic(err)
	}
	publisher, err := amqp.NewPublisher(config, logger)
	if err != nil {
		panic(err)
	}

	router, err := newRouter(subscriber, publisher, logger)
	if err != nil {
		panic(err)
	}
	router.AddPlugin(plugin.SignalsHandler)

	if err := router.Run(context.Background()); err != nil {
		panic(err)
	}
	if err := publisher.Close(); err != nil {
		panic(err)
	}
}
```

## The test cases

The `cases` module holds black-box cases, written once. Each binds a queue
of its own to `invoices` and to `orders.poison`, as a consumer of them would,
publishes orders to `orders` with a `correlation_id` header, and waits for
each order's invoice or dead letter:

```go
type Target struct {
	Addr     string
	Vhost    string
	Username string
	Password string

	OrdersExchange     string
	InvoicesExchange   string
	DeadLetterExchange string

	Timeout time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "AnOrder", Run: AnOrder},
	{Name: "NotOrders", Run: NotOrders},
	{Name: "Orders", Run: Orders},
}
```

One case publishes two orders that aren't orders, then one that is:

```go
func NotOrders(t testing.TB, target Target) {
	ctx, b := begin(t, target)
	notJSON := order("not JSON")
	noID := order(`{"customer":"ada","total":100}`)
	valid := order(`{"id":"order-4","customer":"grace","total":990}`)

	b.publish(ctx, t, notJSON, noID, valid)
	got := b.settle(ctx, t, notJSON, noID, valid)

	require.Len(t, got.deadLetters, 2)
	require.Equal(t, "not JSON", string(got.deadLetters[0].Body))
	require.Equal(t, notJSON.correlationID, header(got.deadLetters[0], headerCorrelationID))
	require.Equal(t, `{"customer":"ada","total":100}`, string(got.deadLetters[1].Body))
	require.Equal(t, noID.correlationID, header(got.deadLetters[1], headerCorrelationID))
	require.Len(t, got.invoices, 1)
	requireInvoice(t, got.invoices[0], valid, `{"order_id":"order-4","customer":"grace","amount":990}`)
}
```

Each case runs against a service and a broker of its own. The Watermill
service runs them against RabbitMQ in a container:

```go
func TestRouter_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			b := startRabbitMQ(t)
			invoicer := b.addUser(t, ".*", ".*", ".*")
			run(t, (&url.URL{Scheme: "amqp", User: url.UserPassword(invoicer.username, invoicer.password), Host: b.addr, Path: "/"}).String())

			c.Run(t, cases.Target{
				Addr:               b.addr,
				Vhost:              "/",
				Username:           b.container.AdminUsername,
				Password:           b.container.AdminPassword,
				OrdersExchange:     ordersTopic,
				InvoicesExchange:   invoicesTopic,
				DeadLetterExchange: poisonTopic,
				Timeout:            time.Minute,
			})
		})
	}
}
```

The tests start containers, so they sit behind the `testcontainers` build
tag:

```sh
go test -tags testcontainers ./...
```

## Step 1: main and its settings

`main` becomes one call to `humus.Main`:

```go
humus.Main(Init, humus.WithTelemetry(buildTelemetry))
```

`Init` builds the `Publisher`, the consumer and its probes:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	publisher, err := rabbitmq.NewPublisher(s, settings.Publisher)
	if err != nil {
		return nil, err
	}
	consumer, err := rabbitmq.NewService(s, settings.Consumer, &invoicer{publisher: publisher, attempts: settings.RetryAttempts})
	if err != nil {
		return nil, err
	}
	probes, err := httphealth.NewService(s, settings.Probes, health.Readiness(consumer.Consuming()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{consumer, probes}, nil
}
```

`readSettings` sources the broker, the retries and the probes. The consumer
consumes `orders_invoicer`, and the `Publisher` publishes to the same broker,
on a connection of its own:

```go
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	return config.Struct(func(r *config.Resolver) settings {
		consumer := rabbitmq.DefaultSettings()
		consumer.Connection = r.Get(env("AMQP_URI").Parse(fromURI))
		consumer.Queue = ordersQueue

		publisher := rabbitmq.DefaultPublisherSettings()
		publisher.Connection = consumer.Connection

		return settings{
			Consumer:      consumer,
			Publisher:     publisher,
			RetryAttempts: r.Get(env("RETRY_ATTEMPTS").Parse(strconv.Atoi).Default(defaultRetryAttempts).Validate(positive)),
			Probes: httphealth.Settings{
				Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
				TLS: r.Get(env("PROBES_TLS_CERT_FILE").FlatMap(func(certFile string) config.Value[*tls.Config] {
					return env("PROBES_TLS_KEY_FILE").Parse(serverTLS(s.Host.FS, certFile))
				}).Validate(httphealth.CheckTLS)),
			},
		}
	}).Read(ctx)
}
```

The service keeps its `AMQP_URI`. `fromURI` fills the `Connection` from it,
as [An AMQP URI](/humus/services/rabbitmq/#an-amqp-uri) shows:

```go
func fromURI(uri string) (rabbitmq.Connection, error) {
	u, err := amqp.ParseURI(uri)
	if err != nil {
		return rabbitmq.Connection{}, errors.New("is not an AMQP URI")
	}
	if u.Username == "guest" {
		return rabbitmq.Connection{}, errors.New("has no login of its own")
	}
	c := rabbitmq.DefaultSettings().Connection
	c.Addr = net.JoinHostPort(u.Host, strconv.Itoa(u.Port))
	c.Vhost = u.Vhost
	c.SASL = u.PlainAuth()
	if u.Scheme == "amqps" {
		c.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return c, nil
}
```

From here on, these are humus's:

- **The process and its shutdown.** On SIGTERM the consumer cancels its
  consumer, lets the order in flight finish and acks it, and nacks what it
  received but didn't process, within a 20s grace period, as
  [Shutdown](/humus/services/rabbitmq/#shutdown) describes.
- **The broker.** The Watermill service panics when it can't connect at
  startup. The humus service waits for the broker, and connects again
  whenever it loses the connection, as
  [Connection loss](/humus/services/rabbitmq/#connection-loss)
  describes.
- **The settings.** A missing or bad one makes the service exit 78, naming
  its variable. A URI with no login, or `guest`'s, is a bad one.
- **TLS.** The broker stays plain text, as it was. An `amqps` URI dials it
  over TLS.
- **Telemetry.** `buildTelemetry` names the service `invoicer`, as
  [Writing the Builder](/humus/fundamentals/telemetry/#writing-the-builder)
  shows. Each message processed and published is a span, with its metrics.
- **The probes.** The kubelet probes `/health/liveness` and
  `/health/readiness` on `PROBES_ADDR`, with `scheme: HTTPS`. The pod is
  ready once the consumer consumes.

## Step 2: the handler becomes a Processor

`issueInvoice` keeps the handler's code, and returns the invoice's body:

```go
func issueInvoice(body []byte) ([]byte, error) {
	var o order
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("the order has no id")
	}
	return json.Marshal(invoice{OrderID: o.ID, Customer: o.Customer, Amount: o.Total})
}
```

The `Processor` publishes the invoice itself, with the order's correlation
ID, before it returns. `Publish` returns once the broker has confirmed the
invoice, and an order is acked only once `Process` returns `nil`:

```go
func (i *invoicer) Process(ctx context.Context, m rabbitmq.Message) error {
	attempt, err := attemptOf(m)
	if err != nil {
		return i.deadLetter(ctx, m, err, 0)
	}

	body, err := issueInvoice(m.Body)
	if err != nil {
		return i.deadLetter(ctx, m, err, 0)
	}

	err = i.publisher.Publish(ctx, rabbitmq.Publishing{
		Exchange: invoicesExchange,
		Headers:  correlationID(m),
		Body:     body,
	})
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	return i.retryLater(ctx, m, attempt, err)
}
```

```go
func correlationID(m rabbitmq.Message) map[string]any {
	v, ok := m.Headers[headerCorrelationID]
	if !ok {
		return nil
	}
	return map[string]any{headerCorrelationID: v}
}
```

It takes the `Publisher` as an interface of its own, so its unit tests fake
it and need no broker:

```go
type publisher interface {
	Publish(ctx context.Context, p rabbitmq.Publishing) error
}
```

## Step 3: the topology

Watermill's config declares the topology when the service starts and when
it first publishes to a topic. The humus service declares nothing. Its
topology is
[`humus/definitions.json`](https://github.com/z5labs/humus/blob/main/example/rabbitmq/watermill/humus/definitions.json),
a [definitions file](https://www.rabbitmq.com/docs/definitions) the broker's
team imports, as the
[mailer walkthrough's](/humus/walkthroughs/rabbitmq/mailer/topology/)
is:

```sh
rabbitmqctl import_definitions definitions.json
```

It declares what Watermill declared, as Watermill declared it, so it imports
over the broker's topology as it is: the fanout exchanges `orders`,
`invoices` and `orders.poison`, and the classic queue `orders_invoicer`,
bound to `orders`. It adds a delay queue, `orders_invoicer.retry`, bound to
an exchange of the same name, and a queue for the dead letters,
`orders.poison`, bound to theirs:

```json
"queues": [
  {
    "name": "orders_invoicer",
    "vhost": "/",
    "durable": true,
    "auto_delete": false,
    "arguments": {}
  },
  {
    "name": "orders_invoicer.retry",
    "vhost": "/",
    "durable": true,
    "auto_delete": false,
    "arguments": {
      "x-queue-type": "quorum"
    }
  },
  {
    "name": "orders.poison",
    "vhost": "/",
    "durable": true,
    "auto_delete": false,
    "arguments": {
      "x-queue-type": "quorum"
    }
  }
],
```

The delay is a policy. `orders_invoicer.retry` holds a parked order for 10s,
then dead-letters it through the default exchange back to `orders_invoicer`,
and to no other queue bound to `orders`:

```json
"definition": {
  "message-ttl": 10000,
  "dead-letter-exchange": "",
  "dead-letter-routing-key": "orders_invoicer",
  "dead-letter-strategy": "at-least-once",
  "overflow": "reject-publish"
}
```

The service's login needs no configure permission, only write on the three
exchanges it publishes to and read on its queue:

```sh
rabbitmqctl set_permissions --vhost / invoicer '^$' '^(invoices|orders\.poison|orders_invoicer\.retry)$' '^orders_invoicer$'
```

## Step 4: retries and dead letters

The Watermill service retries an order in place, three times in under a
second, holding its queue, and then poisons it. The humus service never
holds its queue or crashes on an order:

| The order | Then |
|---|---|
| not an order | a dead letter, at once |
| invoiced | acked |
| its invoice failed, below `RETRY_ATTEMPTS` | parked in `orders_invoicer.retry`, one attempt higher |
| the same, at `RETRY_ATTEMPTS` | a dead letter |
| neither `orders_invoicer.retry` nor `orders.poison` takes it | `Process` returns the error, and the App exits 1 |

A failed invoice is parked, with its attempt and why:

```go
func (i *invoicer) retryLater(ctx context.Context, m rabbitmq.Message, attempt int, cause error) error {
	failed := attempt + 1
	if failed >= i.attempts {
		return i.deadLetter(ctx, m, cause, failed)
	}

	headers := orderHeaders(m)
	headers[headerAttempt] = strconv.Itoa(failed)
	headers[headerError] = cause.Error()
	err := i.publisher.Publish(ctx, rabbitmq.Publishing{Exchange: retryExchange, Headers: headers, Body: m.Body})
	if err != nil {
		return fmt.Errorf("parking the order in %s: %w", retryExchange, err)
	}
	return nil
}
```

The parked order comes back after the policy's `message-ttl`, so the
`Processor` doesn't wait. A dead letter keeps the order's body and its
headers, but for the broker's `x-` headers and the retry's, and adds why. It
goes to `orders.poison`, the poison queue's exchange, so whatever is bound to
it keeps getting them:

```go
func (i *invoicer) deadLetter(ctx context.Context, m rabbitmq.Message, cause error, attempts int) error {
	headers := orderHeaders(m)
	headers[headerDeadError] = cause.Error()
	if attempts > 0 {
		headers[headerDeadAttempts] = strconv.Itoa(attempts)
	}
	err := i.publisher.Publish(ctx, rabbitmq.Publishing{Exchange: deadLetterExchange, Headers: headers, Body: m.Body})
	if err != nil {
		return fmt.Errorf("dead-lettering the order to %s: %w", deadLetterExchange, err)
	}
	return nil
}
```

Every header it adds is a string, as Watermill's marshaler requires, so a
Watermill service that reads `orders.poison` keeps reading it.

## Delivery

Both services deliver at least once, so an order may be processed twice.
Both process `orders_invoicer`'s orders one at a time, in the order the
queue delivers them: Watermill prefetches one, and humus ten, with
`Concurrency` 1.

- **Watermill** acks an order once the Router has published its invoice.
  Its publisher doesn't wait for the broker's confirm, so an invoice the
  broker drops is lost: one no queue takes, or one a full queue refuses. A
  publish that fails nacks the order, and the broker hands it back at once,
  until the publish succeeds.
- **humus**, with `queue.AtLeastOnce`, the default, acks an order once
  `Process` has returned `nil` for it: once the broker has confirmed its
  invoice, its parked copy or its dead letter, as
  [Delivery](/humus/services/rabbitmq/#delivery) describes. A
  crash, or a lost connection, delivers again what wasn't acked.

## Step 5: test it

The cases run against the whole humus service, with `humustest.Start` and
`humus.OSHost()`, against RabbitMQ in a container that has imported the
definitions file. A policy of a higher priority makes the test's delay 1s:

```go
r.ctl(t, "import_definitions", definitionsFile)
r.ctl(t, "set_policy", "--vhost", "/", "--priority", "1", "--apply-to", "queues", "tests.orders_invoicer.retry", `^orders_invoicer\.retry$`, retryPolicy)
```

```go
func TestInit_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			s := start(t)
			c.Run(t, s.target)
			s.stop(t)
		})
	}
}
```

Its other container tests park an order whose invoice no queue takes, twice,
then dead-letter it, and invoice a parked order once a queue is bound to
`invoices`. The module's
[`go.mod`](https://github.com/z5labs/humus/blob/main/example/rabbitmq/watermill/humus/go.mod)
requires neither Watermill nor watermill-amqp.

What clients see change:

- **An invoice** carries a `traceparent` header and no
  `_watermill_message_uuid`.
- **An invoice no queue takes, or the broker refuses,** isn't lost. The
  order is parked and tried again after the delay, and after
  `RETRY_ATTEMPTS` it is a dead letter.
- **An order that isn't one** is a dead letter at once, rather than after
  three retries.
- **A dead letter** carries `dlq.error`, and `dlq.attempts` once its
  attempts ran out, where the poison queue wrote `reason_poisoned`,
  `topic_poisoned`, `handler_poisoned` and `subscriber_poisoned`. The
  `orders.poison` queue keeps a copy of each.
- **The probes** answer on `PROBES_ADDR`.
