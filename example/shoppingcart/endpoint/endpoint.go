// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package endpoint is the shoppingcart's API: its operations on carts, their answers, and the Store and Catalog they are served on.
package endpoint

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/z5labs/bedrock/circuit"

	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

type Cart struct {
	CartID    uuid.UUID  `json:"cartId" required:"true" format:"uuid"`
	Items     []CartItem `json:"items" required:"true"`
	CreatedAt time.Time  `json:"createdAt" required:"true"`
	UpdatedAt time.Time  `json:"updatedAt" required:"true"`
}

type CartItem struct {
	ItemID    uuid.UUID `json:"itemId" required:"true" format:"uuid"`
	CartID    uuid.UUID `json:"cartId" required:"true" format:"uuid"`
	ProductID string    `json:"productId" required:"true"`
	Quantity  int       `json:"quantity" required:"true" minimum:"1"`
	UnitPrice float64   `json:"unitPrice" required:"true" minimum:"0"`
	CreatedAt time.Time `json:"createdAt" required:"true"`
	UpdatedAt time.Time `json:"updatedAt" required:"true"`
}

type AddItemRequest struct {
	ProductID string `json:"productId" required:"true" minLength:"1"`
	Quantity  int    `json:"quantity" required:"true" minimum:"1"`
}

type NewItem struct {
	ProductID string
	Quantity  int
	UnitPrice float64
}

type UpdateItemRequest struct {
	Quantity int `json:"quantity" required:"true" minimum:"1"`
}

type Store interface {
	CreateCart(ctx context.Context, owner string) (Cart, error)
	GetCart(ctx context.Context, owner string, cartID uuid.UUID) (Cart, error)
	DeleteCart(ctx context.Context, owner string, cartID uuid.UUID) error
	AddItem(ctx context.Context, owner string, cartID uuid.UUID, item NewItem) (CartItem, error)
	UpdateItem(ctx context.Context, owner string, cartID, itemID uuid.UUID, quantity int) (CartItem, error)
	RemoveItem(ctx context.Context, owner string, cartID, itemID uuid.UUID) error
}

type Catalog interface {
	Price(ctx context.Context, productID string) (float64, error)
}

