// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus/example/rest/shoppingcart/service/database"

	"github.com/stretchr/testify/require"
)

// mockStore is an in-memory implementation of database.Store for testing.
type mockStore struct {
	createCart    func(ctx context.Context) (database.Cart, error)
	getCart       func(ctx context.Context, cartID uuid.UUID) (database.Cart, error)
	deleteCart    func(ctx context.Context, cartID uuid.UUID) error
	addCartItem   func(ctx context.Context, cartID uuid.UUID, req database.AddItemRequest) (database.CartItem, error)
	updateCartItem func(ctx context.Context, cartID, itemID uuid.UUID, quantity int) (database.CartItem, error)
	removeCartItem func(ctx context.Context, cartID, itemID uuid.UUID) error
}

func (m *mockStore) CreateCart(ctx context.Context) (database.Cart, error) {
	return m.createCart(ctx)
}

func (m *mockStore) GetCart(ctx context.Context, cartID uuid.UUID) (database.Cart, error) {
	return m.getCart(ctx, cartID)
}

func (m *mockStore) DeleteCart(ctx context.Context, cartID uuid.UUID) error {
	return m.deleteCart(ctx, cartID)
}

func (m *mockStore) AddCartItem(ctx context.Context, cartID uuid.UUID, req database.AddItemRequest) (database.CartItem, error) {
	return m.addCartItem(ctx, cartID, req)
}

func (m *mockStore) UpdateCartItem(ctx context.Context, cartID, itemID uuid.UUID, quantity int) (database.CartItem, error) {
	return m.updateCartItem(ctx, cartID, itemID, quantity)
}

func (m *mockStore) RemoveCartItem(ctx context.Context, cartID, itemID uuid.UUID) error {
	return m.removeCartItem(ctx, cartID, itemID)
}

func buildTestHandler(t *testing.T, routes ...bedrockrest.Route) http.Handler {
	t.Helper()
	opts := []bedrockrest.Option{bedrockrest.Title("Test"), bedrockrest.Version("0.0.0")}
	for _, r := range routes {
		opts = append(opts, r.Route())
	}
	handler := bedrockrest.Build(opts...)
	h, err := handler.Build(context.Background())
	require.NoError(t, err)
	return h
}

