# The catalog

The catalog is a second humus service, written as the cart is: an
`openapi/v3` API served by `httpserver`, on a PostgreSQL database of its
own. It has two operations:

- `GET /products/{productId}` answers a product and its price, to a caller
  with a client certificate the internal CA issued, which is the cart;
- `PUT /products/{productId}` creates or replaces one, for a bearer token
  with `catalog:write`.

## The bodies and the Store

```go
type Product struct {
	ProductID string  `json:"productId" required:"true"`
	UnitPrice float64 `json:"unitPrice" required:"true" minimum:"0" maximum:"99999999.99"`
}

type PutProductRequest struct {
	UnitPrice *float64 `json:"unitPrice" required:"true" nullable:"false" minimum:"0" maximum:"99999999.99"`
}

const maxUnitPrice = 99999999.99

type Store interface {
	GetProduct(ctx context.Context, productID string) (Product, error)
	PutProduct(ctx context.Context, product Product) (Product, error)
}

var (
	ErrProductNotFound = errors.New("product not found")
	ErrUnavailable     = errors.New("the database is unavailable")
)
```

The maximum is what the table's `DECIMAL(10, 2)` holds.

Its answers are the cart's: `NotFound`, `Invalid`, `Unavailable`, and a 500
that says nothing of the failure. Its claims are only the scope:

```go
const ScopeWrite = "catalog:write"

type Claims struct {
	Scope string `json:"scope"`
}
```

## The path

```go
var productID = openapi.PathParam[string]("productId").Description("The product's identifier.").MinLength(1).MaxLength(255)

var product = openapi.Root().Lit("products").Param(productID)
```

## The operations

`NewAPI` declares two schemes, and requires one on each operation:

```go
func NewAPI(s *humus.Setup, store Store, verify func(ctx context.Context, token string) (Claims, error)) (*openapi.API, error) {
	client := auth.MutualTLS("mutualTLS", verifiedLeaf).
		Description("A client certificate the catalog's CA issued, such as the shoppingcart's. Reading a product needs one.")
	bearer := auth.Bearer("bearer", keysUnavailable(verify)).
		Format("JWT").
		Description("A JWT from the shoppingcart's identity provider. Changing a product needs the scope catalog:write.")

	p := products{store: store}
	tags := openapi.Tags("products")
	return openapi.NewAPI(s,
		openapi.Info{
			Title:       "Catalog API",
			Version:     "1.0.0",
			Description: "API for the products an e-commerce platform sells, and their prices.",
		},
		openapi.Unmatched{
			NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
			MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
		},
		openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
		openapi.CatchAll(http.StatusInternalServerError, InternalError, jsoncodec.Problem),
		[]openapi.Operation{
			openapi.Get(product, p.getProduct, jsoncodec.Encoder,
				openapi.OperationID("getProduct"),
				openapi.Security(auth.Require(client)),
				openapi.Summary("Get a product and its unit price"),
				openapi.Error[NotFound](http.StatusNotFound, jsoncodec.Problem),
				tags,
			),
			openapi.Put(product, p.putProduct, jsoncodec.Decoder, jsoncodec.Encoder,
				openapi.OperationID("putProduct"),
				openapi.Security(auth.RequireScopes(bearer, ScopeWrite)),
				openapi.Summary("Create or replace a product and its unit price"),
				openapi.Error[Invalid](http.StatusUnprocessableEntity, jsoncodec.Problem),
				tags,
			),
			openapi.Document(openapi.Root().Lit("openapi.json"), openapi.Security(auth.Disabled())),
		},
		openapi.Error[Unavailable](http.StatusServiceUnavailable, jsoncodec.Problem),
	)
}
```

The mutual TLS scheme's principal is the leaf of the chain the TLS
handshake verified:

```go
func verifiedLeaf(_ context.Context, chains [][]*x509.Certificate) (*x509.Certificate, error) {
	if len(chains) == 0 || len(chains[0]) == 0 {
		return nil, fmt.Errorf("no verified chain: %w", auth.ErrUnauthenticated)
	}
	return chains[0][0], nil
}
```

## The handlers

