---
title: Testing
linkTitle: Testing
description: >
  How the cart and the catalog are tested: each service's Init and the
  catalog's client as units, on fakes and the humustest Host; the Stores
  against PostgreSQL; the shared cases, their Target, Parallel and Alone,
  run inside synctest, against containers and on kind; and the failure
  injection cases.
type: docs
weight: 80
---

## The cart's unit tests

The cart's `Init` opens its Store and its Catalog through two functions:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	return initWith(ctx, s, openPostgres, openCatalog)
}

type openStore func(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (store endpoint.Store, ping func(context.Context) error, err error)

type newCatalog func(s *humus.Setup, settings httpclient.Settings) (catalog endpoint.Catalog, ready func(context.Context) error, err error)
```

So its unit tests give it a Store in memory and a fake catalog, which
answers from a price list, or fails when the test says so:

```go
type fakeCatalog struct {
	mu     sync.Mutex
	prices map[string]float64
	err    error
}

func fakePrices() *fakeCatalog {
	return &fakeCatalog{prices: map[string]float64{"sku-1": 9.99, "sku-2": 1.5}}
}

func (c *fakeCatalog) Price(_ context.Context, productID string) (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	price, ok := c.prices[productID]
	if !ok {
		return 0, endpoint.ErrProductNotFound
	}
	return price, nil
}
```

```go
func onFakes(catalog *fakeCatalog) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
		return initWith(ctx, setup, inMemory, catalog.open)
	}
}
```

A test starts the cart on the humustest Host, beside an identity provider
of its own that serves its key set, and calls it over HTTP/2 with a token
the provider signed:

```go
	p, status := httpservertest.Start(t, init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(f.env),
	)
```

Each test runs inside `synctest`, and checks the answers:

```go
func TestInit_answers_a_product_the_catalog_doesnt_have_with_422(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

		d := isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-404","quantity":1}`), http.StatusUnprocessableEntity)
		require.Equal(t, "There is no product sku-404 in the catalog.", d.Detail)

		got := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Empty(t, got.Items)

		s.stop(t)
	})
}
```

A Store that fails shows what the cart answers when its database is down:

```go
type failingStore struct{ err error }

func (f failingStore) CreateCart(context.Context, string) (endpoint.Cart, error) {
	return endpoint.Cart{}, f.err
}
```

```go
			r := s.do(t, op.method, op.path, "application/json", op.body)
			d := isProblem(t, r, http.StatusServiceUnavailable)
			require.Equal(t, "The cart can't reach its database right now. Try again later.", d.Detail, "%s %s", op.method, op.path)
			require.NotContains(t, string(r.body), dbHost)
```

The catalog's `InitWith` takes its Store the same way, and its tests give
it a fake database.

## The catalog's client

`catalogclient`'s tests serve a handler as the catalog on the humustest
Host, and call it from a job:

```go
func price(t *testing.T, handler http.HandlerFunc, productID string) result {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	stop := startCatalog(t, h, ca, handler)
	var r result
	run(t, h, settings(t, ca, "https://"+catalogAddr), func(ctx context.Context, c *catalogclient.Client) {
		r.price, r.err = c.Price(ctx, productID)
	})
	stop()
	return r
}
```

Each answer the catalog can give has a test of its own:

```go
func TestClient_Price_answers_a_404_with_ErrProductNotFound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := price(t, answer(http.StatusNotFound, `{"status":404}`), "sku-1")
		require.ErrorIs(t, r.err, endpoint.ErrProductNotFound)
		require.NotErrorIs(t, r.err, endpoint.ErrCatalogUnavailable)
	})
}
```

Inside the bubble, the retries' backoff and the breaker's timeouts take no
real time:

```go
		last := errs[len(errs)-1]
		require.ErrorIs(t, last, circuit.ErrRejected)
		require.ErrorIs(t, last, endpoint.ErrCatalogUnavailable)
		require.Zero(t, sentWhenRejected)
