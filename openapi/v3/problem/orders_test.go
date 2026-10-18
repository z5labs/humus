// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package problem_test

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/openapi/v3/problem"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run the Init of example_test.go, which the docs
// site quotes, with httpservertest, on a humustest Host.

// ordersApp is Init, running.
type ordersApp struct {
	t      *testing.T
	client *http.Client
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

// keyPairPEM returns c's chain and key, PEM-encoded, as serverTLS reads
// them.
func keyPairPEM(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var b []byte
	for _, der := range cert.Certificate {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

// startOrders runs Init on a new humustest Host, with its certificate in
// the Host's filesystem, and waits for it to listen.
func startOrders(t *testing.T) *ordersApp {
	t.Helper()
	ca := humustest.NewCA(t)
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{"tls.pem": keyPairPEM(t, ca.Issue(t, "localhost"))}))
	p, status := httpservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{"ADDR": ":8443", "TLS_PEM_FILE": "tls.pem"}),
	)
	select {
	case <-h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("Init exited before it listened: %v", st.Cause())
	}
	return &ordersApp{t: t, client: httpservertest.NewClient(h, ca.Pool()), p: p, status: status}
}

// stop sends the App SIGTERM, and checks that it exits 0.
func (a *ordersApp) stop() {
	a.t.Helper()
	<-a.p.Notified(syscall.SIGTERM)
	a.p.Signal(syscall.SIGTERM)
	st := <-a.status
	require.Zero(a.t, st.Code(), "cause: %v", st.Cause())
	require.Empty(a.t, a.p.Stderr())
}

// answer is a response, read in full.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// call sends method path, with headers, and with body as contentType when
// body isn't "".
func (a *ordersApp) call(method, path, contentType, body string, headers ...string) answer {
	a.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(a.t.Context(), method, "https://localhost:8443"+path, r)
	require.NoError(a.t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := a.client.Do(req)
	require.NoError(a.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(a.t, err)
	return answer{status: resp.StatusCode, header: resp.Header, body: b}
}

// as checks that r has status and the media type, and decodes its body as
// T.
func as[T any](t *testing.T, r answer, status int, mediaType string) T {
	t.Helper()
	require.Equal(t, status, r.status, "%s", r.body)
	require.Equal(t, mediaType, r.header.Get("Content-Type"))
	var v T
	require.NoError(t, json.Unmarshal(r.body, &v), "%s", r.body)
	return v
}

// problemAt checks that r is a problem detail at status, and returns its
// detail.
func problemAt(t *testing.T, r answer, status int) string {
	t.Helper()
	d := as[problem.Details](t, r, status, "application/problem+json")
	require.Equal(t, status, d.Status)
	require.Equal(t, http.StatusText(status), d.Title)
	return d.Detail
}

func TestInit_serves_the_orders_API(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := startOrders(t)

		// createOrder, twice with one Idempotency-Key: one order.
		first := as[Order](t, a.call(http.MethodPost, "/orders", "", "", "Idempotency-Key", "k1"), http.StatusCreated, "application/json")
		again := as[Order](t, a.call(http.MethodPost, "/orders", "", "", "Idempotency-Key", "k1"), http.StatusCreated, "application/json")
		require.Equal(t, first, again)
		second := as[Order](t, a.call(http.MethodPost, "/orders", "", ""), http.StatusCreated, "application/json")
		require.NotEqual(t, first.ID, second.ID)

		// addLine, and an addLine the API won't accept.
		line := as[Line](t, a.call(http.MethodPost, "/orders/"+first.ID+"/lines", "application/json", `{"sku":"sku-1","quantity":2}`), http.StatusCreated, "application/json")
		require.Equal(t, Line{SKU: "sku-1", Quantity: 2}, line)
		d := problemAt(t, a.call(http.MethodPost, "/orders/"+first.ID+"/lines", "application/json", `{"sku":"sku-1","quantity":0}`), http.StatusUnprocessableEntity)
		require.Equal(t, "quantity must be at least 1", d)

		// getOrder, of an order that exists and of one that doesn't.
		got := as[Order](t, a.call(http.MethodGet, "/orders/"+first.ID, "", ""), http.StatusOK, "application/json")
		require.Equal(t, []Line{line}, got.Lines)
		d = problemAt(t, a.call(http.MethodGet, "/orders/999", "", ""), http.StatusNotFound)
		require.Equal(t, "There is no order 999.", d)

		// listOrders, with its parameters' defaults, then with values.
		require.Len(t, as[[]Order](t, a.call(http.MethodGet, "/orders", "", ""), http.StatusOK, "application/json"), 2)
		require.Len(t, as[[]Order](t, a.call(http.MethodGet, "/orders?limit=1", "", ""), http.StatusOK, "application/json"), 1)

		// cancelOrder: a 204, and the order is listed as cancelled.
		r := a.call(http.MethodDelete, "/orders/"+second.ID, "", "")
		require.Equal(t, http.StatusNoContent, r.status)
		require.Empty(t, r.body)
		cancelled := as[[]Order](t, a.call(http.MethodGet, "/orders?status=cancelled", "", ""), http.StatusOK, "application/json")
		require.Len(t, cancelled, 1)
		require.Equal(t, second.ID, cancelled[0].ID)

		a.stop()
	})
}

