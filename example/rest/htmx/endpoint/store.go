// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import "sync"

// ItemStore manages the list of items in memory.
type ItemStore struct {
	mu    sync.RWMutex
	items []string
}

// NewItemStore creates a new item store.
func NewItemStore() *ItemStore {
	return &ItemStore{
		items: []string{},
	}
}

// Add appends a new item to the store.
func (s *ItemStore) Add(item string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
}

// GetAll returns all items.
func (s *ItemStore) GetAll() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]string, len(s.items))
	copy(items, s.items)
	return items
}
