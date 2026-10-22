# Talking to PostgreSQL

The `postgres` package provisions and drops a `Database`'s role and
database, through one admin connection to the server.

## The admin interface

The Reconciler and its finalizer call PostgreSQL through an interface, so
their tests can fake it:

```go
type Admin interface {
	Provision(ctx context.Context, name, password string) error
	Drop(ctx context.Context, name string) error
}
```

Both are safe to repeat: a reconcile that fails partway is run again.

## The `Server`

`Server` implements `Admin` on a pool. It traces each statement, and
injects faults before each operation:

```go
type Server struct {
	pool   *pgxpool.Pool
	faults fault.Injector
	tracer trace.Tracer
	attrs  []attribute.KeyValue
}

var _ Admin = (*Server)(nil)

func New(pool *pgxpool.Pool, faults fault.Injector, tp trace.TracerProvider) *Server {
	cc := pool.Config().ConnConfig
	return &Server{
		pool:   pool,
		faults: faults,
		tracer: tp.Tracer(scope),
		attrs: []attribute.KeyValue{
			semconv.DBSystemNamePostgreSQL,
			semconv.DBNamespace(cc.Database),
			semconv.ServerAddress(cc.Host),
			semconv.ServerPort(int(cc.Port)),
		},
	}
}
```

## Provisioning

`Provision` injects, ensures the role, then ensures the database:

```go
func (s *Server) Provision(ctx context.Context, name, password string) error {
	if err := s.inject(ctx, "postgresql:provision"); err != nil {
		return fmt.Errorf("provisioning %s: %w", name, err)
	}
	verifier, err := scramVerifier(password)
	if err != nil {
		return fmt.Errorf("provisioning %s: %w", name, err)
	}
	if err := s.ensureRole(ctx, name, verifier); err != nil {
		return fmt.Errorf("provisioning %s: ensuring its role: %w", name, err)
	}
	if err := s.ensureDatabase(ctx, name); err != nil {
		return fmt.Errorf("provisioning %s: ensuring its database: %w", name, err)
	}
	return nil
}
```

`ensureRole` creates the role, or sets the password of the one already
there. A role created between the check and the create, `42710`, is
altered instead:

```go
func (s *Server) ensureRole(ctx context.Context, name, verifier string) error {
	ident := pgx.Identifier{name}.Sanitize()
	alter := "ALTER ROLE " + ident + " WITH LOGIN PASSWORD " + quoteLiteral(verifier)
	var exists bool
	err := s.queryRow(ctx, "SELECT pg_roles", `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name, &exists)
	if err != nil {
		return err
	}
	if exists {
		return s.exec(ctx, "ALTER ROLE", alter)
	}
	err = s.exec(ctx, "CREATE ROLE", "CREATE ROLE "+ident+" WITH LOGIN PASSWORD "+quoteLiteral(verifier))
	if hasCode(err, "42710") {
		err = s.exec(ctx, "ALTER ROLE", alter)
	}
	return err
}
```

`ensureDatabase` creates the database, owned by the role, or hands an
existing one to it:

```go
func (s *Server) ensureDatabase(ctx context.Context, name string) error {
	ident := pgx.Identifier{name}.Sanitize()
	var owner string
	err := s.queryRow(ctx, "SELECT pg_database", `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, name, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.exec(ctx, "CREATE DATABASE", "CREATE DATABASE "+ident+" OWNER "+ident)
		if !hasCode(err, "42P04") {
			return err
		}
	} else if err != nil {
		return err
	}
	if owner == name {
		return nil
	}
	return s.exec(ctx, "ALTER DATABASE", "ALTER DATABASE "+ident+" OWNER TO "+ident)
}
```

## Keeping the password out of the SQL

`CREATE ROLE` can't bind a password as a parameter, so the password would
be in the statement's text, which the server's log keeps. `Provision`
sends a salted SCRAM-SHA-256 verifier instead, which PostgreSQL stores as it
is:

```go
const scramIterations = 4096

func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salting the password: %w", err)
	}
	return scramVerifierWithSalt(password, salt, scramIterations)
}

func scramVerifierWithSalt(password string, salt []byte, iterations int) (string, error) {
	prepared, err := precis.OpaqueString.String(password)
	if err != nil {
		prepared = password
	}
	salted, err := pbkdf2.Key(sha256.New, prepared, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("hashing the password: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}
```

```go
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
```

Every error names the database, never the password.

## Keeping credentials out of the spans

Each statement's span is named by a summary of it, such as `ALTER ROLE`,
and carries no SQL text:

```go
func (s *Server) exec(ctx context.Context, summary, sql string) error {
	ctx, span := s.start(ctx, summary)
	_, err := s.pool.Exec(ctx, sql)
	end(span, err)
	return err
}
```

```go
func (s *Server) start(ctx context.Context, summary string) (context.Context, trace.Span) {
	operation, _, _ := strings.Cut(summary, " ")
	return s.tracer.Start(ctx, summary,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(s.attrs...),
		trace.WithAttributes(semconv.DBQuerySummary(summary), semconv.DBOperationName(operation)),
	)
}
```

A failed statement's span records PostgreSQL's error code:

```go
func end(span trace.Span, err error) {
	defer span.End()
	if err == nil {
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		span.SetAttributes(semconv.DBResponseStatusCode(pgErr.Code), semconv.ErrorTypeKey.String(pgErr.Code))
	} else {
		span.SetAttributes(semconv.ErrorType(err))
	}
	span.SetStatus(codes.Error, err.Error())
}
```

## The injection points

Each operation evaluates a point of its own before it calls the server, so
an aborted one changes nothing:

| Point | Evaluated |
|---|---|
| `postgresql:provision` | before the role and the database are ensured |
| `postgresql:drop` | before they're dropped |

```go
func (s *Server) inject(ctx context.Context, point string) error {
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

`Drop` is on [Cleaning up](/humus/pr-preview/pr-1289/walkthroughs/databases/cleaning-up/).
