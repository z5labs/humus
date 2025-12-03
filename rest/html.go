// Copyright (c) 2025 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package rest

import (
	"context"
	"html/template"
	"net/http"

	"github.com/swaggest/openapi-go/openapi3"
)

// ReturnHTMLHandler wraps a handler to return HTML template responses.
type ReturnHTMLHandler[Req, Resp any] struct {
	inner Handler[Req, Resp]
	tmpl  *template.Template
}

// ReturnHTML initializes a [ReturnHTMLHandler] that renders responses using the provided template.
// Use this when your template for each request is independent of the request data.
// If you need to specify a response template dynamically, implement your own Handler
// which returns a [HTMLTemplateResponse] with the Template field set based on the request.
func ReturnHTML[Req, Resp any](h Handler[Req, Resp], tmpl *template.Template) *ReturnHTMLHandler[Req, Resp] {
	return &ReturnHTMLHandler[Req, Resp]{
		inner: h,
		tmpl:  tmpl,
	}
}

// HTMLTemplateResponse represents an HTML response that renders data using a template.
type HTMLTemplateResponse[T any] struct {
	Template *template.Template
	Data     *T
}

// Spec implements the [TypedResponse] interface.
func (*HTMLTemplateResponse[T]) Spec() (int, openapi3.ResponseOrRef, error) {
	schemaType := openapi3.SchemaTypeString
	schema := &openapi3.SchemaOrRef{
		Schema: &openapi3.Schema{
			Type: &schemaType,
		},
	}

	spec := &openapi3.Response{
		Content: map[string]openapi3.MediaType{
			"text/html": {
				Schema: schema,
			},
		},
	}

	return http.StatusOK, openapi3.ResponseOrRef{
		Response: spec,
	}, nil
}

// WriteResponse implements the [ResponseWriter] interface.
func (hr *HTMLTemplateResponse[T]) WriteResponse(ctx context.Context, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	return hr.Template.Execute(w, hr.Data)
}

// Handle implements the [Handler] interface.
func (h *ReturnHTMLHandler[Req, Resp]) Handle(ctx context.Context, req *Req) (*HTMLTemplateResponse[Resp], error) {
	resp, err := h.inner.Handle(ctx, req)
	if err != nil {
		return nil, err
	}
	return &HTMLTemplateResponse[Resp]{
		Template: h.tmpl,
		Data:     resp,
	}, nil
}

// ProduceHTML creates a handler that returns HTML responses without consuming a request body.
// Use this for GET endpoints that return server-rendered HTML pages when your template
// for each request is independent of the request data. If you need to specify a response
// template dynamically, implement your own Handler which returns a [HTMLTemplateResponse]
// with the Template field set based on the request.
//
// Example:
//
//	tmpl := template.Must(template.New("page").Parse("<h1>{{.Message}}</h1>"))
//	p := rpc.ProducerFunc[Response](func(ctx context.Context) (*Response, error) {
//	    return &Response{Message: "Hello, World!"}, nil
//	})
//	handler := rpc.ProduceHTML(p, tmpl)
func ProduceHTML[T any](p Producer[T], tmpl *template.Template) *ReturnHTMLHandler[EmptyRequest, T] {
	inner := &ProducerHandler[T]{
		p: p,
	}
	return ReturnHTML(inner, tmpl)
}
