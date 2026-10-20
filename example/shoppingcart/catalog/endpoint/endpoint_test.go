// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package endpoint_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/auth/jwt"
	"github.com/z5labs/humus/example/shoppingcart/catalog/endpoint"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
)

const (
	base     = "https://localhost:8443"
	issuer   = "https://idp.internal"
	audience = "catalog"
)

type memoryStore struct {
	mu       sync.Mutex
	products map[string]endpoint.Product
	err      error
}

func (m *memoryStore) GetProduct(_ context.Context, productID string) (endpoint.Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return endpoint.Product{}, m.err
	}
	p, ok := m.products[productID]
	if !ok {
		return endpoint.Product{}, endpoint.ErrProductNotFound
	}
	return p, nil
}

func (m *memoryStore) PutProduct(_ context.Context, p endpoint.Product) (endpoint.Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return endpoint.Product{}, m.err
	}
	m.products[p.ProductID] = p
	return p, nil
}

func (m *memoryStore) fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

type keys struct {
	kid string
	key *ecdsa.PrivateKey

	mu   sync.Mutex
	down bool
}

func (k *keys) Keys(context.Context) ([]jwt.Key, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.down {
		return nil, errors.New("the key set is down")
	}
	return []jwt.Key{{ID: k.kid, Public: k.key.Public()}}, nil
}

func (k *keys) setDown(down bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.down = down
}

func (k *keys) token(t *testing.T, scopes ...string) string {
	t.Helper()
	now := time.Now()
	claims := struct {
		josejwt.Claims
		Scope string `json:"scope,omitempty"`
	}{
		Claims: josejwt.Claims{
			Issuer:    issuer,
			Audience:  josejwt.Audience{audience},
			Subject:   "operator",
			IssuedAt:  josejwt.NewNumericDate(now),
			NotBefore: josejwt.NewNumericDate(now),
			Expiry:    josejwt.NewNumericDate(now.Add(time.Hour)),
		},
		Scope: strings.Join(scopes, " "),
	}
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: k.key, KeyID: k.kid}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	require.NoError(t, err)
	raw, err := josejwt.Signed(sig).Claims(claims).Serialize()
	require.NoError(t, err)
	return raw
}

type catalog struct {
	store     *memoryStore
	keys      *keys
	tel       *humustest.Telemetry
	anonymous *http.Client
	cart      *http.Client
	p         *bedrocktest.Process
	status    <-chan bedrock.ExitStatus
}

func start(t *testing.T) *catalog {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	c := &catalog{
		store: &memoryStore{products: map[string]endpoint.Product{}},
		keys:  &keys{kid: rand.Text(), key: key},
		tel:   humustest.NewTelemetry(),
	}
	serverCert := ca.Issue(t, "localhost")
	c.p, c.status = httpservertest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
		settings := jwt.DefaultSettings()
		settings.Issuer = issuer
		settings.Audience = audience
		tokens, err := jwt.NewVerifier[endpoint.Claims](s, settings, c.keys)
		if err != nil {
			return nil, err
		}
		api, err := endpoint.NewAPI(s, c.store, tokens.Verify)
		if err != nil {
			return nil, err
		}
		server := httpserver.DefaultSettings()
		server.Addr = ":8443"
		server.TLS = &tls.Config{
			Certificates: []tls.Certificate{serverCert.TLS()},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    ca.Pool(),
		}
		svc, err := httpserver.NewService(s, server, api)
		if err != nil {
			return nil, err
		}
		return []humus.Service{svc}, nil
	}, humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(c.tel.Builder())))
	select {
	case <-h.Listening("localhost:8443"):
	case st := <-c.status:
		t.Fatalf("the catalog exited %d: %v", st.Code(), st.Cause())
	}

	c.anonymous = httpservertest.NewClient(h, ca.Pool())
	c.cart = httpservertest.NewClient(h, ca.Pool())
	c.cart.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{ca.Issue(t, "shoppingcart").TLS()}
	return c
}

func (c *catalog) stop(t *testing.T) {
	t.Helper()
	<-c.p.Notified(syscall.SIGTERM)
	c.p.Signal(syscall.SIGTERM)
	st := <-c.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

func do(t *testing.T, client *http.Client, method, path, token, body string) response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, r)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
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

