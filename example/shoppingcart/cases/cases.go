// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package cases is the shoppingcart's black-box cases, run against a deployment of the cart and the catalog.
package cases

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock/fit"
	"go.opentelemetry.io/otel/baggage"

	"github.com/z5labs/humus/fault/jsonscenario"
)

const (
	cartsWrite   = "carts:write"
	catalogWrite = "catalog:write"

	cartsPoint           = "postgresql:carts"
	catalogDatabasePoint = "postgresql:catalog"

	slowCatalog = 2 * time.Second
	pollEvery   = 500 * time.Millisecond

	tryAgain = "Try again"
	shed     = "isn't calling the catalog"
)

type Target struct {
	Cart       *url.URL
	CartClient *http.Client

	Catalog       *url.URL
	CatalogClient *http.Client

	Token func(subject string, scopes ...string) (string, error)

	CatalogPoint string
	ScenarioKey  string
	TrustScope   string
	Timeout      time.Duration
}

type Case struct {
	Name string
	Run  func(t testing.TB, target Target)
}

var Parallel = []Case{
	{Name: "ACartsLife", Run: ACartsLife},
	{Name: "Prices", Run: Prices},
	{Name: "UnknownProduct", Run: UnknownProduct},
	{Name: "Ownership", Run: Ownership},
	{Name: "Scopes", Run: Scopes},
	{Name: "SlowCatalog", Run: SlowCatalog},
	{Name: "CartDatabaseDown", Run: CartDatabaseDown},
	{Name: "UntrustedScenario", Run: UntrustedScenario},
}

var Alone = []Case{
	{Name: "CatalogDown", Run: CatalogDown},
	{Name: "CatalogDatabaseDown", Run: CatalogDatabaseDown},
	{Name: "CatalogBreakerOpens", Run: CatalogBreakerOpens},
}

func ACartsLife(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 12.5)
	s := newShopper(t, target, cartsWrite)

	c := s.createCart(ctx, t)
	require.Empty(t, c.Items)

	added := s.addItem(ctx, t, c.CartID, p.ProductID, 2)
	require.Equal(t, c.CartID, added.CartID)
	require.Equal(t, p.ProductID, added.ProductID)
	require.Equal(t, 2, added.Quantity)
	require.InDelta(t, p.UnitPrice, added.UnitPrice, 1e-9)
	require.Equal(t, []item{added}, s.getCart(ctx, t, c.CartID).Items)

	changed := s.updateItem(ctx, t, c.CartID, added.ItemID, 3)
	require.Equal(t, added.ItemID, changed.ItemID)
	require.Equal(t, 3, changed.Quantity)
	require.Equal(t, []item{changed}, s.getCart(ctx, t, c.CartID).Items)

	s.call(ctx, t, http.MethodDelete, itemPath(c.CartID, added.ItemID), nil).empty(t, http.StatusNoContent)
	require.Empty(t, s.getCart(ctx, t, c.CartID).Items)

	s.call(ctx, t, http.MethodDelete, cartPath(c.CartID), nil).empty(t, http.StatusNoContent)
	s.call(ctx, t, http.MethodGet, cartPath(c.CartID), nil).problem(t, http.StatusNotFound)
}

func Prices(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 9.99)
	s := newShopper(t, target, cartsWrite)
	c := s.createCart(ctx, t)

	first := s.addItem(ctx, t, c.CartID, p.ProductID, 1)
	require.InDelta(t, 9.99, first.UnitPrice, 1e-9)

	repriced := catalogCall(ctx, t, target, catalogWriter(t, target), http.MethodPut, productPath(p.ProductID), map[string]any{"unitPrice": 12.5})
	var put product
	repriced.decode(t, http.StatusOK, &put)
	require.Equal(t, product{ProductID: p.ProductID, UnitPrice: 12.5}, put)

	got := s.getCart(ctx, t, c.CartID)
	require.Equal(t, []item{first}, got.Items)

	second := s.addItem(ctx, t, c.CartID, p.ProductID, 1)
	require.InDelta(t, 12.5, second.UnitPrice, 1e-9)
	require.Equal(t, []item{first, second}, s.getCart(ctx, t, c.CartID).Items)
}

