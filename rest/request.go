// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"net/http"

	"github.com/z5labs/sdk-go/try"
)

type ServerInterceptor interface {
	Intercept(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error
}

type ServerInterceptorFunc func(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error

func (f ServerInterceptorFunc) Intercept(next func(http.ResponseWriter, *http.Request) error) func(http.ResponseWriter, *http.Request) error {
	return f(next)
}

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
