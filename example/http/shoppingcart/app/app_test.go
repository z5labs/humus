// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/circuit"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/http/shoppingcart/database"
	"github.com/z5labs/humus/example/http/shoppingcart/endpoint"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	base        = "https://localhost:8443"
	dbHost      = "db.internal:5432"
	idpHost     = "idp.internal:9443"
	catalogHost = "catalog.internal:10443"
)

type service struct {
	h       *humustest.Host
	ca      *humustest.CA
	client  *http.Client
	p       *bedrocktest.Process
	status  <-chan bedrock.ExitStatus
	idp     *idp
	stopIdP func()
	token   string
}

type fixture struct {
	h          *humustest.Host
	ca         *humustest.CA
	idpCert    *humustest.Certificate
	dbPassword string
	files      map[string][]byte
	env        map[string]string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ca := humustest.NewCA(t)
	certPEM, keyPEM := pemPair(t, ca.Issue(t, "localhost"))
	idpCert := ca.Issue(t, "idp.internal")
	clientCert, clientKey := pemPair(t, ca.Issue(t, "shoppingcart"))
	password := rand.Text()
	files := map[string][]byte{
		"tls/tls.crt":     certPEM,
		"tls/tls.key":     keyPEM,
		"database/url":    []byte("postgres://carts:" + password + "@" + dbHost + "/carts?sslmode=disable\n"),
		"idp/ca.crt":      caPEM(idpCert),
		"catalog/ca.crt":  caPEM(idpCert),
		"catalog/tls.crt": clientCert,
		"catalog/tls.key": clientKey,
	}
	return fixture{
		h:          humustest.NewHost(t, humustest.WithFiles(files)),
		ca:         ca,
		idpCert:    idpCert,
		dbPassword: password,
		files:      files,
		env: map[string]string{
			"ADDR":                     ":8443",
			"TLS_CERT_FILE":            "tls/tls.crt",
			"TLS_KEY_FILE":             "tls/tls.key",
			"DATABASE_URL_FILE":        "database/url",
			"CATALOG_URL":              "https://" + catalogHost,
			"CATALOG_CA_FILE":          "catalog/ca.crt",
			"CATALOG_CLIENT_CERT_FILE": "catalog/tls.crt",
			"CATALOG_CLIENT_KEY_FILE":  "catalog/tls.key",
			"JWKS_URL":                 "https://" + idpHost + jwksPath,
			"JWKS_CA_FILE":             "idp/ca.crt",
			"JWT_ISSUER":               issuer,
			"JWT_AUDIENCE":             audience,
		},
	}
}

func (f fixture) exit(t *testing.T) bedrock.ExitStatus {
	t.Helper()
	_, status := httpservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(f.env),
	)
	return <-status
}

func start(t *testing.T, init func(context.Context, *humus.Setup) ([]humus.Service, error)) *service {
	t.Helper()
	f := newFixture(t)

	provider := newIdP(t)
	stopIdP := provider.serve(t, f.h.Host(), idpHost, f.idpCert)
	<-f.h.Listening(idpHost)

	p, status := httpservertest.Start(t, init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(f.env),
	)
	select {
	case <-f.h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("the cart exited %d: %v", st.Code(), st.Cause())
	}
	return &service{
		h:       f.h,
		ca:      f.ca,
		client:  httpservertest.NewClient(f.h, f.ca.Pool()),
		p:       p,
		status:  status,
		idp:     provider,
		stopIdP: stopIdP,
		token:   provider.token(t, shopper, endpoint.ScopeWrite),
	}
}

func (s *service) as(token string) *service {
	c := *s
	c.token = token
	return &c
}

func (s *service) stop(t *testing.T) {
	t.Helper()
	<-s.p.Notified(syscall.SIGTERM)
	s.p.Signal(syscall.SIGTERM)
	st := <-s.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	s.stopIdP()
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

func (s *service) do(t *testing.T, method, path, contentType, body string) response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, r)
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 2, resp.ProtoMajor)
	return response{code: resp.StatusCode, header: resp.Header, body: b}
}

func decode[T any](t *testing.T, r response, code int, mediaType string) T {
	t.Helper()
	require.Equal(t, code, r.code, "%s", r.body)
	require.Equal(t, mediaType, r.header.Get("Content-Type"))
	var v T
	require.NoError(t, json.Unmarshal(r.body, &v), "%s", r.body)
	return v
}