func TestInit_answers_a_request_it_rejects_with_a_problem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := startOrders(t)
		id := as[Order](t, a.call(http.MethodPost, "/orders", "", ""), http.StatusCreated, "application/json").ID

		for _, path := range []string{"/orders/not-a-number", "/orders?status=lost", "/orders?limit=0", "/orders?limit=101", "/orders?limit=1&limit=2"} {
			problemAt(t, a.call(http.MethodGet, path, "", ""), http.StatusBadRequest)
		}
		problemAt(t, a.call(http.MethodPost, "/orders", "", "", "Idempotency-Key", strings.Repeat("k", 65)), http.StatusBadRequest)
		problemAt(t, a.call(http.MethodPost, "/orders/"+id+"/lines", "application/json", `{"sku":`), http.StatusBadRequest)
		problemAt(t, a.call(http.MethodPost, "/orders/"+id+"/lines", "text/plain", `{"sku":"sku-1","quantity":1}`), http.StatusUnsupportedMediaType)

		a.stop()
	})
}

func TestInit_answers_unmatched_requests_with_a_problem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := startOrders(t)

		problemAt(t, a.call(http.MethodGet, "/carts", "", ""), http.StatusNotFound)
		r := a.call(http.MethodPut, "/orders", "", "")
		problemAt(t, r, http.StatusMethodNotAllowed)
		require.Equal(t, "GET, POST", r.header.Get("Allow"))

		a.stop()
	})
}

// document is the part of the API's document the tests read.
type document struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Paths map[string]map[string]struct {
		OperationID string                     `json:"operationId"`
		Responses   map[string]json.RawMessage `json:"responses"`
	} `json:"paths"`
}

func TestInit_serves_its_document(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := startOrders(t)

		doc := as[document](t, a.call(http.MethodGet, "/openapi.json", "", ""), http.StatusOK, "application/json")
		require.Equal(t, "3.2.0", doc.OpenAPI)
		require.Equal(t, "Orders", doc.Info.Title)
		require.Equal(t, "addLine", doc.Paths["/orders/{orderId}/lines"]["post"].OperationID)

		responses := func(path, method string) []string {
			var codes []string
			for code := range doc.Paths[path][method].Responses {
				codes = append(codes, code)
			}
			return codes
		}
		require.ElementsMatch(t, []string{"200", "400", "500"}, responses("/orders", "get"))
		require.ElementsMatch(t, []string{"201", "400", "404", "413", "415", "422", "500"}, responses("/orders/{orderId}/lines", "post"))
		require.NotContains(t, doc.Paths, "/openapi.json", "the document doesn't describe itself")

		a.stop()
	})
}

func TestInit_without_its_settings_exits_78(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, status := httpservertest.Start(t, Init,
			humustest.WithApp(humus.WithHost(humustest.NewHost(t).Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
		)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code())
		require.ErrorContains(t, st.Cause(), "config ADDR: not found")
		require.ErrorContains(t, st.Cause(), "config TLS_PEM_FILE: not found")
	})
}
