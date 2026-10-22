# Testing

## The ledger's unit tests

The ledger's server takes a `Store`, so its tests give it a fake one, in
memory, that fails when the test says so:

```go
func (f *fakeStore) Account(_ context.Context, id string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return store.Account{}, fmt.Errorf("reading account %s: %w", id, f.err)
	}
	a, ok := f.accounts[id]
	if !ok {
		return store.Account{}, fmt.Errorf("reading account %s: %w", id, store.ErrNotFound)
	}
	return a, nil
}
```

Each test serves it on the humustest Host, over TLS, and calls it with the
ledger's generated client:

```go
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{ca.Issue(t, "ledger").TLS()},
	})))
	svc, err := server.New(st, watchInterval, tel)
	require.NoError(t, err)
	ledgerpb.RegisterLedgerServer(srv, svc)
```

Each test runs inside `synctest`, so the watch's polling takes no real
time:

```go
	t.Run("reads the account every watch interval", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			st := newFakeStore()
			c := serve(t, st)
			open(t, c, "alice")
			startWatch(t, c, t.Context(), "alice")
			synctest.Wait()
			require.Equal(t, 1, st.readCount())

			time.Sleep(watchInterval - time.Nanosecond)
			synctest.Wait()
			require.Equal(t, 1, st.readCount())

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			require.Equal(t, 2, st.readCount())

			time.Sleep(3 * watchInterval)
			synctest.Wait()
			require.Equal(t, 5, st.readCount())
		})
	})
```

The ledger's `InitWith` takes the `Store` the same way, so its `app` tests
run the whole App on a fake Store, and check who may call it:

```go
func TestInit_answers_a_call_without_a_client_certificate_Unauthenticated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newFakeStore(false)
		l := start(t, onFake(st))
		c := l.client(t, nil)

		_, err := c.OpenAccount(t.Context(), &ledgerpb.OpenAccountRequest{AccountId: "alice"})
		requireCode(t, err, codes.Unauthenticated)
		_, err = c.GetAccount(t.Context(), &ledgerpb.GetAccountRequest{AccountId: store.External})
		requireCode(t, err, codes.Unauthenticated)
```

## The Store

A scenario aborts each of the Store's methods before its transaction, on a
pool whose every dial fails, so no database is needed:

```go
func TestStore_an_abort_at_postgresql_ledger_fails_each_method_as_an_unreachable_database_does(t *testing.T) {
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			u := newUnreachable(t)

			err := m.call(u.under(t, t.Context(), fit.Rule{Point: "postgresql:ledger", Fault: fit.Fault{Abort: true}}), u.store)

			requireInjected(t, err)
			require.ErrorIs(t, err, fit.ErrInjected)
			require.Zero(t, u.dials.Load(), "an abort is evaluated before the transaction")
```

Behind the `testcontainers` tag, the Store runs against PostgreSQL in a
container. One server is shared by the package, and each test gets a
database and a role of its own, with a password generated on each run, and
`schema.sql` applied:

```go
	name := "ledger_" + strings.ToLower(rand.Text())
	password := rand.Text()
	ident := pgx.Identifier{name}.Sanitize()
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, ident, password))
	require.NoError(t, err)
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, ident, ident))
	require.NoError(t, err)
```

Twenty transfers at once from an account that can pay for five never
overdraw it:

```go
	var sent, short int
	for range sends {
		err := <-errs
		if err == nil {
			sent++
			continue
		}
		requireShort(t, err, "alice")
		short++
	}
	require.Equal(t, 5, sent)
	require.Equal(t, 15, short)
	require.Zero(t, l.balance(t, "alice"))
```

The rest show that a repeated ID moves money once, that transfers each way
between two accounts don't deadlock, and that the `CHECK` refuses a negative
balance:

```go
	_, err := l.pool.Exec(ctx, `UPDATE accounts SET balance_cents = -1 WHERE id = 'alice'`)
	requireCheck(t, err)
	_, err = l.pool.Exec(ctx, `INSERT INTO accounts (id, balance_cents) VALUES ('bob', -1)`)
	requireCheck(t, err)
	require.EqualValues(t, 100, l.balance(t, "alice"))
```