type problemDetail struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func isProblem(t *testing.T, r response, code int) problemDetail {
	t.Helper()
	d := decode[problemDetail](t, r, code, "application/problem+json")
	require.Equal(t, code, d.Status)
	require.Equal(t, http.StatusText(code), d.Title)
	return d
}

func TestInit_serves_every_operation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))

		created := decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json")
		require.NotEqual(t, uuid.Nil, created.CartID)
		require.Empty(t, created.Items)
		cart := "/carts/" + created.CartID.String()

		r := s.do(t, http.MethodGet, cart, "", "")
		require.Contains(t, string(r.body), `"items":[]`)
		require.Equal(t, created.CartID, decode[endpoint.Cart](t, r, http.StatusOK, "application/json").CartID)

		added := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":2}`),
			http.StatusCreated, "application/json")
		require.Equal(t, created.CartID, added.CartID)
		require.Equal(t, "sku-1", added.ProductID)
		require.Equal(t, 2, added.Quantity)
		require.InDelta(t, 9.99, added.UnitPrice, 1e-9)
		item := cart + "/items/" + added.ItemID.String()

		updated := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPatch, item, "application/json", `{"quantity":5}`),
			http.StatusOK, "application/json")
		require.Equal(t, added.ItemID, updated.ItemID)
		require.Equal(t, 5, updated.Quantity)

		got := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Len(t, got.Items, 1)
		require.Equal(t, 5, got.Items[0].Quantity)

		r = s.do(t, http.MethodDelete, item, "", "")
		require.Equal(t, http.StatusNoContent, r.code)
		require.Empty(t, r.body)
		d := isProblem(t, s.do(t, http.MethodDelete, item, "", ""), http.StatusNotFound)
		require.Equal(t, "Cart "+created.CartID.String()+" has no item "+added.ItemID.String()+".", d.Detail)

		r = s.do(t, http.MethodDelete, cart, "", "")
		require.Equal(t, http.StatusNoContent, r.code)
		require.Empty(t, r.body)
		gone := "There is no cart " + created.CartID.String() + "."
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodGet, cart, "", ""), http.StatusNotFound).Detail)
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodDelete, cart, "", ""), http.StatusNotFound).Detail)
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":1}`), http.StatusNotFound).Detail)
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodPatch, item, "application/json", `{"quantity":1}`), http.StatusNotFound).Detail)
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodDelete, item, "", ""), http.StatusNotFound).Detail)

		s.stop(t)
	})
}

func TestInit_answers_a_body_it_wont_accept_with_422(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

		for body, detail := range map[string]string{
			`{"quantity":1}`:                     "productId is required",
			`{"productId":"sku-1","quantity":0}`: "quantity must be at least 1",
		} {
			d := isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", body), http.StatusUnprocessableEntity)
			require.Equal(t, detail, d.Detail, body)
		}

		added := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":1}`),
			http.StatusCreated, "application/json")
		d := isProblem(t, s.do(t, http.MethodPatch, cart+"/items/"+added.ItemID.String(), "application/json", `{"quantity":0}`), http.StatusUnprocessableEntity)
		require.Equal(t, "quantity must be at least 1", d.Detail)

		s.stop(t)
	})
}

func TestInit_answers_a_request_it_rejects_with_a_problem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

		d := isProblem(t, s.do(t, http.MethodGet, "/carts/not-a-uuid", "", ""), http.StatusBadRequest)
		require.Contains(t, d.Detail, "cartId")

		isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":`), http.StatusBadRequest)
		isProblem(t, s.do(t, http.MethodPost, cart+"/items", "text/plain", `{"productId":"sku-1","quantity":1}`), http.StatusUnsupportedMediaType)

		s.stop(t)
	})
}

func TestInit_answers_unmatched_requests_with_a_problem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))

		isProblem(t, s.do(t, http.MethodGet, "/orders", "", ""), http.StatusNotFound)

		r := s.do(t, http.MethodPut, "/carts", "", "")
		isProblem(t, r, http.StatusMethodNotAllowed)
		require.Equal(t, http.MethodPost, r.header.Get("Allow"))

		s.stop(t)
	})
}

