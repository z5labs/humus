// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"
	"github.com/z5labs/humus/example/rest/shoppingcart/service/database"
)

// RemoveCartItem returns a Route for DELETE /carts/{cartId}/items/{itemId}.
// It responds with 204 No Content on success, or 404 if cart or item is not found.
func RemoveCartItem(store database.Store) bedrockrest.Route {
	ep := bedrockrest.DELETE("/carts/{cartId}/items/{itemId}", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) (io.Reader, error) {
		cID, err := uuid.Parse(bedrockrest.ParamFrom(req, cartID))
		if err != nil {
			return nil, notFound("cart not found")
		}

		iID, err := uuid.Parse(bedrockrest.ParamFrom(req, itemID))
		if err != nil {
			return nil, notFound("item not found")
		}

		err = store.RemoveCartItem(ctx, cID, iID)
		if errors.Is(err, database.ErrItemNotFound) {
			return nil, notFound("cart or item not found")
		}
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(nil), nil
	})
	ep = cartID.Read(ep)
	ep = itemID.Read(ep)
	ep = bedrockrest.OperationID("removeCartItem", ep)
	ep = bedrockrest.Summary("Remove an item from the shopping cart", ep)
	ep = bedrockrest.Tags([]string{"items"}, ep)
	ep = bedrockrest.WriteBinary(http.StatusNoContent, "", ep)
	ep = bedrockrest.ErrorJSON[NotFoundError](http.StatusNotFound, ep)
	return bedrockrest.CatchAll(http.StatusInternalServerError, internalError, ep)
}
