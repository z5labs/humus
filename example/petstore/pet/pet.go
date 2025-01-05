// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package pet

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
)

type Kind string

const (
	Cat Kind = "cat"
	Dog Kind = "dog"
)

type Pet struct {
	Id           string    `json:"id"`
	Name         string    `json:"name"`
	Kind         Kind      `json:"kind"`
	RegisteredAt time.Time `json:"registered_at"`
	Adopted      bool      `json:"adopted"`
}

type Store struct {
	mu sync.Mutex
	m  map[string]Pet
}

func NewStore() *Store {
	return &Store{
		m: make(map[string]Pet),
	}
}

type RegisterRequest struct {
	Name string
	Kind Kind
}

type RegisterResponse struct {
	Pet Pet
}

func (s *Store) Register(ctx context.Context, req *RegisterRequest) (*RegisterResponse, error) {
	_, span := otel.Tracer("pet").Start(ctx, "Store.Register")
	defer span.End()

	uid, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id := uid.String()
	p := Pet{
		Id:           id,
		Name:         req.Name,
		Kind:         req.Kind,
		RegisteredAt: time.Now(),
	}
	s.m[id] = p

	resp := &RegisterResponse{
		Pet: p,
	}
	return resp, nil
}