func TestInit_answers_a_request_without_a_valid_token_with_401(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()
		item := cart + "/items/" + uuid.NewString()

		anonymous := s.as("")
		for _, op := range []struct{ method, path, body string }{
			{http.MethodPost, "/carts", ""},
			{http.MethodGet, cart, ""},
			{http.MethodDelete, cart, ""},
			{http.MethodPost, cart + "/items", `{"productId":"sku-1","quantity":1}`},
			{http.MethodPatch, item, `{"quantity":1}`},
			{http.MethodDelete, item, ""},
		} {
			r := anonymous.do(t, op.method, op.path, "application/json", op.body)
			isProblem(t, r, http.StatusUnauthorized)
			require.Equal(t, "Bearer", r.header.Get("WWW-Authenticate"), "%s %s", op.method, op.path)
		}

		r := s.as(newIdP(t).token(t, shopper, endpoint.ScopeWrite)).do(t, http.MethodGet, cart, "", "")
		isProblem(t, r, http.StatusUnauthorized)
		require.Equal(t, `Bearer error="invalid_token"`, r.header.Get("WWW-Authenticate"))

		s.stop(t)
	})
}

func TestInit_needs_carts_write_to_change_a_cart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		created := decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json")
		cart := "/carts/" + created.CartID.String()
		added := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":1}`),
			http.StatusCreated, "application/json")
		item := cart + "/items/" + added.ItemID.String()

		reader := s.as(s.idp.token(t, shopper, "carts:read"))
		got := decode[endpoint.Cart](t, reader.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Equal(t, created.CartID, got.CartID)
		require.Len(t, got.Items, 1)

		for _, op := range []struct{ method, path, body string }{
			{http.MethodPost, "/carts", ""},
			{http.MethodDelete, cart, ""},
			{http.MethodPost, cart + "/items", `{"productId":"sku-2","quantity":1}`},
			{http.MethodPatch, item, `{"quantity":2}`},
			{http.MethodDelete, item, ""},
		} {
			r := reader.do(t, op.method, op.path, "application/json", op.body)
			isProblem(t, r, http.StatusForbidden)
			require.Equal(t, `Bearer error="insufficient_scope", scope="carts:write"`, r.header.Get("WWW-Authenticate"), "%s %s", op.method, op.path)
		}

		got = decode[endpoint.Cart](t, reader.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Len(t, got.Items, 1)
		require.Equal(t, 1, got.Items[0].Quantity)

		writer := s.as(s.idp.token(t, shopper, "carts:read", endpoint.ScopeWrite))
		updated := decode[endpoint.CartItem](t,
			writer.do(t, http.MethodPatch, item, "application/json", `{"quantity":2}`),
			http.StatusOK, "application/json")
		require.Equal(t, 2, updated.Quantity)
		r := writer.do(t, http.MethodDelete, cart, "", "")
		require.Equal(t, http.StatusNoContent, r.code)

		s.stop(t)
	})
}

func TestInit_keeps_a_cart_to_its_owner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		created := decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json")
		cart := "/carts/" + created.CartID.String()
		added := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":1}`),
			http.StatusCreated, "application/json")
		item := cart + "/items/" + added.ItemID.String()
		before := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")

		other := s.as(s.idp.token(t, "another-shopper", endpoint.ScopeWrite))
		decode[endpoint.Cart](t, other.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json")
		gone := "There is no cart " + created.CartID.String() + "."
		for _, op := range []struct{ method, path, body string }{
			{http.MethodGet, cart, ""},
			{http.MethodDelete, cart, ""},
			{http.MethodPost, cart + "/items", `{"productId":"sku-2","quantity":1}`},
			{http.MethodPatch, item, `{"quantity":2}`},
			{http.MethodDelete, item, ""},
		} {
			d := isProblem(t, other.do(t, op.method, op.path, "application/json", op.body), http.StatusNotFound)
			require.Equal(t, gone, d.Detail, "%s %s", op.method, op.path)
		}

		after := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Equal(t, before, after)

		s.stop(t)
	})
}

