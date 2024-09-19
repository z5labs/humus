// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package humuspb defines reusable protobuf types for use in APIs.
package humuspb

import (
	"fmt"
	"net/http"
)

// Error implements the [error] interface.
func (s *Status) Error() string {
	return fmt.Sprintf("status: %s: %s", s.Code, s.Message)
}

var httpToStatusCode = map[int]Code{}

func HttpCodeToStatusCode(status int) Code {
	code, ok := httpToStatusCode[status]
	if !ok {
		return Code_UNKNOWN
	}
	return code
}

var statusCodeToHttpCode = map[Code]int{
	Code_OK:                  http.StatusOK,
	Code_UNKNOWN:             http.StatusInternalServerError,
	Code_INVALID_ARGUMENT:    http.StatusBadRequest,
	Code_NOT_FOUND:           http.StatusNotFound,
	Code_ALREADY_EXISTS:      http.StatusConflict,
	Code_PERMISSION_DENIED:   http.StatusForbidden,
	Code_UNAUTHENTICATED:     http.StatusUnauthorized,
	Code_FAILED_PRECONDITION: http.StatusBadRequest,
	Code_INTERNAL:            http.StatusInternalServerError,
	Code_UNAVAILABLE:         http.StatusServiceUnavailable,
}

func StatusCodeToHttpCode(code Code) int {
	status, ok := statusCodeToHttpCode[code]
	if !ok {
		return http.StatusInternalServerError
	}
	return status
}