func UnknownProduct(t testing.TB, target Target) {
	ctx := bounded(t, target)
	s := newShopper(t, target, cartsWrite)
	c := s.createCart(ctx, t)

	s.call(ctx, t, http.MethodPost, itemsPath(c.CartID), newItem(newProductID(), 1)).problem(t, http.StatusUnprocessableEntity)

	require.Empty(t, s.getCart(ctx, t, c.CartID).Items)
}

func Ownership(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 4.5)
	owner := newShopper(t, target, cartsWrite)
	c := owner.createCart(ctx, t)
	added := owner.addItem(ctx, t, c.CartID, p.ProductID, 1)
	before := owner.getCart(ctx, t, c.CartID)

	other := newShopper(t, target, cartsWrite)
	other.createCart(ctx, t)
	for _, op := range append(changes(c.CartID, added.ItemID, p.ProductID)[1:], read(c.CartID)) {
		other.call(ctx, t, op.method, op.path, op.body).problem(t, http.StatusNotFound)
	}

	require.Equal(t, before, owner.getCart(ctx, t, c.CartID))
}

func Scopes(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 3.25)
	writer := newShopper(t, target, cartsWrite)
	c := writer.createCart(ctx, t)
	added := writer.addItem(ctx, t, c.CartID, p.ProductID, 1)

	reader := writer.withScopes(t, target)
	for _, op := range changes(c.CartID, added.ItemID, p.ProductID) {
		reader.call(ctx, t, op.method, op.path, op.body).problem(t, http.StatusForbidden)
	}
	require.Equal(t, []item{added}, reader.getCart(ctx, t, c.CartID).Items)

	anonymous := shopper{target: target}
	for _, op := range append(changes(c.CartID, added.ItemID, p.ProductID), read(c.CartID)) {
		anonymous.call(ctx, t, op.method, op.path, op.body).problem(t, http.StatusUnauthorized)
	}
	anonymous.call(ctx, t, http.MethodGet, "/openapi.json", nil).ok(t)
	require.Equal(t, []item{added}, writer.getCart(ctx, t, c.CartID).Items)

	catalogCall(ctx, t, target, writer.token, http.MethodPut, productPath(p.ProductID), map[string]any{"unitPrice": 1}).problem(t, http.StatusForbidden)
	catalogCall(ctx, t, target, "", http.MethodGet, productPath(p.ProductID), nil).problem(t, http.StatusUnauthorized)
	require.InDelta(t, 3.25, writer.addItem(ctx, t, c.CartID, p.ProductID, 1).UnitPrice, 1e-9)
}

func SlowCatalog(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 7.5)
	s := newRehearser(t, target)
	c := s.createCart(ctx, t)

	start := time.Now()
	added := s.rehearsing(t, delay(catalogPoint(t, target), slowCatalog)).addItem(ctx, t, c.CartID, p.ProductID, 1)
	require.GreaterOrEqual(t, time.Since(start), slowCatalog)
	require.InDelta(t, p.UnitPrice, added.UnitPrice, 1e-9)
	require.Equal(t, []item{added}, s.getCart(ctx, t, c.CartID).Items)
}

func CartDatabaseDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 5.75)
	s := newRehearser(t, target)
	c := s.createCart(ctx, t)
	added := s.addItem(ctx, t, c.CartID, p.ProductID, 1)

	detail := s.rehearsing(t, abort(cartsPoint)).call(ctx, t, http.MethodGet, cartPath(c.CartID), nil).problem(t, http.StatusServiceUnavailable)
	require.Contains(t, detail, tryAgain)

	require.Equal(t, []item{added}, s.getCart(ctx, t, c.CartID).Items)
}

