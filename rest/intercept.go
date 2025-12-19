// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"net/http"

	"github.com/z5labs/sdk-go/try"
)

// ServerInterceptor defines an interceptor for HTTP server requests.
type ServerInterceptor interface {
	Intercept(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error
}

// ServerInterceptorFunc is a function type that implements the ServerInterceptor interface.
type ServerInterceptorFunc func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error

// Intercept calls the ServerInterceptorFunc with the next handler.
func (f ServerInterceptorFunc) Intercept(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
	return f(next)
}

// Intercept registers the given ServerInterceptor for the operation.
// Multiple interceptors can be registered and will be executed in the order they were added.
func Intercept(interceptor ServerInterceptor) OperationOption {
	return func(oo *OperationOptions) {
		oo.interceptors = append(oo.interceptors, interceptor)
	}
}

type interceptHandler struct {
	errHandler ErrorHandler
	serve      func(http.ResponseWriter, *http.Request) error
}

func (ih interceptHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var err error
	defer func() {
		if err == nil {
			return
		}

		ih.errHandler.OnError(r.Context(), w, err)
	}()
	defer try.Recover(&err)

	err = ih.serve(w, r)
}
