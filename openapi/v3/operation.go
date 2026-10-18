// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package openapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Operation is one operation of an API: a method, a path, a typed handler,
// its codecs and its parts. Only an API serves it.
type Operation struct {
	op *operation
}

// operation is an Operation's declaration, with its types erased.
type operation struct {
	method string
	path   Path
	opts   []Option

	// document is set on the operation Document returns, which serves the
	// API's document and has no handler, codecs or parts.
	document bool

	// bind checks the handler and calls each codec function, and each
	// codec's Schema, once.
	bind func() (*codecs, error)
}

// codecs is an operation's handler and codecs, bound, with their types
// erased.
type codecs struct {
	decMediaType string
	encMediaType string
	decSchema    Schema
	encSchema    Schema
	decode       func(r *http.Request) (any, error)
	handle       func(ctx context.Context, st *requestState, body any) (any, error)
	encode       func(resp any) (io.WriterTo, error)
}

// Get is the operation GET p, which reads no body. h's response is written
// by resp's encoder. resp is a function, so a generic codec function is
// passed uninstantiated, and its type argument inferred from h:
//
//	openapi.Get(cart, c.getCart, jsoncodec.Encoder)
func Get[Resp any](p Path, h func(context.Context, Request[EmptyBody]) (Resp, error), resp func() Encoder[Resp], opts ...Option) Operation {
	return newOperation(http.MethodGet, p, h, EmptyDecoder, resp, opts)
}

// Delete is the operation DELETE p, which reads no body. See Get.
func Delete[Resp any](p Path, h func(context.Context, Request[EmptyBody]) (Resp, error), resp func() Encoder[Resp], opts ...Option) Operation {
	return newOperation(http.MethodDelete, p, h, EmptyDecoder, resp, opts)
}

// Post is the operation POST p. Its request body is read by body's decoder
// before h runs, and h's response is written by resp's encoder. Both are
// functions, so a generic codec function is passed uninstantiated, and its
// type argument inferred from h:
//
//	openapi.Post(items, c.addCartItem, jsoncodec.Decoder, jsoncodec.Encoder,
//		openapi.Status(http.StatusCreated),
//	)
func Post[B, Resp any](p Path, h func(context.Context, Request[B]) (Resp, error), body func() Decoder[B], resp func() Encoder[Resp], opts ...Option) Operation {
	return newOperation(http.MethodPost, p, h, body, resp, opts)
}

// Put is the operation PUT p. See Post.
func Put[B, Resp any](p Path, h func(context.Context, Request[B]) (Resp, error), body func() Decoder[B], resp func() Encoder[Resp], opts ...Option) Operation {
	return newOperation(http.MethodPut, p, h, body, resp, opts)
}

// Patch is the operation PATCH p. See Post.
func Patch[B, Resp any](p Path, h func(context.Context, Request[B]) (Resp, error), body func() Decoder[B], resp func() Encoder[Resp], opts ...Option) Operation {
	return newOperation(http.MethodPatch, p, h, body, resp, opts)
}

func newOperation[B, Resp any](method string, p Path, h func(context.Context, Request[B]) (Resp, error), body func() Decoder[B], resp func() Encoder[Resp], opts []Option) Operation {
	return Operation{op: &operation{
		method: method,
		path:   p,
		opts:   opts,
		bind: func() (*codecs, error) {
			var errs []error
			if h == nil {
				errs = append(errs, errors.New("its handler is nil"))
			}
			dec, decSchema, err := bindDecoder(body)
			if err != nil {
				errs = append(errs, fmt.Errorf("request body: %w", err))
			}
			enc, encSchema, err := bindEncoder(resp)
			if err != nil {
				errs = append(errs, fmt.Errorf("response: %w", err))
			}
			if len(errs) > 0 {
				return nil, errors.Join(errs...)
			}
			return &codecs{
				decMediaType: dec.MediaType(),
				encMediaType: enc.MediaType(),
				decSchema:    decSchema,
				encSchema:    encSchema,
				decode: func(r *http.Request) (any, error) {
					return dec.Decode(r)
				},
				handle: func(ctx context.Context, st *requestState, body any) (any, error) {
					b, _ := body.(B)
					return h(ctx, Request[B]{body: b, state: st})
				},
				encode: func(v any) (io.WriterTo, error) {
					r, _ := v.(Resp)
					return enc.Encode(r)
				},
			}, nil
		},
	}}
}

// Request is everything the handler reads: its parameters and its body.
type Request[B any] struct {
	body  B
	state *requestState
}

// requestState is what an API decoded for one request, besides its body.
type requestState struct {
	op     string // such as "GET /carts/{cartId}"
	params map[string]any
}

// Body returns the decoded body.
func (r Request[B]) Body() B {
	return r.body
}
