# The schema and the store

## The schema

A poll and its options, in `store/schema.sql`. A question and an option
can't be empty, and a poll's options are unique by position and by text:

```sql
CREATE TABLE polls (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    question   text        NOT NULL CHECK (question <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE options (
    id       bigint  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    poll_id  uuid    NOT NULL REFERENCES polls (id),
    position integer NOT NULL,
    text     text    NOT NULL CHECK (text <> ''),
    UNIQUE (poll_id, id),
    UNIQUE (poll_id, position),
    UNIQUE (poll_id, text)
);
```

A vote's primary key is its poll and its voter, so a browser has one vote
on each poll. Its foreign key names the poll as well as the option, which
`UNIQUE (poll_id, id)` allows, so a vote is always for an option of its own
poll:

```sql
CREATE TABLE votes (
    poll_id   uuid        NOT NULL,
    option_id bigint      NOT NULL,
    voter     text        NOT NULL,
    voted_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT votes_one_per_voter PRIMARY KEY (poll_id, voter),
    CONSTRAINT votes_option_of_poll FOREIGN KEY (poll_id, option_id) REFERENCES options (poll_id, id)
);

CREATE INDEX votes_by_option ON votes (poll_id, option_id);
```

The trigger that tells every replica about a vote is the rest of the file,
on [Every replica](/humus/pr-preview/pr-1639/walkthroughs/http/polls/replicas/).

## The roles

The database has two roles: its owner, which holds the schema, and the
service's. The platform makes them, and applies `schema.sql` as the owner.
The kind overlay's `up.sh` makes them this way:

```sql
CREATE ROLE polls_owner NOLOGIN;
CREATE ROLE polls LOGIN PASSWORD '$polls_password';
CREATE DATABASE polls OWNER polls_owner;
REVOKE ALL ON DATABASE polls FROM PUBLIC;
GRANT CONNECT ON DATABASE polls TO polls;
```

Then it grants the service only what it does:

```sql
GRANT SELECT, INSERT ON polls, options, votes TO polls;
```

So the service can't change the schema, or change or delete a vote.

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

It reads and writes these:

```go
type Option struct {
	ID    int64
	Text  string
	Votes int64
}

type Poll struct {
	ID       uuid.UUID
	Question string
	Options  []Option
}
```

## Its errors

Every error a method returns is one of these, wrapped:

```go
var (
	ErrNoSuchPoll   = errors.New("no such poll")
	ErrNotAnOption  = errors.New("not an option of the poll")
	ErrAlreadyVoted = errors.New("already voted on the poll")
	ErrInvalidPoll  = errors.New("poll breaks its rules")
	ErrUnavailable  = errors.New("database unavailable")
)
```

Any error that isn't one of the others is `ErrUnavailable`, which the error
page answers with a 503:

```go
func unavailable(err error) error {
	switch {
	case errors.Is(err, ErrNoSuchPoll), errors.Is(err, ErrNotAnOption), errors.Is(err, ErrAlreadyVoted), errors.Is(err, ErrInvalidPoll), errors.Is(err, ErrUnavailable):
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
```

## The injection point

Every method evaluates `postgresql:polls` before its query. A delay holds
the query back, and an abort fails it as an unreachable database does:

```go
const Point = "postgresql:polls"
```

```go
func inject(ctx context.Context, faults fault.Injector) error {
	f := faults.Eval(ctx, Point)
	if err := f.Wait(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, &fault.InjectedError{Point: Point, Err: err})
	}
	if f.Abort {
		return fmt.Errorf("%w: %w", ErrUnavailable, &fault.InjectedError{Point: Point})
	}
	return nil
}
```

## Creating a poll

`CreatePoll` checks the poll's rules itself, whatever its caller checked:

```go
func check(question string, options []string) error {
	if question == "" {
		return fmt.Errorf("%w: it has no question", ErrInvalidPoll)
	}
	if len(options) < 2 {
		return fmt.Errorf("%w: it has %d options, fewer than two", ErrInvalidPoll, len(options))
	}
	seen := make(map[string]bool, len(options))
	for _, o := range options {
		if o == "" {
			return fmt.Errorf("%w: an option has no text", ErrInvalidPoll)
		}
		if seen[o] {
			return fmt.Errorf("%w: the option %q is given twice", ErrInvalidPoll, o)
		}
		seen[o] = true
	}
	return nil
}
```