func TestInit_answers_a_token_without_a_subject_with_401(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()
		item := cart + "/items/" + uuid.NewString()

		nobody := s.as(s.idp.token(t, "", endpoint.ScopeWrite))
		for _, op := range []struct{ method, path, body string }{
			{http.MethodPost, "/carts", ""},
			{http.MethodGet, cart, ""},
			{http.MethodDelete, cart, ""},
			{http.MethodPost, cart + "/items", `{"productId":"sku-1","quantity":1}`},
			{http.MethodPatch, item, `{"quantity":1}`},
			{http.MethodDelete, item, ""},
		} {
			r := nobody.do(t, op.method, op.path, "application/json", op.body)
			isProblem(t, r, http.StatusUnauthorized)
			require.Equal(t, `Bearer error="invalid_token"`, r.header.Get("WWW-Authenticate"), "%s %s", op.method, op.path)
		}

		decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")

		s.stop(t)
	})
}

func TestInit_answers_a_token_it_has_no_keys_for_with_503(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))

		s.idp.setDown(true)
		r := s.do(t, http.MethodPost, "/carts", "", "")
		d := isProblem(t, r, http.StatusServiceUnavailable)
		require.Equal(t, "The server can't verify tokens right now. Try again later.", d.Detail)
		require.Empty(t, r.header.Get("WWW-Authenticate"))
		require.NotContains(t, string(r.body), idpHost)

		s.idp.setDown(false)
		time.Sleep(time.Minute)
		decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json")

		s.stop(t)
	})
}

type failingStore struct{ err error }

func (f failingStore) CreateCart(context.Context, string) (endpoint.Cart, error) {
	return endpoint.Cart{}, f.err
}

func (f failingStore) GetCart(context.Context, string, uuid.UUID) (endpoint.Cart, error) {
	return endpoint.Cart{}, f.err
}

func (f failingStore) DeleteCart(context.Context, string, uuid.UUID) error { return f.err }

func (f failingStore) AddItem(context.Context, string, uuid.UUID, endpoint.NewItem) (endpoint.CartItem, error) {
	return endpoint.CartItem{}, f.err
}

func (f failingStore) UpdateItem(context.Context, string, uuid.UUID, uuid.UUID, int) (endpoint.CartItem, error) {
	return endpoint.CartItem{}, f.err
}

func (f failingStore) RemoveItem(context.Context, string, uuid.UUID, uuid.UUID) error { return f.err }

func TestInit_answers_a_failure_without_its_text(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		secret := rand.Text()
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, func(context.Context, *humus.Setup, *pgxpool.Config) (endpoint.Store, func(context.Context) error, error) {
				return failingStore{err: errors.New("the database said " + secret)}, func(context.Context) error { return nil }, nil
			}, fakePrices().open)
		})

		r := s.do(t, http.MethodPost, "/carts", "", "")
		d := isProblem(t, r, http.StatusInternalServerError)
		require.Equal(t, endpoint.InternalError(nil).Detail, d.Detail)
		require.NotContains(t, string(r.body), secret)

		s.stop(t)
	})
}

func TestInit_keeps_the_catalogs_price_with_an_item(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		catalog := fakePrices()
		s := start(t, onFakes(catalog))
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

		r := s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":2,"unitPrice":0.01}`)
		first := decode[endpoint.CartItem](t, r, http.StatusCreated, "application/json")
		require.InDelta(t, 9.99, first.UnitPrice, 1e-9)

		catalog.set("sku-1", 12.5)
		got := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Len(t, got.Items, 1)
		require.InDelta(t, 9.99, got.Items[0].UnitPrice, 1e-9)

		second := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":1}`),
			http.StatusCreated, "application/json")
		require.InDelta(t, 12.5, second.UnitPrice, 1e-9)

		got = decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Len(t, got.Items, 2)
		require.InDelta(t, 9.99, got.Items[0].UnitPrice, 1e-9)
		require.InDelta(t, 12.5, got.Items[1].UnitPrice, 1e-9)

		s.stop(t)
	})
}

func TestInit_answers_a_product_the_catalog_doesnt_have_with_422(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

		d := isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-404","quantity":1}`), http.StatusUnprocessableEntity)
		require.Equal(t, "There is no product sku-404 in the catalog.", d.Detail)

		got := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Empty(t, got.Items)

		s.stop(t)
	})
}

