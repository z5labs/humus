// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
)

// memoryStore is an endpoint.Store in memory, which the tests serve in
// place of Postgres, since Postgres can't run on humustest's Host. It keeps
// the contract database.Store keeps, which the container tests check
// against Postgres itself.
type memoryStore struct {
	mu    sync.Mutex
	carts map[uuid.UUID]*endpoint.Cart
}

// inMemory is an openStore that opens a new memoryStore, and ignores the
// pool's config.
func inMemory(context.Context, *humus.Setup, *pgxpool.Config) (endpoint.Store, error) {
	return &memoryStore{carts: map[uuid.UUID]*endpoint.Cart{}}, nil
}

func (m *memoryStore) CreateCart(context.Context) (endpoint.Cart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	c := &endpoint.Cart{CartID: uuid.New(), Items: []endpoint.CartItem{}, CreatedAt: now, UpdatedAt: now}
	m.carts[c.CartID] = c
	return clone(c), nil
}

func (m *memoryStore) GetCart(_ context.Context, cartID uuid.UUID) (endpoint.Cart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.carts[cartID]
	if !ok {
		return endpoint.Cart{}, endpoint.ErrCartNotFound
	}
	return clone(c), nil
}

func (m *memoryStore) DeleteCart(_ context.Context, cartID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.carts[cartID]; !ok {
		return endpoint.ErrCartNotFound
	}
	delete(m.carts, cartID)
	return nil
}

func (m *memoryStore) AddItem(_ context.Context, cartID uuid.UUID, req endpoint.AddItemRequest) (endpoint.CartItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.carts[cartID]
	if !ok {
		return endpoint.CartItem{}, endpoint.ErrCartNotFound
	}
	now := time.Now().UTC()
	it := endpoint.CartItem{
		ItemID:    uuid.New(),
		CartID:    cartID,
		ProductID: req.ProductID,
		Quantity:  req.Quantity,
		UnitPrice: req.UnitPrice,
		CreatedAt: now,
		UpdatedAt: now,
	}
	c.Items = append(c.Items, it)
	c.UpdatedAt = now
	return it, nil
}

func (m *memoryStore) UpdateItem(_ context.Context, cartID, itemID uuid.UUID, quantity int) (endpoint.CartItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, i, err := m.find(cartID, itemID)
	if err != nil {
		return endpoint.CartItem{}, err
	}
	now := time.Now().UTC()
	c.Items[i].Quantity = quantity
	c.Items[i].UpdatedAt = now
	c.UpdatedAt = now
	return c.Items[i], nil
}

func (m *memoryStore) RemoveItem(_ context.Context, cartID, itemID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, i, err := m.find(cartID, itemID)
	if err != nil {
		return err
	}
	c.Items = slices.Delete(c.Items, i, i+1)
	c.UpdatedAt = time.Now().UTC()
	return nil
}

// find returns the cart cartID and the index of its item itemID. m.mu is
// held.
func (m *memoryStore) find(cartID, itemID uuid.UUID) (*endpoint.Cart, int, error) {
	c, ok := m.carts[cartID]
	if !ok {
		return nil, 0, endpoint.ErrCartNotFound
	}
	i := slices.IndexFunc(c.Items, func(it endpoint.CartItem) bool { return it.ItemID == itemID })
	if i < 0 {
		return nil, 0, endpoint.ErrItemNotFound
	}
	return c, i, nil
}

// clone returns a copy of c that shares no items with it.
func clone(c *endpoint.Cart) endpoint.Cart {
	out := *c
	out.Items = slices.Clone(c.Items)
	if out.Items == nil {
		out.Items = []endpoint.CartItem{}
	}
	return out
}
