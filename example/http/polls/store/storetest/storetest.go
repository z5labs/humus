// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package storetest is the tests' in-memory Store, which keeps every rule the PostgreSQL Store keeps.
package storetest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/z5labs/humus/example/http/polls/store"
	"github.com/z5labs/humus/fault"
)

type poll struct {
	question string
	options  []store.Option
	voters   map[string]bool
}

type database struct {
	mu     sync.Mutex
	polls  map[uuid.UUID]*poll
	nextID int64
	subs   map[uuid.UUID]map[chan store.Poll]struct{}
}

type Store struct {
	*database
	faults fault.Injector
}

func New() *Store {
	return &Store{database: &database{polls: make(map[uuid.UUID]*poll), subs: make(map[uuid.UUID]map[chan store.Poll]struct{})}}
}

func (s *Store) WithFaults(faults fault.Injector) *Store {
	return &Store{database: s.database, faults: faults}
}

func (s *Store) inject(ctx context.Context) error {
	f := s.faults.Eval(ctx, store.Point)
	if err := f.Wait(ctx); err != nil {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, &fault.InjectedError{Point: store.Point, Err: err})
	}
	if f.Abort {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, &fault.InjectedError{Point: store.Point})
	}
	return nil
}

func check(question string, options []string) error {
	if question == "" {
		return fmt.Errorf("%w: it has no question", store.ErrInvalidPoll)
	}
	if len(options) < 2 {
		return fmt.Errorf("%w: it has %d options, fewer than two", store.ErrInvalidPoll, len(options))
	}
	seen := make(map[string]bool, len(options))
	for _, o := range options {
		if o == "" {
			return fmt.Errorf("%w: an option has no text", store.ErrInvalidPoll)
		}
		if seen[o] {
			return fmt.Errorf("%w: the option %q is given twice", store.ErrInvalidPoll, o)
		}
		seen[o] = true
	}
	return nil
}

func (s *Store) CreatePoll(ctx context.Context, question string, options []string) (uuid.UUID, error) {
	if err := check(question, options); err != nil {
		return uuid.Nil, fmt.Errorf("creating a poll: %w", err)
	}
	if err := s.inject(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("creating a poll: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := &poll{question: question, voters: make(map[string]bool)}
	for _, text := range options {
		s.nextID++
		p.options = append(p.options, store.Option{ID: s.nextID, Text: text})
	}
	id := uuid.New()
	s.polls[id] = p
	return id, nil
}

func (s *Store) Poll(ctx context.Context, id uuid.UUID) (store.Poll, error) {
	if err := s.inject(ctx); err != nil {
		return store.Poll{}, fmt.Errorf("reading poll %s: %w", id, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.polls[id]
	if !ok {
		return store.Poll{}, fmt.Errorf("reading poll %s: %w", id, store.ErrNoSuchPoll)
	}
	return p.results(id), nil
}

func (p *poll) results(id uuid.UUID) store.Poll {
	return store.Poll{ID: id, Question: p.question, Options: slices.Clone(p.options)}
}

func (s *Store) Subscribe(pollID uuid.UUID) (<-chan store.Poll, func()) {
	ch := make(chan store.Poll, 1)
	s.mu.Lock()
	if s.subs[pollID] == nil {
		s.subs[pollID] = make(map[chan store.Poll]struct{})
	}
	s.subs[pollID][ch] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.subs[pollID], ch)
			if len(s.subs[pollID]) == 0 {
				delete(s.subs, pollID)
			}
			close(ch)
		})
	}
}

func (s *database) publish(pollID uuid.UUID) {
	for ch := range s.subs[pollID] {
		select {
		case <-ch:
		default:
		}
		ch <- s.polls[pollID].results(pollID)
	}
}

func (s *Store) Vote(ctx context.Context, pollID uuid.UUID, optionID int64, voter string) error {
	if err := s.inject(ctx); err != nil {
		return fmt.Errorf("voting for option %d of poll %s: %w", optionID, pollID, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.vote(pollID, optionID, voter)
	if err != nil {
		return fmt.Errorf("voting for option %d of poll %s: %w", optionID, pollID, err)
	}
	s.publish(pollID)
	return nil
}

func (s *database) vote(pollID uuid.UUID, optionID int64, voter string) error {
	p, ok := s.polls[pollID]
	if !ok {
		return store.ErrNoSuchPoll
	}
	if p.voters[voter] {
		return store.ErrAlreadyVoted
	}
	i := slices.IndexFunc(p.options, func(o store.Option) bool { return o.ID == optionID })
	if i < 0 {
		return store.ErrNotAnOption
	}
	p.voters[voter] = true
	p.options[i].Votes++
	return nil
}