func TestInit_answers_a_catalog_that_is_down_with_503(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		detail string
	}{
		"the catalog failed": {
			err:    fmt.Errorf("getting product sku-1: %w: the catalog answered 503", endpoint.ErrCatalogUnavailable),
			detail: "The cart can't reach the catalog right now. Try again later.",
		},
		"the breaker shed the call": {
			err:    fmt.Errorf("getting product sku-1: %w: %w", endpoint.ErrCatalogUnavailable, &url.Error{Op: "Get", URL: "https://" + catalogHost + "/products/sku-1", Err: circuit.ErrRejected}),
			detail: "The cart isn't calling the catalog right now, since it has been failing. Try again later.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				catalog := fakePrices()
				s := start(t, onFakes(catalog))
				cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

				catalog.fail(tc.err)
				r := s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":1}`)
				d := isProblem(t, r, http.StatusServiceUnavailable)
				require.Equal(t, tc.detail, d.Detail)
				require.NotContains(t, string(r.body), catalogHost)

				got := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
				require.Empty(t, got.Items)

				s.stop(t)
			})
		})
	}
}

func TestInit_answers_a_database_that_is_down_with_503(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, func(context.Context, *humus.Setup, *pgxpool.Config) (endpoint.Store, func(context.Context) error, error) {
				return failingStore{err: fmt.Errorf("creating a cart: %w: dial tcp %s: connection refused", database.ErrUnavailable, dbHost)}, func(context.Context) error { return nil }, nil
			}, fakePrices().open)
		})
		cart := "/carts/" + uuid.NewString()
		item := cart + "/items/" + uuid.NewString()

		for _, op := range []struct{ method, path, body string }{
			{http.MethodPost, "/carts", ""},
			{http.MethodGet, cart, ""},
			{http.MethodDelete, cart, ""},
			{http.MethodPost, cart + "/items", `{"productId":"sku-1","quantity":1}`},
			{http.MethodPatch, item, `{"quantity":1}`},
			{http.MethodDelete, item, ""},
		} {
			r := s.do(t, op.method, op.path, "application/json", op.body)
			d := isProblem(t, r, http.StatusServiceUnavailable)
			require.Equal(t, "The cart can't reach its database right now. Try again later.", d.Detail, "%s %s", op.method, op.path)
			require.NotContains(t, string(r.body), dbHost)
		}

		s.stop(t)
	})
}

func TestInit_answers_a_body_over_64_KiB_with_413(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

		body := `{"productId":"sku-1","quantity":1,"padding":"` + strings.Repeat("x", 64<<10) + `"}`
		isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", body), http.StatusRequestEntityTooLarge)

		got := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Empty(t, got.Items)

		s.stop(t)
	})
}

func TestInit_gets_a_price_from_the_catalog_with_its_client_certificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, inMemory, openCatalog)
		})
		catalog := &catalogServer{prices: map[string]float64{"sku-1": 12.5}}
		stopCatalog := catalog.serve(t, s.h.Host(), catalogHost, s.ca.Issue(t, "catalog.internal"), s.ca.Pool())
		<-s.h.Listening(catalogHost)

		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()
		added := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":2}`),
			http.StatusCreated, "application/json")
		require.Equal(t, "sku-1", added.ProductID)
		require.InDelta(t, 12.5, added.UnitPrice, 1e-9)

		d := isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-404","quantity":1}`), http.StatusUnprocessableEntity)
		require.Equal(t, "There is no product sku-404 in the catalog.", d.Detail)

		require.Equal(t, []string{"shoppingcart", "shoppingcart"}, catalog.clients())

		s.stop(t)
		stopCatalog()
	})
}

var catalogSettings = []string{"CATALOG_URL", "CATALOG_CA_FILE", "CATALOG_CLIENT_CERT_FILE", "CATALOG_CLIENT_KEY_FILE"}

func TestInit_without_a_catalog_setting_exits_78(t *testing.T) {
	for _, name := range catalogSettings {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				delete(f.env, name)
				_, status := httpservertest.Start(t, Init,
					humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
					humustest.WithEnv(f.env),
				)

				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+name+": not found")
				for _, other := range catalogSettings {
					if other != name {
						require.NotContains(t, st.Cause().Error(), "config "+other+":")
					}
				}
			})
		})
	}
}

