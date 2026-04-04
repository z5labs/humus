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

// GetCart returns a Route for GET /carts/{cartId}.
// It responds with 200 and the cart, or 404 if the cart does not exist.
func GetCart(store database.Store) bedrockrest.Route {
	ep := bedrockrest.GET("/carts/{cartId}", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) (database.Cart, error) {
		id, err := uuid.Parse(bedrockrest.ParamFrom(req, cartID))
		if err != nil {
			return database.Cart{}, notFound("cart not found")
		}

		cart, err := store.GetCart(ctx, id)
		if errors.Is(err, database.ErrCartNotFound) {
			return database.Cart{}, notFound("cart not found")
		}
		return cart, err
	})
	ep = cartID.Read(ep)
	ep = bedrockrest.OperationID("getCart", ep)
	ep = bedrockrest.Summary("Get a shopping cart and its items", ep)
	ep = bedrockrest.Tags([]string{"carts"}, ep)
	ep = bedrockrest.WriteJSON[database.Cart](http.StatusOK, ep)
	ep = bedrockrest.ErrorJSON[NotFoundError](http.StatusNotFound, ep)
	return bedrockrest.CatchAll(http.StatusInternalServerError, internalError, ep)
}
