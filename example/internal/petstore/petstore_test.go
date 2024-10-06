// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package petstore

import (
	"context"
	"testing"

	"github.com/z5labs/humus/example/internal/petstorepb"

	"github.com/stretchr/testify/assert"
)

func TestInMemory_Add(t *testing.T) {
	t.Run("will add pet to store", func(t *testing.T) {
		t.Run("always", func(t *testing.T) {
			store := NewInMemory()

			store.Add(context.Background(), &petstorepb.Pet{
				Id: 1,
			})

			if !assert.Contains(t, store.pets, int64(1)) {
				return
			}
		})
	})
}

func TestInMemory_Get(t *testing.T) {
	t.Run("will not return pet", func(t *testing.T) {
		t.Run("if there are no pets in the store", func(t *testing.T) {
			store := NewInMemory()

			pet, found := store.Get(context.Background(), 1)
			if !assert.False(t, found) {
				return
			}
			if !assert.Nil(t, pet) {
				return
			}
		})

		t.Run("if the pet id is not found", func(t *testing.T) {
			store := NewInMemory()
			store.pets[1] = &petstorepb.Pet{}

			pet, found := store.Get(context.Background(), 2)
			if !assert.False(t, found) {
				return
			}
			if !assert.Nil(t, pet) {
				return
			}
		})
	})

	t.Run("will return pet", func(t *testing.T) {
		t.Run("if pet id is found", func(t *testing.T) {
			store := NewInMemory()
			store.pets[1] = &petstorepb.Pet{}

			pet, found := store.Get(context.Background(), 1)
			if !assert.True(t, found) {
				return
			}
			if !assert.NotNil(t, pet) {
				return
			}
		})
	})
}

func TestInMemory_Delete(t *testing.T) {
	t.Run("will delete pet", func(t *testing.T) {
		t.Run("if the pet id is found", func(t *testing.T) {
			store := NewInMemory()
			store.pets[1] = &petstorepb.Pet{}

			store.Delete(context.Background(), 1)
			if !assert.Empty(t, store.pets) {
				return
			}
		})
	})

	t.Run("will be a no-op", func(t *testing.T) {
		t.Run("if the pet id is not found", func(t *testing.T) {
			store := NewInMemory()

			store.Delete(context.Background(), 1)
			if !assert.Empty(t, store.pets) {
				return
			}
		})
	})
}

func TestInMemory_Pets(t *testing.T) {
	t.Run("will return pets", func(t *testing.T) {
		t.Run("if there are pets in the store", func(t *testing.T) {
			store := NewInMemory()

			store.pets[1] = &petstorepb.Pet{Id: 1}
			store.pets[2] = &petstorepb.Pet{Id: 2}

			pets := store.Pets(context.Background())
			if !assert.Len(t, pets, 2) {
				return
			}
		})
	})
}
