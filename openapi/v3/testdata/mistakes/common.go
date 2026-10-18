// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

//go:build mistakes

// Package mistakes holds code that must not compile. mistakes_test.go builds
// common.go with each other file, one at a time, and checks that each one
// fails, except ok.go, which shows that common.go compiles.
package mistakes

import (
	"context"
	"io"
	"net/http"

	"github.com/z5labs/humus"
	openapi "github.com/z5labs/humus/openapi/v3"
)

type item struct{ Name string }

type cart struct{ ID string }

type other struct{}

type problem struct{ Title string }

func (p problem) Error() string { return p.Title }

type notFound struct{ problem }

var (
	cartID = openapi.PathParam[string]("cartId")
	carts  = openapi.Root().Lit("carts").Param(cartID)
	items  = carts.Lit("items")
)

func addItem(_ context.Context, req openapi.Request[item]) (item, error) { return req.Body(), nil }

func getCart(context.Context, openapi.Request[openapi.EmptyBody]) (cart, error) { return cart{}, nil }

func internalError(error) problem { return problem{Title: "Internal Server Error"} }

func rejected(e *openapi.RequestError) problem { return problem{Title: e.Error()} }

type codec[T any] struct{}

func decoder[T any]() openapi.Decoder[T] { return codec[T]{} }

func encoder[T any]() openapi.Encoder[T] { return codec[T]{} }

// otherEncoder is a codec that isn't generic, of another type than the
// handlers'.
func otherEncoder() openapi.Encoder[other] { return codec[other]{} }

func (codec[T]) MediaType() string               { return "application/x-test" }
func (codec[T]) Schema() (openapi.Schema, error) { return nil, nil }
func (codec[T]) Decode(*http.Request) (T, error) { var v T; return v, nil }
func (codec[T]) Encode(T) (io.WriterTo, error)   { return nil, nil }

var (
	unmatched = openapi.Unmatched{NotFound: http.NotFoundHandler(), MethodNotAllowed: http.NotFoundHandler()}
	info      = openapi.Info{Title: "Carts", Version: "1.0.0"}
)

func newAPI(s *humus.Setup, ops []openapi.Operation) (*openapi.API, error) {
	return openapi.NewAPI(s, info, unmatched,
		openapi.RequestErrors(rejected, encoder),
		openapi.CatchAll(http.StatusInternalServerError, internalError, encoder),
		ops,
		openapi.Error[notFound](http.StatusNotFound, encoder),
	)
}
