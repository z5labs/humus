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

// UpdateItemRequest is the JSON body for PATCH /carts/{cartId}/items/{itemId}.
type UpdateItemRequest struct {
	Quantity int `json:"quantity"`
}

// UpdateCartItem returns a Route for PATCH /carts/{cartId}/items/{itemId}.
// It responds with 200 and the updated CartItem, or 404 if cart or item is not found.
func UpdateCartItem(store database.Store) bedrockrest.Route {
	ep := bedrockrest.PATCH("/carts/{cartId}/items/{itemId}", func(ctx context.Context, req bedrockrest.Request[UpdateItemRequest]) (database.CartItem, error) {
		cID, err := uuid.Parse(bedrockrest.ParamFrom(req, cartID))
		if err != nil {
			return database.CartItem{}, notFound("cart not found")
		}

		iID, err := uuid.Parse(bedrockrest.ParamFrom(req, itemID))
		if err != nil {
			return database.CartItem{}, notFound("item not found")
		}

		item, err := store.UpdateCartItem(ctx, cID, iID, req.Body().Quantity)
		if errors.Is(err, database.ErrItemNotFound) {
			return database.CartItem{}, notFound("cart or item not found")
		}
		return item, err
	})
	ep = cartID.Read(ep)
	ep = itemID.Read(ep)
	ep = bedrockrest.ReadJSON[UpdateItemRequest](ep)
	ep = bedrockrest.OperationID("updateCartItem", ep)
	ep = bedrockrest.Summary("Update the quantity of an item in the cart", ep)
	ep = bedrockrest.Tags([]string{"items"}, ep)
	ep = bedrockrest.WriteJSON[database.CartItem](http.StatusOK, ep)
	ep = bedrockrest.ErrorJSON[NotFoundError](http.StatusNotFound, ep)
	return bedrockrest.CatchAll(http.StatusInternalServerError, internalError, ep)
}
