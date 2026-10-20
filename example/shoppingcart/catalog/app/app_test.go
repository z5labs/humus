// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/shoppingcart/catalog/database"
	"github.com/z5labs/humus/example/shoppingcart/catalog/endpoint"
	"github.com/z5labs/humus/fault"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	base    = "https://localhost:8443"
	dbHost  = "db.internal:5432"
	idpHost = "idp.internal:9443"
)

var requiredSettings = []string{"ADDR", "TLS_CERT_FILE", "TLS_KEY_FILE", "CLIENT_CA_FILE", "DATABASE_URL_FILE", "JWKS_URL", "JWT_ISSUER", "JWT_AUDIENCE"}

type fakeDatabase struct {
	mu       sync.Mutex
	faults   fault.Injector
	products map[string]endpoint.Product
	down     bool
	pings    int
	cfg      *pgxpool.Config
}

func newFakeDatabase(down bool) *fakeDatabase {
	return &fakeDatabase{products: map[string]endpoint.Product{}, down: down}
}

func (db *fakeDatabase) open(_ context.Context, s *humus.Setup, cfg *pgxpool.Config) (endpoint.Store, func(context.Context) error, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.cfg = cfg
	db.faults = s.Faults
	return db, db.ping, nil
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

func (db *fakeDatabase) inject(ctx context.Context) error {
	db.mu.Lock()
	faults := db.faults
	db.mu.Unlock()
	if err := faults.Eval(ctx, database.Point).Inject(ctx); err != nil {
		return fmt.Errorf("%w: %w", endpoint.ErrUnavailable, err)
	}
	return nil
}

func (db *fakeDatabase) GetProduct(ctx context.Context, productID string) (endpoint.Product, error) {
	if err := db.inject(ctx); err != nil {
		return endpoint.Product{}, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	p, ok := db.products[productID]
	if !ok {
		return endpoint.Product{}, endpoint.ErrProductNotFound
	}
	return p, nil
}

func (db *fakeDatabase) PutProduct(ctx context.Context, p endpoint.Product) (endpoint.Product, error) {
	if err := db.inject(ctx); err != nil {
		return endpoint.Product{}, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.products[p.ProductID] = p
	return p, nil
}

func onFake(db *fakeDatabase) func(context.Context, *humus.Setup) ([]humus.Service, error) {
	return func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
		return InitWith(ctx, s, db.open)
	}
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
	password := rand.Text()
	files := map[string][]byte{
		"tls/tls.crt":  certPEM,
		"tls/tls.key":  keyPEM,
		"tls/ca.crt":   caPEM(ca.Issue(t, "catalog")),
		"idp/ca.crt":   caPEM(idpCert),
		"database/url": []byte("postgres://catalog:" + password + "@" + dbHost + "/catalog?sslmode=disable\n"),
	}
	return fixture{
		h:          humustest.NewHost(t, humustest.WithFiles(files)),
		ca:         ca,
		idpCert:    idpCert,
		dbPassword: password,
		files:      files,
		env: map[string]string{
			"ADDR":              ":8443",
			"TLS_CERT_FILE":     "tls/tls.crt",
			"TLS_KEY_FILE":      "tls/tls.key",
			"CLIENT_CA_FILE":    "tls/ca.crt",
			"DATABASE_URL_FILE": "database/url",
			"JWKS_URL":          "https://" + idpHost + jwksPath,
			"JWKS_CA_FILE":      "idp/ca.crt",
			"JWT_ISSUER":        issuer,
			"JWT_AUDIENCE":      audience,
		},
	}
}

func (f fixture) exit(t *testing.T) bedrock.ExitStatus {
	t.Helper()
	_, status := httpservertest.Start(t, Init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(Faults)),
		humustest.WithEnv(f.env),
	)
	return <-status
}

type catalog struct {
	f         fixture
	idp       *idp
	stopIdP   func()
	anonymous *http.Client
	cart      *http.Client
	p         *bedrocktest.Process
	status    <-chan bedrock.ExitStatus
}

func start(t *testing.T, init func(context.Context, *humus.Setup) ([]humus.Service, error)) *catalog {
	t.Helper()
	f := newFixture(t)

	provider := newIdP(t)
	stopIdP := provider.serve(t, f.h.Host(), idpHost, f.idpCert)
	<-f.h.Listening(idpHost)

	p, status := httpservertest.Start(t, init,
		humustest.WithApp(humus.WithHost(f.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder), humus.WithFaults(Faults)),
		humustest.WithEnv(f.env),
	)
	select {
	case <-f.h.Listening("localhost:8443"):
	case st := <-status:
		t.Fatalf("the catalog exited %d: %v", st.Code(), st.Cause())
	}
	return &catalog{
		f:         f,
		idp:       provider,
		stopIdP:   stopIdP,
		anonymous: httpservertest.NewClient(f.h, f.ca.Pool()),
		cart:      withCertificate(httpservertest.NewClient(f.h, f.ca.Pool()), f.ca.Issue(t, "shoppingcart")),
		p:         p,
		status:    status,
	}
}

func withCertificate(c *http.Client, cert *humustest.Certificate) *http.Client {
	c.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{cert.TLS()}
	return c
}

func (c *catalog) stop(t *testing.T) {
	t.Helper()
	<-c.p.Notified(syscall.SIGTERM)
	c.p.Signal(syscall.SIGTERM)
	st := <-c.status
	require.Zero(t, st.Code(), "cause: %v", st.Cause())
	c.stopIdP()
}

type response struct {
	code int
	body []byte
}

func do(t *testing.T, client *http.Client, method, path, token, body string) (response, error) {
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
	if err != nil {
		return response{}, err
	}
	defer func() { require.NoError(t, resp.Body.Close()) }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{code: resp.StatusCode, body: b}, nil
}

func get(t *testing.T, client *http.Client, path string) response {
	t.Helper()
	r, err := do(t, client, http.MethodGet, path, "", "")
	require.NoError(t, err)
	return r
}

func TestInit_serves_the_certificates_and_secrets_in_the_Hosts_files(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase(false)
		c := start(t, onFake(db))

		require.Equal(t, "db.internal", db.cfg.ConnConfig.Host)
		require.EqualValues(t, 5432, db.cfg.ConnConfig.Port)
		require.Equal(t, "catalog", db.cfg.ConnConfig.User)
		require.Equal(t, c.f.dbPassword, db.cfg.ConnConfig.Password)
		require.Equal(t, "catalog", db.cfg.ConnConfig.Database)

		r, err := do(t, c.anonymous, http.MethodPut, "/products/sku-1", c.idp.token(t, endpoint.ScopeWrite), `{"unitPrice":12.5}`)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, r.code, "%s", r.body)

		r = get(t, c.cart, "/products/sku-1")
		require.Equal(t, http.StatusOK, r.code, "%s", r.body)
		var p endpoint.Product
		require.NoError(t, json.Unmarshal(r.body, &p))
		require.Equal(t, endpoint.Product{ProductID: "sku-1", UnitPrice: 12.5}, p)

		r = get(t, c.anonymous, "/products/sku-1")
		require.Equal(t, http.StatusUnauthorized, r.code, "%s", r.body)

		stranger := httpservertest.NewClient(c.f.h, c.f.ca.Pool())
		strangerCert := humustest.NewCA(t).Issue(t, "shoppingcart").TLS()
		stranger.Transport.(*http.Transport).TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &strangerCert, nil
		}
		r, err = do(t, stranger, http.MethodGet, "/products/sku-1", "", "")
		require.Error(t, err, "%d %s", r.code, r.body)

		c.stop(t)
	})
}

