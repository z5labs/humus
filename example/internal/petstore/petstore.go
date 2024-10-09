// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package petstore

import (
	"context"
	"io"
	"sync"

	"github.com/z5labs/humus/example/internal/petstorepb"

	"go.opentelemetry.io/otel"
)

type InMemory struct {
	mu     sync.Mutex
	pets   map[int64]*petstorepb.Pet
	images map[int64][]byte
}

func NewInMemory() *InMemory {
	return &InMemory{
		pets:   make(map[int64]*petstorepb.Pet),
		images: make(map[int64][]byte),
	}
}

func (s *InMemory) Add(ctx context.Context, pet *petstorepb.Pet) {
	_, span := otel.Tracer("petstore").Start(ctx, "InMemory.Add")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.pets[pet.Id] = pet
}

func (s *InMemory) Get(ctx context.Context, id int64) (*petstorepb.Pet, bool) {
	_, span := otel.Tracer("petstore").Start(ctx, "InMemory.Get")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	pet, exists := s.pets[id]
	return pet, exists
}

func (s *InMemory) Delete(ctx context.Context, id int64) {
	_, span := otel.Tracer("petstore").Start(ctx, "InMemory.Delete")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.pets, id)
}

func (s *InMemory) Pets(ctx context.Context) []*petstorepb.Pet {
	_, span := otel.Tracer("petstore").Start(ctx, "InMemory.Pets")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	pets := make([]*petstorepb.Pet, 0, len(s.pets))
	for _, pet := range s.pets {
		pets = append(pets, pet)
	}
	return pets
}

func (s *InMemory) IndexImage(ctx context.Context, pet *petstorepb.Pet, r io.Reader) error {
	_, span := otel.Tracer("petstore").Start(ctx, "InMemory.IndexImage")
	defer span.End()

	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.images[pet.Id] = b
	return nil
}

func (s *InMemory) GetImage(ctx context.Context, id int64) ([]byte, bool) {
	_, span := otel.Tracer("petstore").Start(ctx, "InMemory.GetImage")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	img, exists := s.images[id]
	return img, exists
}
