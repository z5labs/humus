// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package problem answers an API's errors with RFC 9457 problem details.
//
// openapi forces no error format, so problem details are opt-in. Details is
// RFC 9457's problem detail, FromRequestError is a wrap for
// openapi.RequestErrors, and Handler is a handler for openapi.Unmatched.
// They take any encoder, so the end user picks the encoding:
//
//	openapi.Unmatched{
//		NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
//		MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
//	},
//	openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
//
// An error of the end user's own is a type that embeds Details:
//
//	type NotFound struct{ problem.Details }
//
//	openapi.Error[NotFound](http.StatusNotFound, jsoncodec.Problem)
//
// problem uses only openapi's exported API, as a third party's package
// would, and openapi never uses it.
package problem

import (
	"encoding/xml"
	"net/http"
	"strconv"

	openapi "github.com/z5labs/humus/openapi/v3"
)

// Details is a problem detail, RFC 9457. Its tags name its members as RFC
// 9457 does, in both of the serializations the RFC defines: JSON and XML.
// A type that embeds it is a problem detail of its own.
type Details struct {
	XMLName  xml.Name `json:"-" xml:"urn:ietf:rfc:7807 problem"`
	Type     string   `json:"type,omitempty" xml:"type,omitempty"`
	Title    string   `json:"title,omitempty" xml:"title,omitempty"`
	Status   int      `json:"status,omitempty" xml:"status,omitempty"`
	Detail   string   `json:"detail,omitempty" xml:"detail,omitempty"`
	Instance string   `json:"instance,omitempty" xml:"instance,omitempty"`
}

// Error returns Detail, or Title when Detail is empty.
func (d Details) Error() string {
	if d.Detail != "" {
		return d.Detail
	}
	return d.Title
}

// aboutBlank is the problem type of a detail that names nothing more than
// its status, as RFC 9457 defines it.
const aboutBlank = "about:blank"

// FromRequestError returns e as a problem detail: type about:blank, the
// name of e's status as its title, and e's error's text as its detail. It is
// a wrap for openapi.RequestErrors.
func FromRequestError(e *openapi.RequestError) Details {
	d := Details{
		Type:   aboutBlank,
		Title:  http.StatusText(e.Status),
		Status: e.Status,
	}
	if e.Err != nil {
		d.Detail = e.Err.Error()
	}
	return d
}

// Handler answers every request with status and a problem detail, written
// by enc. The detail's type is about:blank and its title is the status's
// name. It is a handler for openapi.Unmatched.
//
// Handler calls enc once, and panics if enc is nil or returns nil. The
// response's Content-Type is the encoder's media type, unless that is "",
// and its Content-Length is the WriterTo's Len, when it has one. A header the API set, such as a 405's Allow, is kept. When the encoder's
// Encode fails, the response is the status alone, and when the WriterTo it
// returned fails, the response is cut short.
func Handler(status int, enc func() openapi.Encoder[Details]) http.Handler {
	if enc == nil {
		panic("problem: Handler's encoder function is nil")
	}
	e := enc()
	if e == nil {
		panic("problem: Handler's encoder function returned nil")
	}
	return handler{
		status: status,
		enc:    e,
		detail: Details{Type: aboutBlank, Title: http.StatusText(status), Status: status},
	}
}

// handler is what Handler returns.
type handler struct {
	status int
	enc    openapi.Encoder[Details]
	detail Details
}

func (h handler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	wt, err := h.enc.Encode(h.detail)
	if err != nil || wt == nil {
		w.WriteHeader(h.status)
		return
	}
	if mt := h.enc.MediaType(); mt != "" {
		w.Header().Set("Content-Type", mt)
	}
	if l, ok := wt.(interface{ Len() int }); ok && bodyAllowed(h.status) {
		w.Header().Set("Content-Length", strconv.Itoa(l.Len()))
	}
	w.WriteHeader(h.status)
	if _, err := wt.WriteTo(w); err != nil {
		// The status is written, so the failure can't change the response:
		// cut it short, so the client sees a failed response rather than a
		// complete one.
		panic(http.ErrAbortHandler)
	}
}

// bodyAllowed reports whether a response with status has a body, and so a
// Content-Length.
func bodyAllowed(status int) bool {
	return status >= http.StatusOK && status != http.StatusNoContent && status != http.StatusNotModified
}
