// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

// The tests in this file run the whole service, with httpservertest.Start,
// inside synctest bubbles, on humustest Hosts, with certificates from a CA
// generated for each test. All but one serve an in-memory Store, which
// initWith swaps in for Postgres.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/endpoint"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// base is where the service listens on its humustest Host.
const base = "https://localhost:8443"

// dbHost is the database's address in the tests' DATABASE_URL.
const dbHost = "db.internal:5432"

// service is the whole service, running on a humustest Host.
type service struct {
	h      *humustest.Host
	client *http.Client
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

// start runs initWith on open, with every setting it needs, on a new
// humustest Host, and waits for it to listen. Call it inside the bubble that
// uses it.
func start(t *testing.T, init func(context.Context, *humus.Setup) ([]humus.Service, error), hostOpts ...humustest.HostOption) *service {
	t.Helper()
	ca := humustest.NewCA(t)
	bundle, password := ca.Issue(t, "localhost").PKCS12(t)
	h := humustest.NewHost(t, append(hostOpts, humustest.WithFiles(map[string][]byte{"tls.p12": bundle}))...)

	p, status := httpservertest.Start(t, init,
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{
			"ADDR":                ":8443",
			"TLS_PKCS12_FILE":     "tls.p12",
			"TLS_PKCS12_PASSWORD": password,
			// Every field pgx uses is in the URL, so it reads nothing from
			// the real environment or ~/.pgpass. The password is new for
			// each test.
			"DATABASE_URL": "postgres://carts:" + rand.Text() + "@" + dbHost + "/carts?sslmode=disable",
		}),
	)
	<-h.Listening("localhost:8443")
	return &service{h: h, client: httpservertest.NewClient(h, ca.Pool()), p: p, status: status}
}

// stop sends the service SIGTERM and checks that it exits 0.
func (s *service) stop(t *testing.T) {
	t.Helper()
	<-s.p.Notified(syscall.SIGTERM)
	s.p.Signal(syscall.SIGTERM)
	st := <-s.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

// response is what the client got back.
type response struct {
	code   int
	header http.Header
	body   []byte
}

// do sends method path, with body as contentType when body isn't "", and
// returns the response.
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
	resp, err := s.client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 2, resp.ProtoMajor)
	return response{code: resp.StatusCode, header: resp.Header, body: b}
}

// decode checks r's status and media type, and decodes its body as T.
func decode[T any](t *testing.T, r response, code int, mediaType string) T {
	t.Helper()
	require.Equal(t, code, r.code, "%s", r.body)
	require.Equal(t, mediaType, r.header.Get("Content-Type"))
	var v T
	require.NoError(t, json.Unmarshal(r.body, &v), "%s", r.body)
	return v
}

// problemDetail is a problem detail as the client reads it.
type problemDetail struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// isProblem checks that r is a problem detail at code, and returns it.
func isProblem(t *testing.T, r response, code int) problemDetail {
	t.Helper()
	d := decode[problemDetail](t, r, code, "application/problem+json")
	require.Equal(t, code, d.Status)
	require.Equal(t, http.StatusText(code), d.Title)
	return d
}