func TestNewAPI_puts_and_gets_a_product(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t)
		writer := c.keys.token(t, endpoint.ScopeWrite)

		d := isProblem(t, do(t, c.cart, http.MethodGet, "/products/sku-1", "", ""), http.StatusNotFound)
		require.Equal(t, "There is no product sku-1.", d.Detail)

		put := decode[endpoint.Product](t,
			do(t, c.anonymous, http.MethodPut, "/products/sku-1", writer, `{"unitPrice":9.99}`),
			http.StatusOK, "application/json")
		require.Equal(t, endpoint.Product{ProductID: "sku-1", UnitPrice: 9.99}, put)

		got := decode[endpoint.Product](t, do(t, c.cart, http.MethodGet, "/products/sku-1", "", ""), http.StatusOK, "application/json")
		require.Equal(t, put, got)

		replaced := decode[endpoint.Product](t,
			do(t, c.anonymous, http.MethodPut, "/products/sku-1", writer, `{"unitPrice":0}`),
			http.StatusOK, "application/json")
		require.Equal(t, endpoint.Product{ProductID: "sku-1", UnitPrice: 0}, replaced)
		r := do(t, c.cart, http.MethodGet, "/products/sku-1", "", "")
		require.JSONEq(t, `{"productId":"sku-1","unitPrice":0}`, string(r.body))
		require.Equal(t, replaced, decode[endpoint.Product](t, r, http.StatusOK, "application/json"))

		c.stop(t)
	})
}

func TestNewAPI_answers_a_body_it_wont_accept_with_422(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t)
		writer := c.keys.token(t, endpoint.ScopeWrite)

		for body, detail := range map[string]string{
			`{}`:                         "unitPrice is required",
			`{"unitPrice":null}`:         "unitPrice is required",
			`{"unitPrice":-0.01}`:        "unitPrice must not be negative",
			`{"unitPrice":100000000.00}`: "unitPrice must be at most 99999999.99",
		} {
			d := isProblem(t, do(t, c.anonymous, http.MethodPut, "/products/sku-1", writer, body), http.StatusUnprocessableEntity)
			require.Equal(t, detail, d.Detail, body)
		}
		isProblem(t, do(t, c.cart, http.MethodGet, "/products/sku-1", "", ""), http.StatusNotFound)

		isProblem(t, do(t, c.anonymous, http.MethodPut, "/products/sku-1", writer, `{"unitPrice":`), http.StatusBadRequest)
		d := isProblem(t, do(t, c.cart, http.MethodGet, "/products/"+strings.Repeat("x", 256), "", ""), http.StatusBadRequest)
		require.Contains(t, d.Detail, "productId")

		c.stop(t)
	})
}

func TestNewAPI_needs_a_client_certificate_to_get_a_product(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t)
		writer := c.keys.token(t, endpoint.ScopeWrite)
		decode[endpoint.Product](t, do(t, c.anonymous, http.MethodPut, "/products/sku-1", writer, `{"unitPrice":1}`), http.StatusOK, "application/json")

		for _, token := range []string{"", writer} {
			r := do(t, c.anonymous, http.MethodGet, "/products/sku-1", token, "")
			isProblem(t, r, http.StatusUnauthorized)
			require.Empty(t, r.header.Get("WWW-Authenticate"))
		}

		decode[endpoint.Product](t, do(t, c.cart, http.MethodGet, "/products/sku-1", "", ""), http.StatusOK, "application/json")

		c.stop(t)
	})
}

