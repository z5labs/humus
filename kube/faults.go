// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube

import (
	"net/http"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus/fault"
)

// stripEvent is the span event of a strip. It is humus's own, so it starts
// with humus.: OpenTelemetry has no convention for failure injection.
const stripEvent = "humus.fit.strip"

// injectFaults is the injection layer, between otelhttp and the token.
// Each request evaluates its point with faults, in the request's context,
// whose span is otelhttp's, or for a watch, which has none, the caller's.
// A Delay is waited there, inside the request's span. An abort, or a Delay
// the request's context cuts short, fails the request with a
// *fault.InjectedError before the token is asked for, and nothing is sent.
//
// The layer then strips the scenario from what the request sends: the API
// server takes no part in failure injection. otelhttp has written the
// request's headers by now, from its context, so the layer clones the
// request, deletes propagator's fields, and injects them again from the
// stripped context: the request keeps its span context and every other
// baggage member. otelhttp writes no header on a watch, so a watch sends no
// baggage, and has nothing to strip.
//
// Last, a stripped request that applies a child of the reconcile its
// context is made from carries the scenario on, in the child's annotation,
// as carryOn says.
type injectFaults struct {
	faults     fault.Injector
	propagator propagation.TextMapPropagator
	next       http.RoundTripper
}

// RoundTrip implements http.RoundTripper. A request a fault ends closes its
// body, as RoundTrip must.
func (t injectFaults) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx := r.Context()
	point := pointOf(r)
	f := t.faults.Eval(ctx, point)
	if err := f.Wait(ctx); err != nil {
		closeBody(r)
		return nil, &fault.InjectedError{Point: point, Err: err}
	}
	if f.Abort {
		closeBody(r)
		return nil, &fault.InjectedError{Point: point}
	}
	if notWatch(r) {
		if stripped, carried := t.faults.Strip(ctx); carried {
			trace.SpanFromContext(ctx).AddEvent(stripEvent)
			r = r.Clone(ctx)
			for _, field := range t.propagator.Fields() {
				r.Header.Del(field)
			}
			t.propagator.Inject(stripped, propagation.HeaderCarrier(r.Header))
			var err error
			if r, err = carryOn(ctx, stripped, r, point); err != nil {
				return nil, err
			}
		}
	}
	return t.next.RoundTrip(r)
}

// The API server's path prefixes: the core group's, which names no group,
// and every other group's.
const (
	corePrefix  = "api"
	groupPrefix = "apis"
)

// pointOf returns r's injection point, which names what r does to which
// resource, as the API server's authorizer and audit log do:
//
//	kube:<resource>[.<group>][/<subresource>]:<verb>
//
// The resource is the plural, and the group is left out for the core
// group. The verb is the API server's: get, list, watch, create, update,
// patch, delete or deletecollection, so a server-side apply is patch, and a
// request to a resource's proxy is proxy. A request for a path that isn't a
// resource, such as discovery's /apis, is kube:<path>:<verb>, whose verb is
// its method in lower case.
//
// It reads r as k8s.io/apiserver's RequestInfoFactory does, with one
// difference: a GET with watch=true is a watch whether or not it names an
// object, as the Client tells a watch from any other request.
func pointOf(r *http.Request) string {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	nonResource := func() string {
		path := r.URL.Path
		if path == "" {
			path = "/"
		}
		return "kube:" + path + ":" + strings.ToLower(method)
	}

	parts := splitPath(r.URL.Path)
	if len(parts) < 3 || (parts[0] != corePrefix && parts[0] != groupPrefix) {
		return nonResource()
	}
	var group string
	if parts[0] == groupPrefix {
		if len(parts) < 4 {
			return nonResource()
		}
		group = parts[1]
		parts = parts[2:]
	} else {
		parts = parts[1:]
	}
	// parts[0] is the version.
	parts = parts[1:]

	var verb string
	switch parts[0] {
	case "watch", "proxy":
		if len(parts) < 2 {
			return nonResource()
		}
		verb = parts[0]
		parts = parts[1:]
	default:
		verb = verbOf(method)
	}

	// /namespaces/<namespace>/<resource>/…, but a namespace's own
	// subresources, status and finalize, are of the namespaces resource.
	if parts[0] == "namespaces" && len(parts) > 2 && parts[2] != "status" && parts[2] != "finalize" {
		parts = parts[2:]
	}

	resource, name, subresource := parts[0], "", ""
	if len(parts) >= 2 {
		name = parts[1]
	}
	if len(parts) >= 3 && verb != "proxy" {
		subresource = parts[2]
	}

	switch {
	case verb == "get" && isWatch(r):
		verb = "watch"
	case verb == "get" && name == "":
		verb = "list"
	case verb == "delete" && name == "":
		verb = "deletecollection"
	}

	point := "kube:" + resource
	if group != "" {
		point += "." + group
	}
	if subresource != "" {
		point += "/" + subresource
	}
	return point + ":" + verb
}

// verbOf returns the API server's verb for a request to a resource with
// method, before it tells a list from a get, or a deletecollection from a
// delete. A method the API server has no verb for is its own name, in lower
// case.
func verbOf(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "get"
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		return "delete"
	}
	return strings.ToLower(method)
}

// isWatch reports whether r asks for a watch with the query parameter
// watch, as client-go sends one.
func isWatch(r *http.Request) bool {
	watch, err := strconv.ParseBool(r.URL.Query().Get("watch"))
	return err == nil && watch
}

// splitPath returns path's segments, without the leading and trailing
// slashes, or none for the root.
func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}
