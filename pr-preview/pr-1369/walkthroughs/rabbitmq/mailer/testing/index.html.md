# Testing

## A unit test of the Processor

The worker takes the provider and its Publishers as interfaces:

```go
type Provider interface {
	Send(ctx context.Context, r mail.Request) (provider.Result, error)
}

type Publisher interface {
	Publish(ctx context.Context, m rabbitmq.Publishing) error
}
```

So its unit tests give it fakes. A fake Publisher records what it was
given, and fails when the test says so:

```go
type fakePublisher struct {
	mu        sync.Mutex
	fail      func(rabbitmq.Publishing) error
	published []rabbitmq.Publishing
}

func (p *fakePublisher) Publish(_ context.Context, m rabbitmq.Publishing) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, m)
	if p.fail != nil {
		return p.fail(m)
	}
	return nil
}
```

```go
func failing(exchange string, err error) func(rabbitmq.Publishing) error {
	return func(m rabbitmq.Publishing) error {
		if m.Exchange == exchange {
			return err
		}
		return nil
	}
}
```

A harness builds the worker on the fakes, with humustest's telemetry:

```go
w, err := worker.New(worker.Settings{
	Provider:    h.provider,
	Retries:     h.retries,
	Results:     h.results,
	DeadLetters: h.deadLetters,
	MaxAttempts: maxAttempts,
	User:        mailerUser,
	Telemetry:   sdk,
})
require.NoError(t, err)
h.worker = w
```

Each test runs inside `synctest`, processes one delivery, and checks what
each fake was given. A request whose event can't be published is parked:

```go
func TestProcess_parks_a_request_whose_event_cannot_be_published(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 5)
		h.results.fail = failing(mail.EventsExchange, errors.New("rabbitmq: the broker nacked the publish"))
		m := delivery(validBody, nil)

		err := h.worker.Process(t.Context(), m)

		require.NoError(t, err)
		require.Equal(t, []rabbitmq.Publishing{event(t, mail.SentKey, map[string]string{"provider_id": "msg_1"})}, h.results.all())
		require.Equal(t, []rabbitmq.Publishing{parked(m, 2)}, h.retries.all())
		require.Empty(t, h.deadLetters.all())
	})
}
```

Run them with `go test`:

```sh
go -C example/rabbitmq/mailer test ./...
```

## The shared cases

The cases are the package `cases`, which imports neither the App nor
humustest, so the compiler keeps them black-box. A `Target` says how to
reach the mailer:

```go
type Target struct {
	Addr     string
	Vhost    string
	TLS      *tls.Config
	Username string
	Password string

	WireMock *url.URL
	Client   *http.Client

	ProviderPoint    string
	ScenarioKey      string
	CloseConnections func(ctx context.Context) error
	Timeout          time.Duration
}
```

| Field | What it is |
|---|---|
| `Addr`, `Vhost`, `TLS` | the broker |
| `Username`, `Password` | the cases' login, which `MAIL_TRUSTED_PUBLISHERS` names |
| `WireMock`, `Client` | WireMock's URL, and a client to read its request journal |
| `ProviderPoint` | the failure-injection point of the mailer's calls to the provider |
| `ScenarioKey` | the baggage member the mailer decodes a scenario from |
| `CloseConnections` | closes every connection to the broker. `LostConnection` is skipped without it |
| `Timeout` | how long a case may take |

The cases come in two lists. Each of `Alone`'s runs by itself, since it
closes the broker's connections or opens a breaker every request goes
through:

```go
type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var Parallel = []Case{
	{Name: "Sent", Run: Sent},
	{Name: "SentOnce", Run: SentOnce},
	{Name: "Refused", Run: Refused},
	{Name: "Malformed", Run: Malformed},
	{Name: "SlowProvider", Run: SlowProvider},
	{Name: "UntrustedScenario", Run: UntrustedScenario},
}

var Alone = []Case{
	{Name: "LostConnection", Run: LostConnection},
	{Name: "ResultNotPublished", Run: ResultNotPublished},
	{Name: "ProviderDown", Run: ProviderDown},
}
```

A case publishes a request, waits for its outcome, and checks it. Then it
reads what WireMock was sent for the request's address:

```go
func Sent(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(t, target)
	r := newRequest("example.com")

	b.publish(ctx, t, r)
	event := b.outcome(ctx, t, r.messageID)

	require.Equal(t, sentKey, event.RoutingKey)
	require.Equal(t, r.messageID, event.MessageId)
	require.Equal(t, r.correlationID, event.CorrelationId)
	require.NotEmpty(t, providerID(t, event))

	sent := journal(ctx, t, target, r.to)
	require.Len(t, sent, 1)
	require.Equal(t, r.messageID, sent[0].header("Idempotency-Key"))
	require.Regexp(t, `^Bearer \S+$`, sent[0].header("Authorization"))
}
```

Each request goes to an address of its own, so cases that run at once read
only their own requests from the journal:

```go
func newRequest(domain string) request {
	to := strings.ToLower(rand.Text()) + "@" + domain
	return request{
		messageID:     rand.Text(),
		correlationID: rand.Text(),
		to:            to,
		body: map[string]string{
			"to":      to,
			"subject": "Your order has shipped",
			"text":    "It arrives on Tuesday.",
		},
	}
}
```

## The barrier

A case waits for the request's outcome, its event on `mail.events` or its
dead letter on `mail.dead`. The mailer publishes it last, so once it has
arrived, what WireMock's journal holds for the address is final.

Before it publishes, the case binds a queue of its own to each exchange:

```go
func watch(t testing.TB, target Target) *broker {
	t.Helper()
	conn := dial(t, target)

	ch, err := conn.Channel()
	require.NoError(t, err)
	events := bind(t, ch, eventsExchange, "#")
	dead := bind(t, ch, deadExchange, "")
	retry := bind(t, ch, retryExchange, sendKey)

	publishing, err := conn.Channel()
	require.NoError(t, err)
	require.NoError(t, publishing.Confirm(false))
	returns := publishing.NotifyReturn(make(chan amqp.Return, 1))

	return &broker{publishing: publishing, returns: returns, eventsIn: events, deadIn: dead, retryIn: retry}
}
```

Each queue is exclusive, so it goes away when the case's connection closes.
The case unbinds it first, since RabbitMQ nacks a publish routed to a queue
that goes away, and the mailer's Publishers count a nack against their
breakers:

```go
func bind(t testing.TB, ch *amqp.Channel, exchange, key string) <-chan amqp.Delivery {
	t.Helper()
	q, err := ch.QueueDeclare("", false, false, true, false, nil)
	require.NoError(t, err)
	require.NoError(t, ch.QueueBind(q.Name, key, exchange, false, nil))
	t.Cleanup(func() { _ = ch.QueueUnbind(q.Name, key, exchange, nil) })
	deliveries, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	require.NoError(t, err)
	return deliveries
}
```

It reads them with a plain loop, which works in both harnesses:

```go
func (b *broker) outcome(ctx context.Context, t testing.TB, messageID string) amqp.Delivery {
	t.Helper()
	b.await(ctx, t, func() bool { return len(b.outcomes(messageID)) > 0 }, "the outcome of %s", messageID)
	return b.outcomes(messageID)[0]
}
```

```go
func (b *broker) await(ctx context.Context, t testing.TB, done func() bool, format string, args ...any) {
	t.Helper()
	for !done() {
		select {
		case d, ok := <-b.eventsIn:
			require.True(t, ok, "the consumer of %s was cancelled", eventsExchange)
			b.seen = append(b.seen, d)
		case d, ok := <-b.deadIn:
			require.True(t, ok, "the consumer of %s was cancelled", deadExchange)
			b.seen = append(b.seen, d)
		case d, ok := <-b.retryIn:
			require.True(t, ok, "the consumer of %s was cancelled", retryExchange)
			b.seen = append(b.seen, d)
		case <-ctx.Done():
			require.FailNowf(t, "timed out", "waiting for "+format+": %v", append(args, context.Cause(ctx))...)
		}
	}
}
```

## The cases, in process

Behind the `testcontainers` tag, the App runs on `humus.OSHost()`, against
RabbitMQ and WireMock in containers. RabbitMQ imports `definitions.json`,
then a policy of the test's own, so a request is parked for 1s and
dead-lettered after 2 deliveries:

```go
const (
	sendPolicy = `{"delivery-limit":2,"dead-letter-exchange":"mail.dead","dead-letter-strategy":"at-least-once","overflow":"reject-publish"}`

	retryPolicy = `{"message-ttl":1000,"dead-letter-exchange":"mail","dead-letter-routing-key":"mail.send","dead-letter-strategy":"at-least-once","overflow":"reject-publish"}`
)
```