func TestNewAPI_needs_catalog_write_to_put_a_product(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t)

		for _, client := range []*http.Client{c.anonymous, c.cart} {
			r := do(t, client, http.MethodPut, "/products/sku-1", "", `{"unitPrice":1}`)
			isProblem(t, r, http.StatusUnauthorized)
			require.Equal(t, "Bearer", r.header.Get("WWW-Authenticate"))
		}

		other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		forged := (&keys{kid: c.keys.kid, key: other}).token(t, endpoint.ScopeWrite)
		r := do(t, c.anonymous, http.MethodPut, "/products/sku-1", forged, `{"unitPrice":1}`)
		isProblem(t, r, http.StatusUnauthorized)
		require.Equal(t, `Bearer error="invalid_token"`, r.header.Get("WWW-Authenticate"))

		r = do(t, c.cart, http.MethodPut, "/products/sku-1", c.keys.token(t, "carts:write"), `{"unitPrice":1}`)
		isProblem(t, r, http.StatusForbidden)
		require.Equal(t, `Bearer error="insufficient_scope", scope="catalog:write"`, r.header.Get("WWW-Authenticate"))

		isProblem(t, do(t, c.cart, http.MethodGet, "/products/sku-1", "", ""), http.StatusNotFound)

		c.stop(t)
	})
}

func TestNewAPI_answers_a_token_it_has_no_keys_for_with_503(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t)

		c.keys.setDown(true)
		r := do(t, c.anonymous, http.MethodPut, "/products/sku-1", c.keys.token(t, endpoint.ScopeWrite), `{"unitPrice":1}`)
		d := isProblem(t, r, http.StatusServiceUnavailable)
		require.Equal(t, "The server can't verify tokens right now. Try again later.", d.Detail)
		require.Empty(t, r.header.Get("WWW-Authenticate"))

		c.stop(t)
	})
}

func TestNewAPI_answers_a_database_that_is_down_with_503(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t)
		c.store.fail(errors.Join(endpoint.ErrUnavailable, errors.New("dial tcp db.internal:5432: connection refused")))

		for _, r := range []response{
			do(t, c.cart, http.MethodGet, "/products/sku-1", "", ""),
			do(t, c.anonymous, http.MethodPut, "/products/sku-1", c.keys.token(t, endpoint.ScopeWrite), `{"unitPrice":1}`),
		} {
			d := isProblem(t, r, http.StatusServiceUnavailable)
			require.Equal(t, "The catalog can't reach its database right now. Try again later.", d.Detail)
			require.NotContains(t, string(r.body), "db.internal")
		}

		c.stop(t)
		require.Empty(t, failures(c.tel))
	})
}

func TestNewAPI_answers_a_failure_without_its_text(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t)
		secret := rand.Text()
		c.store.fail(errors.New("the database said " + secret))

		for _, r := range []response{
			do(t, c.cart, http.MethodGet, "/products/sku-1", "", ""),
			do(t, c.anonymous, http.MethodPut, "/products/sku-1", c.keys.token(t, endpoint.ScopeWrite), `{"unitPrice":1}`),
		} {
			d := isProblem(t, r, http.StatusInternalServerError)
			require.Equal(t, endpoint.InternalError(nil).Detail, d.Detail)
			require.NotContains(t, string(r.body), secret)
		}

		c.stop(t)
		require.Len(t, failures(c.tel), 2)
	})
}

func failures(tel *humustest.Telemetry) []string {
	var msgs []string
	for _, r := range tel.Logs() {
		if r.Body().AsString() == "request failed" {
			msgs = append(msgs, r.Body().AsString())
		}
	}
	return msgs
}

func TestNewAPI_answers_unmatched_requests_with_a_problem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t)

		isProblem(t, do(t, c.cart, http.MethodGet, "/carts", "", ""), http.StatusNotFound)
		r := do(t, c.cart, http.MethodDelete, "/products/sku-1", "", "")
		isProblem(t, r, http.StatusMethodNotAllowed)
		require.ElementsMatch(t, []string{http.MethodGet, http.MethodPut}, strings.Split(strings.ReplaceAll(r.header.Get("Allow"), " ", ""), ","))

		c.stop(t)
	})
}

func TestNewAPI_serves_its_document(t *testing.T) {
	var doc []byte
	synctest.Test(t, func(t *testing.T) {
		c := start(t)
		r := do(t, c.anonymous, http.MethodGet, "/openapi.json", "", "")
		require.Equal(t, http.StatusOK, r.code)
		require.Equal(t, "application/json", r.header.Get("Content-Type"))
		doc = r.body
		c.stop(t)
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
