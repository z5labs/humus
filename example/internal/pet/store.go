// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package pet

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

type record interface {
	At() time.Time
}

type registrationRecord struct {
	at time.Time

	id    string
	name  string
	age   uint64
	kind  string
	breed string
	fur   FurDesc
}

func (r registrationRecord) At() time.Time {
	return r.at
}

type Store struct {
	mu  sync.Mutex
	log map[uuid.UUID][]record
}

func NewStore() *Store {
	return &Store{
		log: make(map[uuid.UUID][]record),
	}
}

type FurDesc struct {
	Kind  string
	Color string
}

type RegisterRequest struct {
	Age   uint64
	Name  string
	Kind  string
	Breed string
	Fur   FurDesc
}

type RegisterResponse struct {
	ID       string
	TempName string
}

func (s *Store) Register(ctx context.Context, req *RegisterRequest) (*RegisterResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newID(s.log)
	resp := &RegisterResponse{
		ID: id.String(),
	}

	name := req.Name
	if len(name) == 0 {
		name = id.String()
		resp.TempName = name
	}

	s.log[id] = []record{registrationRecord{
		at:    time.Now(),
		id:    resp.ID,
		name:  name,
		age:   req.Age,
		kind:  req.Kind,
		breed: req.Breed,
		fur:   req.Fur,
	}}

	return resp, nil
}

func newID[T any](ids map[uuid.UUID]T) uuid.UUID {
	id := uuid.New()
	for _, exists := ids[id]; !exists; {
		id = uuid.New()
		_, exists = ids[id]
	}
	return id
}
