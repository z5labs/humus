# Testing

## A unit test of the projector

The projector takes the store and its `Producer`s as interfaces, so its
unit tests give it fakes. A store in memory holds what it was given, and
fails when the test says so:

```go
type memoryStore struct {
	mu      sync.Mutex
	events  []store.Event
	applied []applied

	appliedErrs []error
	eventsErrs  []error
	applyErrs   []error

	appliedErr error
	eventsErr  error

	appliedCalls int
	eventsCalls  int
	applyCalls   int
}
```

```go
func (s *memoryStore) Apply(ctx context.Context, e store.Event, status store.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyCalls++
	if err := next(&s.applyErrs); err != nil {
		return err
	}
	s.events = append(s.events, e)
	s.applied = append(s.applied, applied{event: e, status: status})
	return nil
}
```

A fake `Producer` records what it was given:

```go
type producer struct {
	mu      sync.Mutex
	records []kafka.Record
	times   []time.Time
	err     error
	block   bool
}

func (p *producer) Produce(ctx context.Context, records ...kafka.Record) error {
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.records = append(p.records, records...)
	for range records {
		p.times = append(p.times, time.Now())
	}
	return nil
}
```

Each test runs inside `synctest`, processes records, and checks what the
store and each `Producer` were given:

```go
func TestProcess_ProducesEachChangeThenStoresTheEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &memoryStore{}
		status := &producer{}
		dlq := &producer{}
		p := newProjector(t, s, status, &producer{}, dlq, settings)

		require.NoError(t, p.Process(t.Context(), eventMessage(10, projector.Placed, 0)))
		require.NoError(t, p.Process(t.Context(), eventMessage(11, projector.Paid, 1)))
		require.NoError(t, p.Process(t.Context(), eventMessage(12, projector.Shipped, 2)))

		require.Equal(t, []statusChange{
			{OrderID: "order-1", Status: projector.Placed, Previous: "", At: start},
			{OrderID: "order-1", Status: projector.Paid, Previous: projector.Placed, At: start.Add(time.Minute)},
			{OrderID: "order-1", Status: projector.Shipped, Previous: projector.Paid, At: start.Add(2 * time.Minute)},
		}, changes(t, status.records))
		require.Equal(t, []applied{
			{event: storedEvent(10, projector.Placed, 0), status: store.Status{Status: projector.Placed, At: start}},
			{event: storedEvent(11, projector.Paid, 1), status: store.Status{Status: projector.Paid, At: start.Add(time.Minute)}},
			{event: storedEvent(12, projector.Shipped, 2), status: store.Status{Status: projector.Shipped, At: start.Add(2 * time.Minute)}},
		}, s.applied)
		require.Empty(t, dlq.records)
	})
}
```

```sh
go -C example/orderstatus test ./...
```

## The store, against PostgreSQL

Behind the `testcontainers` tag, the store's tests run its SQL against
PostgreSQL in a container:

```go
func startPostgres(ctx context.Context, opts ...testcontainers.ContainerCustomizer) (*tcpostgres.PostgresContainer, error) {
	opts = append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
	}, opts...)
	return tcpostgres.Run(ctx, postgresImage, opts...)
}
```

```sh
go -C example/orderstatus test -tags testcontainers ./store/
```

## The shared cases

The cases are the package `cases`, which imports neither the App nor
humustest, so the compiler keeps them black-box. A `Target` says how to
reach the processor:

```go
type Target struct {
	Kafka []kgo.Opt

	EventsTopic     string
	StatusTopic     string
	RetryTopic      string
	DeadLetterTopic string
	Group           string

	Scenario    jsonscenario.Codec
	ScenarioKey string

	RetryAttempts int
	RetryMinDelay time.Duration
	RetryMaxDelay time.Duration

	Timeout time.Duration
	Rollout func(ctx context.Context) error
}
```

| Field | What it is |
|---|---|
| `Kafka` | the client options that reach the cluster |
| `EventsTopic`, `StatusTopic`, `RetryTopic`, `DeadLetterTopic`, `Group` | the processor's topics and its consumer group |
| `Scenario`, `ScenarioKey` | the codec the processor decodes a scenario with, and its baggage member |
| `RetryAttempts`, `RetryMinDelay`, `RetryMaxDelay` | the processor's retry settings, so a case knows how many retries to expect and how long they take |
| `Timeout` | how long a case may take, on top of its retries' backoff |
| `Rollout` | replaces every replica of the processor. `Rollout` is skipped without it |

