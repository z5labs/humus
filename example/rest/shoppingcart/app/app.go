// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package app wires together the shopping cart endpoints and database service.
package app

import (
	"github.com/z5labs/humus/example/rest/shoppingcart/endpoint"
	"github.com/z5labs/humus/example/rest/shoppingcart/service/database"
	"github.com/z5labs/humus/rest"
)

// Options returns the REST server options with all registered shopping cart endpoints.
func Options(store database.Store) []rest.Option {
	return []rest.Option{
		rest.Title("Shopping Cart API"),
		rest.Version("1.0.0"),
		rest.APIDescription("REST API for managing shopping carts on an e-commerce platform."),

		// POST /carts
		rest.Handle(endpoint.CreateCart(store)),

		// GET /carts/{cartId}
		rest.Handle(endpoint.GetCart(store)),

		// DELETE /carts/{cartId}
		rest.Handle(endpoint.DeleteCart(store)),

		// POST /carts/{cartId}/items
		rest.Handle(endpoint.AddCartItem(store)),

		// PATCH /carts/{cartId}/items/{itemId}
		rest.Handle(endpoint.UpdateCartItem(store)),

		// DELETE /carts/{cartId}/items/{itemId}
		rest.Handle(endpoint.RemoveCartItem(store)),
	}
}