## The wallet's unit tests

The wallet's server tests serve a fake ledger on the humustest Host, and
start the wallet with its real `grpcclient` Client. The Host's `Dial` sends
the ledger's address to whichever replica the test routes it to:

```go
	host := e.h.Host()
	host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == ledgerAddr {
			address = *e.route.Load()
		}
		return e.h.Dial(ctx, network, address)
	}
```

So a test can start a second replica, route to it, and stop the first, and
check that the watch is relayed through the ledger's shutdown:

```go
	t.Run("relays the watch through the ledger's shutdown", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := newEnv(t)
			e.ledger.open("alice")
			w := watch(t, e, "alice")
			requireBalance(t, w, 0)

			e.startReplica("ledger-2:9090")
			e.routeTo("ledger-2:9090")
			e.stopReplica("ledger-1:9090")
			synctest.Wait()

			e.ledger.deposit("alice", 500)
			requireBalance(t, w, 500)
			e.ledger.deposit("alice", 250)
			requireBalance(t, w, 750)
			require.GreaterOrEqual(t, e.ledger.watchOpens(), 2)
		})
	})
```

The wallet's `app` tests run the whole App, its `Init` unchanged, beside a
fake ledger and the tests' key set:

```go
func start(t *testing.T) *wallet {
	t.Helper()
	f := newFixture(t, nil)
	f.startKeys(t)
	l, _ := f.startLedger(t)
	w := startWallet(t, f)
	w.ledger = l
	return w
}
```

```go
func TestInit_serves_a_token_with_the_scopes_each_method_needs_through_the_ledger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		c := w.client(t)
		ctx := w.as(t, "alice", server.ScopeRead, server.ScopeWrite)

		_, err := c.OpenAccount(ctx, &walletpb.OpenAccountRequest{})
		require.NoError(t, err)
		w.ledger.deposit("alice", 500)
```

`internal/certfile`'s tests rewrite the certificate's files under a
running server, and `walletctl`'s check that a token it signs is verified
against the key set it makes.

Run the unit tests with `go test`:

```sh
go -C example/wallet test ./...
```

## The key set, served by the tests

The wallet fetches its key set over HTTPS, so the tests serve one from a
handler of their own, made from a key each test generates:

```go
func (p *idp) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	down := p.down
	p.mu.Unlock()
	if down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       p.key.Public(),
		KeyID:     p.kid,
		Algorithm: string(jose.ES256),
		Use:       "sig",
	}}}
```

`httpserver` serves it, on the same Host as the wallet, so it runs inside
synctest too:

```go
	return humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := httpserver.DefaultSettings()
		settings.Addr = addr
		settings.TLS = &tls.Config{Certificates: []tls.Certificate{cert.TLS()}}
		mux := http.NewServeMux()
		mux.Handle("GET "+jwksPath, p)
		svc, err := httpserver.NewService(s, settings, mux)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
```

The same key signs the tests' tokens:

```go
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: p.key, KeyID: p.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
```

A test takes the handler down to see what the wallet answers when its key
set can't be fetched:

```go
func TestInit_answers_Unavailable_KEYS_UNAVAILABLE_while_the_key_set_cant_be_fetched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := start(t)
		c := w.client(t)
		w.ledger.deposit("alice", 0)
		w.f.idp.setDown(true)

		for _, m := range methods {
			err := m.call(w.as(t, "alice", server.ScopeRead, server.ScopeWrite), c)
			st := requireCode(t, err, codes.Unavailable)
```

## The two ledger replicas

A rollout replaces one replica at a time, so the shared cases run two
ledger Apps. Their dialer does what a ClusterIP Service does: it sends the
ledger's address to one of the replicas still in its endpoints, and refuses
the dial when there is none:

```go
func (n *network) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	s := n.services[address]
	n.mu.Unlock()
	if s == nil {
		return n.base(ctx, network, address)
	}
	endpoint, ok := s.pick()
	if !ok {
		return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	return n.base(ctx, network, endpoint)
}
```

