---
title: Migrating from Watermill
linkTitle: Watermill
description: >
  How to move a Watermill v1 Kafka consumer to humus's queue/kafka: what each
  Watermill concept becomes, then its process, settings, telemetry, probes
  and shutdown on humus, its handler as a queue.Processor that produces
  through a kafka.Producer, and its retries and poison queue as a retry topic
  and a dead-letter topic. One set of test cases passes against both
  services.
type: docs
weight: 10
---

This guide moves a small [Watermill](https://watermill.io) v1 service on
Kafka to humus's `queue/kafka`. Its Router's handler becomes a
`queue.Processor`, and its publishing goes through a `kafka.Producer`. Its
process, settings, telemetry, probes and shutdown become humus's.

Watermill's Kafka backend,
[watermill-kafka](https://github.com/ThreeDotsLabs/watermill-kafka), is
built on Sarama, and `queue/kafka` is built on franz-go, so the client
changes with the Router. The guide's steps go from the Watermill service to
the humus service, rather than each leaving a service that runs.

## Watermill and humus

| Watermill | humus |
|---|---|
| `message.Router` and its `Run` | `humus.Main`, and an `Init` that returns the Service from `kafka.NewService`: [A consumer]({{< relref "/services/kafka#a-consumer" >}}) |
| `plugin.SignalsHandler`, and the Router's `CloseTimeout` | none: `humus.Main` stops the App on SIGTERM, and the consumer drains within a 20s grace period: [Shutdown and rebalances]({{< relref "/services/kafka#shutdown-and-rebalances" >}}) |
| `kafka.Subscriber` and its `ConsumerGroup` | `kafka.Settings`, with its `Brokers`, `GroupID` and `Topics`: [Settings]({{< relref "/services/kafka#settings" >}}) |
| `Consumer.Offsets.Initial = sarama.OffsetOldest` | none: a partition with no committed offset is consumed from its earliest record |
| Sarama's `Net.TLS` | `Settings.TLS` and `ProducerSettings.TLS`: [TLS and mTLS]({{< relref "/services/kafka#tls-and-mtls" >}}) |
| the handler, a `message.HandlerFunc` | a `queue.Processor`, whose `Process` gets a `kafka.Message`: [The contract]({{< relref "/services/kafka#the-contract" >}}) |
| `kafka.DefaultMarshaler`, and a message's payload and metadata | the record's key, value and headers, in `kafka.Message` and `kafka.Record`: [The contract]({{< relref "/services/kafka#the-contract" >}}) |
| a message's UUID, from `watermill.NewUUID` | none: a record is known by its topic, partition and offset, and an order by its key |
| `kafka.Publisher`, and the messages the handler returns | a `kafka.Producer`, from `kafka.NewProducer`, which the `Processor` calls before it returns: [Producing]({{< relref "/services/kafka#producing" >}}) |
| `middleware.Retry` | a retry topic, `orders.retry`, for what may pass later, tried again after `RETRY_DELAY`: [Retries and dead letters]({{< relref "/walkthroughs/kafka/orderstatus/retries" >}}) |
| `middleware.PoisonQueue` | a dead-letter topic, for what never will pass, and for what still fails after `RETRY_ATTEMPTS`: [Dead letters]({{< relref "/services/kafka#dead-letters" >}}) |
| `middleware.Recoverer` | none: a panicking `Processor` crashes the process, and its record is processed again after the restart. The `Processor` returns an error instead: [Retries and panics]({{< relref "/services/kafka#retries-and-panics" >}}) |
| `middleware.CorrelationID` | none: the `Processor` copies the `correlation_id` header to the invoice. Each produced record's span continues the trace of the record it came from: [The send span]({{< relref "/services/kafka#the-send-span" >}}) |
| `watermill.NewStdLogger` | the Setup's `LoggerProvider`: [The logs]({{< relref "/services/kafka#the-logs" >}}) |
| no traces or metrics | a span for each record processed and produced, with metrics: [Telemetry]({{< relref "/services/kafka#telemetry" >}}) |
| no probes | `httphealth.NewService`, ready once the consumer has joined its group: [Readiness]({{< relref "/services/kafka#readiness" >}}) |
| a test against a cluster | `humustest.Start` and kfake, inside `testing/synctest`: [Testing]({{< relref "/services/kafka#testing" >}}) |

## The Watermill service

The guide's code is under
[`example/kafka/watermill`](https://github.com/z5labs/humus/tree/main/example/kafka/watermill),
a module for each service:

```text
example/kafka/watermill/
├── cases/    the black-box test cases both services pass
├── start/    the Watermill service the guide starts from
└── humus/    the humus service it ends with
```

The service consumes orders from `orders` and publishes an invoice for each
to `invoices`. Its Router is built as
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

Its subscriber joins the group `invoicer`, from the earliest offset:

```go
func newSubscriber(brokers []string, logger watermill.LoggerAdapter) (*kafka.Subscriber, error) {
	saramaConfig := kafka.DefaultSaramaSubscriberConfig()
	saramaConfig.Consumer.Offsets.Initial = sarama.OffsetOldest

	return kafka.NewSubscriber(kafka.SubscriberConfig{
		Brokers:               brokers,
		Unmarshaler:           kafka.DefaultMarshaler{},
		OverwriteSaramaConfig: saramaConfig,
		ConsumerGroup:         consumerGroup,
	}, logger)
}
```

`main` runs the Router until SIGINT or SIGTERM:

```go
func main() {
	logger := watermill.NewStdLogger(false, false)
	brokers := strings.Split(os.Getenv("KAFKA_BROKERS"), ",")

	subscriber, err := newSubscriber(brokers, logger)
	if err != nil {
		panic(err)
	}
	publisher, err := newPublisher(brokers, logger)
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

The `cases` module holds black-box cases, written once. Each produces
orders, waits until the group has committed every record of the topics the
service consumes, and reads `invoices` and the dead-letter topic:

```go
type Target struct {
	Cluster *kfake.Cluster
	Kafka   []kgo.Opt

	Group    string
	Consumes []string

	OrdersTopic     string
	InvoicesTopic   string
	DeadLetterTopic string

	Timeout time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "AnOrder", Run: AnOrder},
	{Name: "NotOrders", Run: NotOrders},
	{Name: "ARefusedInvoice", Run: ARefusedInvoice},
}
```

One case has the cluster refuse the first two invoices, which neither
service may lose:

```go
func ARefusedInvoice(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	refused := target.Cluster.Fault(kfake.Fault{
		Keys:  []kmsg.Key{kmsg.Produce},
		Topic: target.InvoicesTopic,
		Err:   kerr.TopicAuthorizationFailed,
		Count: 2,
	})

	r.produce(ctx, t, order("order-5", "corr-5", `{"id":"order-5","customer":"ada","total":1200}`))
	got := r.settle(ctx, t)

	require.Equal(t, 2, refused.Hits())
	require.Len(t, got.invoices, 1)
	requireInvoice(t, got.invoices[0], "corr-5", `{"order_id":"order-5","customer":"ada","amount":1200}`)
	require.Empty(t, got.deadLetters)
}
```

Each case runs against a service of its own. The Watermill service runs them
against a kfake cluster on the loopback network:

```go
func TestRouter_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, ordersTopic, invoicesTopic, poisonTopic))
			require.NoError(t, err)
			t.Cleanup(cluster.Close)
			run(t, cluster.ListenAddrs())

			c.Run(t, cases.Target{
				Cluster:         cluster,
				Kafka:           []kgo.Opt{kgo.SeedBrokers(cluster.ListenAddrs()...)},
				Group:           consumerGroup,
				Consumes:        []string{ordersTopic},
				OrdersTopic:     ordersTopic,
				InvoicesTopic:   invoicesTopic,
				DeadLetterTopic: poisonTopic,
				Timeout:         30 * time.Second,
			})
		})
	}
}
```

## Step 1: main and its settings

`main` becomes one call to `humus.Main`:

```go
humus.Main(Init, humus.WithTelemetry(buildTelemetry))
```

`Init` builds the `Producer`, the consumer and its probes:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}

	producer, err := kafka.NewProducer(s, settings.Producer)
	if err != nil {
		return nil, err
	}
	consumer, err := kafka.NewService(s, settings.Consumer, &invoicer{producer: producer, retry: settings.Retry})
	if err != nil {
		return nil, err
	}
	probes, err := httphealth.NewService(s, settings.Probes, health.Readiness(consumer.Joined()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{consumer, probes}, nil
}
```