```go
r := &rabbitMQ{container: c, addr: addr}
r.ctl(t, "import_definitions", definitionsFile)
r.ctl(t, "set_policy", "--vhost", "/", "--priority", "1", "--apply-to", "queues", "tests.mail.send", `^mail\.send$`, sendPolicy)
r.ctl(t, "set_policy", "--vhost", "/", "--priority", "1", "--apply-to", "queues", "tests.mail.send.retry", `^mail\.send\.retry$`, retryPolicy)
return r
```

A helper generates the mailer's login and the cases', then starts the App
with `humustest.Start` and humustest's telemetry:

```go
func startSharedMailer(t *testing.T) *sharedMailer {
	t.Helper()
	broker := startRabbitMQ(t)
	provider := startWireMock(t)
	mailer := broker.addUser(t, `^$`, `^mail\.(retry|dead|events)$`, `^mail\.send$`)
	cased := broker.addUser(t, `^amq\.gen-.*$`, `^(mail|amq\.gen-.*)$`, `^(mail\.(events|dead|retry)|amq\.gen-.*)$`)

	ca := humustest.NewCA(t)
	dir := t.TempDir()
	probesAddr := freeAddr(t)
	tel := humustest.NewTelemetry()
	p, status := humustest.Start(t, app.Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(tel.Builder()), humus.WithFaults(app.Faults)),
		humustest.WithEnv(map[string]string{
			"RABBITMQ_ADDR":           broker.addr,
			"RABBITMQ_USERNAME":       mailer.username,
			"RABBITMQ_PASSWORD_FILE":  writeFile(t, dir, "rabbitmq-password", []byte(mailer.password+"\n")),
			"MAIL_FROM":               "mailer@example.com",
			"MAIL_TRUSTED_PUBLISHERS": cased.username,
			"PROVIDER_URL":            provider.String(),
			"PROVIDER_API_KEY_FILE":   writeFile(t, dir, "provider-api-key", []byte(rand.Text())),
			"PROBES_ADDR":             probesAddr,
			"PROBES_TLS_PEM_FILE":     writeFile(t, dir, "probes.pem", pemOf(t, ca.Issue(t, "127.0.0.1"))),
		}),
	)
```

Once the probes say it's ready, it returns the `Target`, with the App's
process and telemetry:

```go
return &sharedMailer{
	target: cases.Target{
		Addr:             broker.addr,
		Vhost:            "/",
		Username:         cased.username,
		Password:         cased.password,
		WireMock:         provider,
		Client:           &http.Client{Timeout: waitLimit},
		ProviderPoint:    "http:" + provider.Host,
		ScenarioKey:      "acme.fault",
		CloseConnections: broker.closeConnections,
		Timeout:          waitLimit,
	},
	tel:    tel,
	p:      p,
	status: status,
}
```

The test runs the cases against it, then stops the App:

```go
func TestInit_passes_the_shared_cases_against_RabbitMQ_and_WireMock(t *testing.T) {
	m := startSharedMailer(t)

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, m.target)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, m.target)
		})
	}

	stop(t, m.p, m.status)
}
```

Another runs `Parallel` four times over at once, then checks that the App
is still running, and that its Publishers' breakers counted calls but none
opened:

```go
select {
case st := <-m.status:
	require.FailNowf(t, "the mailer exited", "exit %d: %v", st.Code(), st.Cause())
default:
}
rm, err := m.tel.Metrics(t.Context())
require.NoError(t, err)
ms := metricsOf(rm, rabbitmqScope)
require.Positive(t, sumBy(ms["humus.circuit.calls"], "humus.circuit.outcome")["success"], "the Publishers' breakers counted no calls")
transitions := sumBy(ms["humus.circuit.transitions"], "humus.circuit.state")
require.Zero(t, transitions["open"], "a Publisher's breaker opened")
```

```sh
go -C example/rabbitmq/mailer test -tags testcontainers ./...
```

## The cases, on kind

Behind the `functional` tag, the same cases run against the kind overlay
that `up.sh` brings up, which [Deploying](/humus/pr-preview/pr-1369/walkthroughs/rabbitmq/mailer/deploying/)
covers:

```go
func TestMailer_passes_the_shared_cases_on_kind(t *testing.T) {
	kubectl(t, "rollout", "status", "deployment/mailer", "--timeout", "5m")

	target := sharedTarget(t)
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, target)
		})
	}
}
```

The `Target` reaches the broker and WireMock through `kubectl
port-forward`, and reads the cases' login from its Secret:

```go
func sharedTarget(t *testing.T) cases.Target {
	t.Helper()
	brokerCA := pool(t, "mailer-rabbitmq-ca")
	providerCA := pool(t, "mailer-provider-ca")
	broker := forward(t, "rabbitmq", 5671)
	provider := forward(t, "wiremock", 443)

	return cases.Target{
		Addr:     broker,
		Vhost:    "/",
		TLS:      &tls.Config{RootCAs: brokerCA, ServerName: "rabbitmq", MinVersion: tls.VersionTLS12},
		Username: secret(t, "cases-rabbitmq", "username"),
		Password: secret(t, "cases-rabbitmq", "password"),
		WireMock: &url.URL{Scheme: "https", Host: provider},
		Client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: providerCA, ServerName: "wiremock", MinVersion: tls.VersionTLS12},
			},
		},
		ProviderPoint:    providerPoint,
		ScenarioKey:      scenarioKey,
		CloseConnections: closeConnections,
		Timeout:          timeout,
	}
}
```

```go
providerPoint = "http:wiremock:443"
```

```sh
go -C example/rabbitmq/mailer test -tags functional ./functional/
```

## The failure injection cases

A case in a scenario publishes as the cases' login, with the scenario in
the request's baggage:

```go
func rehearsal(t testing.TB, target Target, rules ...fit.Rule) request {
	t.Helper()
	b, err := codec(t, target).Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	r := newRequest("example.com")
	r.userID = target.Username
	r.baggage = b.String()
	return r
}
```

| Case | Its scenario | What it shows |
|---|---|---|
| `SlowProvider` | delays the provider 2s | the request is sent, not parked, and its event carries no scenario |
| `UntrustedScenario` | aborts the provider, from a request with no `UserID` | the scenario is ignored, and the request is sent |
| `ProviderDown` | aborts the provider | each attempt is parked, scenario and all, then the request is dead-lettered `gave-up` without it, and WireMock was sent nothing |
| `LostConnection` | delays the provider 3s, while the broker closes every connection | every request WireMock was sent has the same `Idempotency-Key`, and a request published after it is sent too |
| `ResultNotPublished` | aborts publishing `mail.sent` | each attempt calls the provider and is parked, then the request is dead-lettered `gave-up`, and no `mail.sent` arrives |

`ProviderDown`:

```go
func ProviderDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	b := watch(t, target)
	r := rehearsal(t, target, abort(providerPoint(t, target)))

	b.publish(ctx, t, r)
	dead := b.outcome(ctx, t, r.messageID)

	attempts := b.gaveUp(ctx, t, target, r, dead)
	require.Empty(t, b.sent(r.messageID))
	require.Len(t, b.parked(r.messageID), attempts-1)

	require.Empty(t, journal(ctx, t, target, r.to))
}
```

## The delivery limit

A request that can go to neither `mail.retry` nor `mail.dead` stops the
App on each delivery. On kind, that is every replica crash-looping, so it
isn't a shared case. The in-process test restarts the App after each exit,
as the kubelet does, until the delivery limit dead-letters the request:

```go
t.Run("a request that can go nowhere is dead-lettered by its delivery limit", func(t *testing.T) {
	o := observe(t, broker, watcher, map[string]string{"mail.dead": ""})
	to := address()
	id := trusted.request(t, to, abort(providerPoint), abort(retryPoint))

	exits := 0
	exited := func(st bedrock.ExitStatus) {
		t.Helper()
		require.Equal(t, exitedOnError, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), id)
		exits++
	}
	for len(o.of(id, "mail.dead")) == 0 {
		require.Less(t, exits, 5, "the request was never dead-lettered")
		p, status := m.start(t)
		select {
		case st := <-status:
			exited(st)
			continue
		case d := <-o.in:
			o.seen = append(o.seen, d)
		case <-time.After(waitLimit):
			t.Fatal("the mailer neither exited nor dead-lettered the request")
		}
		select {
		case st := <-status:
			exited(st)
		case <-time.After(10 * time.Second):
			stop(t, p, status)
		}
	}

	require.Equal(t, 3, exits, "the delivery limit of 2 allows 3 deliveries")
	dead := o.of(id, "mail.dead")
	require.Len(t, dead, 1)
	require.Equal(t, "delivery_limit", dead[0].Headers["x-first-death-reason"])
	require.NotContains(t, dead[0].Headers, "x-mail-dead-reason")
	require.Empty(t, journal(t, client, provider, to))
})
```

Next: [Deploying](/humus/pr-preview/pr-1369/walkthroughs/rabbitmq/mailer/deploying/)
