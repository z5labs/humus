// Copyright (c) 2024 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humuspb

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHttpAndStatusCodes(t *testing.T) {
	httpCodes := make(map[int]struct{})
	for httpCode := range httpToStatusCode {
		httpCodes[httpCode] = struct{}{}
	}

	statusCodes := make(map[Code]struct{})
	for statusCode := range statusCodeToHttpCode {
		statusCodes[statusCode] = struct{}{}
	}

	t.Run("all status codes should be mapped", func(t *testing.T) {
		for code := range Code_name {
			if !assert.Contains(t, statusCodes, Code(code)) {
				return
			}
		}
	})

	t.Run("should be cyclic between the sets", func(t *testing.T) {
		// the mappings are not guaranteed to be idempotent compositions
		// but we do want to at least guarantee that one of the possible mappings
		// is in both sets. this test is thus meant to ensure that no one
		// can update the status to http mappings without at least checking
		// that there's a possible http to status mapping that matches.

		for httpCode := range httpCodes {
			statusCode := HttpCodeToStatusCode(httpCode)
			if !assert.Contains(t, statusCodes, statusCode) {
				return
			}
		}

		for statusCode := range statusCodes {
			httpCode := StatusCodeToHttpCode(statusCode)
			if !assert.Contains(t, httpCodes, httpCode) {
				return
			}
		}
	})
}
