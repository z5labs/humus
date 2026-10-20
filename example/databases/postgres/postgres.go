// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package postgres provisions and drops each Database's database and role in PostgreSQL.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/fault"
)

const scope = "github.com/z5labs/humus/example/databases/postgres"

type Admin interface {
	Provision(ctx context.Context, name, password string) error
	Drop(ctx context.Context, name string) error
}

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

func (s *Server) exec(ctx context.Context, summary, sql string) error {
	ctx, span := s.start(ctx, summary)
	_, err := s.pool.Exec(ctx, sql)
	end(span, err)
	return err
}

func (s *Server) queryRow(ctx context.Context, summary, sql string, arg any, dest any) error {
	ctx, span := s.start(ctx, summary)
	err := s.pool.QueryRow(ctx, sql, arg).Scan(dest)
	if errors.Is(err, pgx.ErrNoRows) {
		span.End()
		return err
	}
	end(span, err)
	return err
}

func (s *Server) start(ctx context.Context, summary string) (context.Context, trace.Span) {
	operation, _, _ := strings.Cut(summary, " ")
	return s.tracer.Start(ctx, summary,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(s.attrs...),
		trace.WithAttributes(semconv.DBQuerySummary(summary), semconv.DBOperationName(operation)),
	)
}

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

func (s *Server) Drop(ctx context.Context, name string) error {
	if err := s.inject(ctx, "postgresql:drop"); err != nil {
		return fmt.Errorf("dropping %s: %w", name, err)
	}
	ident := pgx.Identifier{name}.Sanitize()
	if err := s.exec(ctx, "DROP DATABASE", "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("dropping %s: dropping its database: %w", name, err)
	}
	if err := s.exec(ctx, "DROP ROLE", "DROP ROLE IF EXISTS "+ident); err != nil {
		return fmt.Errorf("dropping %s: dropping its role: %w", name, err)
	}
	return nil
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