```

## The Stores

A scenario aborts each of the Store's queries, without a database:

```go
func TestStore_fails_every_query_a_scenario_aborts_as_ErrUnavailable(t *testing.T) {
	faults, ctx := rehearsing(t, fit.Rule{Point: database.Point, Fault: fit.Fault{Abort: true}})
	s := database.New(nil, faults)
	cartID, itemID := uuid.New(), uuid.New()

	_, err := s.CreateCart(ctx, "shopper")
	require.ErrorIs(t, err, database.ErrUnavailable)
	require.ErrorIs(t, err, fit.ErrInjected)
```

Behind the `testcontainers` tag, the Stores run against PostgreSQL in a
container. `postgrestest` starts one server for the package, with a
password generated on each run, and gives each test a database, an owner
and an app role of its own. `migrate` applies the migrations as the owner,
and the Store connects as the app role:

```go
func store(t *testing.T) *database.Store {
	t.Helper()
	db := postgrestest.NewDatabase(t)
	db.Migrated(t, "carts")

	pool, err := pgxpool.New(t.Context(), db.AppURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return database.New(pool, fault.Injector{})
}
```

```go
	for _, stmt := range []string{
		`CREATE ROLE ` + db.Owner + ` LOGIN PASSWORD '` + ownerPassword + `'`,
		`CREATE ROLE ` + db.App + ` LOGIN PASSWORD '` + appPassword + `'`,
		`CREATE DATABASE ` + db.Name + ` OWNER ` + db.Owner,
	} {
```

Run the unit tests with `go test`:

```sh
go -C example/http/shoppingcart test ./...
```

## The shared cases

The cases are the package `cases`, which imports neither App nor
humustest, so the compiler keeps them black-box. A `Target` says how to
reach both services:

```go
type Target struct {
	Cart       *url.URL
	CartClient *http.Client

	Catalog       *url.URL
	CatalogClient *http.Client

	Token func(subject string, scopes ...string) (string, error)

	CatalogPoint string
	ScenarioKey  string
	TrustScope   string
	Timeout      time.Duration
}
```

| Field | What it is |
|---|---|
| `Cart`, `CartClient` | the cart's URL, and a client that trusts its certificate |
| `Catalog`, `CatalogClient` | the catalog's URL, and a client that trusts its certificate |
| `Token` | signs a token for a subject, with scopes, that both services accept |
| `CatalogPoint` | the failure-injection point of the cart's calls to the catalog |
| `ScenarioKey` | the baggage member both services decode a scenario from |
| `TrustScope` | the scope a token needs for the cart to trust its scenario |
| `Timeout` | how long a case may take |

The cases come in two lists. Each case in `Alone` runs by itself, since the
catalog's failures it rehearses count toward the breaker every add goes
through:

```go
type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var Parallel = []Case{
	{Name: "ACartsLife", Run: ACartsLife},
	{Name: "Prices", Run: Prices},
	{Name: "UnknownProduct", Run: UnknownProduct},
	{Name: "Ownership", Run: Ownership},
	{Name: "Scopes", Run: Scopes},
	{Name: "SlowCatalog", Run: SlowCatalog},
	{Name: "CartDatabaseDown", Run: CartDatabaseDown},
	{Name: "UntrustedScenario", Run: UntrustedScenario},
}

var Alone = []Case{
	{Name: "CatalogDown", Run: CatalogDown},
	{Name: "CatalogDatabaseDown", Run: CatalogDatabaseDown},
	{Name: "CatalogBreakerOpens", Run: CatalogBreakerOpens},
}
```

| Case | What it shows |
|---|---|
| `ACartsLife` | a cart is created, an item is added at the catalog's price, changed and removed, and the cart is deleted |
| `Prices` | an item keeps the price it was added at, and the next one gets the catalog's new price |
| `UnknownProduct` | a product the catalog doesn't have is a 422, and no item |
| `Ownership` | another subject's token gets a 404 for every operation on the cart |
| `Scopes` | a token without `carts:write` gets a 403 for every change, and no token a 401. The catalog needs `catalog:write` to put a product |

The rest rehearse failures, as [The failure injection cases](#the-failure-injection-cases)
shows.

A case puts a product in the catalog, then calls the cart as a shopper:

```go
func ACartsLife(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 12.5)
	s := newShopper(t, target, cartsWrite)

	c := s.createCart(ctx, t)
	require.Empty(t, c.Items)

	added := s.addItem(ctx, t, c.CartID, p.ProductID, 2)
	require.Equal(t, c.CartID, added.CartID)
	require.Equal(t, p.ProductID, added.ProductID)
	require.Equal(t, 2, added.Quantity)
	require.InDelta(t, p.UnitPrice, added.UnitPrice, 1e-9)
	require.Equal(t, []item{added}, s.getCart(ctx, t, c.CartID).Items)
```

Each shopper is a subject of its own, and each product has an ID of its
own, so cases that run at once never see each other's carts:

```go
func newShopper(t testing.TB, target Target, scopes ...string) shopper {
	t.Helper()
	return shopper{target: target, subject: rand.Text()}.withScopes(t, target, scopes...)
}
```

```go
func newProductID() string { return "sku-" + rand.Text() }
```

## The cases, inside synctest

Both Apps run on one humustest Host, beside the identity provider, with
their certificates and secrets as the Host's files:

```go
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"cart/tls.crt":         cartCert,
		"cart/tls.key":         cartKey,
		"cart/database-url":    []byte("postgres://carts:" + rand.Text() + "@" + dbHost + "/carts?sslmode=disable\n"),
		"cart/catalog.crt":     clientCert,
		"cart/catalog.key":     clientKey,
		"catalog/tls.crt":      catalogCert,
		"catalog/tls.key":      catalogKey,
		"catalog/database-url": []byte("postgres://catalog:" + rand.Text() + "@" + dbHost + "/catalog?sslmode=disable\n"),
		"ca.crt":               trusted,
	}))