func UntrustedScenario(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 6.25)
	s := newShopper(t, target, cartsWrite)
	c := s.createCart(ctx, t)

	added := s.rehearsing(t, abort(catalogPoint(t, target))).addItem(ctx, t, c.CartID, p.ProductID, 1)
	require.InDelta(t, p.UnitPrice, added.UnitPrice, 1e-9)
	require.Equal(t, []item{added}, s.getCart(ctx, t, c.CartID).Items)
}

func CatalogDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 8.5)
	s := newRehearser(t, target)
	c := s.createCart(ctx, t)

	detail := s.rehearsing(t, abort(catalogPoint(t, target))).call(ctx, t, http.MethodPost, itemsPath(c.CartID), newItem(p.ProductID, 1)).problem(t, http.StatusServiceUnavailable)
	require.Contains(t, detail, "catalog")
	require.Empty(t, s.getCart(ctx, t, c.CartID).Items)

	added := s.addItemOnceAdmitted(ctx, t, c.CartID, p.ProductID)
	require.InDelta(t, p.UnitPrice, added.UnitPrice, 1e-9)
	require.Equal(t, []item{added}, s.getCart(ctx, t, c.CartID).Items)
}

func CatalogDatabaseDown(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 10.5)
	s := newRehearser(t, target)
	c := s.createCart(ctx, t)

	detail := s.rehearsing(t, abort(catalogDatabasePoint)).call(ctx, t, http.MethodPost, itemsPath(c.CartID), newItem(p.ProductID, 1)).problem(t, http.StatusServiceUnavailable)
	require.Contains(t, detail, "catalog")
	require.Empty(t, s.getCart(ctx, t, c.CartID).Items)
}

func CatalogBreakerOpens(t testing.TB, target Target) {
	ctx := bounded(t, target)
	p := putProduct(ctx, t, target, 11.25)
	s := newRehearser(t, target)
	c := s.createCart(ctx, t)

	failing := s.rehearsing(t, abort(catalogPoint(t, target)))
	for {
		detail := failing.call(ctx, t, http.MethodPost, itemsPath(c.CartID), newItem(p.ProductID, 1)).problem(t, http.StatusServiceUnavailable)
		if strings.Contains(detail, shed) {
			break
		}
	}
	require.Empty(t, s.getCart(ctx, t, c.CartID).Items)

	added := s.addItemOnceAdmitted(ctx, t, c.CartID, p.ProductID)
	require.InDelta(t, p.UnitPrice, added.UnitPrice, 1e-9)
	require.Equal(t, []item{added}, s.getCart(ctx, t, c.CartID).Items)
}

type cart struct {
	CartID string `json:"cartId"`
	Items  []item `json:"items"`
}

type item struct {
	ItemID    string  `json:"itemId"`
	CartID    string  `json:"cartId"`
	ProductID string  `json:"productId"`
	Quantity  int     `json:"quantity"`
	UnitPrice float64 `json:"unitPrice"`
}

type product struct {
	ProductID string  `json:"productId"`
	UnitPrice float64 `json:"unitPrice"`
}

type operation struct {
	method string
	path   string
	body   any
}

func changes(cartID, itemID, productID string) []operation {
	return []operation{
		{http.MethodPost, "/carts", nil},
		{http.MethodDelete, cartPath(cartID), nil},
		{http.MethodPost, itemsPath(cartID), newItem(productID, 1)},
		{http.MethodPatch, itemPath(cartID, itemID), map[string]any{"quantity": 2}},
		{http.MethodDelete, itemPath(cartID, itemID), nil},
	}
}

func read(cartID string) operation {
	return operation{http.MethodGet, cartPath(cartID), nil}
}

func cartPath(cartID string) string { return "/carts/" + cartID }

func itemsPath(cartID string) string { return cartPath(cartID) + "/items" }

