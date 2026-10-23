# Shoppingcart

The shoppingcart is two services, each an `openapi/v3` API served by
`httpserver`:

- **the cart** keeps shopping carts and their items, each cart its owner's;
- **the catalog** keeps the products for sale and their prices.

Adding an item to a cart crosses both:

1. the client sends `POST /carts/{cartId}/items` to the cart, with a bearer
   token whose scopes hold `carts:write`;
2. the cart verifies the token against the identity provider's key set;
3. the cart asks the catalog for the product's price, over mTLS;
4. the cart stores the item, with that price, in its own database.

```text
client ──HTTPS + bearer──▶ cart (httpserver, openapi/v3) ──HTTPS, mTLS──▶ catalog (httpserver, openapi/v3)
                               │                                           │
                               ▼                                           ▼
                          PostgreSQL: carts                         PostgreSQL: catalog
```

Each service owns its data, and neither reads the other's.

## The files it ends with

Its code is a module of its own, at
[`example/http/shoppingcart`](https://github.com/z5labs/humus/tree/main/example/http/shoppingcart):

```text
example/http/shoppingcart/
├── main.go                 the cart: httpserver.Main(app.Init, ...)
├── app/                    the cart's Init, settings, trust, readiness, BuildTelemetry and Faults
├── endpoint/               the cart's API: its operations, on a Store and a Catalog
├── database/               the Store on PostgreSQL, its injection point, migrations/
├── catalogclient/          the cart's client of the catalog: the request, and what each answer means
├── catalog/
│   ├── main.go             the catalog: httpserver.Main(app.Init, ...)
│   ├── app/                its Init, settings, trust, probes, BuildTelemetry and Faults
│   ├── endpoint/           its API: its operations on products, and who may call each
│   └── database/           its Store on PostgreSQL, its injection point, migrations/
├── migrate/                the migrations' init container: its Init and its job
├── cmd/migrate/            humus.Main(migrate.Init, ...)
├── cmd/carttoken/          a CLI that makes the key set and signs tokens, to call the cart by hand
├── internal/certfile/      certificates served and presented from their files, read again when renewed
├── internal/latch/         a monitor that is healthy for good once its check first succeeds
├── internal/postgrestest/  PostgreSQL in a container, for the tests
├── cases/                  the test cases the in-process tests, the container tests and kind share
├── functional/             the cases against kind
├── deploy/                 the base, the kind overlay and up.sh, the alerts and the dashboard
└── Dockerfile              one image, with the cart, the catalog and migrate
```

These pages build the cart's API, who may call it, and the schema, then
the catalog and the cart's calls to it, then each service's `Init` and
`main`. Then they test both services, deploy them, and observe them, and
end with the checklist they meet.

Next: [The cart's API](/humus/walkthroughs/http/shoppingcart/api/)
