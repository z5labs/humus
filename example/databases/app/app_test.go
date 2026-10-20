// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	apiServerAddr = "apiserver:6443"
	probesAddr    = "localhost:8443"
	unset         = "\x00unset"
)

type target struct {
	h        *humustest.Host
	ca       *humustest.CA
	env      map[string]string
	probes   *http.Client
	token    string
	password string
}

func newTarget(t *testing.T, files map[string][]byte) *target {
	t.Helper()
	ca := humustest.NewCA(t)
	token, password := rand.Text(), rand.Text()
	all := map[string][]byte{
		"serviceaccount/ca.crt": caPEM(t, ca),
		"serviceaccount/token":  []byte(token + "\n"),
		"secrets/admin-dsn":     []byte("postgres://postgres:" + password + "@postgres.internal:5432/postgres?sslmode=disable\n"),
		"secrets/probes.pem":    pemOf(t, ca.Issue(t, "localhost")),
	}
	maps.Copy(all, files)
	h := humustest.NewHost(t, humustest.WithFiles(all))

	probes := h.HTTPClient()
	probes.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	return &target{
		h:  h,
		ca: ca,
		env: map[string]string{
			"KUBERNETES_SERVICE_HOST": "apiserver",
			"KUBERNETES_SERVICE_PORT": "6443",
			"KUBERNETES_CA_FILE":      h.Path("serviceaccount/ca.crt"),
			"KUBERNETES_TOKEN_FILE":   h.Path("serviceaccount/token"),
			"POD_NAMESPACE":           "databases",
			"POD_NAME":                "databases-controller-0",
			"POSTGRES_ADMIN_DSN_FILE": h.Path("secrets/admin-dsn"),
			"POSTGRES_HOST":           "postgres.databases.svc",
			"PROBES_ADDR":             probesAddr,
			"PROBES_TLS_PEM_FILE":     h.Path("secrets/probes.pem"),
		},
		probes:   probes,
		token:    token,
		password: password,
	}
}

func (c *target) start(t *testing.T, env map[string]string) (signal func(os.Signal), status <-chan bedrock.ExitStatus) {
	t.Helper()
	return c.startWith(t, env, telemetry.NoopBuilder)
}

func (c *target) startWith(t *testing.T, env map[string]string, build telemetry.Builder) (signal func(os.Signal), status <-chan bedrock.ExitStatus) {
	t.Helper()
	merged := maps.Clone(c.env)
	for name, value := range env {
		if value == unset {
			delete(merged, name)
			continue
		}
		merged[name] = value
	}
	proc, st := humustest.Start(t, Init,
		humustest.WithApp(humus.WithHost(c.h.Host()), humus.WithTelemetry(build), humus.WithFaults(Faults)),
		humustest.WithEnv(merged),
	)
	return proc.Signal, st
}

func (c *target) probe(t *testing.T, path string) int {
	t.Helper()
	resp, err := c.probes.Get("https://" + probesAddr + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	return resp.StatusCode
}

func pemOf(t *testing.T, c *humustest.Certificate) []byte {
	t.Helper()
	cert := c.TLS()
	var out []byte
	for _, der := range cert.Certificate {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
}

func caPEM(t *testing.T, ca *humustest.CA) []byte {
	t.Helper()
	c := ca.Issue(t, "unused").TLS()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[len(c.Certificate)-1]})
}

func exitsConfig(t *testing.T, env map[string]string, files map[string][]byte) error {
	t.Helper()
	var cause error
	synctest.Test(t, func(t *testing.T) {
		c := newTarget(t, files)
		_, status := c.start(t, env)
		st := <-status
		cause = st.Cause()
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", cause)
		require.NotContains(t, cause.Error(), c.token)
		require.NotContains(t, cause.Error(), c.password)
	})
	require.Error(t, cause)
	return cause
}

