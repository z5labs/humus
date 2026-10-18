// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build mistakes

package mistakes

import (
	"github.com/z5labs/humus"
	openapi "github.com/z5labs/humus/openapi/v3"
)

func noCatchAll(s *humus.Setup) {
	// RequestErrors where the catch-all goes: the argument counts match,
	// and the types don't.
	_, _ = openapi.NewAPI(s, info, unmatched,
		openapi.RequestErrors(rejected, encoder),
		openapi.RequestErrors(rejected, encoder),
		[]openapi.Operation{openapi.Get(carts, getCart, encoder)},
	)
}
