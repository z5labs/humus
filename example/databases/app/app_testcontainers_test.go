//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package app_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tck3s "github.com/testcontainers/testcontainers-go/modules/k3s"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/example/databases/app"
	"github.com/z5labs/humus/example/databases/cases"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const (
	k3sImage      = "docker.io/rancher/k3s:v1.37.1-k3s1"
	postgresImage = "docker.io/library/postgres:18.0"
)

const (
	controllerNamespace = "databases"
	controllerName      = "databases-controller"
	probesAddr          = "127.0.0.1:18443"
	waitLimit           = 2 * time.Minute
	startLimit          = 3 * time.Minute
)

func startPostgres(t *testing.T) (dsn, host, port string) {
	t.Helper()
	c, err := tcpostgres.Run(t.Context(), postgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("admin"),
		tcpostgres.WithPassword(rand.Text()),
		tcpostgres.BasicWaitStrategies(),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting PostgreSQL in a container")
	dsn, err = c.ConnectionString(t.Context(), "sslmode=disable")
	require.NoError(t, err)
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	return dsn, u.Hostname(), u.Port()
}

func startK3s(t *testing.T) (*rest.Config, []byte) {
	t.Helper()
	c, err := tck3s.Run(t.Context(), k3sImage,
		testcontainers.WithCmdArgs("--disable-agent", "--disable=metrics-server"),
		testcontainers.WithEnv(map[string]string{"K3S_TOKEN": rand.Text()}),
		testcontainers.WithWaitStrategyAndDeadline(startLimit, wait.ForLog("k3s is up and running").WithStartupTimeout(startLimit)),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting k3s in a container")
	kubeconfig, err := c.GetKubeConfig(t.Context())
	require.NoError(t, err)
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	require.NoError(t, err)
	return cfg, cfg.CAData
}

func createCRD(t *testing.T, cfg *rest.Config) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "deploy", "crd.yaml"))
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.UnmarshalStrict(b, &crd))

	ext, err := apiextensions.NewForConfig(cfg)
	require.NoError(t, err)
	crds := ext.ApiextensionsV1().CustomResourceDefinitions()
	_, err = crds.Create(t.Context(), &crd, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		got, err := crds.Get(t.Context(), crd.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				return true
			}
		}
		return false
	}, waitLimit, 250*time.Millisecond, "the CRD %s wasn't established", crd.Name)
}

func serviceAccountToken(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	kc, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ctx := t.Context()

	_, err = kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: controllerNamespace},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kc.CoreV1().ServiceAccounts(controllerNamespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: controllerName},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kc.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: controllerName},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: controllerName, Namespace: controllerNamespace}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	expiry := int64(time.Hour / time.Second)
	tr, err := kc.CoreV1().ServiceAccounts(controllerNamespace).CreateToken(ctx, controllerName, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	return tr.Status.Token
}

func writeFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}

func TestInit_passes_the_shared_cases_against_k3s_and_PostgreSQL(t *testing.T) {
	dsn, pgHost, pgPort := startPostgres(t)
	cfg, caData := startK3s(t)
	createCRD(t, cfg)
	token := serviceAccountToken(t, cfg)

	server, err := url.Parse(cfg.Host)
	require.NoError(t, err)
	dir := t.TempDir()
	ca := humustest.NewCA(t)
	p, status := humustest.Start(t, app.Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{
			"KUBERNETES_SERVICE_HOST": server.Hostname(),
			"KUBERNETES_SERVICE_PORT": server.Port(),
			"KUBERNETES_CA_FILE":      writeFile(t, dir, "ca.crt", caData),
			"KUBERNETES_TOKEN_FILE":   writeFile(t, dir, "token", []byte(token+"\n")),
			"POD_NAMESPACE":           controllerNamespace,
			"POD_NAME":                controllerName + "-0",
			"POSTGRES_ADMIN_DSN_FILE": writeFile(t, dir, "admin-dsn", []byte(dsn+"\n")),
			"POSTGRES_HOST":           pgHost,
			"POSTGRES_PORT":           pgPort,
			"PROBES_ADDR":             probesAddr,
			"PROBES_TLS_PEM_FILE":     writeFile(t, dir, "probes.pem", pemOf(t, ca.Issue(t, "127.0.0.1"))),
		}),
	)

	probes := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}},
	}
	require.Eventually(t, func() bool {
		resp, err := probes.Get("https://" + probesAddr + "/health/readiness")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, waitLimit, 100*time.Millisecond, "the controller didn't become ready")

	target := cases.Target{
		Config:      rest.CopyConfig(cfg),
		Prefix:      "databases-cases-",
		Annotation:  "postgres.example.com/failure-scenario",
		ScenarioKey: "acme.fault",
		Postgres:    dsn,
		Timeout:     waitLimit,
	}
	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, target)
			})
		}
	})

	select {
	case <-p.Notified(syscall.SIGTERM):
	case <-time.After(waitLimit):
		t.Fatal("the controller never listened for SIGTERM")
	}
	p.Signal(syscall.SIGTERM)
	select {
	case st := <-status:
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the controller didn't exit")
	}
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