A rollout takes each replica out of the endpoints, stops it with SIGTERM,
starts it again, and puts it back once it is ready:

```go
func (s *service) rollout(ctx context.Context) error {
	s.mu.Lock()
	replicas := slices.Clone(s.replicas)
	s.mu.Unlock()
	for _, r := range replicas {
		s.remove(r)
		if err := r.stop(ctx); err != nil {
			return err
		}
		r.run()
		if err := r.ready(ctx); err != nil {
			return err
		}
		s.add(r)
	}
	return nil
}
```

The wallet's Host dials through it, so the wallet's `Init` doesn't change:

```go
	n := newNetwork(pl.host.Dial)
	host := pl.host
	host.Dial = n.Dial
```

```go
	ledger := n.service(ledgerAddr)
	for i := range ledgerReplicas {
		r := &replica{name: fmt.Sprintf("ledger %d", i), addr: pl.addr(fmt.Sprintf("ledger-%d:9090", i))}
		probes := pl.addr(fmt.Sprintf("ledger-%d:8081", i))
		env := maps.Clone(ledgerEnv)
		env["GRPC_ADDR"] = r.addr
		env["PROBES_ADDR"] = probes
```

## The shared cases

The cases are the package `cases`, which imports neither App nor
humustest, so the compiler keeps them black-box. It imports `walletpb` and
`ledgerpb`, as any client would. A `Target` says how to reach both
services:

```go
type Target struct {
	Wallet grpc.ClientConnInterface

	Ledger                   grpc.ClientConnInterface
	LedgerWithoutCertificate grpc.ClientConnInterface

	Token func(subject string, scopes ...string) (string, error)

	RolloutWallet func(ctx context.Context) error
	RolloutLedger func(ctx context.Context) error

	ScenarioKey string
	TrustScope  string
	Timeout     time.Duration
}
```

| Field | What it is |
|---|---|
| `Wallet` | a connection to the wallet that trusts its certificate |
| `Ledger` | a connection to the ledger with the cases' client certificate, to open and fund accounts |
| `LedgerWithoutCertificate` | a connection to the ledger with no client certificate |
| `Token` | signs a token for a subject, with scopes, that the wallet accepts |
| `RolloutWallet`, `RolloutLedger` | roll each service out, one replica at a time, and return once it is done |
| `ScenarioKey` | the baggage member the services decode a scenario from |
| `TrustScope` | the scope a token needs for the wallet to trust its scenario |
| `Timeout` | how long a case may take |

The cases come in two lists. `Parallel` runs at once. `Alone` then runs one
case at a time, since its cases fail calls to the ledger, which count
toward the breaker every call through a wallet replica shares, or roll a
service out:

```go
type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var Parallel = []Case{
	{Name: "AnAccountsLife", Run: AnAccountsLife},
	{Name: "Sending", Run: Sending},
	{Name: "SendingAgain", Run: SendingAgain},
	{Name: "NotEnoughMoney", Run: NotEnoughMoney},
	{Name: "SendsAtOnce", Run: SendsAtOnce},
	{Name: "WhoMayDoWhat", Run: WhoMayDoWhat},
	{Name: "AWatch", Run: AWatch},
	{Name: "ASlowLedger", Run: ASlowLedger},
	{Name: "AnUntrustedScenario", Run: AnUntrustedScenario},
}

var Alone = []Case{
	{Name: "TheLedgerIsDown", Run: TheLedgerIsDown},
	{Name: "TheLedgersDatabaseIsDown", Run: TheLedgersDatabaseIsDown},
	{Name: "ARolloutOfTheWallet", Run: ARolloutOfTheWallet},
	{Name: "ARolloutOfTheLedger", Run: ARolloutOfTheLedger},
	{Name: "TheLedgersBreakerOpens", Run: TheLedgersBreakerOpens},
}
```