func TestInit_with_a_bad_catalog_setting_exits_78(t *testing.T) {
	for _, tc := range []struct{ name, value, cause string }{
		{"CATALOG_URL", "http://" + catalogHost, "must be an https URL"},
		{"CATALOG_URL", "catalog.internal", "absolute"},
		{"CATALOG_CA_FILE", "catalog/tls.key", "no PEM certificate in the file"},
		{"CATALOG_CA_FILE", "missing.pem", "missing.pem"},
		{"CATALOG_CLIENT_KEY_FILE", "catalog/ca.crt", "catalog/tls.crt"},
		{"CATALOG_TIMEOUT", "0s", "positive"},
		{"CATALOG_ATTEMPT_TIMEOUT", "5", "missing unit"},
		{"CATALOG_MAX_ATTEMPTS", "0", "at least 1"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				f.env[tc.name] = tc.value
				_, status := httpservertest.Start(t, Init,
					humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
					humustest.WithEnv(f.env),
				)

				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+tc.name+": ")
				require.ErrorContains(t, st.Cause(), tc.cause)
			})
		})
	}
}

var secretSettings = []string{"TLS_CERT_FILE", "TLS_KEY_FILE", "DATABASE_URL_FILE"}

func TestInit_without_a_secret_setting_exits_78(t *testing.T) {
	for _, name := range secretSettings {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				delete(f.env, name)

				st := f.exit(t)
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+name+": not found")
				for _, other := range secretSettings {
					if other != name {
						require.NotContains(t, st.Cause().Error(), "config "+other+":")
					}
				}
			})
		})
	}
}

func TestInit_with_a_bad_secret_file_exits_78(t *testing.T) {
	for _, tc := range []struct{ name, value, file, cause string }{
		{name: "TLS_CERT_FILE", value: "missing.crt", cause: "missing.crt"},
		{name: "TLS_CERT_FILE", value: "tls/tls.key", cause: "no PEM certificate in the file"},
		{name: "TLS_KEY_FILE", value: "missing.key", cause: "missing.key"},
		{name: "TLS_KEY_FILE", value: "catalog/tls.key", cause: "tls/tls.crt"},
		{name: "TLS_KEY_FILE", value: "tls/tls.crt", cause: "tls/tls.crt"},
		{name: "DATABASE_URL_FILE", value: "missing.url", cause: "missing.url"},
		{name: "DATABASE_URL_FILE", value: "bad.url", file: "postgres://carts:" + rand.Text() + "@db.internal:port/carts", cause: "must hold a PostgreSQL URL"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				if tc.file != "" {
					f.h = humustest.NewHost(t, humustest.WithFiles(f.files), humustest.WithFiles(map[string][]byte{tc.value: []byte(tc.file)}))
				}
				f.env[tc.name] = tc.value

				st := f.exit(t)
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+tc.name+": ")
				require.ErrorContains(t, st.Cause(), tc.cause)
				require.NotContains(t, st.Cause().Error(), f.dbPassword)
				if tc.file != "" {
					require.NotContains(t, st.Cause().Error(), tc.file)
				}
			})
		})
	}
}

func TestInit_reads_its_files_only_through_the_Host(t *testing.T) {
	for name, file := range map[string]string{
		"TLS_CERT_FILE":            "tls/tls.crt",
		"TLS_KEY_FILE":             "tls/tls.key",
		"DATABASE_URL_FILE":        "database/url",
		"CATALOG_CA_FILE":          "catalog/ca.crt",
		"CATALOG_CLIENT_CERT_FILE": "catalog/tls.crt",
		"CATALOG_CLIENT_KEY_FILE":  "catalog/tls.key",
		"JWKS_CA_FILE":             "idp/ca.crt",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				outside := filepath.Join(t.TempDir(), filepath.Base(file))
				require.NoError(t, os.WriteFile(outside, f.files[file], 0o600))
				f.env[name] = outside

				st := f.exit(t)
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+name+": ")
			})
		})
	}
}

