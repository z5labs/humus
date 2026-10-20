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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/z5labs/humus/fault"
)

type Admin interface {
	Provision(ctx context.Context, name, password string) error
	Drop(ctx context.Context, name string) error
}

type Server struct {
	pool   *pgxpool.Pool
	faults fault.Injector
}

var _ Admin = (*Server)(nil)

func New(pool *pgxpool.Pool, faults fault.Injector) *Server {
	return &Server{pool: pool, faults: faults}
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
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		_, err = s.pool.Exec(ctx, alter)
		return err
	}
	_, err = s.pool.Exec(ctx, "CREATE ROLE "+ident+" WITH LOGIN PASSWORD "+quoteLiteral(verifier))
	if hasCode(err, "42710") {
		_, err = s.pool.Exec(ctx, alter)
	}
	return err
}

func (s *Server) ensureDatabase(ctx context.Context, name string) error {
	ident := pgx.Identifier{name}.Sanitize()
	var owner string
	err := s.pool.QueryRow(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = $1`, name).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = s.pool.Exec(ctx, "CREATE DATABASE "+ident+" OWNER "+ident)
		if !hasCode(err, "42P04") {
			return err
		}
	} else if err != nil {
		return err
	}
	if owner == name {
		return nil
	}
	_, err = s.pool.Exec(ctx, "ALTER DATABASE "+ident+" OWNER TO "+ident)
	return err
}

func (s *Server) Drop(ctx context.Context, name string) error {
	if err := s.inject(ctx, "postgresql:drop"); err != nil {
		return fmt.Errorf("dropping %s: %w", name, err)
	}
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := s.pool.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("dropping %s: dropping its database: %w", name, err)
	}
	if _, err := s.pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident); err != nil {
		return fmt.Errorf("dropping %s: dropping its role: %w", name, err)
	}
	return nil
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
