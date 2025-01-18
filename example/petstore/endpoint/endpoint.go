// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint

import "github.com/z5labs/humus/rest/mux"

type Router interface {
	Route(method, pattern string, op mux.Operation) error
}

func mustRoute(r Router, method, pattern string, op mux.Operation) {
	err := r.Route(method, pattern, op)
	if err == nil {
		return
	}
	panic(err)
}
