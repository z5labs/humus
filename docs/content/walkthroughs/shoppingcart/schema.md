---
title: The schema
linkTitle: The schema
description: >
  Each service's schema as versioned migrations, migrate, a humus job that
  applies them under tern's lock and grants the service's role read and
  write, the init container that runs it, the roles, and the cart's Store
  on PostgreSQL with its injection point.
type: docs
weight: 40
---

Each service's schema is a directory of versioned SQL migrations, applied
before the service starts by `migrate`, as a role that owns the schema. The
services connect as roles that can only read and write.

## The migrations

The cart's first migration, `database/migrations/001_create_carts.sql`:

```sql
CREATE TABLE carts (
    id          UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
    owner       TEXT            NOT NULL CHECK (owner <> ''),
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ     NOT NULL DEFAULT now()
);

CREATE TABLE cart_items (
    id          UUID            PRIMARY KEY DEFAULT gen_random_uuid(),
    cart_id     UUID            NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    product_id  VARCHAR(255)    NOT NULL,
    quantity    INTEGER         NOT NULL CHECK (quantity > 0),
    unit_price  DECIMAL(10, 2)  NOT NULL CHECK (unit_price >= 0),
    created_at  TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ     NOT NULL DEFAULT now()
);

CREATE INDEX idx_cart_items_cart_id ON cart_items(cart_id);
```

The catalog's, `catalog/database/migrations/001_create_products.sql`:

```sql
CREATE TABLE products (
    id          VARCHAR(255)    PRIMARY KEY CHECK (id <> ''),
    unit_price  DECIMAL(10, 2)  NOT NULL CHECK (unit_price >= 0)
);
```

Each `database` package embeds its own:

```go
//go:embed migrations/*.sql
var Migrations embed.FS
```

A migration is backwards-compatible with the code it replaces, because the
old pods keep serving during a rollout.

## `migrate`'s settings

`migrate` is a program of its own, and a humus job. It reads three
settings:

```go
type settings struct {
	Database   *pgx.ConnConfig
	Migrations fs.FS
	AppRole    string
}

func readSettings(ctx context.Context, s *humus.Setup) (settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	file := func(name string) config.Value[[]byte] { return env(name).Parse(readFile(s.Host.FS)) }

	return config.Struct(func(r *config.Resolver) settings {
		return settings{
			Database:   r.Get(file("DATABASE_URL_FILE").Parse(databaseURL)),
			Migrations: r.Get(env("MIGRATIONS").Parse(migrations)),
			AppRole:    r.Get(env("APP_ROLE").Validate(notEmpty)),
		}
	}).Read(ctx)
}
```

`MIGRATIONS` names which service's migrations it applies:

```go
func migrations(name string) (fs.FS, error) {
	switch name {
	case "carts":
		return fs.Sub(database.Migrations, "migrations")
	case "catalog":
		return fs.Sub(catalogdb.Migrations, "migrations")
	}
	return nil, errors.New("must be carts or catalog")
}
```

| Variable | What it is | Default |
|---|---|---|
| `DATABASE_URL_FILE` | a file holding the URL of the schema's owner, PostgreSQL's | required |
| `MIGRATIONS` | `carts` or `catalog` | required |
| `APP_ROLE` | the service's role, which gets read and write | required |

## The job

`Init` reads the settings, and its one Service is the job:

```go
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	set, err := readSettings(ctx, s)
	if err != nil {
		return nil, err
	}
	return []humus.Service{job.NewService(s, func(ctx context.Context) error {
		return run(ctx, s, set)
	})}, nil
}
```

The job connects through the Host, and runs tern's `Migrator`:

```go
func run(ctx context.Context, s *humus.Setup, set settings) (err error) {
	cfg := set.Database
	cfg.DialFunc = s.Host.Dial
	cfg.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	defer func() {
		err = errors.Join(err, conn.Close(context.WithoutCancel(ctx)))
	}()

	m, err := tern.NewMigrator(ctx, conn, versionTable)
	if err != nil {
		return fmt.Errorf("preparing %s: %w", versionTable, err)
	}
	if err := m.LoadMigrations(set.Migrations); err != nil {
		return fmt.Errorf("loading the migrations: %w", err)
	}
	if err := m.Migrate(ctx); err != nil {
		return fmt.Errorf("migrating: %w", err)
	}
	if err := grant(ctx, conn, set.AppRole); err != nil {
		return fmt.Errorf("granting %s read and write: %w", set.AppRole, err)
	}
	return nil
}
```

