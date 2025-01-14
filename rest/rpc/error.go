// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rpc

import (
	"fmt"
	"net/http"
)

// ErrorHandler
type ErrorHandler interface {
	Handle(http.ResponseWriter, error)
}

// InvalidContentTypeError
type InvalidContentTypeError struct {
	ContentType string
}

// Error implements the [error] interface.
func (e InvalidContentTypeError) Error() string {
	return fmt.Sprintf("invalid content type for request: %s", e.ContentType)
}
