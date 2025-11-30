// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"sync"

	"github.com/google/uuid"
)

// User represents a user in the system
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// UserStore is a simple in-memory user store
type UserStore struct {
	mu    sync.RWMutex
	users map[string]*User
	emails map[string]bool
}

// NewUserStore creates a new user store
func NewUserStore() *UserStore {
	return &UserStore{
		users:  make(map[string]*User),
		emails: make(map[string]bool),
	}
}

// Create adds a new user to the store
func (s *UserStore) Create(name, email string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if email already exists
	if s.emails[email] {
		return nil, newConflictError("email", email)
	}

	user := &User{
		ID:    uuid.New().String(),
		Name:  name,
		Email: email,
	}

	s.users[user.ID] = user
	s.emails[email] = true

	return user, nil
}

// Get retrieves a user by ID
func (s *UserStore) Get(id string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[id]
	if !ok {
		return nil, newNotFoundError("User", id)
	}

	return user, nil
}

// List returns all users
func (s *UserStore) List() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]*User, 0, len(s.users))
	for _, user := range s.users {
		users = append(users, user)
	}

	return users
}