func itemPath(cartID, itemID string) string { return itemsPath(cartID) + "/" + itemID }

func productPath(productID string) string { return "/products/" + productID }

func newItem(productID string, quantity int) map[string]any {
	return map[string]any{"productId": productID, "quantity": quantity}
}

func newProductID() string { return "sku-" + rand.Text() }

type shopper struct {
	target  Target
	subject string
	token   string
	baggage string
}

func newShopper(t testing.TB, target Target, scopes ...string) shopper {
	t.Helper()
	return shopper{target: target, subject: rand.Text()}.withScopes(t, target, scopes...)
}

func newRehearser(t testing.TB, target Target) shopper {
	t.Helper()
	require.NotEmpty(t, target.TrustScope, "Target.TrustScope")
	return newShopper(t, target, cartsWrite, target.TrustScope)
}

func (s shopper) rehearsing(t testing.TB, rules ...fit.Rule) shopper {
	t.Helper()
	require.NotEmpty(t, s.target.ScenarioKey, "Target.ScenarioKey")
	codec, err := jsonscenario.New(s.target.ScenarioKey)
	require.NoError(t, err, "Target.ScenarioKey")
	b, err := codec.Encode(baggage.Baggage{}, fit.Scenario{Rules: rules})
	require.NoError(t, err)
	s.baggage = b.String()
	return s
}

func (s shopper) withScopes(t testing.TB, target Target, scopes ...string) shopper {
	t.Helper()
	s.token = token(t, target, s.subject, scopes...)
	return s
}

func (s shopper) call(ctx context.Context, t testing.TB, method, path string, body any) answer {
	t.Helper()
	require.NotNil(t, s.target.Cart, "Target.Cart")
	require.NotNil(t, s.target.CartClient, "Target.CartClient")
	return call(ctx, t, s.target.CartClient, s.target.Cart, method, path, s.token, s.baggage, body)
}

func (s shopper) createCart(ctx context.Context, t testing.TB) cart {
	t.Helper()
	var c cart
	s.call(ctx, t, http.MethodPost, "/carts", nil).decode(t, http.StatusCreated, &c)
	require.NotEmpty(t, c.CartID)
	return c
}

func (s shopper) getCart(ctx context.Context, t testing.TB, cartID string) cart {
	t.Helper()
	var c cart
	s.call(ctx, t, http.MethodGet, cartPath(cartID), nil).decode(t, http.StatusOK, &c)
	require.Equal(t, cartID, c.CartID)
	return c
}

func (s shopper) addItem(ctx context.Context, t testing.TB, cartID, productID string, quantity int) item {
	t.Helper()
	var it item
	s.call(ctx, t, http.MethodPost, itemsPath(cartID), newItem(productID, quantity)).decode(t, http.StatusCreated, &it)
	require.NotEmpty(t, it.ItemID)
	return it
}

func (s shopper) addItemOnceAdmitted(ctx context.Context, t testing.TB, cartID, productID string) item {
	t.Helper()
	for {
		a := s.call(ctx, t, http.MethodPost, itemsPath(cartID), newItem(productID, 1))
		if a.code != http.StatusServiceUnavailable {
			var it item
			a.decode(t, http.StatusCreated, &it)
			require.NotEmpty(t, it.ItemID)
			return it
		}
		require.Contains(t, a.problem(t, http.StatusServiceUnavailable), shed)
		pause(ctx, t, pollEvery)
	}
}

func (s shopper) updateItem(ctx context.Context, t testing.TB, cartID, itemID string, quantity int) item {
	t.Helper()
	var it item
	s.call(ctx, t, http.MethodPatch, itemPath(cartID, itemID), map[string]any{"quantity": quantity}).decode(t, http.StatusOK, &it)
	return it
}