`readSettings` sources the brokers, the retries and the probes. The consumer
reads `orders` and `orders.retry`, and the `Producer` produces to the same
cluster:

```go
func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	list := func(v string) []string { return strings.Split(v, ",") }

	return config.Struct(func(r *config.Resolver) settings {
		consumer := kafka.DefaultSettings()
		consumer.Brokers = r.Get(env("KAFKA_BROKERS").Map(list).Validate(kafka.CheckBrokers))
		consumer.GroupID = consumerGroup
		consumer.Topics = []string{ordersTopic, retryTopic}
		consumer.Delivery = queue.AtLeastOnce

		producer := kafka.DefaultProducerSettings()
		producer.Brokers = consumer.Brokers

		return settings{
			Consumer: consumer,
			Producer: producer,
			Retry: retry{
				Attempts: r.Get(env("RETRY_ATTEMPTS").Parse(strconv.Atoi).Default(defaultRetryAttempts).Validate(positive[int])),
				Delay:    r.Get(env("RETRY_DELAY").Parse(time.ParseDuration).Default(defaultRetryDelay).Validate(positive[time.Duration])),
			},
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

From here on, these are humus's:

- **The process and its shutdown.** On SIGTERM the consumer stops taking
  records, lets each partition's record in flight finish, commits, and
  leaves the group, within a 20s grace period, as
  [Shutdown and rebalances]({{< relref "/services/kafka#shutdown-and-rebalances" >}})
  describes.
- **The settings.** A missing or bad one makes the service exit 78, naming
  its variable.
- **TLS.** The brokers stay plaintext, as they were. A cluster that serves
  TLS sets `Settings.TLS` and `ProducerSettings.TLS`.
- **Telemetry.** `buildTelemetry` names the service `invoicer`, as
  [Writing the Builder]({{< relref "/fundamentals/telemetry#writing-the-builder" >}})
  shows. Each record processed and produced is a span, with its metrics.
- **The probes.** The kubelet probes `/health/liveness` and
  `/health/readiness` on `PROBES_ADDR`, with `scheme: HTTPS`. The pod is
  ready once the consumer has joined its group.

## Step 2: the handler becomes a Processor

`issueInvoice` keeps the handler's code, and returns the invoice's value:

```go
func issueInvoice(value []byte) ([]byte, error) {
	var o order
	if err := json.Unmarshal(value, &o); err != nil {
		return nil, err
	}
	if o.ID == "" {
		return nil, errors.New("the order has no id")
	}
	return json.Marshal(invoice{OrderID: o.ID, Customer: o.Customer, Amount: o.Total})
}
```

The `Processor` produces the invoice itself, with the order's key and
correlation ID, before it returns. A record is committed only once
`Process` returns `nil`:

```go
func (i *invoicer) Process(ctx context.Context, m kafka.Message) error {
	attempt, err := attemptOf(m)
	if err != nil {
		return i.deadLetter(ctx, m, err, 0)
	}

	payload, err := issueInvoice(m.Value)
	if err != nil {
		return i.deadLetter(ctx, m, err, 0)
	}

	if err := i.wait(ctx, m, attempt); err != nil {
		return err
	}

	err = i.producer.Produce(ctx, kafka.Record{
		Topic:   invoicesTopic,
		Key:     m.Key,
		Value:   payload,
		Headers: correlationID(m),
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
func correlationID(m kafka.Message) []kafka.Header {
	for _, h := range m.Headers {
		if h.Key == headerCorrelationID {
			return []kafka.Header{h}
		}
	}
	return nil
}
```

## Step 3: retries and dead letters

The Watermill service retries a record in place, blocking its partition, and
then poisons it. The humus service never blocks or crashes on a record:

| The record | Then |
|---|---|
| not an order | a dead letter, at once |
| invoiced | committed |
| its invoice failed, below `RETRY_ATTEMPTS` | produced to `orders.retry`, one attempt higher |
| the same, at `RETRY_ATTEMPTS` | a dead letter |
| neither `orders.retry` nor the dead-letter topic stores it | `Process` returns the error, and the App exits 1 |

A failed invoice goes to `orders.retry`, with its attempt and why:

```go
func (i *invoicer) retryLater(ctx context.Context, m kafka.Message, attempt int, cause error) error {
	failed := attempt + 1
	if failed >= i.retry.Attempts {
		return i.deadLetter(ctx, m, cause, failed)
	}

	headers := make([]kafka.Header, 0, len(m.Headers)+2)
	for _, h := range m.Headers {
		if !strings.HasPrefix(h.Key, "retry.") {
			headers = append(headers, h)
		}
	}
	headers = append(headers,
		kafka.Header{Key: headerAttempt, Value: []byte(strconv.Itoa(failed))},
		kafka.Header{Key: headerError, Value: []byte(cause.Error())},
	)
	err := i.producer.Produce(ctx, kafka.Record{Topic: retryTopic, Key: m.Key, Value: m.Value, Headers: headers})
	if err != nil {
		return fmt.Errorf("retrying %s, partition %d, offset %d: %w", m.Topic, m.Partition, m.Offset, err)
	}
	return nil
}
```

A retried record waits until `RETRY_DELAY` after it was produced:

```go
func (i *invoicer) wait(ctx context.Context, m kafka.Message, attempt int) error {
	if attempt == 0 {
		return nil
	}
	t := time.NewTimer(time.Until(m.Timestamp.Add(i.retry.Delay)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}
```

A dead letter keeps the record's key, value and headers, and adds why. It
goes to `orders.poison`, the poison queue's topic, so whatever reads that
keeps reading it:

```go
func (i *invoicer) deadLetter(ctx context.Context, m kafka.Message, cause error, attempts int) error {
	headers := make([]kafka.Header, 0, len(m.Headers)+2)
	headers = append(headers, m.Headers...)
	headers = append(headers, kafka.Header{Key: "dlq.error", Value: []byte(cause.Error())})
	if attempts > 0 {
		headers = append(headers, kafka.Header{Key: "dlq.attempts", Value: []byte(strconv.Itoa(attempts))})
	}
	err := i.producer.Produce(ctx, kafka.Record{Topic: deadLetterTopic, Key: m.Key, Value: m.Value, Headers: headers})
	if err != nil {
		return fmt.Errorf("dead-lettering %s, partition %d, offset %d: %w", m.Topic, m.Partition, m.Offset, err)
	}
	return nil
}
```

## Delivery

Both services deliver at least once, so a record may be processed twice:

- **Watermill** marks an order's offset once the Router has published its
  invoice and acked it, and Sarama commits the marked offsets every second.
  A crash processes again what was acked since the last commit. A publish
  that fails is nacked, and the order is handed to the handler again every
  100ms until the publish succeeds, blocking its partition.
- **humus**, with `queue.AtLeastOnce`, commits each partition after a batch
  of records, once `Process` has returned `nil` for each: after its invoice,
  its retry or its dead letter is stored, as
  [Delivery]({{< relref "/services/kafka#delivery" >}}) describes. A crash
  processes again what was processed since the last commit.

## Step 4: test it

The cases run against the whole humus service, with `humustest.Start`,
inside `synctest`, on a kfake cluster that listens on the test's Host:

```go
cluster, err := kfake.NewCluster(
	kfake.ListenFn(h.Listen),
	kfake.NumBrokers(1),
	kfake.SeedTopics(1, ordersTopic, invoicesTopic, retryTopic, deadLetterTopic),
)
```

```go
func TestInit_passes_the_cases(t *testing.T) {
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				s := start(t, nil)
				c.Run(t, s.target)
				s.stop(t)
			})
		})
	}
}
```

The humus service consumes `orders.retry` too, so its `Target` names both
topics:

```go
Consumes:        []string{ordersTopic, retryTopic},
```

The module's
[`go.mod`](https://github.com/z5labs/humus/blob/main/example/kafka/watermill/humus/go.mod)
requires neither Watermill nor Sarama.

What clients see change:

- **An invoice** is keyed by its order's key, and carries a `traceparent`
  header and no `_watermill_message_uuid`.
- **An order that isn't one** is a dead letter at once, rather than after
  three retries.
- **A refused invoice** goes through `orders.retry`, so the orders after it
  are invoiced first. After `RETRY_ATTEMPTS` it is a dead letter, where
  Watermill handed it to the handler again until the publish succeeded.
- **A dead letter** carries `dlq.error`, and `dlq.attempts` once its
  attempts ran out, where the poison queue wrote `reason_poisoned`,
  `topic_poisoned`, `handler_poisoned` and `subscriber_poisoned`.
- **The probes** answer on `PROBES_ADDR`.
