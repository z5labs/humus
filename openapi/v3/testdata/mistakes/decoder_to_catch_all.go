// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build mistakes

package mistakes

import (
	"net/http"

	openapi "github.com/z5labs/humus/openapi/v3"
)

var _ = openapi.CatchAll(http.StatusInternalServerError, internalError, decoder)