func newCart() database.Cart {
	return database.Cart{
		CartID:    uuid.New(),
		Items:     []database.CartItem{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

func newItem(cartID uuid.UUID) database.CartItem {
	return database.CartItem{
		ItemID:    uuid.New(),
		CartID:    cartID,
		ProductID: "product-1",
		Quantity:  2,
		UnitPrice: 9.99,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

func TestCreateCart(t *testing.T) {
	t.Run("returns 201 with new cart", func(t *testing.T) {
		cart := newCart()
		store := &mockStore{
			createCart: func(_ context.Context) (database.Cart, error) {
				return cart, nil
			},
		}
		h := buildTestHandler(t, CreateCart(store))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/carts", nil)
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusCreated, w.Code)

		var got database.Cart
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Equal(t, cart.CartID, got.CartID)
		require.Empty(t, got.Items)
	})
}

func TestGetCart(t *testing.T) {
	t.Run("returns 200 with cart", func(t *testing.T) {
		cart := newCart()
		store := &mockStore{
			getCart: func(_ context.Context, id uuid.UUID) (database.Cart, error) {
				if id != cart.CartID {
					return database.Cart{}, database.ErrCartNotFound
				}
				return cart, nil
			},
		}
		h := buildTestHandler(t, GetCart(store))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/carts/%s", cart.CartID), nil)
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code)

		var got database.Cart
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Equal(t, cart.CartID, got.CartID)
	})

	t.Run("returns 404 when cart not found", func(t *testing.T) {
		store := &mockStore{
			getCart: func(_ context.Context, _ uuid.UUID) (database.Cart, error) {
				return database.Cart{}, database.ErrCartNotFound
			},
		}
		h := buildTestHandler(t, GetCart(store))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/carts/%s", uuid.New()), nil)
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusNotFound, w.Code)

		var got NotFoundError
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Equal(t, 404, got.Status)
	})

	t.Run("returns 404 for invalid cart ID", func(t *testing.T) {
		store := &mockStore{}
		h := buildTestHandler(t, GetCart(store))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/carts/not-a-uuid", nil)
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDeleteCart(t *testing.T) {
	t.Run("returns 204 on success", func(t *testing.T) {
		cart := newCart()
		store := &mockStore{
			deleteCart: func(_ context.Context, id uuid.UUID) error {
				if id != cart.CartID {
					return database.ErrCartNotFound
				}
				return nil
			},
		}
		h := buildTestHandler(t, DeleteCart(store))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/carts/%s", cart.CartID), nil)
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusNoContent, w.Code)
		require.Empty(t, w.Body.Bytes())
	})

	t.Run("returns 404 when cart not found", func(t *testing.T) {
		store := &mockStore{
			deleteCart: func(_ context.Context, _ uuid.UUID) error {
				return database.ErrCartNotFound
			},
		}
		h := buildTestHandler(t, DeleteCart(store))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/carts/%s", uuid.New()), nil)
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestAddCartItem(t *testing.T) {
	t.Run("returns 201 with new item", func(t *testing.T) {
		cart := newCart()
		item := newItem(cart.CartID)
		store := &mockStore{
			addCartItem: func(_ context.Context, id uuid.UUID, req database.AddItemRequest) (database.CartItem, error) {
				if id != cart.CartID {
					return database.CartItem{}, database.ErrCartNotFound
				}
				return item, nil
			},
		}
		h := buildTestHandler(t, AddCartItem(store))

		body, _ := json.Marshal(AddItemRequest{ProductID: "product-1", Quantity: 2, UnitPrice: 9.99})
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/carts/%s/items", cart.CartID), bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusCreated, w.Code)

		var got database.CartItem
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Equal(t, item.ItemID, got.ItemID)
		require.Equal(t, "product-1", got.ProductID)
	})

	t.Run("returns 404 when cart not found", func(t *testing.T) {
		store := &mockStore{
			addCartItem: func(_ context.Context, _ uuid.UUID, _ database.AddItemRequest) (database.CartItem, error) {
				return database.CartItem{}, database.ErrCartNotFound
			},
		}
		h := buildTestHandler(t, AddCartItem(store))

		body := `{"productId":"p1","quantity":1,"unitPrice":5.00}`
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/carts/%s/items", uuid.New()), strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestUpdateCartItem(t *testing.T) {
	t.Run("returns 200 with updated item", func(t *testing.T) {
		cart := newCart()
		item := newItem(cart.CartID)
		updated := item
		updated.Quantity = 5

		store := &mockStore{
			updateCartItem: func(_ context.Context, cID, iID uuid.UUID, qty int) (database.CartItem, error) {
				if cID != cart.CartID || iID != item.ItemID {
					return database.CartItem{}, database.ErrItemNotFound
				}
				updated.Quantity = qty
				return updated, nil
			},
		}
		h := buildTestHandler(t, UpdateCartItem(store))

		body := `{"quantity":5}`
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/carts/%s/items/%s", cart.CartID, item.ItemID), strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusOK, w.Code)

		var got database.CartItem
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		require.Equal(t, 5, got.Quantity)
	})

	t.Run("returns 404 when item not found", func(t *testing.T) {
		store := &mockStore{
			updateCartItem: func(_ context.Context, _, _ uuid.UUID, _ int) (database.CartItem, error) {
				return database.CartItem{}, database.ErrItemNotFound
			},
		}
		h := buildTestHandler(t, UpdateCartItem(store))

		body := `{"quantity":3}`
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/carts/%s/items/%s", uuid.New(), uuid.New()), strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestRemoveCartItem(t *testing.T) {
	t.Run("returns 204 on success", func(t *testing.T) {
		cart := newCart()
		item := newItem(cart.CartID)
		store := &mockStore{
			removeCartItem: func(_ context.Context, cID, iID uuid.UUID) error {
				if cID != cart.CartID || iID != item.ItemID {
					return database.ErrItemNotFound
				}
				return nil
			},
		}
		h := buildTestHandler(t, RemoveCartItem(store))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/carts/%s/items/%s", cart.CartID, item.ItemID), nil)
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusNoContent, w.Code)
		require.Empty(t, w.Body.Bytes())
	})

	t.Run("returns 404 when item not found", func(t *testing.T) {
		store := &mockStore{
			removeCartItem: func(_ context.Context, _, _ uuid.UUID) error {
				return database.ErrItemNotFound
			},
		}
		h := buildTestHandler(t, RemoveCartItem(store))

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/carts/%s/items/%s", uuid.New(), uuid.New()), nil)
		h.ServeHTTP(w, r)

		require.Equal(t, http.StatusNotFound, w.Code)
	})
}
