// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package problem_test

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"

	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver"
	openapi "github.com/z5labs/humus/openapi/v3"
	"github.com/z5labs/humus/openapi/v3/jsoncodec"
	"github.com/z5labs/humus/openapi/v3/problem"
)

// The "Serving an API with OpenAPI" page of the docs site quotes the code in
// this file, and TestDocs_code_samples checks that every quote still matches
// it. The tests in orders_test.go run Init, and check what the page says
// about the orders API.

// Init is the end user's Init: it sources the server's Settings, builds the
// orders API with NewAPI, and serves it with httpserver.NewService.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	settings, err := readSettings(ctx, s)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	api, err := openapi.NewAPI(s,
		openapi.Info{Title: "Orders", Version: "1.0.0"},
		openapi.Unmatched{
			NotFound:         problem.Handler(http.StatusNotFound, jsoncodec.Problem),
			MethodNotAllowed: problem.Handler(http.StatusMethodNotAllowed, jsoncodec.Problem),
		},
		openapi.RequestErrors(problem.FromRequestError, jsoncodec.Problem),
		openapi.CatchAll(http.StatusInternalServerError, serverError, jsoncodec.Problem),
		append(NewOrders().Operations(),
			openapi.Document(openapi.Root().Lit("openapi.json")),
		),
	)
	if err != nil {
		return nil, err // every mistake in the declarations, named
	}

	svc, err := httpserver.NewService(s, settings, api)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc}, nil
}

// readSettings sources the server's address and certificate, and keeps
// DefaultSettings' every other field.
func readSettings(ctx context.Context, s *humus.Setup) (httpserver.Settings, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }
	return config.Struct(func(r *config.Resolver) httpserver.Settings {
		settings := httpserver.DefaultSettings()
		settings.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		settings.TLS = r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS))
		return settings
	}).Read(ctx)
}

// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

// serverTLS returns a server's TLS config for a PEM file that holds its
// certificate chain and its private key.
func serverTLS(pem []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// Order is an order, as the API answers with it.
type Order struct {
	ID     string      `json:"id" required:"true"`
	Status OrderStatus `json:"status" required:"true"`
	Lines  []Line      `json:"lines" required:"true"`
}

// Line is one line of an order: a quantity of one product.
type Line struct {
	SKU      string `json:"sku" required:"true" minLength:"1"`
	Quantity int    `json:"quantity" required:"true" minimum:"1"`
}

// OrderStatus is where an order is.
type OrderStatus string

// The statuses of an order.
const (
	Open      OrderStatus = "open"
	Cancelled OrderStatus = "cancelled"
)

// orderIDPattern is an order's identifier, written in the syntax RE2 and
// ECMA-262 share, so the document's pattern means what the API checks.
var orderIDPattern = regexp.MustCompile(`^[0-9]{1,9}$`)

// orderID is the path parameter of the operations on one order.
var orderID = openapi.PathParam[string]("orderId").
	Description("The order's identifier.").
	Pattern(orderIDPattern)

// byStatus and pageSize are listOrders' query parameters.
var byStatus = openapi.QueryParam[OrderStatus]("status").
	Description("List only the orders with this status.").
	Enum(Open, Cancelled).
	Default(Open)

var pageSize = openapi.QueryParam[int]("limit").
	Description("The most orders to list.").
	Minimum(1).
	Maximum(100).
	Default(20)

// idempotencyKey is createOrder's header parameter.
var idempotencyKey = openapi.HeaderParam[string]("Idempotency-Key").
	Description("Sent again with the same key, the request creates no second order.").
	MaxLength(64)

// The paths, built from the root.
var (
	orders = openapi.Root().Lit("orders") // /orders
	order  = orders.Param(orderID)        // /orders/{orderId}
	lines  = order.Lit("lines")           // /orders/{orderId}/lines
)

// NotFound is the answer for an order that doesn't exist: a 404, as an RFC
// 9457 problem detail.
type NotFound struct{ problem.Details }

// Invalid is the answer for a body that decodes, but that the API won't
// accept, such as a quantity below 1: a 422. It isn't a 400, which is
// RequestErrors' answer for a body that doesn't decode.
type Invalid struct{ problem.Details }

// Parts that several operations share.
var (
	// orderNotFound answers a NotFound, on the operations that address an
	// order.
	orderNotFound = openapi.Error[NotFound](http.StatusNotFound, jsoncodec.Problem)

	// invalidLine answers an Invalid, on the operation that reads a line.
	invalidLine = openapi.Error[Invalid](http.StatusUnprocessableEntity, jsoncodec.Problem)
)

// serverError is the catch-all's wrap. Its answer is fixed, and never
// carries err's text, which the API records instead.
func serverError(err error) problem.Details {
	return problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
		Detail: "The server failed to answer the request.",
	}
}