func TestInit_serves_every_operation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, inMemory)
		})

		// createCart
		created := decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json")
		require.NotEqual(t, uuid.Nil, created.CartID)
		require.Empty(t, created.Items)
		cart := "/carts/" + created.CartID.String()

		// getCart, of an empty cart: its items are [], never null.
		r := s.do(t, http.MethodGet, cart, "", "")
		require.Contains(t, string(r.body), `"items":[]`)
		require.Equal(t, created.CartID, decode[endpoint.Cart](t, r, http.StatusOK, "application/json").CartID)

		// addCartItem
		added := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":2,"unitPrice":9.99}`),
			http.StatusCreated, "application/json")
		require.Equal(t, created.CartID, added.CartID)
		require.Equal(t, "sku-1", added.ProductID)
		require.Equal(t, 2, added.Quantity)
		require.InDelta(t, 9.99, added.UnitPrice, 1e-9)
		item := cart + "/items/" + added.ItemID.String()

		// updateCartItem
		updated := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPatch, item, "application/json", `{"quantity":5}`),
			http.StatusOK, "application/json")
		require.Equal(t, added.ItemID, updated.ItemID)
		require.Equal(t, 5, updated.Quantity)

		got := decode[endpoint.Cart](t, s.do(t, http.MethodGet, cart, "", ""), http.StatusOK, "application/json")
		require.Len(t, got.Items, 1)
		require.Equal(t, 5, got.Items[0].Quantity)

		// removeCartItem, then again: the item is gone.
		r = s.do(t, http.MethodDelete, item, "", "")
		require.Equal(t, http.StatusNoContent, r.code)
		require.Empty(t, r.body)
		d := isProblem(t, s.do(t, http.MethodDelete, item, "", ""), http.StatusNotFound)
		require.Equal(t, "Cart "+created.CartID.String()+" has no item "+added.ItemID.String()+".", d.Detail)

		// deleteCart, then every operation on it is a 404.
		r = s.do(t, http.MethodDelete, cart, "", "")
		require.Equal(t, http.StatusNoContent, r.code)
		require.Empty(t, r.body)
		gone := "There is no cart " + created.CartID.String() + "."
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodGet, cart, "", ""), http.StatusNotFound).Detail)
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodDelete, cart, "", ""), http.StatusNotFound).Detail)
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":1,"unitPrice":1}`), http.StatusNotFound).Detail)
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodPatch, item, "application/json", `{"quantity":1}`), http.StatusNotFound).Detail)
		require.Equal(t, gone, isProblem(t, s.do(t, http.MethodDelete, item, "", ""), http.StatusNotFound).Detail)

		s.stop(t)
	})
}

func TestInit_answers_a_body_it_wont_accept_with_422(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, inMemory)
		})
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

		for body, detail := range map[string]string{
			`{"quantity":1,"unitPrice":1}`:                      "productId is required",
			`{"productId":"sku-1","quantity":0,"unitPrice":1}`:  "quantity must be at least 1",
			`{"productId":"sku-1","quantity":1,"unitPrice":-1}`: "unitPrice must not be negative",
		} {
			d := isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", body), http.StatusUnprocessableEntity)
			require.Equal(t, detail, d.Detail, body)
		}

		added := decode[endpoint.CartItem](t,
			s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":"sku-1","quantity":1,"unitPrice":1}`),
			http.StatusCreated, "application/json")
		d := isProblem(t, s.do(t, http.MethodPatch, cart+"/items/"+added.ItemID.String(), "application/json", `{"quantity":0}`), http.StatusUnprocessableEntity)
		require.Equal(t, "quantity must be at least 1", d.Detail)

		s.stop(t)
	})
}

func TestInit_answers_a_request_it_rejects_with_a_problem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, inMemory)
		})
		cart := "/carts/" + decode[endpoint.Cart](t, s.do(t, http.MethodPost, "/carts", "", ""), http.StatusCreated, "application/json").CartID.String()

		// A cartId that isn't a UUID breaks its Pattern.
		d := isProblem(t, s.do(t, http.MethodGet, "/carts/not-a-uuid", "", ""), http.StatusBadRequest)
		require.Contains(t, d.Detail, "cartId")

		// A body that isn't JSON, and one that isn't application/json.
		isProblem(t, s.do(t, http.MethodPost, cart+"/items", "application/json", `{"productId":`), http.StatusBadRequest)
		isProblem(t, s.do(t, http.MethodPost, cart+"/items", "text/plain", `{"productId":"sku-1","quantity":1,"unitPrice":1}`), http.StatusUnsupportedMediaType)

		s.stop(t)
	})
}

