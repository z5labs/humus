// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"maps"
	"slices"
	"sync"
)

type store struct {
	mu     sync.Mutex
	nextID int
	users  map[int]user
	keys   map[string]int
}

func newStore() *store {
	return &store{nextID: 1, users: map[int]user{}, keys: map[string]int{}}
}

func (s *store) list(offset, limit int) []user {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := slices.Sorted(maps.Keys(s.users))
	start := min(offset, len(ids))
	end := min(start+limit, len(ids))
	listed := []user{}
	for _, id := range ids[start:end] {
		listed = append(listed, s.users[id])
	}
	return listed
}

func (s *store) create(key string, in userInput) user {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[s.keys[key]]; ok && key != "" {
		return u
	}
	u := user{ID: s.nextID, Name: in.Name, Email: in.Email}
	s.users[u.ID] = u
	s.nextID++
	if key != "" {
		s.keys[key] = u.ID
	}
	return u
}

func (s *store) get(id int) (user, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	return u, ok
}

func (s *store) update(id int, in userInput) (user, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return user{}, false
	}
	u := user{ID: id, Name: in.Name, Email: in.Email}
	s.users[id] = u
	return u, true
}

func (s *store) delete(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[id]; !ok {
		return false
	}
	delete(s.users, id)
	return true
}
