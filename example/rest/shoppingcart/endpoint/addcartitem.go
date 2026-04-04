// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus/example/rest/shoppingcart/service/database"
)

// AddItemRequest is the JSON body for POST /carts/{cartId}/items.
type AddItemRequest struct {
	ProductID string  `json:"productId"`
	Quantity  int     `json:"quantity"`
	UnitPrice float64 `json:"unitPrice"`
}

// AddCartItem returns a Route for POST /carts/{cartId}/items.
// It responds with 201 and the new CartItem, or 404 if the cart does not exist.
func AddCartItem(store database.Store) bedrockrest.Route {
	ep := bedrockrest.POST("/carts/{cartId}/items", func(ctx context.Context, req bedrockrest.Request[AddItemRequest]) (database.CartItem, error) {
		id, err := uuid.Parse(bedrockrest.ParamFrom(req, cartID))
		if err != nil {
			return database.CartItem{}, notFound("cart not found")
		}

		body := req.Body()
		item, err := store.AddCartItem(ctx, id, database.AddItemRequest{
			ProductID: body.ProductID,
			Quantity:  body.Quantity,
			UnitPrice: body.UnitPrice,
		})
		if errors.Is(err, database.ErrCartNotFound) {
			return database.CartItem{}, notFound("cart not found")
		}
		return item, err
	})
	ep = cartID.Read(ep)
	ep = bedrockrest.ReadJSON[AddItemRequest](ep)
	ep = bedrockrest.OperationID("addCartItem", ep)
	ep = bedrockrest.Summary("Add an item to a shopping cart", ep)
	ep = bedrockrest.Tags([]string{"items"}, ep)
	ep = bedrockrest.WriteJSON[database.CartItem](http.StatusCreated, ep)
	ep = bedrockrest.ErrorJSON[NotFoundError](http.StatusNotFound, ep)
	return bedrockrest.CatchAll(http.StatusInternalServerError, internalError, ep)
}