```go
const (
	versionTable = "public.schema_version"
	grantLock    = int64(4217863019455312)
)
```

Two pods that start at once both run it. tern migrates under an advisory
lock, so one migrates, and the other waits, then finds nothing to do.

## The grant

Then it grants the service's role read and write on the tables, and nothing
on tern's version table. Two grants at once would collide, so the grant
takes an advisory lock of its own:

```go
func grant(ctx context.Context, conn *pgx.Conn, role string) error {
	r := pgx.Identifier{role}.Sanitize()
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, grantLock); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+r+`;
			REVOKE ALL ON `+versionTable+` FROM `+r)
		return err
	})
}
```

## `main`

`migrate`'s telemetry is named `migrate`:

```go
func BuildTelemetry(ctx context.Context, p bedrock.Process, dial telemetry.Dialer) (telemetry.SDK, error) {
	s := telemetry.DefaultSettings()
	s.ServiceName = "migrate"
	return telemetry.New(ctx, s, dial, p.Stdout)
}
```

```go
func main() {
	humus.Main(migrate.Init, humus.WithTelemetry(migrate.BuildTelemetry))
}
```

## The init container

`migrate` is each Deployment's init container, so a pod's service starts
only once its schema is current. The cart's:

```yaml
initContainers:
  - name: migrate
    image: shoppingcart
    command: ["/migrate"]
    env:
      - name: MIGRATIONS
        value: carts
      - name: APP_ROLE
        value: cart
      - name: DATABASE_URL_FILE
        value: /etc/cart/database-owner/url
```

The catalog's is the same, with `catalog` for both, and its own owner's
URL.

## The roles

Each database has two roles: its owner, which `migrate` connects as, and
the service's, which can connect and, once `migrate` has granted it, read
and write. The platform makes them. The kind overlay makes the cart's this
way:

```sql
CREATE ROLE carts_owner LOGIN PASSWORD '$carts_owner_password';
CREATE ROLE cart LOGIN PASSWORD '$cart_password';
CREATE DATABASE carts OWNER carts_owner;
REVOKE ALL ON DATABASE carts FROM PUBLIC;
GRANT CONNECT ON DATABASE carts TO cart;
```

So a service can't change its schema, and neither can read the other's
database.

## The cart's Store

The cart's `Store` is a `pgxpool.Pool` and the App's `fault.Injector`:

```go
type Store struct {
	pool   *pgxpool.Pool
	faults fault.Injector
}

func New(pool *pgxpool.Pool, faults fault.Injector) *Store {
	return &Store{pool: pool, faults: faults}
}
```

Every method evaluates its injection point, `postgresql:carts`, before its
query. An abort fails the query as an unreachable database does:

```go
var ErrUnavailable = endpoint.ErrDatabaseUnavailable

const Point = "postgresql:carts"
```

```go
func (s *Store) inject(ctx context.Context) error {
	f := s.faults.Eval(ctx, Point)
	if err := f.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: Point, Err: err}
	}
	if f.Abort {
		return fmt.Errorf("%w: %w", ErrUnavailable, &fault.InjectedError{Point: Point})
	}
	return nil
}
```

A connection that failed is `ErrUnavailable` too, which the API answers
with a 503:

```go
func wrap(op string, err error) error {
	var connect *pgconn.ConnectError
	var network net.Error
	if errors.As(err, &connect) || errors.As(err, &network) {
		return fmt.Errorf("%s: %w: %w", op, ErrUnavailable, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}
```

Each change to an item updates its cart's `updated_at` in the same
statement:

```go
rows, err := s.pool.Query(ctx, `
	WITH cart AS (
		UPDATE carts SET updated_at = now() WHERE id = $1 AND owner = $2 RETURNING id
	)
	INSERT INTO cart_items (cart_id, product_id, quantity, unit_price)
	SELECT cart.id, $3, $4, $5 FROM cart
	RETURNING `+itemColumns,
	cartID, owner, item.ProductID, item.Quantity, item.UnitPrice,
)
```
