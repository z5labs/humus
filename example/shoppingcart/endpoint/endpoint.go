// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package endpoint is the shoppingcart's API: six typed operations on carts
// and their items, served by the methods of [Carts], on a [Store].
//
// Every operation names its codecs, and every error has an answer the
// service chose: a cart or an item that doesn't exist is a [NotFound], a
// body the service won't accept is an [Invalid], and anything else is
// answered by the catch-all with [InternalError].
//
// Every operation requires a bearer token, which [Carts.Bearer]
// authenticates into the token's [Claims]. Reading a cart needs nothing
// more; every operation that changes one, creating it included, also needs
// the scope [ScopeWrite].
package endpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/z5labs/humus/auth"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

// Cart is a shopping cart and its items.
type Cart struct {
	CartID    uuid.UUID  `json:"cartId" required:"true" format:"uuid"`
	Items     []CartItem `json:"items" required:"true"`
	CreatedAt time.Time  `json:"createdAt" required:"true"`
	UpdatedAt time.Time  `json:"updatedAt" required:"true"`
}

// CartItem is an item in a cart: a quantity of one product, at a unit price.
type CartItem struct {
	ItemID    uuid.UUID `json:"itemId" required:"true" format:"uuid"`
	CartID    uuid.UUID `json:"cartId" required:"true" format:"uuid"`
	ProductID string    `json:"productId" required:"true"`
	Quantity  int       `json:"quantity" required:"true" minimum:"1"`
	UnitPrice float64   `json:"unitPrice" required:"true" minimum:"0"`
	CreatedAt time.Time `json:"createdAt" required:"true"`
	UpdatedAt time.Time `json:"updatedAt" required:"true"`
}

// AddItemRequest is the body of addCartItem.
type AddItemRequest struct {
	ProductID string  `json:"productId" required:"true" minLength:"1"`
	Quantity  int     `json:"quantity" required:"true" minimum:"1"`
	UnitPrice float64 `json:"unitPrice" required:"true" minimum:"0"`
}

// UpdateItemRequest is the body of updateCartItem.
type UpdateItemRequest struct {
	Quantity int `json:"quantity" required:"true" minimum:"1"`
}

// Store keeps the carts. A method given a cart that doesn't exist returns an
// error that wraps ErrCartNotFound, and one given an item its cart doesn't
// have returns an error that wraps ErrItemNotFound.
type Store interface {
	// CreateCart creates an empty cart.
	CreateCart(ctx context.Context) (Cart, error)

	// GetCart returns a cart and its items, oldest first.
	GetCart(ctx context.Context, cartID uuid.UUID) (Cart, error)

	// DeleteCart deletes a cart and its items.
	DeleteCart(ctx context.Context, cartID uuid.UUID) error

	// AddItem adds an item to a cart.
	AddItem(ctx context.Context, cartID uuid.UUID, item AddItemRequest) (CartItem, error)

	// UpdateItem sets the quantity of an item in a cart.
	UpdateItem(ctx context.Context, cartID, itemID uuid.UUID, quantity int) (CartItem, error)

	// RemoveItem removes an item from a cart.
	RemoveItem(ctx context.Context, cartID, itemID uuid.UUID) error
}

var (
	// ErrCartNotFound is the error a Store wraps for a cart that doesn't
	// exist.
	ErrCartNotFound = errors.New("cart not found")

	// ErrItemNotFound is the error a Store wraps for an item its cart
	// doesn't have.
	ErrItemNotFound = errors.New("item not found")
)

// NotFound is the answer for a cart or an item that doesn't exist: a 404,
// as an RFC 9457 problem detail.
type NotFound struct{ problem.Details }

// Invalid is the answer for a body that decodes but that the service won't
// accept, such as a quantity below 1: a 422, as an RFC 9457 problem detail.
// It isn't a 400, which is the API's answer for a body that doesn't decode.
type Invalid struct{ problem.Details }

// InternalError is the catch-all's wrap. Its answer is fixed, and never
// carries err's text, which the API records instead.
func InternalError(err error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
		Detail: "The server failed to answer the request.",
	}
}

// ScopeWrite is the scope a token needs to change a cart: to create one,
// delete one, or add, update or remove its items. A token without it can
// read a cart, and nothing more.
const ScopeWrite = "carts:write"

// Claims are what the shoppingcart reads from a bearer token: its scopes,
// in the scope claim, separated by spaces, as RFC 8693 has it. The
// registered claims, iss, aud, exp and the rest, are checked by auth/jwt's
// Verifier, so Claims needn't hold them.
type Claims struct {
	Scope string `json:"scope"`
}

// HasScope reports whether the token was granted scope. It makes Claims an
// auth.Scoped, so a requirement can name a scope of it.
func (c Claims) HasScope(scope string) bool {
	return slices.Contains(strings.Fields(c.Scope), scope)
}

// KeysUnavailable is the encoder of the answer to a *jwt.KeySetError: a
// 503, as an RFC 9457 problem detail, which says to try again. A token the
// service couldn't get keys for may be valid, so it is never a 401, which
// would tell the client to drop it. The KeySet's error isn't in the body,
// which would hand the client the identity provider's address and how it
// failed.
func KeysUnavailable() openapi.Encoder[*jwt.KeySetError] {
	return keysUnavailable{jsoncodec.Problem[problem.Details]()}
}