func TestInit_without_its_settings_exits_78(t *testing.T) {
	required := []string{
		"KUBERNETES_SERVICE_HOST",
		"KUBERNETES_SERVICE_PORT",
		"POD_NAMESPACE",
		"POD_NAME",
		"POSTGRES_ADMIN_DSN_FILE",
		"POSTGRES_HOST",
		"PROBES_ADDR",
		"PROBES_TLS_PEM_FILE",
	}
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{name: unset}, nil)
			require.ErrorContains(t, cause, "config "+name+": not found")
		})
	}

	t.Run("all of them", func(t *testing.T) {
		env := map[string]string{}
		for _, name := range required {
			env[name] = unset
		}
		cause := exitsConfig(t, env, nil)
		for _, name := range required {
			require.ErrorContains(t, cause, "config "+name+": not found")
		}
	})
}

func TestInit_reads_the_service_accounts_files_by_default(t *testing.T) {
	for name, file := range map[string]string{
		"KUBERNETES_CA_FILE":    "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
		"KUBERNETES_TOKEN_FILE": "/var/run/secrets/kubernetes.io/serviceaccount/token",
	} {
		t.Run(name, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{name: unset}, nil)
			require.ErrorContains(t, cause, "config "+name+": ")
			require.ErrorContains(t, cause, file)
		})
	}
}

func TestInit_with_a_bad_setting_exits_78(t *testing.T) {
	password := rand.Text()
	testCases := map[string]struct {
		name   string
		value  string
		files  map[string][]byte
		secret string
	}{
		"an empty API server host":               {name: "KUBERNETES_SERVICE_HOST", value: ""},
		"an API server port that isn't a number": {name: "KUBERNETES_SERVICE_PORT", value: "https"},
		"an API server port of 0":                {name: "KUBERNETES_SERVICE_PORT", value: "0"},
		"an API server port above 65535":         {name: "KUBERNETES_SERVICE_PORT", value: "65536"},
		"a CA file that doesn't exist":           {name: "KUBERNETES_CA_FILE", value: "serviceaccount/missing.crt"},
		"a CA file with no certificate":          {name: "KUBERNETES_CA_FILE", value: "certs/junk.pem", files: map[string][]byte{"certs/junk.pem": []byte("junk")}},
		"a token file that doesn't exist":        {name: "KUBERNETES_TOKEN_FILE", value: "serviceaccount/missing"},
		"an empty token file":                    {name: "KUBERNETES_TOKEN_FILE", value: "secrets/empty", files: map[string][]byte{"secrets/empty": []byte("\n")}},
		"a controller name that isn't DNS-1123":  {name: "CONTROLLER_NAME", value: "Databases_Controller"},
		"a namespace that isn't DNS-1123":        {name: "CONTROLLER_NAMESPACE", value: "Not A Namespace"},
		"workers of 0":                           {name: "CONTROLLER_WORKERS", value: "0"},
		"workers that aren't a number":           {name: "CONTROLLER_WORKERS", value: "many"},
		"a resync of 0":                          {name: "CONTROLLER_RESYNC", value: "0s"},
		"a resync with no unit":                  {name: "CONTROLLER_RESYNC", value: "10"},
		"an empty Lease namespace":               {name: "POD_NAMESPACE", value: ""},
		"a Lease namespace that isn't DNS-1123":  {name: "POD_NAMESPACE", value: "Databases"},
		"an empty Lease identity":                {name: "POD_NAME", value: ""},
		"an admin DSN file that doesn't exist":   {name: "POSTGRES_ADMIN_DSN_FILE", value: "secrets/missing"},
		"an admin DSN that doesn't parse":        {name: "POSTGRES_ADMIN_DSN_FILE", value: "secrets/junk-dsn", files: map[string][]byte{"secrets/junk-dsn": []byte("postgres://postgres:" + password + "@postgres.internal:port/postgres")}, secret: password},
		"an admin DSN with no password":          {name: "POSTGRES_ADMIN_DSN_FILE", value: "secrets/no-password", files: map[string][]byte{"secrets/no-password": []byte("postgres://postgres@postgres.internal:5432/postgres")}},
		"an empty PostgreSQL host":               {name: "POSTGRES_HOST", value: ""},
		"a PostgreSQL port of 0":                 {name: "POSTGRES_PORT", value: "0"},
		"a PostgreSQL port above 65535":          {name: "POSTGRES_PORT", value: "65536"},
		"a PostgreSQL port that isn't a number":  {name: "POSTGRES_PORT", value: "five"},
		"a probes address with no port":          {name: "PROBES_ADDR", value: "localhost"},
		"a probes PEM file that doesn't exist":   {name: "PROBES_TLS_PEM_FILE", value: "secrets/missing.pem"},
		"a probes PEM file with no key":          {name: "PROBES_TLS_PEM_FILE", value: "serviceaccount/ca.crt"},
		"a fault max delay of 0":                 {name: "FAULT_MAX_DELAY", value: "0s"},
		"a fault max delay with no unit":         {name: "FAULT_MAX_DELAY", value: "10"},
	}
	for desc, tc := range testCases {
		t.Run(desc, func(t *testing.T) {
			cause := exitsConfig(t, map[string]string{tc.name: tc.value}, tc.files)
			require.ErrorContains(t, cause, "config "+tc.name+":")
			if tc.secret != "" {
				require.NotContains(t, cause.Error(), tc.secret)
			}
		})
	}
}