```go
func (p products) getProduct(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (Product, error) {
	id := openapi.ParamFrom(req, productID)
	got, err := p.store.GetProduct(ctx, id)
	if err != nil {
		return Product{}, answer(err, id)
	}
	return got, nil
}

func (p products) putProduct(ctx context.Context, req openapi.Request[PutProductRequest]) (Product, error) {
	id := openapi.ParamFrom(req, productID)
	price := req.Body().UnitPrice
	switch {
	case price == nil:
		return Product{}, invalid("unitPrice is required")
	case *price < 0:
		return Product{}, invalid("unitPrice must not be negative")
	case *price > maxUnitPrice:
		return Product{}, invalid(fmt.Sprintf("unitPrice must be at most %.2f", maxUnitPrice))
	}
	put, err := p.store.PutProduct(ctx, Product{ProductID: id, UnitPrice: *price})
	if err != nil {
		return Product{}, answer(err, id)
	}
	return put, nil
}
```

`unitPrice` is a pointer, so a body without one is a 422, not a price of 0.

```go
func answer(err error, productID string) error {
	switch {
	case errors.Is(err, ErrProductNotFound):
		return NotFound{problem.Details{
			Type:   "about:blank",
			Title:  http.StatusText(http.StatusNotFound),
			Status: http.StatusNotFound,
			Detail: fmt.Sprintf("There is no product %s.", productID),
		}}
	case errors.Is(err, ErrUnavailable):
		return unavailable("The catalog can't reach its database right now. Try again later.")
	default:
		return err
	}
}
```

## mTLS

The catalog's TLS asks for a client certificate, and verifies one against
`CLIENT_CA_FILE`:

```go
ClientAuth: tls.VerifyClientCertIfGiven,
ClientCAs:  r.Get(file("CLIENT_CA_FILE").Parse(certPool)),
```

It doesn't require one, so the kubelet's probes and a writer with only a
token still connect. `auth.Require(client)` is what makes a read need one.

## The Store and its injection point

The catalog's `Store` is written as the cart's, with its own injection
point, `postgresql:catalog`:

```go
const Point = "postgresql:catalog"
```

```go
func (s *Store) GetProduct(ctx context.Context, productID string) (endpoint.Product, error) {
	if err := s.inject(ctx); err != nil {
		return endpoint.Product{}, wrap("getting product "+productID, err)
	}
	p := endpoint.Product{ProductID: productID}
	err := s.pool.QueryRow(ctx, `SELECT unit_price::float8 FROM products WHERE id = $1`, productID).Scan(&p.UnitPrice)
	if errors.Is(err, pgx.ErrNoRows) {
		err = endpoint.ErrProductNotFound
	}
	if err != nil {
		return endpoint.Product{}, wrap("getting product "+productID, err)
	}
	return p, nil
}
```

A put is one upsert:

```go
err := s.pool.QueryRow(ctx, `
	INSERT INTO products (id, unit_price) VALUES ($1, $2)
	ON CONFLICT (id) DO UPDATE SET unit_price = EXCLUDED.unit_price
	RETURNING unit_price::float8`,
	product.ProductID, product.UnitPrice,
).Scan(&p.UnitPrice)
```

## The scenarios it trusts

The catalog trusts a request's failure-injection scenario when its client
certificate verified, which is the cart's, since the cart forwards its
scenarios:

```go
func issuedByTheCA(r *http.Request) bool {
	return r.TLS != nil && len(r.TLS.VerifiedChains) > 0
}
```

```go
TrustScenario:     issuedByTheCA,
```

## The probes

The probes are attached to the catalog's own port, at `/health/`, beside
the API rather than behind it, so they need no token or certificate:

```go
mux := http.NewServeMux()
mux.Handle("/health/", httphealth.NewHandler(s, health.Probes{
	Liveness:  health.And(),
	Readiness: latch.New(ping),
}))
mux.Handle("/", http.MaxBytesHandler(api, maxBodyBytes))
```

It is ready once it has reached its database, and stays ready through an
outage after that, so callers get its own 503 rather than no catalog at
all. `latch` is a monitor of the walkthrough's own:

```go
type Monitor struct {
	check   func(context.Context) error
	turn    chan struct{}
	reached atomic.Bool
}

func New(check func(ctx context.Context) error) *Monitor {
	return &Monitor{check: check, turn: make(chan struct{}, 1)}
}

func (m *Monitor) Healthy(ctx context.Context) (bool, error) {
	if m.reached.Load() {
		return true, nil
	}
	select {
	case m.turn <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-m.turn }()
	if m.reached.Load() {
		return true, nil
	}
	if err := m.check(ctx); err != nil {
		return false, err
	}
	m.reached.Store(true)
	return true, nil
}
```

One probe checks at a time, so a slow database isn't checked by every
probe at once.

Next: [Calling the catalog](/humus/pr-preview/pr-1359/walkthroughs/http/shoppingcart/calling-the-catalog/)
