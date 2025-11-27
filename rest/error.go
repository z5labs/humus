// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"fmt"
)

// InvalidContentTypeError represents an error when the request Content-Type
// header does not match the expected value. It implements [rest.HttpResponseWriter]
// to return a 400 Bad Request response.
type InvalidContentTypeError struct {
	ContentType string
}

// Error implements the [error] interface.
func (e InvalidContentTypeError) Error() string {
	return fmt.Sprintf("invalid content type for request: %s", e.ContentType)
}
