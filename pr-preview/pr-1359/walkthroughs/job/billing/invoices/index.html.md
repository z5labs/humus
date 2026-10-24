# The invoices

## The subscriptions

`postgres/schema.sql` starts with the subscriptions. Your other services keep
them, and the job only reads them:

```sql
CREATE TABLE IF NOT EXISTS subscriptions (
    account_id    TEXT     PRIMARY KEY,
    amount_cents  BIGINT   NOT NULL CHECK (amount_cents > 0),
    currency      CHAR(3)  NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    active        BOOLEAN  NOT NULL
);
```

## The invoices

One invoice per account and month. It stays `pending` until it is charged,
then it is `charged`, `declined` or `failed`, each with the one column that
says why:

```sql
CREATE TABLE IF NOT EXISTS invoices (
    id            UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    TEXT         NOT NULL,
    period        DATE         NOT NULL CHECK (period = date_trunc('month', period)),
    amount_cents  BIGINT       NOT NULL CHECK (amount_cents > 0),
    currency      CHAR(3)      NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    state         TEXT         NOT NULL DEFAULT 'pending',
    charge_id     TEXT,
    decline_code  TEXT,
    answer        TEXT,
    issued_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    settled_at    TIMESTAMPTZ,
    UNIQUE (account_id, period),
    CHECK (
        (state = 'pending'  AND charge_id IS NULL     AND decline_code IS NULL     AND answer IS NULL     AND settled_at IS NULL) OR
        (state = 'charged'  AND charge_id IS NOT NULL AND decline_code IS NULL     AND answer IS NULL     AND settled_at IS NOT NULL) OR
        (state = 'declined' AND charge_id IS NULL     AND decline_code IS NOT NULL AND answer IS NULL     AND settled_at IS NOT NULL) OR
        (state = 'failed'   AND charge_id IS NULL     AND decline_code IS NULL     AND answer IS NOT NULL AND settled_at IS NOT NULL)
    )
);
```

The `id` is also the charge's idempotency key, so the API answers a charge
sent twice with the charge it already made.

## The pending invoices

Every run reads the pending invoices, oldest first:

```sql
CREATE INDEX IF NOT EXISTS invoices_pending ON invoices (period, issued_at, account_id) WHERE state = 'pending';
```

The job applies no migrations. Apply `schema.sql` before it runs, with your
migration tool.

## The store

Start the `postgres` package with an invoice and a store over a pgx pool:

```go
type Invoice struct {
	ID          uuid.UUID
	AccountID   string
	Period      time.Time
	AmountCents int64
	Currency    string
}

type Store struct {
	pool   *pgxpool.Pool
	faults fault.Injector
}

func New(pool *pgxpool.Pool, faults fault.Injector) *Store {
	return &Store{pool: pool, faults: faults}
}
```

## Injection points

Before each statement, the store asks the App's `fault.Injector` whether a
scenario fails it, and returns a `*fault.InjectedError` if it does:

```go
func (s *Store) inject(ctx context.Context, point string) error {
	f := s.faults.Eval(ctx, point)
	if err := f.Wait(ctx); err != nil {
		return &fault.InjectedError{Point: point, Err: err}
	}
	if f.Abort {
		return &fault.InjectedError{Point: point}
	}
	return nil
}
```

## Issuing a month

One statement issues an invoice for every active subscription.
`ON CONFLICT DO NOTHING` makes running it again issue nothing:

```go
func (s *Store) Issue(ctx context.Context, period time.Time) (issued int64, err error) {
	if err := s.inject(ctx, "postgresql:issue"); err != nil {
		return 0, fmt.Errorf("issuing %s: %w", period.Format("2006-01"), err)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO invoices (account_id, period, amount_cents, currency)
		SELECT account_id, $1::date, amount_cents, currency FROM subscriptions WHERE active
		ON CONFLICT (account_id, period) DO NOTHING`,
		period,
	)
	if err != nil {
		return 0, fmt.Errorf("issuing %s: %w", period.Format("2006-01"), err)
	}
	return tag.RowsAffected(), nil
}
```

## Reading what is pending

Pending invoices from every month, not just the run's, so a run catches up on
what an earlier one left:

```go
func (s *Store) Pending(ctx context.Context) ([]Invoice, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, account_id, period, amount_cents, currency FROM invoices
		WHERE state = 'pending'
		ORDER BY period, issued_at, account_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("reading pending invoices: %w", err)
	}
	invoices, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invoice, error) {
		var inv Invoice
		err := row.Scan(&inv.ID, &inv.AccountID, &inv.Period, &inv.AmountCents, &inv.Currency)
		return inv, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading pending invoices: %w", err)
	}
	return invoices, nil
}
```

## Settling an invoice

Each final state has a method. They all update the invoice only while it is
`pending`, so nothing overwrites a final state:

```go
func (s *Store) SettleCharged(ctx context.Context, id uuid.UUID, chargeID string) (settled bool, err error) {
	return s.settle(ctx, id, "charged", `charge_id`, chargeID)
}

func (s *Store) SettleDeclined(ctx context.Context, id uuid.UUID, declineCode string) (settled bool, err error) {
	return s.settle(ctx, id, "declined", `decline_code`, declineCode)
}

func (s *Store) SettleFailed(ctx context.Context, id uuid.UUID, answer string) (settled bool, err error) {
	return s.settle(ctx, id, "failed", `answer`, answer)
}

func (s *Store) settle(ctx context.Context, id uuid.UUID, state, column, value string) (bool, error) {
	if err := s.inject(ctx, "postgresql:settle"); err != nil {
		return false, fmt.Errorf("settling invoice %s as %s: %w", id, state, err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE invoices SET state = $2, `+column+` = $3, settled_at = now() WHERE id = $1 AND state = 'pending'`,
		id, state, value,
	)
	if err != nil {
		return false, fmt.Errorf("settling invoice %s as %s: %w", id, state, err)
	}
	return tag.RowsAffected() == 1, nil
}
```

`settled` is false when the invoice was already final, such as when two runs
that overlap both charge it.

Next: [Charging](/humus/pr-preview/pr-1359/walkthroughs/job/billing/charging/)