| Case | What it shows |
|---|---|
| `AnAccountsLife` | an account opened twice is one account, with a balance of 0, and a deposit through the ledger is its balance |
| `Sending` | a send changes both balances by its amount, and answers with the transfer |
| `SendingAgain` | the same request ID answers with the same transfer, and moves the money once. The same ID for another amount is `AlreadyExists` |
| `NotEnoughMoney` | a send of more than the balance is `FailedPrecondition`, with a `PreconditionFailure` naming the account, and moves nothing |
| `SendsAtOnce` | of ten sends at once from an account that can pay for five, five succeed and five are `FailedPrecondition` |
| `WhoMayDoWhat` | a token without `wallet:write` gets `PermissionDenied` on `Send`, and no token `Unauthenticated`. The ledger answers a call without a client certificate `Unauthenticated` |
| `AWatch` | a watch sees the balance at once, then the balance after each send |
| `ARolloutOfTheWallet` | a watch open through a rollout of the wallet ends with `Unavailable`, `server is shutting down`. Watching again sees the balance, and a send made during the rollout moves the money once |
| `ARolloutOfTheLedger` | eight watches stay open through a rollout of the ledger, see every balance, and every send made during it succeeds |

The rest rehearse failures, as
[The failure injection cases](#the-failure-injection-cases) shows.

Each holder is a subject of its own, with an account of its own, so cases
that run at once never see each other's money:

```go
func newFunded(ctx context.Context, t testing.TB, target Target, amountCents int64) holder {
	t.Helper()
	h := newHolder(t, target, scopeRead, scopeWrite)
	require.Zero(t, h.openAccount(ctx, t).GetBalanceCents())
	if amountCents > 0 {
		deposit(ctx, t, target, h.subject, amountCents)
	}
	return h
}
```

A case sends as one holder to another, then reads both balances:

```go
func Sending(t testing.TB, target Target) {
	ctx := bounded(t, target)
	alice := newFunded(ctx, t, target, 1000)
	bob := newFunded(ctx, t, target, 0)

	sent := alice.send(ctx, t, bob, rand.Text(), 300)
	require.NotEmpty(t, sent.GetId())
	require.Equal(t, alice.subject, sent.GetFromAccountId())
	require.Equal(t, bob.subject, sent.GetToAccountId())
	require.EqualValues(t, 300, sent.GetAmountCents())

	require.EqualValues(t, 700, alice.balance(ctx, t))
	require.EqualValues(t, 300, bob.balance(ctx, t))
}
```

A rollout of the ledger sends from every holder until it is done, and
checks that each watch saw every balance:

```go
	require.NotNil(t, target.RolloutLedger, "Target.RolloutLedger")
	rolledOut := make(chan error, 1)
	go func() { rolledOut <- target.RolloutLedger(ctx) }()

	var rounds int
	for done := false; !done; {
		sendFromEach()
		rounds++
		select {
		case err := <-rolledOut:
			require.NoError(t, err, "rolling the ledger out")
			done = true
		default:
		}
	}
```

## The cases, inside synctest

Both Apps run on one humustest Host, beside the tests' key set, with the
two ledger replicas sharing one ledger in memory:

```go
func inSynctest(t *testing.T) place {
	h := humustest.NewHost(t)
	memory := newMemoryLedger()
	return place{
		host:   h.Host(),
		addr:   func(name string) string { return name },
		ledger: memory.open,
```

The ledger in memory evaluates the same injection point as the Store on
PostgreSQL, so the failure injection cases run here too:

```go
func (s *memoryStore) inject(ctx context.Context) error {
	f := s.faults.Eval(ctx, ledgerPoint)
	if err := f.Wait(ctx); err != nil {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, &fault.InjectedError{Point: ledgerPoint, Err: err})
	}
	if f.Abort {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, &fault.InjectedError{Point: ledgerPoint})
	}
	return nil
}
```

The harness hands the cases a `Target`, with a rollout of each service:

```go
		target: cases.Target{
			Wallet:                   connect(t, n, walletAddr, &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}),
			Ledger:                   connect(t, n, ledgerAddr, &tls.Config{RootCAs: ca.Pool(), Certificates: []tls.Certificate{casesCert}, MinVersion: tls.VersionTLS12}),
			LedgerWithoutCertificate: connect(t, n, ledgerAddr, &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS12}),
			Token:                    provider.sign,
			RolloutWallet:            wallet.rollout,
			RolloutLedger:            ledger.rollout,
			ScenarioKey:              scenarioKey,
			TrustScope:               trustScope,
			Timeout:                  pl.timeout,
		},
```

Each case runs in a bubble of its own, on a harness of its own, so a
breaker or a delay takes no real time and touches no other case:

```go
func runCase(t *testing.T, c cases.Case) {
	synctest.Test(t, func(t *testing.T) {
		h := startHarness(t, inSynctest(t))
		c.Run(t, h.target)
		h.stop(t)
	})
}

func TestInit_passes_the_shared_cases(t *testing.T) {
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				runCase(t, c)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			runCase(t, c)
		})
	}
}
```

They run with the unit tests:

```sh
go -C example/wallet test ./app/
```

## The cases, against PostgreSQL

Behind the `testcontainers` tag, the same harness runs on
`humus.OSHost()`, each replica on a free port, and the ledger on PostgreSQL
in a container, with TLS:

```go
func onOSHost(t *testing.T) place {
	return place{
		host: humus.OSHost(),
		dir:  t.TempDir(),
		addr: func(string) string { return freeAddr(t) },
		postgres: func(ca *humustest.CA, file func(string, []byte) string) map[string]string {
			return startPostgres(t, ca, file)
		},
		timeout: waitLimit,
	}
}
```

The ledger's database gets a role of its own, and `schema.sql`, applied by
its owner:

```go
	schema, err := os.ReadFile(filepath.Join("..", "ledger", "store", "schema.sql"))
	require.NoError(t, err)
	_, err = owner.Exec(ctx, string(schema))
	require.NoError(t, err)
```

Every case runs against one harness, whose services then have to exit 0 on
SIGTERM:

```go
func TestInit_passes_the_shared_cases_against_PostgreSQL(t *testing.T) {
	h := startHarness(t, onOSHost(t))

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, h.target)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, h.target)
		})
	}

	h.stop(t)
}
```

```sh
go -C example/wallet test -tags testcontainers ./...
```

## The cases, on kind

Behind the `functional` tag, the same cases run against the kind overlay
that `up.sh` brings up, which
[Deploying](/humus/pr-preview/pr-1283/walkthroughs/wallet/deploying/) covers:

```go
func TestWallet_passes_the_shared_cases_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", "wiremock", ledger, wallet} {
		kubectl(t, "rollout", "status", "deployment/"+d, "--timeout", "5m")
	}

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

The `Target` reaches both services through `kubectl port-forward`, and
reads the rest from the cluster: the issuer, the audience and the trust
scope from the wallet's ConfigMap, and the CA and the cases' certificate
from their Secret:

```go
	walletPort := newForward(t, wallet)
	ledgerPort := newForward(t, ledger)
	return cases.Target{
		Wallet:                   connect(t, wallet, walletPort, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}),
		Ledger:                   connect(t, ledger, ledgerPort, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}),
		LedgerWithoutCertificate: connect(t, ledger, ledgerPort, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}),
		Token:                    signer(t, env.get(t, "TOKEN_ISSUER"), env.get(t, "TOKEN_AUDIENCE")),
		RolloutWallet:            walletPort.rollout,
		RolloutLedger:            ledgerPort.rollout,
		ScenarioKey:              scenarioKey,
		TrustScope:               env.get(t, "FAULT_TRUST_SCOPE"),
		Timeout:                  timeout,
	}
