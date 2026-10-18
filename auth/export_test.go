// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth

import (
	"net/http"

	"github.com/z5labs/humus"
)

// NewHandlerTakingDisabled is NewHandler without its Disabled mistake, so
// that a test serves a Disabled list through the engine as openapi and
// grpcauth will, and sees what From returns behind it.
func NewHandlerTakingDisabled(s *humus.Setup, h http.Handler, answers Answers, reqs ...Requirement) (http.Handler, error) {
	return newHandler(s, h, answers, reqs, true)
}