The cases run one at a time:

```go
type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "Projection", Run: Projection},
	{Name: "Redelivery", Run: Redelivery},
	{Name: "DeadLetters", Run: DeadLetters},
	{Name: "OutOfOrder", Run: OutOfOrder},
	{Name: "ALateEvent", Run: ALateEvent},
	{Name: "Rollout", Run: Rollout},
	{Name: "SlowPostgreSQL", Run: SlowPostgreSQL},
	{Name: "SlowStatusTopic", Run: SlowStatusTopic},
	{Name: "PostgreSQLDown", Run: PostgreSQLDown},
}
```

A case produces an order's events, waits for the processor to settle, and
checks the order's changes on `orders.status`:

```go
func Projection(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	order := newOrderID()
	at := r.at(0)

	r.produce(ctx, t, event(order, placed, at), event(order, paid, r.at(1)), event(order, shipped, r.at(2)))
	got := r.settle(ctx, t)

	require.Equal(t, []change{
		{OrderID: order, Status: placed, At: at},
		{OrderID: order, Status: paid, Previous: placed, At: r.at(1)},
		{OrderID: order, Status: shipped, Previous: paid, At: r.at(2)},
	}, got.statuses(t, order))
	require.Empty(t, got.deadLetters(order))
}
```

Each order has an ID of its own, so a case reads only its own records:

```go
func newOrderID() string {
	return "order-" + rand.Text()
}
```

## The barrier

The processor commits a record only once it has processed it. So a case
waits until the processor's group has committed every record it produced,
and every one retried for it on `orders.retry`. Then what `orders.status`,
`orders.retry` and `orders.dlq` hold for its orders is final:

```go
func (r *run) settle(ctx context.Context, t testing.TB) results {
	t.Helper()
	for {
		committed, err := r.admin.FetchOffsets(ctx, r.target.Group)
		if err == nil {
			err = committed.Error()
		}
		require.NoError(t, err, "fetching %s's committed offsets", r.target.Group)

		retried := r.read(ctx, t, r.target.RetryTopic)
		pending := r.uncommitted(committed, r.produced) + r.uncommitted(committed, retried)
		if pending == 0 {
			return results{
				retry:  retried,
				status: r.read(ctx, t, r.target.StatusTopic),
				dead:   r.read(ctx, t, r.target.DeadLetterTopic),
			}
		}
		pause(ctx, t, fmt.Sprintf("%s to commit %d of this case's records", r.target.Group, pending))
	}
}
```

```go
func (r *run) uncommitted(committed kadm.OffsetResponses, records []*kgo.Record) int {
	n := 0
	for _, rec := range records {
		o, ok := committed.Lookup(rec.Topic, rec.Partition)
		if !ok || o.At <= rec.Offset {
			n++
		}
	}
	return n
}
```

## The cases, in process

The App runs inside `synctest` on humustest's Host, against
[kfake](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kfake) over mTLS:

```go
func (p *processor) cluster(t *testing.T, opts ...kfake.Opt) *kfake.Cluster {
	t.Helper()
	c, err := kfake.NewCluster(append([]kfake.Opt{
		kfake.ListenFn(p.h.Listen),
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, eventsTopic, statusTopic, retryTopic, deadLetterTopic),
		kfake.TLS(&tls.Config{
			Certificates: []tls.Certificate{p.ca.Issue(t, "127.0.0.1").TLS()},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    p.ca.Pool(),
			MinVersion:   tls.VersionTLS12,
		}),
	}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(c.Close)
	p.env["KAFKA_BROKERS"] = strings.Join(c.ListenAddrs(), ",")
	return c
}
```

The test starts it with a store in memory in place of PostgreSQL's:

```go
	init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
		return initWith(ctx, s, p.store.open)
	}
```

It retries quickly, and trusts the scenarios on both topics it consumes:

```go
var casesEnv = map[string]string{
	"RETRY_ATTEMPTS":       "3",
	"RETRY_MIN_DELAY":      "100ms",
	"RETRY_MAX_DELAY":      "400ms",
	"FAULT_TRUSTED_TOPICS": eventsTopic + "," + retryTopic,
}
```

Each case runs against an App of its own, on a cluster of its own:

```go
func TestInit_passes_the_shared_cases(t *testing.T) {
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newProcessor(t, nil)
				cl := p.cluster(t, kfake.SeedTopics(3, eventsTopic, statusTopic, retryTopic, deadLetterTopic))
				r := &rollout{t: t, p: p, c: cl, current: p.startForCases(t, probesAddr), probes: []string{"localhost:8444", probesAddr}}

				c.Run(t, cases.Target{
					Kafka:           []kgo.Opt{kgo.SeedBrokers(cl.ListenAddrs()...), kgo.Dialer(p.dial)},
					EventsTopic:     eventsTopic,
					StatusTopic:     statusTopic,
					RetryTopic:      retryTopic,
					DeadLetterTopic: deadLetterTopic,
					Group:           group,
					Scenario:        codec,
					ScenarioKey:     "acme.fault",
					RetryAttempts:   3,
					RetryMinDelay:   100 * time.Millisecond,
					RetryMaxDelay:   400 * time.Millisecond,
					Timeout:         time.Minute,
					Rollout:         r.run,
				})

				r.current.stop(t)
			})
		})
	}
}
```

`Rollout` starts a second App and waits for the group to hold both, then
stops the first, as a rolling update does:

```go
func (r *rollout) run(ctx context.Context) error {
	release := pauseFetches(r.c)
	defer release()
	synctest.Wait()

	next := r.p.startForCases(r.t, r.probes[0])
	r.probes = append(r.probes[1:], r.probes[0])
	if err := r.await(ctx, next, 2); err != nil {
		return err
	}

	r.current.signal(syscall.SIGTERM)
	var st bedrock.ExitStatus
	select {
	case st = <-r.current.status:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	r.current = next
	if st.Code() != 0 {
		return fmt.Errorf("the replaced App exited %d: %w", st.Code(), st.Cause())
	}
	return r.await(ctx, next, 1)
}
```

```sh
go -C example/orderstatus test ./app/
```

## The cases, on kind

Behind the `functional` tag, the same cases run against the kind overlay
that `up.sh` brings up, which [Deploying](/humus/pr-preview/pr-1237/walkthroughs/orderstatus/deploying/)
covers:

```go
func TestOrderStatus_passes_the_shared_cases_on_kind(t *testing.T) {
	kubectl(t, "rollout", "status", deployment, "--timeout", "5m")

	target := sharedTarget(t)
	for _, c := range cases.All {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, target)
		})
	}
}
```

The `Target` takes the topics and the retry settings from the processor's
ConfigMap, so the cases expect what the Deployment runs with. It reaches
Kafka through `kubectl port-forward`, as the `cases` principal, whose
certificate is in a Secret:

```go
func sharedTarget(t *testing.T) cases.Target {
	t.Helper()
	env := settings(t)
	codec, err := jsonscenario.New(scenarioKey)
	require.NoError(t, err)

	return cases.Target{
		Kafka:           []kgo.Opt{kgo.SeedBrokers(broker), kgo.Dialer(dialer(t, forward(t, "kafka", 9093)))},
		EventsTopic:     env.get(t, "KAFKA_EVENTS_TOPIC"),
		StatusTopic:     env.get(t, "KAFKA_STATUS_TOPIC"),
		RetryTopic:      env.get(t, "KAFKA_RETRY_TOPIC"),
		DeadLetterTopic: env.get(t, "KAFKA_DEAD_LETTER_TOPIC"),
		Group:           env.get(t, "KAFKA_GROUP_ID"),
		Scenario:        codec,
		ScenarioKey:     scenarioKey,
		RetryAttempts:   env.attempts(t, "RETRY_ATTEMPTS"),
		RetryMinDelay:   env.duration(t, "RETRY_MIN_DELAY"),
		RetryMaxDelay:   env.duration(t, "RETRY_MAX_DELAY"),
		Timeout:         timeout,
		Rollout:         rollout,
	}
}
```

Kafka advertises itself as `kafka:9093`, so the dialer sends that address
to the port forward:

```go
	return func(ctx context.Context, network, host string) (net.Conn, error) {
		if host != broker {
			return nil, fmt.Errorf("no port forward to %s, only to %s", host, broker)
		}
		return d.DialContext(ctx, network, forwarded)
	}
```

`Rollout` restarts the Deployment:

```go
func rollout(ctx context.Context) error {
	if _, err := run(ctx, "rollout", "restart", deployment); err != nil {
		return err
	}
	_, err := run(ctx, "rollout", "status", deployment, "--timeout", "5m")
	return err
}
```

```sh
go -C example/orderstatus test -tags functional ./functional/
```

## The failure injection cases

A case rehearses a failure by putting a scenario in the event's `baggage`
header:

```go
func (r *run) rehearse(t testing.TB, rec *kgo.Record, rules ...fit.Rule) *kgo.Record {
	t.Helper()
	require.NotEmpty(t, r.target.ScenarioKey, "Target.ScenarioKey")
	b, err := r.target.Scenario.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err, "Target.Scenario")
	require.NotEmpty(t, b.Member(r.target.ScenarioKey).Key(), "Target.Scenario does not write Target.ScenarioKey")
	rec.Headers = append(rec.Headers, kgo.RecordHeader{Key: "baggage", Value: []byte(b.String())})
	return rec
}
```

```go
func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func delay(point string, d time.Duration) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Delay: d}}
}
```

| Case | Its scenario | What it shows |
|---|---|---|
| `SlowPostgreSQL` | delays the store 1s | the order is projected once, and its change carries no scenario |
| `SlowStatusTopic` | delays producing to `orders.status` 1s | the order is projected, and nothing is dead-lettered |
| `PostgreSQLDown` | aborts the store | each attempt is retried, scenario and all, then the event is dead-lettered `unavailable` without it, and the next event on its partition is projected |

`PostgreSQLDown`:

```go
func PostgreSQLDown(t testing.TB, target Target) {
	ctx, r := begin(t, target)
	down, next := newOrderID(), newOrderID()

	failing := r.produce(ctx, t, r.rehearse(t, event(down, placed, r.at(0)), abort(storePoint)))[0]
	following := r.produceTo(ctx, t, failing.Partition, event(next, placed, r.at(0)))[0]
	require.Greater(t, following.Offset, failing.Offset)
	got := r.settle(ctx, t)

	require.Empty(t, got.statuses(t, down))
	dead := got.deadLetters(down)
	requireDeadLetter(t, dead, failing, unavailable, target.RetryAttempts)
	require.False(t, r.carriesScenario(t, dead[0]), "the dead letter carries the scenario")
	retried := got.retried(down)
	require.Len(t, retried, target.RetryAttempts-1)
	for i, rec := range retried {
		require.Equal(t, strconv.Itoa(i+1), header(rec, "retry.attempt"))
		require.Equal(t, unavailable, header(rec, "retry.reason"))
		require.True(t, r.carriesScenario(t, rec), "retried copy %d has no scenario", i+1)
	}

	require.Equal(t, []change{{OrderID: next, Status: placed, At: r.at(0)}}, got.statuses(t, next))
	require.Empty(t, got.deadLetters(next))
}
```

## An untrusted scenario

The processor ignores a scenario on a topic `FAULT_TRUSTED_TOPICS` doesn't
name. kind trusts both topics, so this isn't a shared case. An in-process
test checks it:

```go
	testCases := map[string]map[string]string{
		"with no trusted topics":            nil,
		"with only the retry topic trusted": {"FAULT_TRUSTED_TOPICS": retryTopic},
	}
```

## A record that can go nowhere

A record that can go to neither `orders.retry` nor `orders.dlq` stops the
App, uncommitted. On kind, that is every replica crash-looping, so it isn't
a shared case. An in-process test refuses every produce, then starts the
App again once Kafka takes them:

```go
func TestInit_stops_on_a_record_that_can_go_to_neither_the_retry_nor_the_dead_letter_topic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newProcessor(t, nil)
		c := p.cluster(t)
		p.produce(t, c, event("order-1", projector.Placed, time.Now().UTC()))
		var refusing atomic.Bool
		refusing.Store(true)
		refuseProduce(c, &refusing)

		_, status := p.start(t, nil)
		st := <-status
		require.Equal(t, 1, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), "processing "+eventsTopic+", partition 0, offset 0")
		require.ErrorContains(t, st.Cause(), "retrying "+eventsTopic+", partition 0, offset 0")
		require.Empty(t, p.store.status("order-1"))
		require.Less(t, committed(c, eventsTopic), int64(1))

		refusing.Store(false)
		signal, status := p.start(t, nil)
		waitFor(t, "order-1 is projected by the second App", func() bool { return p.store.status("order-1") == projector.Placed })
		signal(syscall.SIGTERM)
		st = <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		require.Len(t, p.readAll(t, c, statusTopic), 1)
		require.Empty(t, p.readAll(t, c, retryTopic))
		require.Empty(t, p.readAll(t, c, deadLetterTopic))
		require.Equal(t, int64(1), committed(c, eventsTopic))
	})
}
```
