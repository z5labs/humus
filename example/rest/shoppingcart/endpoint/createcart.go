// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"context"
	"net/http"

	"github.com/z5labs/humus/example/rest/shoppingcart/service/database"
	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
)

// CreateCart returns a Route for POST /carts.
// It creates a new empty shopping cart and responds with 201 Created.
func CreateCart(store database.Store) bedrockrest.Route {
	ep := bedrockrest.POST("/carts", func(ctx context.Context, _ bedrockrest.Request[bedrockrest.EmptyBody]) (database.Cart, error) {
		return store.CreateCart(ctx)
	})
	ep = bedrockrest.OperationID("createCart", ep)
	ep = bedrockrest.Summary("Create a new shopping cart", ep)
	ep = bedrockrest.Tags([]string{"carts"}, ep)
	ep = bedrockrest.WriteJSON[database.Cart](http.StatusCreated, ep)
	return bedrockrest.CatchAll(http.StatusInternalServerError, internalError, ep)
}
