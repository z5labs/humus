// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build mistakes

package mistakes

import (
	"net/http"

	"github.com/z5labs/humus"
	openapi "github.com/z5labs/humus/openapi/v3"
)

func ok(s *humus.Setup) {
	_, _ = newAPI(s, []openapi.Operation{
		openapi.Post(items, addItem, decoder, encoder, openapi.Status(http.StatusCreated)),
		openapi.Get(carts, getCart, encoder, openapi.Query(includeParam), openapi.Header(limit)),
	})
}
