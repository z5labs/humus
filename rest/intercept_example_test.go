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
)

func ExampleIntercept() {
	interceptor := ServerInterceptorFunc(func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
		return func(w http.ResponseWriter, r *http.Request) error {
			// Pre-processing logic before the handler is called
			// e.g., logging, authentication, etc.
			w.Header().Set("X-Custom-Header", "InterceptorWasHere")

			return next(w, r)
		}
	})

	api := NewApi(
		"Example",
		"v0.0.0",
		Operation(
			http.MethodGet,
			BasePath("/"),
			HandlerFunc[EmptyRequest, EmptyResponse](func(ctx context.Context, req *EmptyRequest) (*EmptyResponse, error) {
				return &EmptyResponse{}, nil
			}),
			Intercept(interceptor),
		),
	)

	srv := httptest.NewServer(api)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
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

	fmt.Println("X-Custom-Header:", resp.Header.Get("X-Custom-Header"))

	// Output:
	// X-Custom-Header: InterceptorWasHere
}
