// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package concurrent

import "sync"

// Cache
type Cache[K comparable, V any] struct {
	mu   sync.Mutex
	data map[K]V
}

// NewCache
func NewCache[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{
		data: make(map[K]V),
	}
}

// Get
func (c *Cache[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	v, ok := c.data[k]
	return v, ok
}

// GetOr
func (c *Cache[K, V]) GetOr(k K, f func() (V, error)) (V, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	v, ok := c.data[k]
	if ok {
		return v, nil
	}

	v, err := f()
	if err != nil {
		return v, err
	}

	c.data[k] = v
	return v, nil
}
