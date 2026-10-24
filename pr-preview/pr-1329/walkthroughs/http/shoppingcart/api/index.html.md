# The cart's API

## The bodies

Start the `endpoint` package with what the API reads and writes. Its tags
become the document's schemas:

```go
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
```

```go
type UpdateItemRequest struct {
	Quantity int `json:"quantity" required:"true" minimum:"1"`
}
```

An item's request has no price. The price comes from the catalog, so a
client can't set its own.

## The Store and the Catalog

The operations are served on two interfaces. Every `Store` method takes the
cart's owner:

```go
type NewItem struct {
	ProductID string
	Quantity  int
	UnitPrice float64
}
```

```go
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
```

What they return, besides a result, is one of these errors, or another
that wraps none of them:

```go
var (
	ErrCartNotFound        = errors.New("cart not found")
	ErrItemNotFound        = errors.New("item not found")
	ErrProductNotFound     = errors.New("product not found")
	ErrCatalogUnavailable  = errors.New("the catalog is unavailable")
	ErrDatabaseUnavailable = errors.New("the database is unavailable")
)
```

## The answers

Three answers are problem details of the API's own, and anything else is a
500 that says nothing of the failure:

```go
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
```

Each is made with its status and a detail:

```go
func notFound(detail string) NotFound {
	return NotFound{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusNotFound),
		Status: http.StatusNotFound,
		Detail: detail,
	}}
}
```

`answer` turns the `Store`'s and the `Catalog`'s errors into them. A
dependency that is down is a 503, never a 500, and its detail says which:

```go
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
```

## The paths

The paths are built from typed parts. Both IDs are UUIDs:

```go
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
```

The pattern is checked before a handler runs, so a handler parses an ID
that is already well formed:

```go
func uuidFrom[B any](req openapi.Request[B], p openapi.PathParameter[string]) (uuid.UUID, error) {
	return uuid.Parse(openapi.ParamFrom(req, p))
}
```

## The operations

`Carts` holds what the operations are served on, and declares the six of
them:

```go
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
```

The two with a body answer one it won't accept with an `Invalid`:

```go
var (
	tags        = openapi.Tags("carts")
	invalidBody = openapi.Error[Invalid](http.StatusUnprocessableEntity, jsoncodec.Problem)
)
```

[Who may do what](/humus/pr-preview/pr-1329/walkthroughs/http/shoppingcart/access/) builds
`write` and the bearer scheme.

## A handler

Each handler is a plain function from a `Request` to its answer. Adding an
item checks the body, asks the catalog for the price, and stores the item
with it:

```go
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
```

The item keeps the price it was added at, so a later change of price
doesn't change a cart.

A cart with no items answers `"items": []`, never `null`:

```go
func withItems(c Cart) Cart {
	if c.Items == nil {
		c.Items = []CartItem{}
	}
	return c
}
```

## The API

The cart's `Init` builds the API from the operations, the document, and the
answers every operation shares:

```go
api, err := openapi.NewAPI(s,
	openapi.Info{
		Title:       "Shopping Cart API",
		Version:     "1.0.0",
		Description: "API for managing shopping carts on an e-commerce platform.",
	},
	openapi.Unmatched{
		NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
		MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
	},
	openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
	openapi.CatchAll(http.StatusInternalServerError, endpoint.InternalError, jsoncodec.Problem),
	append(endpoint.Carts{Store: store, Catalog: catalog, Bearer: bearer}.Operations(),
		openapi.Document(openapi.Root().Lit("openapi.json"), openapi.Security(auth.Disabled())),
	),
	openapi.Error[endpoint.NotFound](http.StatusNotFound, jsoncodec.Problem),
	openapi.Security(auth.Require(bearer)),
	openapi.Error[endpoint.Unavailable](http.StatusServiceUnavailable, jsoncodec.Problem),
)
if err != nil {
	return nil, err
}
```

It is served behind `http.MaxBytesHandler`, so a body over 64 KiB is a 413:

```go
mux.Handle("/", http.MaxBytesHandler(api, maxBodyBytes))
```

```go
const maxBodyBytes = 64 << 10
```

| Operation | Path | Answers |
|---|---|---|
| `createCart` | `POST /carts` | 201, 401, 403, 404, 500, 503 |
| `getCart` | `GET /carts/{cartId}` | 200, 400, 401, 404, 500, 503 |
| `deleteCart` | `DELETE /carts/{cartId}` | 204, 400, 401, 403, 404, 500, 503 |
| `addCartItem` | `POST /carts/{cartId}/items` | 201, 400, 401, 403, 404, 413, 415, 422, 500, 503 |
| `updateCartItem` | `PATCH /carts/{cartId}/items/{itemId}` | 200, 400, 401, 403, 404, 413, 415, 422, 500, 503 |
| `removeCartItem` | `DELETE /carts/{cartId}/items/{itemId}` | 204, 400, 401, 403, 404, 500, 503 |

A 400, a 413 and a 415 are `RequestErrors`': a parameter, or a body, the API
couldn't read. A path or a method the API doesn't serve is `Unmatched`'s 404
or 405.

## The document

The document is served at `GET /openapi.json`, to anyone. The cart's tests
keep it as a golden file, `app/testdata/openapi.json`, so a change to any
declaration shows up in review as a change to the document.

Next: [Who may do what](/humus/pr-preview/pr-1329/walkthroughs/http/shoppingcart/access/)
