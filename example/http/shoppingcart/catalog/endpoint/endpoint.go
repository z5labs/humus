// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package endpoint is the catalog's API: its operations on products, their answers and the Store they are served on.
package endpoint

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

type Product struct {
	ProductID string  `json:"productId" required:"true"`
	UnitPrice float64 `json:"unitPrice" required:"true" minimum:"0" maximum:"99999999.99"`
}

type PutProductRequest struct {
	UnitPrice *float64 `json:"unitPrice" required:"true" nullable:"false" minimum:"0" maximum:"99999999.99"`
}

const maxUnitPrice = 99999999.99

type Store interface {
	GetProduct(ctx context.Context, productID string) (Product, error)
	PutProduct(ctx context.Context, product Product) (Product, error)
}

var (
	ErrProductNotFound = errors.New("product not found")
	ErrUnavailable     = errors.New("the database is unavailable")
)

type NotFound struct{ problem.Details }

type Invalid struct{ problem.Details }

type Unavailable struct{ problem.Details }

func InternalError(err error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
		Detail: "The server failed to answer the request.",
	}
}

const ScopeWrite = "catalog:write"

type Claims struct {
	Scope string `json:"scope"`
}

func (c Claims) HasScope(scope string) bool {
	return slices.Contains(strings.Fields(c.Scope), scope)
}

var productID = openapi.PathParam[string]("productId").Description("The product's identifier.").MinLength(1).MaxLength(255)

var product = openapi.Root().Lit("products").Param(productID)

func NewAPI(s *humus.Setup, store Store, verify func(ctx context.Context, token string) (Claims, error)) (*openapi.API, error) {
	client := auth.MutualTLS("mutualTLS", verifiedLeaf).
		Description("A client certificate the catalog's CA issued, such as the shoppingcart's. Reading a product needs one.")
	bearer := auth.Bearer("bearer", keysUnavailable(verify)).
		Format("JWT").
		Description("A JWT from the shoppingcart's identity provider. Changing a product needs the scope catalog:write.")

	p := products{store: store}
	tags := openapi.Tags("products")
	return openapi.NewAPI(s,
		openapi.Info{
			Title:       "Catalog API",
			Version:     "1.0.0",
			Description: "API for the products an e-commerce platform sells, and their prices.",
		},
		openapi.Unmatched{
			NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
			MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
		},
		openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
		openapi.CatchAll(http.StatusInternalServerError, InternalError, jsoncodec.Problem),
		[]openapi.Operation{
			openapi.Get(product, p.getProduct, jsoncodec.Encoder,
				openapi.OperationID("getProduct"),
				openapi.Security(auth.Require(client)),
				openapi.Summary("Get a product and its unit price"),
				openapi.Error[NotFound](http.StatusNotFound, jsoncodec.Problem),
				tags,
			),
			openapi.Put(product, p.putProduct, jsoncodec.Decoder, jsoncodec.Encoder,
				openapi.OperationID("putProduct"),
				openapi.Security(auth.RequireScopes(bearer, ScopeWrite)),
				openapi.Summary("Create or replace a product and its unit price"),
				openapi.Error[Invalid](http.StatusUnprocessableEntity, jsoncodec.Problem),
				tags,
			),
			openapi.Document(openapi.Root().Lit("openapi.json"), openapi.Security(auth.Disabled())),
		},
		openapi.Error[Unavailable](http.StatusServiceUnavailable, jsoncodec.Problem),
	)
}

func keysUnavailable(verify func(context.Context, string) (Claims, error)) func(context.Context, string) (Claims, error) {
	return func(ctx context.Context, token string) (Claims, error) {
		claims, err := verify(ctx, token)
		if keys := (*jwt.KeySetError)(nil); errors.As(err, &keys) {
			return Claims{}, unavailable("The server can't verify tokens right now. Try again later.")
		}
		return claims, err
	}
}

func verifiedLeaf(_ context.Context, chains [][]*x509.Certificate) (*x509.Certificate, error) {
	if len(chains) == 0 || len(chains[0]) == 0 {
		return nil, fmt.Errorf("no verified chain: %w", auth.ErrUnauthenticated)
	}
	return chains[0][0], nil
}

type products struct {
	store Store
}

func (p products) getProduct(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (Product, error) {
	id := openapi.ParamFrom(req, productID)
	got, err := p.store.GetProduct(ctx, id)
	if err != nil {
		return Product{}, answer(err, id)
	}
	return got, nil
}

func (p products) putProduct(ctx context.Context, req openapi.Request[PutProductRequest]) (Product, error) {
	id := openapi.ParamFrom(req, productID)
	price := req.Body().UnitPrice
	switch {
	case price == nil:
		return Product{}, invalid("unitPrice is required")
	case *price < 0:
		return Product{}, invalid("unitPrice must not be negative")
	case *price > maxUnitPrice:
		return Product{}, invalid(fmt.Sprintf("unitPrice must be at most %.2f", maxUnitPrice))
	}
	put, err := p.store.PutProduct(ctx, Product{ProductID: id, UnitPrice: *price})
	if err != nil {
		return Product{}, answer(err, id)
	}
	return put, nil
}

func answer(err error, productID string) error {
	switch {
	case errors.Is(err, ErrProductNotFound):
		return NotFound{problem.Details{
			Type:   "about:blank",
			Title:  http.StatusText(http.StatusNotFound),
			Status: http.StatusNotFound,
			Detail: fmt.Sprintf("There is no product %s.", productID),
		}}
	case errors.Is(err, ErrUnavailable):
		return unavailable("The catalog can't reach its database right now. Try again later.")
	default:
		return err
	}
}

func unavailable(detail string) Unavailable {
	return Unavailable{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusServiceUnavailable),
		Status: http.StatusServiceUnavailable,
		Detail: detail,
	}}
}

func invalid(detail string) Invalid {
	return Invalid{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
	}}
}