```

A rollout restarts the Deployment, waits for its old pods to go, and
forwards the port again, since `kubectl port-forward` follows one pod:

```go
	if _, err := run(ctx, "rollout", "restart", deployment); err != nil {
		return err
	}
	if _, err := run(ctx, "rollout", "status", deployment, "--timeout", "5m"); err != nil {
		return err
	}
```

It signs tokens with the key `up.sh` made the key set from:

```go
	block, _ := pem.Decode(secret(t, "token-signing-key", "key.pem"))
	require.NotNil(t, block, "secret token-signing-key holds no PEM block")
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err, "secret token-signing-key's key.pem")
```

```sh
go -C example/wallet test -tags functional ./functional/
```

## The failure injection cases

A case rehearses a failure by sending a scenario in the call's `baggage`
metadata, with a token whose scopes hold the trust scope:

```go
func newRehearser(ctx context.Context, t testing.TB, target Target, amountCents int64) holder {
	t.Helper()
	require.NotEmpty(t, target.TrustScope, "Target.TrustScope")
	return newFunded(ctx, t, target, amountCents).withScopes(t, target, scopeRead, scopeWrite, target.TrustScope)
}

func (h holder) rehearsing(t testing.TB, rules ...fit.Rule) holder {
	t.Helper()
	require.NotEmpty(t, h.target.ScenarioKey, "Target.ScenarioKey")
	codec, err := jsonscenario.New(h.target.ScenarioKey)
	require.NoError(t, err, "Target.ScenarioKey")
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err, "encoding the scenario")
	h.baggage = b.String()
	return h
}
```

```go
func (h holder) with(ctx context.Context) context.Context {
	if h.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+h.token)
	}
	if h.baggage != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "baggage", h.baggage)
	}
	return ctx
}
```

| Case | Its scenario | What it shows |
|---|---|---|
| `ASlowLedger` | delays `grpc:ledger.v1.Ledger/Transfer` 2s | the send still succeeds, after the delay |
| `AnUntrustedScenario` | aborts `grpc:ledger.v1.Ledger/Transfer`, from a token without the trust scope | the scenario is stripped, and the send succeeds |
| `TheLedgerIsDown` | aborts `grpc:ledger.v1.Ledger/Transfer` | `Unavailable`, `LEDGER_UNAVAILABLE`, and nothing moves. The same request ID without the scenario then moves the money once |
| `TheLedgersDatabaseIsDown` | aborts `postgresql:ledger` | the wallet forwards the scenario to the ledger, and the send is `Unavailable`, and moves nothing |
| `TheLedgersBreakerOpens` | aborts `grpc:ledger.v1.Ledger/Transfer` on every send | the breaker opens and sheds the sends, `LEDGER_SHED` with a `RetryInfo`, until the scenario ends |

`TheLedgersBreakerOpens` sends until the breaker sheds the send, then
sends without the scenario until the breaker admits one. It runs last,
since it leaves a breaker open:

```go
	failing := alice.rehearsing(t, abort(transferPoint))
	for {
		_, err := failing.trySend(ctx, bob, requestID, 300)
		st := requireCode(t, err, codes.Unavailable)
		if reason(st) == ledgerShed {
			require.NotNil(t, retryInfo(st), "a LEDGER_SHED answer carries a RetryInfo: %s", st.Message())
			break
		}
		require.Equal(t, ledgerUnavailable, reason(st), "%s", st.Message())
		pause(ctx, t, pollEvery)
	}

	alice.sendOnceAdmitted(ctx, t, bob, requestID, 300)
	require.EqualValues(t, 700, alice.balance(ctx, t))
	require.EqualValues(t, 300, bob.balance(ctx, t))
```

More run inside synctest only, where the test can see inside the ledger: an
aborted ledger point never reaches it, and is evaluated on each of the
client's attempts:

```go
		st := requireCode(t, w.rehearse(t, token, abort(getAccountPoint)), codes.Unavailable)
		var reasons []string
		for _, d := range st.Details() {
			if info, ok := d.(*errdetails.ErrorInfo); ok {
				reasons = append(reasons, info.GetReason())
			}
		}
		require.Equal(t, []string{"LEDGER_UNAVAILABLE"}, reasons, st.Message())
		require.Zero(t, w.ledger.called(), "the ledger was called")
```

```go
		evals := events(tel.Spans(), "humus.fit.evaluation")
		require.Len(t, evals, grpcclient.DefaultSettings().MaxAttempts)
```

A scenario the wallet doesn't trust is stripped, recorded on its span, and
never reaches the ledger:

```go
				require.Zero(t, w.ledger.scenarios(), "the ledger got the scenario")

				w.stop(t)
				require.Len(t, events(tel.Spans(), "humus.fit.strip"), 1)
				require.Empty(t, events(tel.Spans(), "humus.fit.evaluation"))
```