func TestInit_reads_its_secrets_through_the_Host(t *testing.T) {
	ca := humustest.NewCA(t)
	for name, contents := range map[string][]byte{
		"KUBERNETES_CA_FILE":      caPEM(t, ca),
		"KUBERNETES_TOKEN_FILE":   []byte(rand.Text()),
		"POSTGRES_ADMIN_DSN_FILE": []byte("postgres://postgres:" + rand.Text() + "@postgres.internal:5432/postgres"),
		"PROBES_TLS_PEM_FILE":     pemOf(t, ca.Issue(t, "localhost")),
	} {
		t.Run(name, func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(outside, contents, 0o600))

			cause := exitsConfig(t, map[string]string{name: outside}, nil)
			require.ErrorContains(t, cause, "config "+name+": ")
			require.ErrorContains(t, cause, "path escapes from parent")
		})
	}
}

func TestInit_reads_no_secret_from_the_environment(t *testing.T) {
	cause := exitsConfig(t, map[string]string{
		"POSTGRES_ADMIN_DSN_FILE": unset,
		"POSTGRES_ADMIN_DSN":      "postgres://postgres:" + rand.Text() + "@postgres.internal:5432/postgres",
		"PROBES_TLS_PEM_FILE":     unset,
		"PROBES_TLS_PEM":          "junk",
	}, nil)
	require.ErrorContains(t, cause, "config POSTGRES_ADMIN_DSN_FILE: not found")
	require.ErrorContains(t, cause, "config PROBES_TLS_PEM_FILE: not found")
}

type request struct {
	method string
	path   string
	auth   string
}

