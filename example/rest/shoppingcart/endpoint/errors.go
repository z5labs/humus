// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

// NotFoundError is an RFC 7807 Problem Details error for 404 responses.
type NotFoundError struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title,omitempty"`
	Status   int    `json:"status,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

func (e NotFoundError) Error() string { return e.Detail }

func notFound(detail string) NotFoundError {
	return NotFoundError{
		Type:   "about:blank",
		Title:  "Not Found",
		Status: 404,
		Detail: detail,
	}
}

// InternalServerError is an RFC 7807 Problem Details error for 500 responses.
type InternalServerError struct {
	Type   string `json:"type,omitempty"`
	Title  string `json:"title,omitempty"`
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func (e InternalServerError) Error() string { return e.Detail }

func internalError(_ error) InternalServerError {
	return InternalServerError{
		Type:   "about:blank",
		Title:  "Internal Server Error",
		Status: 500,
		Detail: "An unexpected error occurred.",
	}
}
