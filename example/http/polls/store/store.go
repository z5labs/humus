// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package store keeps polls and their votes in PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNoSuchPoll   = errors.New("no such poll")
	ErrNotAnOption  = errors.New("not an option of the poll")
	ErrAlreadyVoted = errors.New("already voted on the poll")
	ErrInvalidPoll  = errors.New("poll breaks its rules")
	ErrUnavailable  = errors.New("database unavailable")
)

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	onePerVoter         = "votes_one_per_voter"
	optionOfPoll        = "votes_option_of_poll"
)

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

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

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

func unavailable(err error) error {
	switch {
	case errors.Is(err, ErrNoSuchPoll), errors.Is(err, ErrNotAnOption), errors.Is(err, ErrAlreadyVoted), errors.Is(err, ErrInvalidPoll), errors.Is(err, ErrUnavailable):
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func (s *Store) CreatePoll(ctx context.Context, question string, options []string) (uuid.UUID, error) {
	if err := check(question, options); err != nil {
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

func (s *Store) Poll(ctx context.Context, id uuid.UUID) (Poll, error) {
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

func (s *Store) Vote(ctx context.Context, pollID uuid.UUID, optionID int64, voter string) error {
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