```

The catalog keeps its products in memory, and the cart its carts, and the
cart calls the catalog with its real client, over mTLS:

```go
	cart := run(t, "cart", h, func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
		return initWith(ctx, s, inMemory, openCatalog)
	}, []humus.Option{humus.WithFaults(Faults), humus.WithTelemetry(cartTelemetry)}, tokens, cartEnv, map[string]string{
```

The Stores in memory evaluate the same injection points as the Stores on
PostgreSQL, so the failure injection cases run here too:

```go
func (m *memoryStore) CreateCart(ctx context.Context, owner string) (endpoint.Cart, error) {
	if err := inject(ctx, m.faults, database.Point, database.ErrUnavailable); err != nil {
		return endpoint.Cart{}, err
	}
```

Each case runs in a bubble of its own, on a shoppingcart of its own, so a
case's breaker or a delay takes no real time and touches no other case:

```go
func runCase(t *testing.T, c cases.Case) {
	synctest.Test(t, func(t *testing.T) {
		s := startShoppingcart(t)
		c.Run(t, s.target)
		s.stop(t)
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
go -C example/http/shoppingcart test ./app/
```

## The cases, against PostgreSQL and WireMock

Behind the `testcontainers` tag, both Apps run on `humus.OSHost()`, each on
a database of its own, migrated by `migrate`:

```go
	carts := postgrestest.NewDatabase(t)
	carts.Migrated(t, "carts")
	catalogDB := postgrestest.NewDatabase(t)
	catalogDB.Migrated(t, "catalog")
```

`carttoken` makes the key set from a key the test generates:

```go
	cmd := exec.CommandContext(t.Context(), "go", "run", filepath.Join("..", "cmd", "carttoken"), "jwks", "-key", keyFile)
```

WireMock serves it over TLS, with the kind overlay's mappings:

```go
	mappings, err := filepath.Glob(filepath.Join("..", "deploy", "kind", "wiremock", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, mappings)
	keystore, password := ca.Issue(t, host, "localhost", "127.0.0.1").PKCS12(t)
```

The test starts the catalog and the cart, waits for each to be ready, and
hands the cases a `Target`:

```go
	target := cases.Target{
		Cart:          &url.URL{Scheme: "https", Host: cartAddr},
		CartClient:    client(),
		Catalog:       &url.URL{Scheme: "https", Host: catalogAddr},
		CatalogClient: client(),
		Token:         provider.sign,
		CatalogPoint:  "http:" + catalogAddr,
		ScenarioKey:   scenarioKey,
		TrustScope:    trustScope,
		Timeout:       waitLimit,
	}
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

	cart.stop(t)
	catalog.stop(t)
```

Every case runs against one cart and one catalog, which then have to exit
0 on SIGTERM.

```sh
go -C example/http/shoppingcart test -tags testcontainers ./...
```

## The cases, on kind

Behind the `functional` tag, the same cases run against the kind overlay
that `up.sh` brings up, which [Deploying]({{< relref "/walkthroughs/http/shoppingcart/deploying" >}})
covers:

```go
func TestShoppingcart_passes_the_shared_cases_on_kind(t *testing.T) {
	for _, d := range []string{"postgres", "wiremock", catalog, cart} {
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
reads the rest from the cluster: the issuer, the audiences and the trust
scope from the services' ConfigMaps, and the CA from its Secret:

```go
	client := newClient(t)
	return cases.Target{
		Cart:          &url.URL{Scheme: "https", Host: forward(t, cart, port)},
		CartClient:    client,
		Catalog:       &url.URL{Scheme: "https", Host: forward(t, catalog, port)},
		CatalogClient: client,
		Token:         signer(t, issuer, cartEnv.get(t, "JWT_AUDIENCE"), catalogEnv.get(t, "JWT_AUDIENCE")),
		CatalogPoint:  pointOf(t, cartEnv.get(t, "CATALOG_URL")),
		ScenarioKey:   scenarioKey,
		TrustScope:    cartEnv.get(t, "FAULT_TRUST_SCOPE"),
		Timeout:       timeout,
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
go -C example/http/shoppingcart test -tags functional ./functional/
```

## The failure injection cases

A case rehearses a failure by sending a scenario in the request's
`Baggage` header, with a token whose scopes hold the trust scope:

```go
func newRehearser(t testing.TB, target Target) shopper {
	t.Helper()
	require.NotEmpty(t, target.TrustScope, "Target.TrustScope")
	return newShopper(t, target, cartsWrite, target.TrustScope)
}

func (s shopper) rehearsing(t testing.TB, rules ...fit.Rule) shopper {
	t.Helper()
	require.NotEmpty(t, s.target.ScenarioKey, "Target.ScenarioKey")
	codec, err := jsonscenario.New(s.target.ScenarioKey)
	require.NoError(t, err, "Target.ScenarioKey")
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	s.baggage = b.String()
	return s
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
| `SlowCatalog` | delays the catalog point 2s | the item is still added, after the delay |
| `CartDatabaseDown` | aborts `postgresql:carts` | a 503 saying to try again, and the cart is unchanged after it |
| `UntrustedScenario` | aborts the catalog point, from a token without the trust scope | the scenario is stripped, and the item is added |
| `CatalogDown` | aborts the catalog point | a 503 naming the catalog, and no item, then an add once the breaker admits it |
| `CatalogDatabaseDown` | aborts `postgresql:catalog` | the cart forwards the scenario to the catalog, and answers the catalog's 503 with a 503 naming the catalog, and no item |
| `CatalogBreakerOpens` | aborts the catalog point on every add | the breaker opens, and sheds the adds, until the scenario ends |

`CatalogBreakerOpens` adds until the breaker sheds the add, then waits for
it to admit one again:

```go
func CatalogBreakerOpens(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 11.25)
	s := newRehearser(t, target)
	c := s.createCart(ctx, t)

	failing := s.rehearsing(t, abort(catalogPoint(t, target)))
	for {
		detail := failing.call(ctx, t, http.MethodPost, itemsPath(c.CartID), newItem(p.ProductID, 1)).problem(t, http.StatusServiceUnavailable)
		if strings.Contains(detail, shed) {
			break
		}
		pause(ctx, t, pollEvery)
	}
	require.Empty(t, s.getCart(ctx, t, c.CartID).Items)

	added := s.addItemOnceAdmitted(ctx, t, c.CartID, p.ProductID)
	require.InDelta(t, p.UnitPrice, added.UnitPrice, 1e-9)
	require.Equal(t, []item{added}, s.getCart(ctx, t, c.CartID).Items)
}
```

More run inside synctest only, beside the shared cases, where the test can
see inside the catalog: an aborted catalog point never reaches it, and an
aborted catalog query is tried as many times as the cart's client
attempts:

```go
func TestInit_answers_503_without_calling_the_catalog_when_a_trusted_scenario_aborts_its_point(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := startShoppingcart(t)
		s.stock(t)
		token := s.token(t, endpoint.ScopeWrite, trustScope)
		cartID := s.createCart(t, token)

		d := unavailable(t, s.addItem(t, cartID, token, abort(s.target.CatalogPoint)))
		require.Contains(t, d.Detail, "catalog")
		require.Zero(t, s.products.queried(), "the catalog was asked for a price")

		s.stop(t)
	})
}
```

```go
		d := unavailable(t, s.addItem(t, cartID, token, abort(catalogdatabase.Point)))
		require.Contains(t, d.Detail, "catalog")
		require.Equal(t, httpclient.DefaultSettings().MaxAttempts, s.products.queried())
```

A scenario the cart doesn't trust is stripped, and recorded on its span:

```go
			a := s.addItem(t, cartID, token, abort(s.target.CatalogPoint), abort(catalogdatabase.Point), abort(database.Point))
			require.Equal(t, http.StatusCreated, a.code, "%s", a.body)
			require.Equal(t, 1, s.products.queried())

			s.stop(t)
			require.Equal(t, 1, stripped(tel.Spans()))
```
