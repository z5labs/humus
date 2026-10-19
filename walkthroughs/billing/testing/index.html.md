# Testing

## A store in memory

The unit tests give `Run` an `Invoices` of their own, which keeps the
subscriptions and invoices in memory:

```go
type store struct {
	mu            sync.Mutex
	subscriptions []subscription
	invoices      []*invoice
	issueErr      error
	settleErr     error
}
```

Like PostgreSQL's, it settles only an invoice still pending:

```go
func (s *store) settle(id uuid.UUID, set func(*invoice)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settleErr != nil {
		return false, s.settleErr
	}
	for _, inv := range s.invoices {
		if inv.ID == id && inv.state == "pending" {
			set(inv)
			return true, nil
		}
	}
	return false, nil
}
```

The real store is checked against the same interface:

```go
var _ billing.Invoices = (*postgres.Store)(nil)
```

## A payments API of the test's own

A handler records each charge's key and account, then answers as the test
needs:

```go
type api struct {
	mu     sync.Mutex
	sent   []sent
	answer func(r *http.Request, account string, w http.ResponseWriter)
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	a.sent = append(a.sent, sent{idempotencyKey: r.Header.Get("Idempotency-Key"), account: body.Account})
	a.mu.Unlock()
	a.answer(r, body.Account, w)
}
```

Most tests answer as WireMock does:

```go
func charging(r *http.Request, account string, w http.ResponseWriter) {
	if strings.HasPrefix(account, "declined-") {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"code":"card_declined"}`)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"id":"ch_`+r.Header.Get("Idempotency-Key")+`"}`)
}
```

## On the test's Host

The handler serves HTTPS on a humustest `Host`, with a certificate from a
humustest `CA`:

```go
func newEnv(t *testing.T, answer func(*http.Request, string, http.ResponseWriter)) *env {
	t.Helper()
	e := &env{
		host:  humustest.NewHost(t),
		ca:    humustest.NewCA(t),
		store: &store{},
		api:   &api{answer: answer},
		tel:   humustest.NewTelemetry(),
		key:   rand.Text(),
	}
	l, err := e.host.Listen("tcp", ":8443")
	require.NoError(t, err)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	srv := &http.Server{
		Handler:   e.api,
		Protocols: &protocols,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{e.ca.Issue(t, "payments").TLS()}},
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		require.ErrorIs(t, <-done, http.ErrServerClosed)
	})
	return e
}
```

The run's payments client dials `payments:8443` through that `Host`, and
trusts that `CA`:

```go
func (e *env) run(t *testing.T, period time.Time, concurrency int) billing.Run {
	t.Helper()
	u, err := url.Parse("https://payments:8443")
	require.NoError(t, err)
	settings := httpclient.DefaultSettings()
	settings.BaseURL = u
	settings.TLS = &tls.Config{RootCAs: e.ca.Pool()}
	sdk := e.sdk(t)
	client, err := payments.NewClient(&humus.Setup{Host: e.host.Host(), Telemetry: sdk}, settings, e.key)
	require.NoError(t, err)
	return billing.Run{Invoices: e.store, Payments: client, Period: period, Concurrency: concurrency, Telemetry: sdk}
}
```

## A unit test of the run

Each test runs inside `synctest`, whose clock starts on 1 January 2000 in
UTC, so the run's default month is December 1999:

```go
func TestRun_a_month_billed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t, charging)
		e.subscribe("acct-1", "acct-2", "acct-3")
		e.store.subscriptions = append(e.store.subscriptions, subscription{accountID: "acct-inactive", amountCents: 999, currency: "USD", active: false})

		err := e.run(t, time.Time{}, 4).Run(t.Context())

		require.NoError(t, err)
		invoices := e.store.all()
		require.Len(t, invoices, 3)
		for _, inv := range invoices {
			require.Equal(t, month(1999, time.December), inv.Period, "the month before the run started, in UTC")
			require.Equal(t, "charged", inv.state)
			require.Equal(t, "ch_"+inv.ID.String(), inv.chargeID)
			to := e.api.to(inv.AccountID)
			require.Len(t, to, 1)
			require.Equal(t, inv.ID.String(), to[0].idempotencyKey)
		}
		require.Empty(t, e.api.to("acct-inactive"))
	})
}
```

The other unit tests cover a 422, a 500, a 401 and a 403, an API down until
the breaker opens, the bound on charges in flight, a failed statement, and an
answer lost to a signal.

```sh
go -C example/billing test -race ./...
```

## The shared cases