func TestInit_reads_its_files_only_through_the_Host(t *testing.T) {
	for name, file := range map[string]string{
		"TLS_CERT_FILE":     "tls/tls.crt",
		"TLS_KEY_FILE":      "tls/tls.key",
		"CLIENT_CA_FILE":    "tls/ca.crt",
		"DATABASE_URL_FILE": "database/url",
		"JWKS_CA_FILE":      "idp/ca.crt",
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

func TestInit_dials_Postgres_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t, Init)

		db, err := c.f.h.Listen("tcp", dbHost)
		require.NoError(t, err)
		var wg sync.WaitGroup
		var dialled bool
		wg.Go(func() {
			conn, err := db.Accept()
			if err != nil {
				return
			}
			dialled = true
			_ = conn.Close()
		})

		r := get(t, c.anonymous, "/health/readiness")
		require.Equal(t, http.StatusServiceUnavailable, r.code, "%s", r.body)
		wg.Wait()
		require.True(t, dialled)

		require.NoError(t, db.Close())
		c.stop(t)
	})
}

func TestInit_is_ready_once_the_database_answers_a_ping_and_stays_ready(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := newFakeDatabase(true)
		c := start(t, onFake(db))

		for range 2 {
			r := get(t, c.anonymous, "/health/readiness")
			require.Equal(t, http.StatusServiceUnavailable, r.code, "%s", r.body)
			r = get(t, c.anonymous, "/health/liveness")
			require.Equal(t, http.StatusOK, r.code, "%s", r.body)
		}
		require.Equal(t, 2, db.pinged())

		db.setDown(false)
		r := get(t, c.anonymous, "/health/readiness")
		require.Equal(t, http.StatusOK, r.code, "%s", r.body)
		require.Equal(t, 3, db.pinged())

		db.setDown(true)
		for range 2 {
			r := get(t, c.anonymous, "/health/readiness")
			require.Equal(t, http.StatusOK, r.code, "%s", r.body)
		}
		require.Equal(t, 3, db.pinged())

		c.stop(t)
	})
}

