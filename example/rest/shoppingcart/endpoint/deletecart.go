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

// DeleteCart returns a Route for DELETE /carts/{cartId}.
// It responds with 204 No Content on success, or 404 if the cart does not exist.
func DeleteCart(store database.Store) bedrockrest.Route {
	ep := bedrockrest.DELETE("/carts/{cartId}", func(ctx context.Context, req bedrockrest.Request[bedrockrest.EmptyBody]) (io.Reader, error) {
		id, err := uuid.Parse(bedrockrest.ParamFrom(req, cartID))
		if err != nil {
			return nil, notFound("cart not found")
		}

		err = store.DeleteCart(ctx, id)
		if errors.Is(err, database.ErrCartNotFound) {
			return nil, notFound("cart not found")
		}
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(nil), nil
	})
	ep = cartID.Read(ep)
	ep = bedrockrest.OperationID("deleteCart", ep)
	ep = bedrockrest.Summary("Delete a shopping cart and all its items", ep)
	ep = bedrockrest.Tags([]string{"carts"}, ep)
	ep = bedrockrest.WriteBinary(http.StatusNoContent, "", ep)
	ep = bedrockrest.ErrorJSON[NotFoundError](http.StatusNotFound, ep)
	return bedrockrest.CatchAll(http.StatusInternalServerError, internalError, ep)
}
