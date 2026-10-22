// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"net/http"

	"github.com/z5labs/humus/internal/httproute"
)

// nameRoute is the layer right below otelhttp, which names the request's
// span, and sets http.route on its metrics, from the Pattern of the request
// it passes nameRoute once nameRoute returns.
//
// It sets that Pattern to the route an openapi API recorded, whatever
// handlers between them passed on a copy of the request. With none
// recorded, it sets the Pattern its own copy was given, as by an
// http.ServeMux below it.
type nameRoute struct {
	next http.Handler
}

func (h nameRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tracked, recorded := httproute.Track(r)
	defer func() {
		switch pattern := recorded(); {
		case pattern != "":
			r.Pattern = pattern
		case tracked.Pattern != "":
			r.Pattern = tracked.Pattern
		}
	}()
	h.next.ServeHTTP(w, tracked)
}