type apiServer struct {
	first   sync.Once
	arrived chan request
	release chan struct{}
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.first.Do(func() {
		a.arrived <- request{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")}
		<-a.release
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`)
}

func startAPIServer(t *testing.T, c *target, api http.Handler) {
	t.Helper()
	l, err := c.h.Listen("tcp", apiServerAddr)
	require.NoError(t, err)
	srv := &http.Server{
		Handler:   api,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{c.ca.Issue(t, "apiserver").TLS()}},
	}
	var wg sync.WaitGroup
	wg.Go(func() { _ = srv.ServeTLS(l, "", "") })
	t.Cleanup(func() {
		require.NoError(t, srv.Close())
		wg.Wait()
	})
}

func TestInit_reaches_the_API_server_with_the_service_accounts_token_and_is_ready_only_once_synced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTarget(t, nil)
		api := &apiServer{arrived: make(chan request), release: make(chan struct{})}
		startAPIServer(t, c, api)

		_, status := c.start(t, nil)

		first := <-api.arrived
		require.Equal(t, http.MethodGet, first.method)
		require.Equal(t, "/apis/postgres.example.com/v1alpha1/databases", first.path)
		require.Equal(t, "Bearer "+c.token, first.auth, "the token is read from its file, trimmed of its newline")

		require.Equal(t, http.StatusServiceUnavailable, c.probe(t, "/health/readiness"))
		require.Equal(t, http.StatusOK, c.probe(t, "/health/liveness"))
		close(api.release)

		st := <-status
		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		require.ErrorContains(t, st.Cause(), "databases")
	})
}

func TestOpenPostgres_dials_through_the_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTarget(t, nil)
		l, err := c.h.Listen("tcp", "postgres.internal:5432")
		require.NoError(t, err)
		accepted := make(chan struct{})
		go func() {
			conn, err := l.Accept()
			if err == nil {
				_ = conn.Close()
			}
			close(accepted)
		}()

		cfg, err := adminDSN([]byte("postgres://postgres:" + c.password + "@postgres.internal:5432/postgres?sslmode=disable"))
		require.NoError(t, err)
		init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			admin, err := openPostgres(ctx, s, cfg)
			if err != nil {
				return nil, err
			}
			return nil, admin.Drop(ctx, "orders")
		}
		_, status := humustest.Start(t, init, humustest.WithApp(humus.WithHost(c.h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)))

		st := <-status
		<-accepted
		require.NoError(t, l.Close())
		require.Equal(t, humus.ExitFailure, st.Code(), "cause: %v", st.Cause())
		require.NotContains(t, st.Cause().Error(), c.password)
	})
}

func TestTrustPrefix(t *testing.T) {
	testCases := map[string]struct {
		prefix    string
		namespace string
		want      bool
	}{
		"a namespace with the prefix":           {prefix: "staging-", namespace: "staging-orders", want: true},
		"a namespace that is the prefix":        {prefix: "staging-", namespace: "staging-", want: true},
		"a namespace without the prefix":        {prefix: "staging-", namespace: "production-orders"},
		"a namespace with the prefix inside it": {prefix: "staging-", namespace: "orders-staging-1"},
		"an empty prefix":                       {prefix: "", namespace: "staging-orders"},
		"an empty prefix and the default":       {prefix: "", namespace: "default"},
	}
	for desc, tc := range testCases {
		t.Run(desc, func(t *testing.T) {
			obj := &metav1.ObjectMeta{Name: "orders", Namespace: tc.namespace}
			require.Equal(t, tc.want, trustPrefix(tc.prefix)(obj))
		})
	}
}

func TestInit_with_a_bad_telemetry_setting_exits_78(t *testing.T) {
	for name, value := range map[string]string{
		"OTEL_SDK_DISABLED":           "maybe",
		"OTEL_SERVICE_NAME":           "",
		"OTEL_TRACES_EXPORTER":        "zipkin",
		"OTEL_METRICS_EXPORTER":       "prometheus",
		"OTEL_LOGS_EXPORTER":          "file",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "collector",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newTarget(t, nil)
				_, status := c.startWith(t, map[string]string{name: value}, BuildTelemetry)
				st := <-status
				require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
				require.ErrorContains(t, st.Cause(), "config "+name+":")
			})
		})
	}
}

func TestBuildTelemetry_names_the_service_databases(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"by default":           {env: map[string]string{}, want: "databases"},
		"unless it is renamed": {env: map[string]string{"OTEL_SERVICE_NAME": "databases-canary"}, want: "databases-canary"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"OTEL_TRACES_EXPORTER": "console", "OTEL_LOGS_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none"}
			maps.Copy(env, tc.env)
			var stdout bytes.Buffer
			sdk, err := BuildTelemetry(t.Context(), bedrock.Process{Environment: bedrock.NewStaticEnvironment(env), Stdout: &stdout}, nil)
			require.NoError(t, err)

			_, span := sdk.TracerProvider.Tracer("test").Start(t.Context(), "probe")
			span.End()
			require.NoError(t, sdk.Shutdown(context.Background()))

			require.Contains(t, stdout.String(), `"Key":"service.name","Value":{"Type":"STRING","Value":"`+tc.want+`"}`)
		})
	}
}
