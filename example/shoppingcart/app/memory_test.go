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

type memoryStore struct {
	mu    sync.Mutex
	carts map[uuid.UUID]*ownedCart
}

type ownedCart struct {
	owner string
	endpoint.Cart
}

func inMemory(context.Context, *humus.Setup, *pgxpool.Config) (endpoint.Store, error) {
	return &memoryStore{carts: map[uuid.UUID]*ownedCart{}}, nil
}

func (m *memoryStore) CreateCart(_ context.Context, owner string) (endpoint.Cart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	c := &ownedCart{owner: owner, Cart: endpoint.Cart{CartID: uuid.New(), Items: []endpoint.CartItem{}, CreatedAt: now, UpdatedAt: now}}
	m.carts[c.CartID] = c
	return clone(c), nil
}

func (m *memoryStore) GetCart(_ context.Context, owner string, cartID uuid.UUID) (endpoint.Cart, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.cart(owner, cartID)
	if err != nil {
		return endpoint.Cart{}, err
	}
	return clone(c), nil
}

func (m *memoryStore) DeleteCart(_ context.Context, owner string, cartID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.cart(owner, cartID); err != nil {
		return err
	}
	delete(m.carts, cartID)
	return nil
}

func (m *memoryStore) AddItem(_ context.Context, owner string, cartID uuid.UUID, req endpoint.NewItem) (endpoint.CartItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.cart(owner, cartID)
	if err != nil {
		return endpoint.CartItem{}, err
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

func (m *memoryStore) UpdateItem(_ context.Context, owner string, cartID, itemID uuid.UUID, quantity int) (endpoint.CartItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, i, err := m.item(owner, cartID, itemID)
	if err != nil {
		return endpoint.CartItem{}, err
	}
	now := time.Now().UTC()
	c.Items[i].Quantity = quantity
	c.Items[i].UpdatedAt = now
	c.UpdatedAt = now
	return c.Items[i], nil
}

func (m *memoryStore) RemoveItem(_ context.Context, owner string, cartID, itemID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, i, err := m.item(owner, cartID, itemID)
	if err != nil {
		return err
	}
	c.Items = slices.Delete(c.Items, i, i+1)
	c.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *memoryStore) cart(owner string, cartID uuid.UUID) (*ownedCart, error) {
	c, ok := m.carts[cartID]
	if !ok || c.owner != owner {
		return nil, endpoint.ErrCartNotFound
	}
	return c, nil
}

func (m *memoryStore) item(owner string, cartID, itemID uuid.UUID) (*ownedCart, int, error) {
	c, err := m.cart(owner, cartID)
	if err != nil {
		return nil, 0, err
	}
	i := slices.IndexFunc(c.Items, func(it endpoint.CartItem) bool { return it.ItemID == itemID })
	if i < 0 {
		return nil, 0, endpoint.ErrItemNotFound
	}
	return c, i, nil
}

func clone(c *ownedCart) endpoint.Cart {
	out := c.Cart
	out.Items = slices.Clone(c.Items)
	if out.Items == nil {
		out.Items = []endpoint.CartItem{}
	}
	return out
}