// Orders serves the orders API, on orders it keeps in memory.
type Orders struct {
	mu     sync.Mutex
	orders []*Order          // oldest first
	byID   map[string]*Order // the same orders, by ID
	keys   map[string]string // an Idempotency-Key's order
}

// NewOrders returns an Orders that keeps no orders yet.
func NewOrders() *Orders {
	return &Orders{byID: make(map[string]*Order), keys: make(map[string]string)}
}

// Operations returns the orders API's operations.
func (o *Orders) Operations() []openapi.Operation {
	return []openapi.Operation{
		openapi.Get(orders, o.listOrders, jsoncodec.Encoder,
			openapi.OperationID("listOrders"),
			openapi.Summary("List the orders"),
			openapi.Query(byStatus),
			openapi.Query(pageSize),
		),
		openapi.Post(orders, o.createOrder, openapi.EmptyDecoder, jsoncodec.Encoder,
			openapi.OperationID("createOrder"),
			openapi.Summary("Create an order"),
			openapi.Status(http.StatusCreated),
			openapi.Header(idempotencyKey),
		),
		openapi.Get(order, o.getOrder, jsoncodec.Encoder,
			openapi.OperationID("getOrder"),
			openapi.Summary("Get an order"),
			orderNotFound,
		),
		openapi.Delete(order, o.cancelOrder, openapi.EmptyEncoder,
			openapi.OperationID("cancelOrder"),
			openapi.Summary("Cancel an order"),
			orderNotFound,
		),
		openapi.Post(lines, o.addLine, jsoncodec.Decoder, jsoncodec.Encoder,
			openapi.OperationID("addLine"),
			openapi.Summary("Add a line to an order"),
			openapi.Status(http.StatusCreated),
			orderNotFound,
			invalidLine,
		),
	}
}

func (o *Orders) listOrders(_ context.Context, req openapi.Request[openapi.EmptyBody]) ([]Order, error) {
	status := openapi.ParamFrom(req, byStatus) // Open when the request has no status
	limit := openapi.ParamFrom(req, pageSize)  // 1 to 100, or 20
	o.mu.Lock()
	defer o.mu.Unlock()
	listed := []Order{}
	for _, ord := range o.orders {
		if ord.Status == status && len(listed) < limit {
			listed = append(listed, *ord)
		}
	}
	return listed, nil
}

func (o *Orders) createOrder(_ context.Context, req openapi.Request[openapi.EmptyBody]) (Order, error) {
	key := openapi.ParamFrom(req, idempotencyKey) // "" when the request has none
	o.mu.Lock()
	defer o.mu.Unlock()
	if id, ok := o.keys[key]; ok && key != "" {
		return *o.byID[id], nil
	}
	ord := &Order{ID: strconv.Itoa(len(o.orders) + 1), Status: Open, Lines: []Line{}}
	o.orders = append(o.orders, ord)
	o.byID[ord.ID] = ord
	if key != "" {
		o.keys[key] = ord.ID
	}
	return *ord, nil
}

func (o *Orders) getOrder(_ context.Context, req openapi.Request[openapi.EmptyBody]) (Order, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ord, err := o.find(openapi.ParamFrom(req, orderID))
	if err != nil {
		return Order{}, err
	}
	return *ord, nil
}

func (o *Orders) cancelOrder(_ context.Context, req openapi.Request[openapi.EmptyBody]) (openapi.EmptyBody, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ord, err := o.find(openapi.ParamFrom(req, orderID))
	if err != nil {
		return openapi.EmptyBody{}, err
	}
	ord.Status = Cancelled
	return openapi.EmptyBody{}, nil
}

func (o *Orders) addLine(_ context.Context, req openapi.Request[Line]) (Line, error) {
	line := req.Body()
	switch {
	case line.SKU == "":
		return Line{}, invalid("sku is required")
	case line.Quantity < 1:
		return Line{}, invalid("quantity must be at least 1")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	ord, err := o.find(openapi.ParamFrom(req, orderID))
	if err != nil {
		return Line{}, err
	}
	ord.Lines = append(ord.Lines, line)
	return line, nil
}

// find returns the order id, or a NotFound naming it.
func (o *Orders) find(id string) (*Order, error) {
	ord, ok := o.byID[id]
	if !ok {
		return nil, NotFound{problem.Details{
			Type:   "about:blank",
			Title:  http.StatusText(http.StatusNotFound),
			Status: http.StatusNotFound,
			Detail: "There is no order " + id + ".",
		}}
	}
	return ord, nil
}

func invalid(detail string) Invalid {
	return Invalid{problem.Details{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
	}}
}

// An API's main is one call to httpserver.Main, with an Init that returns
// the Service serving it. This example has no Output comment, so it is
// compiled but never run: running it would exit the test binary.
func Example() {
	httpserver.Main(Init)
}
