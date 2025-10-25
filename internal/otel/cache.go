// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package otel

import "sync"

// cache is a thread-safe generic cache for storing key-value pairs.
type cache[K comparable, V any] struct {
	mu   sync.Mutex
	data map[K]V
}

// newCache creates a new cache instance.
func newCache[K comparable, V any]() *cache[K, V] {
	return &cache[K, V]{
		data: make(map[K]V),
	}
}

// getOr retrieves a value from the cache, or creates it using the provided function if not found.
func (c *cache[K, V]) getOr(k K, f func() (V, error)) (V, error) {
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
