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

	"github.com/swaggest/openapi-go/openapi3"
)

// exampleHandler implements the Handler interface for testing
type exampleHandler struct{}

func (e exampleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("book data"))
}

func (e exampleHandler) RequestBody() openapi3.RequestBodyOrRef {
	return openapi3.RequestBodyOrRef{}
}

func (e exampleHandler) Responses() openapi3.Responses {
	return openapi3.Responses{}
}

// exampleJWTVerifier is a simple JWT verifier for testing purposes
type exampleJWTVerifier struct{}

func (v exampleJWTVerifier) Verify(ctx context.Context, token string) (context.Context, error) {
	// In a real implementation, you would verify the JWT signature and claims here
	// For this example, we just accept any token
	return ctx, nil
}

func TestHandle(t *testing.T) {
	getBook := Handle(
		http.MethodGet,
		BasePath("/book"),
		exampleHandler{},
		Header(
			"Authorization",
			Required(),
			JWTAuth("jwt", exampleJWTVerifier{}),
		),
	)

	api := NewApi(
		"example",
		"v0.0.0",
		getBook,
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/book?id=1", nil)
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
