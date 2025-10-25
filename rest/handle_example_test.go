// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandle(t *testing.T) {
	getBook := Handle(
		http.MethodGet,
		BasePath("/book"),
		nil,
		Header(
			"Authorization",
			Required(),
			JWTAuth("jwt"),
		),
	)

	api := NewApi(
		"example",
		"v0.0.0",
		getBook,
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/book?id=1", nil)
	if err != nil {
		fmt.Println(err)
		return
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	fmt.Println(resp.StatusCode)

	// Should received 400 because the Authorization is not present in request
	// Output: 400
}