func TestInit_answers_unmatched_requests_with_a_problem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, inMemory)
		})

		isProblem(t, s.do(t, http.MethodGet, "/orders", "", ""), http.StatusNotFound)

		r := s.do(t, http.MethodPut, "/carts", "", "")
		isProblem(t, r, http.StatusMethodNotAllowed)
		require.Equal(t, http.MethodPost, r.header.Get("Allow"))

		s.stop(t)
	})
}

// failingStore is an endpoint.Store whose every method fails with err.
type failingStore struct{ err error }

func (f failingStore) CreateCart(context.Context) (endpoint.Cart, error) {
	return endpoint.Cart{}, f.err
}

func (f failingStore) GetCart(context.Context, uuid.UUID) (endpoint.Cart, error) {
	return endpoint.Cart{}, f.err
}

func (f failingStore) DeleteCart(context.Context, uuid.UUID) error { return f.err }

func (f failingStore) AddItem(context.Context, uuid.UUID, endpoint.AddItemRequest) (endpoint.CartItem, error) {
	return endpoint.CartItem{}, f.err
}

func (f failingStore) UpdateItem(context.Context, uuid.UUID, uuid.UUID, int) (endpoint.CartItem, error) {
	return endpoint.CartItem{}, f.err
}

func (f failingStore) RemoveItem(context.Context, uuid.UUID, uuid.UUID) error { return f.err }

func TestInit_answers_a_failure_without_its_text(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		secret := rand.Text()
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, func(context.Context, *humus.Setup, *pgxpool.Config) (endpoint.Store, error) {
				return failingStore{err: errors.New("the database said " + secret)}, nil
			})
		})

		r := s.do(t, http.MethodPost, "/carts", "", "")
		d := isProblem(t, r, http.StatusInternalServerError)
		require.Equal(t, endpoint.InternalError(nil).Detail, d.Detail)
		require.NotContains(t, string(r.body), secret)

		s.stop(t)
	})
}

func TestInit_dials_Postgres_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := start(t, Init)

		// The database's name reaches the Host's Dial unresolved: nothing
		// resolves db.internal, and the Host's listener accepts it.
		db, err := s.h.Listen("tcp", dbHost)
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Go(func() {
			conn, err := db.Accept()
			if err != nil {
				return
			}
			// Hang up before the startup message is answered, so the
			// request fails, and the catch-all answers it.
			_ = conn.Close()
		})

		r := s.do(t, http.MethodGet, "/carts/"+uuid.NewString(), "", "")
		d := isProblem(t, r, http.StatusInternalServerError)
		require.Equal(t, endpoint.InternalError(nil).Detail, d.Detail)
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
		for _, name := range []string{"ADDR", "TLS_PKCS12_FILE", "DATABASE_URL"} {
			require.ErrorContains(t, st.Cause(), "config "+name+": not found")
		}
	})
}

func TestInit_serves_its_document(t *testing.T) {
	var doc []byte
	synctest.Test(t, func(t *testing.T) {
		s := start(t, func(ctx context.Context, setup *humus.Setup) ([]humus.Service, error) {
			return initWith(ctx, setup, inMemory)
		})
		r := s.do(t, http.MethodGet, "/openapi.json", "", "")
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

// oas32 is the directory of the OpenAPI Initiative's 3.2 schema-base, with
// the schema, the dialect and the meta-schema it refers to, which
// openapi/v3's tests keep. The example module replaces humus with the one in
// the same commit, so the files are always beside it.
var oas32 = filepath.Join("..", "..", "..", "openapi", "v3", "testdata", "oas3.2")

// schemaBaseID is the $id of the 3.2 schema-base, which checks each Schema
// Object against the 3.2 dialect too.
const schemaBaseID = "https://spec.openapis.org/oas/3.2/schema-base/2025-09-17"

// validate validates doc against the 3.2 schema-base, reading nothing from
// the network.
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