func putProduct(ctx context.Context, t testing.TB, target Target, price float64) product {
	t.Helper()
	id := newProductID()
	var p product
	catalogCall(ctx, t, target, catalogWriter(t, target), http.MethodPut, productPath(id), map[string]any{"unitPrice": price}).decode(t, http.StatusOK, &p)
	require.Equal(t, product{ProductID: id, UnitPrice: price}, p)
	return p
}

func catalogWriter(t testing.TB, target Target) string {
	t.Helper()
	return token(t, target, rand.Text(), catalogWrite)
}

func catalogCall(ctx context.Context, t testing.TB, target Target, token, method, path string, body any) answer {
	t.Helper()
	require.NotNil(t, target.Catalog, "Target.Catalog")
	require.NotNil(t, target.CatalogClient, "Target.CatalogClient")
	return call(ctx, t, target.CatalogClient, target.Catalog, method, path, token, "", body)
}

func catalogPoint(t testing.TB, target Target) string {
	t.Helper()
	require.NotEmpty(t, target.CatalogPoint, "Target.CatalogPoint")
	return target.CatalogPoint
}

func abort(point string) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Abort: true}}
}

func delay(point string, d time.Duration) fit.Rule {
	return fit.Rule{Point: point, Fault: fit.Fault{Delay: d}}
}

func token(t testing.TB, target Target, subject string, scopes ...string) string {
	t.Helper()
	require.NotNil(t, target.Token, "Target.Token")
	tok, err := target.Token(subject, scopes...)
	require.NoError(t, err, "signing a token for %s", subject)
	return tok
}

func bounded(t testing.TB, target Target) context.Context {
	require.Positive(t, target.Timeout, "Target.Timeout")
	ctx, cancel := context.WithTimeout(t.Context(), target.Timeout)
	t.Cleanup(cancel)
	return ctx
}

func pause(ctx context.Context, t testing.TB, d time.Duration) {
	t.Helper()
	select {
	case <-time.After(d):
	case <-ctx.Done():
		require.FailNowf(t, "timed out", "pausing for %s: %v", d, context.Cause(ctx))
	}
}

type answer struct {
	request     string
	code        int
	contentType string
	body        []byte
}

func call(ctx context.Context, t testing.TB, client *http.Client, base *url.URL, method, path, token, scenario string, body any) answer {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(b)
	}
	u := base.JoinPath(path)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if scenario != "" {
		req.Header.Set("Baggage", scenario)
	}
	resp, err := client.Do(req)
	require.NoError(t, err, "%s %s", method, u)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "%s %s", method, u)
	return answer{request: method + " " + u.String(), code: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: b}
}

func (a answer) ok(t testing.TB) {
	t.Helper()
	require.Equal(t, http.StatusOK, a.code, "%s: %s", a.request, a.body)
}

func (a answer) empty(t testing.TB, code int) {
	t.Helper()
	require.Equal(t, code, a.code, "%s: %s", a.request, a.body)
	require.Empty(t, a.body, a.request)
}

func (a answer) decode(t testing.TB, code int, v any) {
	t.Helper()
	require.Equal(t, code, a.code, "%s: %s", a.request, a.body)
	require.Equal(t, "application/json", mediaType(t, a), a.request)
	require.NoError(t, json.Unmarshal(a.body, v), "%s: %s", a.request, a.body)
}

func (a answer) problem(t testing.TB, code int) string {
	t.Helper()
	require.Equal(t, code, a.code, "%s: %s", a.request, a.body)
	require.Equal(t, "application/problem+json", mediaType(t, a), a.request)
	var p struct {
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.Unmarshal(a.body, &p), "%s: %s", a.request, a.body)
	require.Equal(t, code, p.Status, "%s: %s", a.request, a.body)
	return p.Detail
}

func mediaType(t testing.TB, a answer) string {
	t.Helper()
	m, _, err := mime.ParseMediaType(a.contentType)
	require.NoError(t, err, "%s: Content-Type %q", a.request, a.contentType)
	return m
}