func TestInit_answers_its_probes_without_a_token_or_a_client_certificate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t, onFake(newFakeDatabase(false)))

		r := get(t, c.anonymous, "/products/sku-1")
		require.Equal(t, http.StatusUnauthorized, r.code, "%s", r.body)
		for _, path := range []string{"/health/liveness", "/health/readiness"} {
			r := get(t, c.anonymous, path)
			require.Equal(t, http.StatusOK, r.code, "%s: %s", path, r.body)
			require.Equal(t, "OK", strings.TrimSpace(string(r.body)), path)
		}

		c.stop(t)
	})
}

func TestInit_exits_0_on_SIGTERM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := start(t, Init)
		c.p.Signal(syscall.SIGTERM)
		st := <-c.status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		c.stopIdP()
	})
}

func TestInit_without_a_required_setting_exits_78(t *testing.T) {
	for _, name := range requiredSettings {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t)
				delete(f.env, name)

				st := f.exit(t)
				require.Equal(t, humus.ExitConfig, st.Code())
				require.ErrorContains(t, st.Cause(), "config "+name+": not found")
				for _, other := range requiredSettings {
					if other != name {
						require.NotContains(t, st.Cause().Error(), "config "+other+":")
					}
				}
			})
		})
	}
}

func TestInit_with_a_bad_setting_exits_78(t *testing.T) {
	for _, tc := range []struct{ name, value, file, cause string }{
		{name: "ADDR", value: "localhost", cause: "port"},
		{name: "TLS_CERT_FILE", value: "missing.crt", cause: "missing.crt"},
		{name: "TLS_CERT_FILE", value: "tls/tls.key", cause: "no PEM certificate in the file"},
		{name: "TLS_KEY_FILE", value: "missing.key", cause: "missing.key"},
		{name: "TLS_KEY_FILE", value: "tls/ca.crt", cause: "tls/tls.crt"},
		{name: "CLIENT_CA_FILE", value: "missing.crt", cause: "missing.crt"},
		{name: "CLIENT_CA_FILE", value: "tls/tls.key", cause: "no PEM certificate in the file"},
		{name: "DATABASE_URL_FILE", value: "missing.url", cause: "missing.url"},
		{name: "DATABASE_URL_FILE", value: "bad.url", file: "postgres://catalog:" + rand.Text() + "@db.internal:port/catalog", cause: "must hold a PostgreSQL URL"},
		{name: "JWKS_URL", value: "http://" + idpHost + jwksPath, cause: "must be an absolute https URL"},
		{name: "JWKS_CA_FILE", value: "tls/tls.key", cause: "no PEM certificate in the file"},
		{name: "JWKS_TIMEOUT", value: "0s", cause: "positive"},
		{name: "JWKS_ATTEMPT_TIMEOUT", value: "5", cause: "missing unit"},
		{name: "JWKS_MAX_ATTEMPTS", value: "0", cause: "at least 1"},
		{name: "JWT_ISSUER", value: "", cause: "must not be empty"},
		{name: "JWT_AUDIENCE", value: "", cause: "must not be empty"},
		{name: "JWT_ALGORITHMS", value: "none", cause: "none"},
		{name: "JWT_LEEWAY", value: "-1s", cause: "must be from 0s"},
		{name: "JWT_MIN_REFRESH_INTERVAL", value: "-1s", cause: "must be positive"},
		{name: "JWT_MAX_KEY_AGE", value: "-1s", cause: "must be positive"},
		{name: "HTTP2_ONLY", value: "maybe", cause: "invalid syntax"},
		{name: "READ_HEADER_TIMEOUT", value: "-1s", cause: "negative"},
		{name: "READ_TIMEOUT", value: "-1s", cause: "negative"},
		{name: "WRITE_TIMEOUT", value: "-1s", cause: "negative"},
		{name: "IDLE_TIMEOUT", value: "-1s", cause: "negative"},
		{name: "MAX_HEADER_BYTES", value: "0", cause: "positive"},
		{name: "FAULT_MAX_DELAY", value: "0s", cause: "positive"},
		{name: "FAULT_MAX_DELAY", value: "10", cause: "missing unit"},
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

func pemPair(t *testing.T, c *humustest.Certificate) (certPEM, keyPEM []byte) {
	t.Helper()
	cert := c.TLS()
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}

func caPEM(cert *humustest.Certificate) []byte {
	chain := cert.TLS().Certificate
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain[len(chain)-1]})
}
