// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/rand"
	"maps"
	"net/url"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/cases"
	catalogapp "github.com/z5labs/humus/example/shoppingcart/catalog/app"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	cartHost   = "localhost:8443"
	trustScope = "faults:rehearse"
)

type running struct {
	name   string
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
}

func (r running) listening(t *testing.T, h *humustest.Host, addr string) {
	t.Helper()
	select {
	case <-h.Listening(addr):
	case st := <-r.status:
		t.Fatalf("the %s exited %d: %v", r.name, st.Code(), st.Cause())
	}
}

func (r running) stop(t *testing.T) {
	t.Helper()
	<-r.p.Notified(syscall.SIGTERM)
	r.p.Signal(syscall.SIGTERM)
	st := <-r.status
	require.Zero(t, st.Code(), "the %s's cause: %v", r.name, st.Cause())
}

type shoppingcart struct {
	target   cases.Target
	cart     running
	catalog  running
	products *productMemory
	stopIdP  func()
}

func startShoppingcart(t *testing.T) *shoppingcart {
	t.Helper()
	return startShoppingcartWith(t, map[string]string{"FAULT_TRUST_SCOPE": trustScope}, telemetry.NoopBuilder)
}

func startShoppingcartWith(t *testing.T, cartEnv map[string]string, cartTelemetry telemetry.Builder) *shoppingcart {
	t.Helper()
	ca := humustest.NewCA(t)
	idpCert := ca.Issue(t, "idp.internal")
	trusted := caPEM(idpCert)
	cartCert, cartKey := pemPair(t, ca.Issue(t, "localhost"))
	clientCert, clientKey := pemPair(t, ca.Issue(t, "shoppingcart"))
	catalogCert, catalogKey := pemPair(t, ca.Issue(t, "catalog.internal"))
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"cart/tls.crt":         cartCert,
		"cart/tls.key":         cartKey,
		"cart/database-url":    []byte("postgres://carts:" + rand.Text() + "@" + dbHost + "/carts?sslmode=disable\n"),
		"cart/catalog.crt":     clientCert,
		"cart/catalog.key":     clientKey,
		"catalog/tls.crt":      catalogCert,
		"catalog/tls.key":      catalogKey,
		"catalog/database-url": []byte("postgres://catalog:" + rand.Text() + "@" + dbHost + "/catalog?sslmode=disable\n"),
		"ca.crt":               trusted,
	}))

	provider := newIdP(t)
	stopIdP := provider.serve(t, h.Host(), idpHost, idpCert)
	<-h.Listening(idpHost)

	tokens := map[string]string{
		"JWKS_URL":     "https://" + idpHost + jwksPath,
		"JWKS_CA_FILE": "ca.crt",
		"JWT_ISSUER":   issuer,
		"JWT_AUDIENCE": audience,
	}
	products := productsInMemory()
	catalog := run(t, "catalog", h, func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
		return catalogapp.InitWith(ctx, s, products.open)
	}, []humus.Option{humus.WithFaults(catalogapp.Faults)}, tokens, map[string]string{
		"ADDR":              ":10443",
		"TLS_CERT_FILE":     "catalog/tls.crt",
		"TLS_KEY_FILE":      "catalog/tls.key",
		"CLIENT_CA_FILE":    "ca.crt",
		"DATABASE_URL_FILE": "catalog/database-url",
	})
	catalog.listening(t, h, catalogHost)

	cart := run(t, "cart", h, func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
		return initWith(ctx, s, inMemory, openCatalog)
	}, []humus.Option{humus.WithFaults(Faults), humus.WithTelemetry(cartTelemetry)}, tokens, cartEnv, map[string]string{
		"ADDR":                     ":8443",
		"TLS_CERT_FILE":            "cart/tls.crt",
		"TLS_KEY_FILE":             "cart/tls.key",
		"DATABASE_URL_FILE":        "cart/database-url",
		"CATALOG_URL":              "https://" + catalogHost,
		"CATALOG_CA_FILE":          "ca.crt",
		"CATALOG_CLIENT_CERT_FILE": "cart/catalog.crt",
		"CATALOG_CLIENT_KEY_FILE":  "cart/catalog.key",
	})
	cart.listening(t, h, cartHost)

	return &shoppingcart{
		target: cases.Target{
			Cart:          &url.URL{Scheme: "https", Host: cartHost},
			CartClient:    httpservertest.NewClient(h, ca.Pool()),
			Catalog:       &url.URL{Scheme: "https", Host: catalogHost},
			CatalogClient: httpservertest.NewClient(h, ca.Pool()),
			Token:         provider.sign,
			CatalogPoint:  "http:" + catalogHost,
			ScenarioKey:   scenarioKey,
			TrustScope:    trustScope,
			Timeout:       time.Minute,
		},
		cart:     cart,
		catalog:  catalog,
		products: products,
		stopIdP:  stopIdP,
	}
}

func run(t *testing.T, name string, h *humustest.Host, init func(context.Context, *humus.Setup) ([]humus.Service, error), app []humus.Option, envs ...map[string]string) running {
	t.Helper()
	env := map[string]string{}
	for _, e := range envs {
		maps.Copy(env, e)
	}
	p, status := httpservertest.Start(t, init,
		humustest.WithApp(append([]humus.Option{humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)}, app...)...),
		humustest.WithEnv(env),
	)
	return running{name: name, p: p, status: status}
}

func (s *shoppingcart) stop(t *testing.T) {
	t.Helper()
	s.cart.stop(t)
	s.catalog.stop(t)
	s.stopIdP()
}

func runCase(t *testing.T, c cases.Case) {
	synctest.Test(t, func(t *testing.T) {
		s := startShoppingcart(t)
		c.Run(t, s.target)
		s.stop(t)
	})
}

func TestInit_passes_the_shared_cases(t *testing.T) {
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.Parallel {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				runCase(t, c)
			})
		}
	})
	for _, c := range cases.Alone {
		t.Run(c.Name, func(t *testing.T) {
			runCase(t, c)
		})
	}
}
