# The ledger's accounts

The ledger keeps its accounts and its transfers in two PostgreSQL tables,
and moves money between accounts in one transaction per transfer, so money
is never made or lost.

## `schema.sql`

`ledger/store/schema.sql` makes the accounts, and the ledger's own
`external` account, which deposits come from:

```sql
CREATE TABLE accounts (
    id                 text        PRIMARY KEY,
    balance_cents      bigint      NOT NULL DEFAULT 0,
    balance_changed_at timestamptz NOT NULL DEFAULT now(),
    CHECK (id = 'external' OR balance_cents >= 0)
);

INSERT INTO accounts (id) VALUES ('external');
```

The `CHECK` refuses a balance below zero for every account but `external`.
`external`'s balance is the negative of every deposit, so the balances
always sum to zero.

Then the transfers:

```sql
CREATE TABLE transfers (
    id              text        PRIMARY KEY,
    from_account_id text        NOT NULL REFERENCES accounts (id) DEFERRABLE INITIALLY DEFERRED,
    to_account_id   text        NOT NULL REFERENCES accounts (id) DEFERRABLE INITIALLY DEFERRED,
    amount_cents    bigint      NOT NULL CHECK (amount_cents > 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (from_account_id <> to_account_id)
);
```

The foreign keys are deferred, because a transfer is inserted before its
accounts are read, and the Store answers a missing account itself.

## The Store

The `Store` is a `pgxpool.Pool` and the App's `fault.Injector`:

```go
type Store struct {
	pool   *pgxpool.Pool
	faults fault.Injector
}

func New(pool *pgxpool.Pool, faults fault.Injector) *Store {
	return &Store{pool: pool, faults: faults}
}
```

Its errors are what the ledger's methods answer:

```go
var (
	ErrNotFound    = errors.New("no such account")
	ErrNotPositive = errors.New("amount isn't positive")
	ErrSameAccount = errors.New("account sent to itself")
	ErrReusedID    = errors.New("transfer ID reused for another transfer")
	ErrUnavailable = errors.New("database unavailable")
)

type ShortBalanceError struct {
	AccountID    string
	BalanceCents int64
	AmountCents  int64
}
```

Every other error is the database's, and is `ErrUnavailable`:

```go
func unavailable(err error) error {
	var short *ShortBalanceError
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrReusedID), errors.As(err, &short), errors.Is(err, ErrUnavailable):
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
```

## Opening an account

An account opened twice is answered as it is:

```go
err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `INSERT INTO accounts (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, id)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx,
		`SELECT id, balance_cents, balance_changed_at FROM accounts WHERE id = $1`, id,
	).Scan(&a.ID, &a.BalanceCents, &a.BalanceChangedAt)
})
```

## A transfer's transaction

A deposit and a transfer both move money. A deposit is from `external`, and
skips the balance check:

```go
func (s *Store) Deposit(ctx context.Context, transferID, accountID string, amountCents int64) (Transfer, error) {
	t, err := s.move(ctx, Transfer{ID: transferID, FromAccountID: External, ToAccountID: accountID, AmountCents: amountCents}, false)
	if err != nil {
		return Transfer{}, fmt.Errorf("depositing %s into account %s: %w", transferID, accountID, err)
	}
	return t, nil
}
```

`move` checks the amount and the accounts, then runs one transaction:

```go
func (s *Store) move(ctx context.Context, want Transfer, checkBalance bool) (Transfer, error) {
	if want.AmountCents <= 0 {
		return Transfer{}, ErrNotPositive
	}
	if want.FromAccountID == want.ToAccountID {
		return Transfer{}, ErrSameAccount
	}
	if err := s.inject(ctx); err != nil {
		return Transfer{}, unavailable(err)
	}
	var t Transfer
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO transfers (id, from_account_id, to_account_id, amount_cents) VALUES ($1, $2, $3, $4)
			ON CONFLICT (id) DO NOTHING
			RETURNING id, from_account_id, to_account_id, amount_cents, created_at`,
			want.ID, want.FromAccountID, want.ToAccountID, want.AmountCents,
		).Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.AmountCents, &t.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return replay(ctx, tx, want, &t)
		}
		if err != nil {
			return err
		}
		return post(ctx, tx, t, checkBalance)
	})
	if err != nil {
		return Transfer{}, unavailable(err)
	}
	return t, nil
}
```

1. **It inserts the transfer,** `ON CONFLICT (id) DO NOTHING`.
2. **A transfer already there is replayed:** answered as it was made, or
   with `ErrReusedID` if it differs:

   ```go
   func replay(ctx context.Context, tx pgx.Tx, want Transfer, t *Transfer) error {
   	err := tx.QueryRow(ctx,
   		`SELECT id, from_account_id, to_account_id, amount_cents, created_at FROM transfers WHERE id = $1`, want.ID,
   	).Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.AmountCents, &t.CreatedAt)
   	if err != nil {
   		return err
   	}
   	if t.FromAccountID != want.FromAccountID || t.ToAccountID != want.ToAccountID || t.AmountCents != want.AmountCents {
   		return ErrReusedID
   	}
   	t.Replayed = true
   	return nil
   }
   ```

3. **A new one is posted.** `post` locks both accounts, in the order of
   their IDs, so two transfers between the same accounts can't deadlock:

   ```go
   rows, err := tx.Query(ctx,
   	`SELECT id, balance_cents FROM accounts WHERE id IN ($1, $2) ORDER BY id FOR UPDATE`,
   	t.FromAccountID, t.ToAccountID,
   )
   ```

4. **It checks both accounts and the source's balance:**

   ```go
   for _, id := range []string{t.FromAccountID, t.ToAccountID} {
   	if _, ok := balances[id]; !ok {
   		return fmt.Errorf("account %s: %w", id, ErrNotFound)
   	}
   }
   if from := balances[t.FromAccountID]; checkBalance && from < t.AmountCents {
   	return &ShortBalanceError{AccountID: t.FromAccountID, BalanceCents: from, AmountCents: t.AmountCents}
   }
   ```

5. **It updates both balances,** in one statement:

   ```go
   _, err = tx.Exec(ctx, `
   	UPDATE accounts
   	SET balance_cents = balance_cents + CASE WHEN id = $1 THEN -$3::bigint ELSE $3::bigint END,
   	    balance_changed_at = $4
   	WHERE id IN ($1, $2)`,
   	t.FromAccountID, t.ToAccountID, t.AmountCents, t.CreatedAt,
   )
   ```

An error rolls the transaction back, the transfer's row with it. So a
transfer refused for its balance isn't kept, and its ID can be sent again
once the money is there.

## Reading an account

```go
err := s.pool.QueryRow(ctx,
	`SELECT id, balance_cents, balance_changed_at FROM accounts WHERE id = $1`, id,
).Scan(&a.ID, &a.BalanceCents, &a.BalanceChangedAt)
if errors.Is(err, pgx.ErrNoRows) {
	err = ErrNotFound
}
```

Next: [Serving the ledger](/humus/pr-preview/pr-1369/walkthroughs/grpc/wallet/serving-the-ledger/)
