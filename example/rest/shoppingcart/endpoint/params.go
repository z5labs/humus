// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import bedrockrest "github.com/z5labs/bedrock/runtime/http/rest"

var (
	cartID = bedrockrest.PathParam[string]("cartId",
		bedrockrest.ParamDescription("Shopping cart identifier (UUID)"),
	)
	itemID = bedrockrest.PathParam[string]("itemId",
		bedrockrest.ParamDescription("Cart item identifier (UUID)"),
	)
)
