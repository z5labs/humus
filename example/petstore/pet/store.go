// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package pet

import (
	"context"
	"sync"

	"github.com/z5labs/humus/example/petstore/petstorepb"

	"go.opentelemetry.io/otel"
)

type Store struct {
	mu   sync.Mutex
	pets map[int64]*petstorepb.Pet
}

func NewStore() *Store {
	return &Store{
		pets: make(map[int64]*petstorepb.Pet),
	}
}

func (s *Store) Add(ctx context.Context, pet *petstorepb.Pet) {
	_, span := otel.Tracer("pet").Start(ctx, "Store.Add")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pets[pet.Id] = pet
}

func (s *Store) Get(ctx context.Context, id int64) (*petstorepb.Pet, bool) {
	_, span := otel.Tracer("pet").Start(ctx, "Store.Get")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	pet, exists := s.pets[id]
	return pet, exists
}

func (s *Store) Delete(ctx context.Context, id int64) {
	_, span := otel.Tracer("pet").Start(ctx, "Store.Delete")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.pets, id)
}

func (s *Store) Pets(ctx context.Context) []*petstorepb.Pet {
	_, span := otel.Tracer("pet").Start(ctx, "Store.Pets")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	pets := make([]*petstorepb.Pet, len(s.pets))
	for _, pet := range s.pets {
		pets = append(pets, pet)
	}
	return pets
}