func TestInit_reads_the_database_URL_from_its_file(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var cfg *pgxpool.Config
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, func(ctx context.Context, setup *humus.Setup, c *pgxpool.Config) (endpoint.Store, func(context.Context) error, error) {
				cfg = c
				return inMemory(ctx, setup, c)
			}, fakePrices().open)
		})

		require.Equal(t, "db.internal", cfg.ConnConfig.Host)
		require.EqualValues(t, 5432, cfg.ConnConfig.Port)
		require.Equal(t, "carts", cfg.ConnConfig.User)
		require.NotEmpty(t, cfg.ConnConfig.Password)
		require.Equal(t, "carts", cfg.ConnConfig.Database)

		s.stop(t)
	})
}

type fakeDatabase struct {
	mu    sync.Mutex
	down  bool
	pings int
}

func (db *fakeDatabase) open(ctx context.Context, s *humus.Setup, cfg *pgxpool.Config) (endpoint.Store, func(context.Context) error, error) {
	store, _, err := inMemory(ctx, s, cfg)
	return store, db.ping, err
}

func (db *fakeDatabase) ping(context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.pings++
	if db.down {
		return errors.New("the database is down")
	}
	return nil
}

func (db *fakeDatabase) setDown(down bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.down = down
}

func (db *fakeDatabase) pinged() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.pings
}

func (s *service) probe(t *testing.T, path string) response {
	t.Helper()
	return s.as("").do(t, http.MethodGet, path, "", "")
}

func TestInit_is_ready_once_the_database_and_the_catalog_answer_and_stays_ready(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := &fakeDatabase{down: true}
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, db.open, openCatalog)
		})
		catalog := &catalogServer{down: true}
		stopCatalog := catalog.serve(t, s.h.Host(), catalogHost, s.ca.Issue(t, "catalog.internal"), s.ca.Pool())
		<-s.h.Listening(catalogHost)

		for range 2 {
			r := s.probe(t, "/health/readiness")
			require.Equal(t, http.StatusServiceUnavailable, r.code, "%s", r.body)
			r = s.probe(t, "/health/liveness")
			require.Equal(t, http.StatusOK, r.code, "%s", r.body)
		}
		require.Equal(t, 2, db.pinged())
		require.Zero(t, catalog.probed())

		db.setDown(false)
		r := s.probe(t, "/health/readiness")
		require.Equal(t, http.StatusServiceUnavailable, r.code, "%s", r.body)
		require.Equal(t, 3, db.pinged())
		require.NotZero(t, catalog.probed())

		catalog.setDown(false)
		time.Sleep(time.Minute)
		r = s.probe(t, "/health/readiness")
		require.Equal(t, http.StatusOK, r.code, "%s", r.body)
		require.Equal(t, 3, db.pinged())
		probed := catalog.probed()

		db.setDown(true)
		r = s.probe(t, "/health/readiness")
		require.Equal(t, http.StatusOK, r.code, "%s", r.body)
		catalog.setDown(true)
		r = s.probe(t, "/health/readiness")
		require.Equal(t, http.StatusOK, r.code, "%s", r.body)
		require.Equal(t, 3, db.pinged())
		require.Equal(t, probed, catalog.probed())

		s.stop(t)
		stopCatalog()
	})
}

func TestInit_stays_unready_on_a_catalog_it_cant_trust_or_that_rejects_its_certificate(t *testing.T) {
	for name, tc := range map[string]struct {
		cert      func(s *service) *humustest.Certificate
		clientCAs func(s *service) *x509.CertPool
	}{
		"a certificate the CA didn't sign": {
			cert:      func(*service) *humustest.Certificate { return humustest.NewCA(t).Issue(t, "catalog.internal") },
			clientCAs: func(s *service) *x509.CertPool { return s.ca.Pool() },
		},
		"a client certificate it rejects": {
			cert:      func(s *service) *humustest.Certificate { return s.ca.Issue(t, "catalog.internal") },
			clientCAs: func(*service) *x509.CertPool { return humustest.NewCA(t).Pool() },
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
					return initWith(ctx, setup, (&fakeDatabase{}).open, openCatalog)
				})
				catalog := &catalogServer{}
				stopCatalog := catalog.serve(t, s.h.Host(), catalogHost, tc.cert(s), tc.clientCAs(s))
				<-s.h.Listening(catalogHost)

				for range 2 {
					r := s.probe(t, "/health/readiness")
					require.Equal(t, http.StatusServiceUnavailable, r.code, "%s", r.body)
					time.Sleep(time.Minute)
				}
				require.Zero(t, catalog.probed())

				s.stop(t)
				stopCatalog()
			})
		})
	}
}