Then it inserts the poll and its options, numbered in order, in one
transaction:

```go
func (s *Store) CreatePoll(ctx context.Context, question string, options []string) (uuid.UUID, error) {
	if err := check(question, options); err != nil {
		return uuid.Nil, fmt.Errorf("creating a poll: %w", err)
	}
	if err := inject(ctx, s.faults); err != nil {
		return uuid.Nil, fmt.Errorf("creating a poll: %w", err)
	}
	var id uuid.UUID
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO polls (question) VALUES ($1) RETURNING id`, question).Scan(&id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO options (poll_id, position, text)
			SELECT $1, o.position, o.text FROM unnest($2::text[]) WITH ORDINALITY AS o (text, position)`,
			id, options,
		)
		return err
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("creating a poll: %w", unavailable(err))
	}
	return id, nil
}
```

## Reading a poll

`Poll` reads the question, the options and their counts in one query. A
poll with no options doesn't exist:

```go
func (s *Store) Poll(ctx context.Context, id uuid.UUID) (Poll, error) {
	if err := inject(ctx, s.faults); err != nil {
		return Poll{}, fmt.Errorf("reading poll %s: %w", id, err)
	}
	p := Poll{ID: id}
	rows, err := s.pool.Query(ctx, `
		SELECT p.question, o.id, o.text, count(v.voter)
		FROM polls p
		JOIN options o ON o.poll_id = p.id
		LEFT JOIN votes v ON v.poll_id = o.poll_id AND v.option_id = o.id
		WHERE p.id = $1
		GROUP BY p.question, o.id
		ORDER BY o.position`,
		id,
	)
	if err != nil {
		return Poll{}, fmt.Errorf("reading poll %s: %w", id, unavailable(err))
	}
	var o Option
	_, err = pgx.ForEachRow(rows, []any{&p.Question, &o.ID, &o.Text, &o.Votes}, func() error {
		p.Options = append(p.Options, o)
		return nil
	})
	if err == nil && len(p.Options) == 0 {
		err = ErrNoSuchPoll
	}
	if err != nil {
		return Poll{}, fmt.Errorf("reading poll %s: %w", id, unavailable(err))
	}
	return p, nil
}
```

## Voting

`Vote` inserts the vote and lets the constraints decide, so two replicas
that take the same browser's vote at once can't both count it:

```go
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	onePerVoter         = "votes_one_per_voter"
	optionOfPoll        = "votes_option_of_poll"
)
```

```go
func (s *Store) Vote(ctx context.Context, pollID uuid.UUID, optionID int64, voter string) error {
	if err := inject(ctx, s.faults); err != nil {
		return fmt.Errorf("voting for option %d of poll %s: %w", optionID, pollID, err)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO votes (poll_id, option_id, voter) VALUES ($1, $2, $3)`,
		pollID, optionID, voter,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == uniqueViolation && pgErr.ConstraintName == onePerVoter:
			err = ErrAlreadyVoted
		case pgErr.Code == foreignKeyViolation && pgErr.ConstraintName == optionOfPoll:
			err = s.notAnOption(ctx, pollID)
		}
	}
	if err != nil {
		return fmt.Errorf("voting for option %d of poll %s: %w", optionID, pollID, unavailable(err))
	}
	return nil
}
```

A vote that breaks the foreign key is for a poll that doesn't exist, or for
an option that isn't the poll's:

```go
func (s *Store) notAnOption(ctx context.Context, pollID uuid.UUID) error {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM polls WHERE id = $1)`, pollID).Scan(&exists)
	switch {
	case err != nil:
		return err
	case !exists:
		return ErrNoSuchPoll
	}
	return ErrNotAnOption
}
```

Next: [Every replica](/humus/pr-preview/pr-1639/walkthroughs/http/polls/replicas/)