// keysUnavailable answers every *jwt.KeySetError with one problem detail,
// written by a problem.Details encoder, whose media type and schema it
// keeps.
type keysUnavailable struct {
	openapi.Encoder[problem.Details]
}

func (e keysUnavailable) Encode(*jwt.KeySetError) (io.WriterTo, error) {
	return e.Encoder.Encode(problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusServiceUnavailable),
		Status: http.StatusServiceUnavailable,
		Detail: "The server can't verify tokens right now. Try again later.",
	})
}

// uuidPattern is a UUID in its canonical form, written in the syntax RE2 and
// ECMA-262 share, so the document's pattern means what the API checks.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// The path parameters. A value that isn't a UUID is a 400, answered by the
// API's RequestErrors before any handler runs.
var (
	cartID = openapi.PathParam[string]("cartId").Description("The cart's identifier, a UUID.").Pattern(uuidPattern)
	itemID = openapi.PathParam[string]("itemId").Description("The item's identifier, a UUID.").Pattern(uuidPattern)
)

// The paths, built from the root.
var (
	carts = openapi.Root().Lit("carts") // /carts
	cart  = carts.Param(cartID)         // /carts/{cartId}
	items = cart.Lit("items")           // /carts/{cartId}/items
	item  = items.Param(itemID)         // /carts/{cartId}/items/{itemId}
)

// Parts that several operations share.
var (
	// tags is every operation's tag.
	tags = openapi.Tags("carts")

	// invalidBody answers an Invalid, on the operations that read a body.
	invalidBody = openapi.Error[Invalid](http.StatusUnprocessableEntity, jsoncodec.Problem)
)

// Carts serves the API's operations on Store, to the requests Bearer
// authenticates.
type Carts struct {
	Store Store

	// Bearer is the scheme every operation's requirement names. The API's
	// Security requires it of every operation; Operations adds ScopeWrite
	// to the ones that change a cart.
	Bearer auth.Scheme[Claims]
}

// Operations returns the API's six operations. A 404 isn't among any
// operation's parts: NewAPI gives Error[NotFound] to every operation. Nor is
// the bearer token getCart needs: NewAPI's Security requires it of every
// operation, and an operation that changes a cart replaces it with one that
// names ScopeWrite too.
func (c Carts) Operations() []openapi.Operation {
	// write is the requirement of every operation that changes a cart.
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
	created, err := c.Store.CreateCart(ctx)
	if err != nil {
		return Cart{}, err
	}
	return withItems(created), nil
}

func (c Carts) getCart(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (Cart, error) {
	id, err := uuidFrom(req, cartID)
	if err != nil {
		return Cart{}, err
	}
	got, err := c.Store.GetCart(ctx, id)
	if err != nil {
		return Cart{}, answer(err, id, uuid.Nil)
	}
	return withItems(got), nil
}

func (c Carts) deleteCart(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	id, err := uuidFrom(req, cartID)
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	return openapi.EmptyBody{}, answer(c.Store.DeleteCart(ctx, id), id, uuid.Nil)
}

func (c Carts) addCartItem(ctx context.Context, req openapi.Request[AddItemRequest]) (CartItem, error) {
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
	case body.UnitPrice < 0:
		return CartItem{}, invalid("unitPrice must not be negative")
	}
	added, err := c.Store.AddItem(ctx, id, body)
	if err != nil {
		return CartItem{}, answer(err, id, uuid.Nil)
	}
	return added, nil
}

func (c Carts) updateCartItem(ctx context.Context, req openapi.Request[UpdateItemRequest]) (CartItem, error) {
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
	updated, err := c.Store.UpdateItem(ctx, cid, iid, req.Body().Quantity)
	if err != nil {
		return CartItem{}, answer(err, cid, iid)
	}
	return updated, nil
}

func (c Carts) removeCartItem(ctx context.Context, req openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	cid, err := uuidFrom(req, cartID)
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	iid, err := uuidFrom(req, itemID)
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	return openapi.EmptyBody{}, answer(c.Store.RemoveItem(ctx, cid, iid), cid, iid)
}

// uuidFrom returns p's value in req as a UUID. p's Pattern has already
// checked it, so an error here is a bug, which the catch-all answers.
func uuidFrom[B any](req openapi.Request[B], p openapi.PathParameter[string]) (uuid.UUID, error) {
	return uuid.Parse(openapi.ParamFrom(req, p))
}

// withItems returns c with an empty, rather than a nil, Items, so its JSON
// has the array the document requires.
func withItems(c Cart) Cart {
	if c.Items == nil {
		c.Items = []CartItem{}
	}
	return c
}

// answer returns err as the NotFound it is, naming the cart and the item,
// or err itself, which the catch-all answers. It returns nil for nil.
func answer(err error, cartID, itemID uuid.UUID) error {
	switch {
	case errors.Is(err, ErrCartNotFound):
		return notFound(fmt.Sprintf("There is no cart %s.", cartID))
	case errors.Is(err, ErrItemNotFound):
		return notFound(fmt.Sprintf("Cart %s has no item %s.", cartID, itemID))
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

func invalid(detail string) Invalid {
	return Invalid{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
	}}
}