var (
	ErrCartNotFound        = errors.New("cart not found")
	ErrItemNotFound        = errors.New("item not found")
	ErrProductNotFound     = errors.New("product not found")
	ErrCatalogUnavailable  = errors.New("the catalog is unavailable")
	ErrDatabaseUnavailable = errors.New("the database is unavailable")
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

const ScopeWrite = "carts:write"

type Claims struct {
	Subject string `json:"sub"`
	Scope   string `json:"scope"`
}

func (c Claims) HasScope(scope string) bool {
	return slices.Contains(strings.Fields(c.Scope), scope)
}

func KeysUnavailable(verify func(context.Context, string) (Claims, error)) func(context.Context, string) (Claims, error) {
	return func(ctx context.Context, token string) (Claims, error) {
		claims, err := verify(ctx, token)
		if _, ok := errors.AsType[*jwt.KeySetError](err); ok {
			return Claims{}, unavailable("The server can't verify tokens right now. Try again later.")
		}
		return claims, err
	}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var (
	cartID = openapi.PathParam[string]("cartId").Description("The cart's identifier, a UUID.").Pattern(uuidPattern)
	itemID = openapi.PathParam[string]("itemId").Description("The item's identifier, a UUID.").Pattern(uuidPattern)
)

var (
	carts = openapi.Root().Lit("carts")
	cart  = carts.Param(cartID)
	items = cart.Lit("items")
	item  = items.Param(itemID)
)

var (
	tags        = openapi.Tags("carts")
	invalidBody = openapi.Error[Invalid](http.StatusUnprocessableEntity, jsoncodec.Problem)
)

type Carts struct {
	Store   Store
	Catalog Catalog
	Bearer  auth.Scheme[Claims]
}

func (c Carts) Operations() []openapi.Operation {
	write := openapi.Security(auth.RequireScopes(c.Bearer, ScopeWrite))
	return []openapi.Operation{
		openapi.Post(carts, c.createCart, openapi.EmptyDecoder, jsoncodec.Encoder,
			openapi.OperationID("createCart"),
			write,
			openapi.Summary("Create a new shopping cart"),
			openapi.Status(http.StatusCreated),
			tags,
		),
		openapi.Get(cart, c.getCart, jsoncodec.Encoder,
			openapi.OperationID("getCart"),
			openapi.Summary("Get a shopping cart and its items"),
			tags,
		),
		openapi.Delete(cart, c.deleteCart, openapi.EmptyEncoder,
			openapi.OperationID("deleteCart"),
			write,
			openapi.Summary("Delete a shopping cart and all its items"),
			tags,
		),
		openapi.Post(items, c.addCartItem, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("addCartItem"),
			write,
			openapi.Summary("Add an item to a shopping cart"),
			openapi.Status(http.StatusCreated),
			invalidBody,
			tags,
		),
		openapi.Patch(item, c.updateCartItem, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("updateCartItem"),
			write,
			openapi.Summary("Update the quantity of an item in a shopping cart"),
			invalidBody,
			tags,
		),
		openapi.Delete(item, c.removeCartItem, openapi.EmptyEncoder,
			openapi.OperationID("removeCartItem"),
			write,
			openapi.Summary("Remove an item from a shopping cart"),
			tags,
		),
	}
}

func (c Carts) createCart(ctx context.Context, _ openapi.Request[openapi.EmptyBody]) (Cart, error) {
	owner, err := c.owner(ctx)
	if err != nil {
		return Cart{}, err
	}
	created, err := c.Store.CreateCart(ctx, owner)
	if err != nil {
		return Cart{}, answer(err, uuid.Nil, uuid.Nil)
	}
	return withItems(created), nil
}

func (c Carts) getCart(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (Cart, error) {
	owner, err := c.owner(ctx)
	if err != nil {
		return Cart{}, err
	}
	id, err := uuidFrom(req, cartID)
	if err != nil {
		return Cart{}, err
	}
	got, err := c.Store.GetCart(ctx, owner, id)
	if err != nil {
		return Cart{}, answer(err, id, uuid.Nil)
	}
	return withItems(got), nil
}

func (c Carts) deleteCart(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	owner, err := c.owner(ctx)
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	id, err := uuidFrom(req, cartID)
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	return openapi.EmptyBody{}, answer(c.Store.DeleteCart(ctx, owner, id), id, uuid.Nil)
}

func (c Carts) addCartItem(ctx context.Context, req openapi.Request[AddItemRequest]) (CartItem, error) {
	owner, err := c.owner(ctx)
	if err != nil {
		return CartItem{}, err
	}
	id, err := uuidFrom(req, cartID)
	if err != nil {
		return CartItem{}, err
	}
	body := req.Body()
	switch {
	case body.ProductID == "":
		return CartItem{}, invalid("productId is required")
	case body.Quantity < 1:
		return CartItem{}, invalid("quantity must be at least 1")
	}
	price, err := c.Catalog.Price(ctx, body.ProductID)
	if errors.Is(err, ErrProductNotFound) {
		return CartItem{}, invalid(fmt.Sprintf("There is no product %s in the catalog.", body.ProductID))
	}
	if err != nil {
		return CartItem{}, answer(err, id, uuid.Nil)
	}
	added, err := c.Store.AddItem(ctx, owner, id, NewItem{ProductID: body.ProductID, Quantity: body.Quantity, UnitPrice: price})
	if err != nil {
		return CartItem{}, answer(err, id, uuid.Nil)
	}
	return added, nil
}

func (c Carts) updateCartItem(ctx context.Context, req openapi.Request[UpdateItemRequest]) (CartItem, error) {
	owner, err := c.owner(ctx)
	if err != nil {
		return CartItem{}, err
	}
	cid, err := uuidFrom(req, cartID)
	if err != nil {
		return CartItem{}, err
	}
	iid, err := uuidFrom(req, itemID)
	if err != nil {
		return CartItem{}, err
	}
	if req.Body().Quantity < 1 {
		return CartItem{}, invalid("quantity must be at least 1")
	}
	updated, err := c.Store.UpdateItem(ctx, owner, cid, iid, req.Body().Quantity)
	if err != nil {
		return CartItem{}, answer(err, cid, iid)
	}
	return updated, nil
}

func (c Carts) removeCartItem(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	owner, err := c.owner(ctx)
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	cid, err := uuidFrom(req, cartID)
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	iid, err := uuidFrom(req, itemID)
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	return openapi.EmptyBody{}, answer(c.Store.RemoveItem(ctx, owner, cid, iid), cid, iid)
}

func (c Carts) owner(ctx context.Context) (string, error) {
	claims, ok := c.Bearer.From(ctx)
	if !ok || claims.Subject == "" {
		return "", errors.New("the request has no bearer token with a subject")
	}
	return claims.Subject, nil
}

func uuidFrom[B any](req openapi.Request[B], p openapi.PathParameter[string]) (uuid.UUID, error) {
	return uuid.Parse(openapi.ParamFrom(req, p))
}

func withItems(c Cart) Cart {
	if c.Items == nil {
		c.Items = []CartItem{}
	}
	return c
}

func answer(err error, cartID, itemID uuid.UUID) error {
	switch {
	case errors.Is(err, ErrCartNotFound):
		return notFound(fmt.Sprintf("There is no cart %s.", cartID))
	case errors.Is(err, ErrItemNotFound):
		return notFound(fmt.Sprintf("Cart %s has no item %s.", cartID, itemID))
	case errors.Is(err, ErrDatabaseUnavailable):
		return unavailable("The cart can't reach its database right now. Try again later.")
	case errors.Is(err, ErrCatalogUnavailable) && errors.Is(err, circuit.ErrRejected):
		return unavailable("The cart isn't calling the catalog right now, since it has been failing. Try again later.")
	case errors.Is(err, ErrCatalogUnavailable):
		return unavailable("The cart can't reach the catalog right now. Try again later.")
	default:
		return err
	}
}

func notFound(detail string) NotFound {
	return NotFound{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusNotFound),
		Status: http.StatusNotFound,
		Detail: detail,
	}}
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
