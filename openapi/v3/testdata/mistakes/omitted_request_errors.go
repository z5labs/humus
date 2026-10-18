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

func omittedRequestErrors(s *humus.Setup) {
	_, _ = openapi.NewAPI(s, info, unmatched,
		openapi.CatchAll(http.StatusInternalServerError, internalError, encoder),
		[]openapi.Operation{openapi.Get(carts, getCart, encoder)},
	)
}