func TestInit_answers_its_probes_without_a_token(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))

		isProblem(t, s.probe(t, "/carts/"+uuid.NewString()), http.StatusUnauthorized)
		for _, path := range []string{"/health/liveness", "/health/readiness"} {
			r := s.probe(t, path)
			require.Equal(t, http.StatusOK, r.code, "%s: %s", path, r.body)
			require.Equal(t, "OK", strings.TrimSpace(string(r.body)), path)
		}

		s.stop(t)
	})
}

func TestInit_dials_Postgres_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, Init)

		db, err := s.h.Listen("tcp", dbHost)
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Go(func() {
			conn, err := db.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		})

		r := s.do(t, http.MethodGet, "/carts/"+uuid.NewString(), "", "")
		d := isProblem(t, r, http.StatusServiceUnavailable)
		require.Equal(t, "The cart can't reach its database right now. Try again later.", d.Detail)
		require.NotContains(t, string(r.body), dbHost)
		wg.Wait()

		require.NoError(t, db.Close())
		s.stop(t)
	})
}

func TestInit_without_its_settings_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := httpservertest.Start(t, Init,
			humustest.WithApp(humus.WithHost(humustest.NewHost(t).Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		for _, name := range []string{"ADDR", "TLS_CERT_FILE", "DATABASE_URL_FILE", "CATALOG_URL", "CATALOG_CA_FILE", "CATALOG_CLIENT_CERT_FILE", "JWKS_URL", "JWT_ISSUER", "JWT_AUDIENCE"} {
			require.ErrorContains(t, st.Cause(), "config "+name+": not found")
		}
	})
}

func TestInit_with_a_bad_key_set_URL_exits_78(t *testing.T) {
	for name, jwksURL := range map[string]string{
		"plain text": "http://idp.internal:9443" + jwksPath,
		"relative":   jwksPath,
		"user info":  "https://shoppingcart:" + rand.Text() + "@idp.internal:9443" + jwksPath,
		"a fragment": "https://idp.internal:9443" + jwksPath + "#keys",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, status := httpservertest.Start(t, Init,
					humustest.WithApp(humus.WithHost(humustest.NewHost(t).Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
					humustest.WithEnv(map[string]string{"JWKS_URL": jwksURL}),
				)

				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config JWKS_URL: must")
				require.NotContains(t, st.Cause().Error(), jwksURL)
			})
		})
	}
}

func TestInit_serves_its_document(t *testing.T) {
	var doc []byte
	synctest.Test(t, func(t *testing.T) {
		s := start(t, onFakes(fakePrices()))
		r := s.as("").do(t, http.MethodGet, "/openapi.json", "", "")
		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, "application/json", r.header.Get("Content-Type"))
		doc = r.body
		s.stop(t)
	})

	t.Run("as the golden file", func(t *testing.T) {
		var indented bytes.Buffer
		require.NoError(t, json.Indent(&indented, doc, "", "  "))
		indented.WriteByte('\n')
		want, err := os.ReadFile(filepath.Join("testdata", "openapi.json"))
		require.NoError(t, err)
		require.Equal(t, string(want), indented.String())
	})

	t.Run("valid against OpenAPI 3.2", func(t *testing.T) {
		require.NoError(t, validate(doc))
	})
}

var oas32 = filepath.Join("..", "..", "..", "..", "openapi", "v3", "testdata", "oas3.2")

const schemaBaseID = "https://spec.openapis.org/oas/3.2/schema-base/2025-09-17"

func validate(doc []byte) error {
	files, err := filepath.Glob(filepath.Join(oas32, "*.json"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no schema in " + oas32)
	}
	c := jsonschema.NewCompiler()
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		res, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return err
		}
		id, _ := res.(map[string]any)["$id"].(string)
		if err := c.AddResource(id, res); err != nil {
			return err
		}
	}
	schema, err := c.Compile(schemaBaseID)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return err
	}
	return schema.Validate(inst)
}