The cases are black-box: each one sets up PostgreSQL, runs the job, and
reads what it sent from WireMock's request journal. `Target` is what they
need from a deployment:

```go
type Target struct {
	Postgres string
	Schema   string
	WireMock *url.URL
	Client   *http.Client
	Key      string
	Run      func(ctx context.Context, databaseURL string, env map[string]string) (int, error)
	Timeout  time.Duration
}
```

Each case runs against a `Target`:

```go
type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var All = []Case{
	{Name: "BillingAMonth", Run: BillingAMonth},
	{Name: "RunningAgain", Run: RunningAgain},
	{Name: "ADeclinedCard", Run: ADeclinedCard},
	{Name: "ABadSetting", Run: ABadSetting},
	{Name: "ThePaymentsAPIIsDown", Run: ThePaymentsAPIIsDown},
	{Name: "ThePaymentsAPIIsSlow", Run: ThePaymentsAPIIsSlow},
	{Name: "ChargedButNotRecorded", Run: ChargedButNotRecorded},
	{Name: "TheMonthCantBeIssued", Run: TheMonthCantBeIssued},
}
```

## A case

Each case creates a database of its own, applies `Schema` to it, and drops it
at the end. Its account IDs start with the database's name:

```go
func (db database) account(suffix string) string {
	return db.name + "-" + suffix
}
```

So the cases share one WireMock, run in parallel, and each reads only its
own charges from the journal:

```go
query, err := json.Marshal(map[string]any{
	"method": http.MethodPost,
	"url":    "/v1/charges",
	"bodyPatterns": []any{
		map[string]any{"matchesJsonPath": map[string]any{"expression": "$.account", "matches": "^" + db.name + "-.*"}},
	},
})
```

Billing a month:

```go
func BillingAMonth(t testing.TB, target Target) {
	ctx := bounded(t, target)
	db := newDatabase(ctx, t, target, "billing-")
	active := db.subscribe(ctx, t, "a", "b", "c")
	inactive := db.unsubscribed(ctx, t, "gone")

	require.Equal(t, 0, db.run(ctx, t, target, period))

	invoices := db.invoices(ctx, t)
	require.Equal(t, active, accounts(invoices))
	for _, inv := range invoices {
		require.Equal(t, period, inv.Period)
		require.Equal(t, "charged", inv.State)
		require.Equal(t, "ch_"+inv.ID, inv.ChargeID)
	}
	charges := db.journal(ctx, t, target)
	requireSentOnce(t, target, invoices, charges)
	for _, c := range charges {
		require.NotEqual(t, inactive, c.account(t))
	}
}
```

## In containers

The `testcontainers` test runs the cases against PostgreSQL and WireMock in
containers, with WireMock serving the kind overlay's mappings:

```go
func TestInit_passes_the_shared_cases_against_PostgreSQL_and_WireMock(t *testing.T) {
	pg, postgresURL := startPostgres(t)
	host, err := pg.Host(t.Context())
	require.NoError(t, err)
	ca := humustest.NewCA(t)
	wiremock := startWireMock(t, ca, host)
	schema, err := os.ReadFile(filepath.Join("..", "postgres", "schema.sql"))
	require.NoError(t, err)

	dir := t.TempDir()
	key := rand.Text()
	target := cases.Target{
		Postgres: postgresURL,
		Schema:   string(schema),
		WireMock: wiremock,
		Client: &http.Client{
			Timeout:   waitLimit,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}},
		},
		Key: key,
		Run: runner(t, dir, map[string]string{
			"PAYMENTS_URL":          wiremock.String(),
			"PAYMENTS_API_KEY_FILE": writeFile(t, dir, "api-key", []byte(key+"\n")),
			"PAYMENTS_CA_FILE":      writeFile(t, dir, "payments-ca.pem", caPEM(t, ca)),
		}),
		Timeout: waitLimit,
	}

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})
}
```

Its `Run` starts the whole App, `Init` and `Options()`, on the OS's Host:

```go
p, status := humustest.Start(t, app.Init,
	humustest.WithApp(app.Options()...),
	humustest.WithApp(humus.WithHost(humus.OSHost())),
	humustest.WithEnv(all),
)
```

```sh
go -C example/billing test -race -tags testcontainers ./app/
```

## On kind

The `functional` test runs the same cases against the deployed image. Its
`Run` makes a Job from the deployed CronJob's template, with the case's
variables added:

```go
func (d deployment) newJob(env map[string]string) (*batchv1.Job, error) {
	tmpl := d.cronJob.Spec.JobTemplate
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        d.cronJob.Name + "-" + strings.ToLower(rand.Text()),
			Namespace:   d.cronJob.Namespace,
			Labels:      maps.Clone(tmpl.Labels),
			Annotations: maps.Clone(tmpl.Annotations),
		},
		Spec: *tmpl.Spec.DeepCopy(),
	}
	c, err := billingContainer(&job.Spec.Template.Spec)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(env)) {
		setEnv(c, name, env[name])
	}
	return job, nil
}
```

The Job gets no retries, so the case reads its one pod's exit code:

```go
var noRetries int32
job.Spec.BackoffLimit = &noRetries
```

```go
func TestBilling_passes_the_shared_cases_as_deployed(t *testing.T) {
	require.NotEmpty(t, *postgresURL, "-postgres")
	require.NotEmpty(t, *wiremockURL, "-wiremock")
	d := deployed(t)
	target := d.target(t)

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				own, ownTarget := d, target
				own.t = t
				ownTarget.Run = own.run
				c.Run(t, ownTarget)
			})
		}
	})
}
```

Once the job is
[deployed](/humus/walkthroughs/billing/deploying/), `test.sh`
forwards the ports and runs them:

```sh
example/billing/deploy/kind/test.sh
```

## The failure injection cases

Each one runs the job under a scenario in `BAGGAGE`, encoded as `Faults`
decodes it:

```go
func rehearsing(t testing.TB, rule fit.Rule) map[string]string {
	t.Helper()
	codec, err := jsonscenario.New("acme.fault")
	require.NoError(t, err)
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: []fit.Rule{rule}})
	require.NoError(t, err)
	return map[string]string{"BILLING_PERIOD": period, "BAGGAGE": b.String()}
}
```

The payments API is down:

```go
func ThePaymentsAPIIsDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	db := newDatabase(ctx, t, target, "billing-")
	db.subscribe(ctx, t, "a", "b", "c")

	require.Equal(t, exitFailure, db.runWith(ctx, t, target, rehearsing(t, fit.Rule{Point: "http:*", Fault: fit.Fault{Abort: true}})))

	invoices := db.invoices(ctx, t)
	require.Len(t, invoices, 3)
	for _, inv := range invoices {
		require.Equal(t, "pending", inv.State)
	}
	require.Empty(t, db.journal(ctx, t, target))

	require.Equal(t, 0, db.run(ctx, t, target, period))

	invoices = db.invoices(ctx, t)
	requireCharged(t, invoices)
	requireSentOnce(t, target, invoices, db.journal(ctx, t, target))
}
```

Charged, but not recorded. One charge is in flight at a time, so exactly one
is sent before its aborted settle stops the run:

```go
func ChargedButNotRecorded(t testing.TB, target Target) {
	ctx := bounded(t, target)
	db := newDatabase(ctx, t, target, "billing-")
	db.subscribe(ctx, t, "a", "b", "c")
	env := rehearsing(t, fit.Rule{Point: "postgresql:settle", Fault: fit.Fault{Abort: true}})
	env["BILLING_CONCURRENCY"] = "1"

	require.Equal(t, exitFailure, db.runWith(ctx, t, target, env))

	invoices := db.invoices(ctx, t)
	require.Len(t, invoices, 3)
	for _, inv := range invoices {
		require.Equal(t, "pending", inv.State)
	}
	first := db.journal(ctx, t, target)
	require.Len(t, first, 1, "one invoice sent before its settle was aborted")
	key := first[0].header("Idempotency-Key")
	require.Contains(t, ids(invoices), key)

	require.Equal(t, 0, db.run(ctx, t, target, period))

	invoices = db.invoices(ctx, t)
	requireCharged(t, invoices)
	want := make(map[string]int, len(invoices))
	for _, inv := range invoices {
		want[inv.ID] = 1
	}
	want[key] = 2
	requireSent(t, target, invoices, db.journal(ctx, t, target), want)
}
```

| Case | Scenario | The run | The next run |
|---|---|---|---|
| `ThePaymentsAPIIsDown` | abort `http:*` | exits 1, sends nothing | exits 0, sends each invoice once |
| `ThePaymentsAPIIsSlow` | delay `http:*` by 2s | exits 0, charges everything | |
| `ChargedButNotRecorded` | abort `postgresql:settle` | exits 1, sends one, leaves all pending | exits 0, sends that one again with the same key |
| `TheMonthCantBeIssued` | abort `postgresql:issue` | exits 1, issues and sends nothing | exits 0, bills the month |
