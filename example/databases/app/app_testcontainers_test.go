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
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tck3s "github.com/testcontainers/testcontainers-go/modules/k3s"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/fit"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
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
	casesPrefix         = "databases-cases-"
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

type controller struct {
	p      *bedrocktest.Process
	status <-chan bedrock.ExitStatus
	target cases.Target
	token  string
}

func startController(t *testing.T, build telemetry.Builder) controller {
	t.Helper()
	dsn, pgHost, pgPort := startPostgres(t)
	cfg, caData := startK3s(t)
	createCRD(t, cfg)
	token := serviceAccountToken(t, cfg)

	server, err := url.Parse(cfg.Host)
	require.NoError(t, err)
	dir := t.TempDir()
	ca := humustest.NewCA(t)
	p, status := humustest.Start(t, app.Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(build), humus.WithFaults(app.Faults)),
		humustest.WithEnv(map[string]string{
			"KUBERNETES_SERVICE_HOST":        server.Hostname(),
			"KUBERNETES_SERVICE_PORT":        server.Port(),
			"KUBERNETES_CA_FILE":             writeFile(t, dir, "ca.crt", caData),
			"KUBERNETES_TOKEN_FILE":          writeFile(t, dir, "token", []byte(token+"\n")),
			"POD_NAMESPACE":                  controllerNamespace,
			"POD_NAME":                       controllerName + "-0",
			"POSTGRES_ADMIN_DSN_FILE":        writeFile(t, dir, "admin-dsn", []byte(dsn+"\n")),
			"POSTGRES_HOST":                  pgHost,
			"POSTGRES_PORT":                  pgPort,
			"PROBES_ADDR":                    probesAddr,
			"PROBES_TLS_PEM_FILE":            writeFile(t, dir, "probes.pem", pemOf(t, ca.Issue(t, "127.0.0.1"))),
			"FAULT_TRUSTED_NAMESPACE_PREFIX": casesPrefix,
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

	return controller{
		p:      p,
		status: status,
		target: cases.Target{
			Config:      rest.CopyConfig(cfg),
			Prefix:      casesPrefix,
			Annotation:  "postgres.example.com/failure-scenario",
			ScenarioKey: "acme.fault",
			Postgres:    dsn,
			Timeout:     waitLimit,
		},
		token: token,
	}
}

func (c controller) stop(t *testing.T) {
	t.Helper()
	select {
	case <-c.p.Notified(syscall.SIGTERM):
	case <-time.After(waitLimit):
		t.Fatal("the controller never listened for SIGTERM")
	}
	c.p.Signal(syscall.SIGTERM)
	select {
	case st := <-c.status:
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	case <-time.After(waitLimit):
		t.Fatal("the controller didn't exit")
	}
}

func TestInit_passes_the_shared_cases_against_k3s_and_PostgreSQL(t *testing.T) {
	c := startController(t, telemetry.NoopBuilder)

	t.Run("in parallel", func(t *testing.T) {
		for _, tc := range cases.All {
			t.Run(tc.Name, func(t *testing.T) {
				t.Parallel()
				tc.Run(t, c.target)
			})
		}
		for name, run := range faultCases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				run(t, c.target)
			})
		}
	})

	c.stop(t)
}

const (
	provisionDelay = 3 * time.Second
	reconcilesDone = 3 * provisionDelay
	clock          = "15:04:05.000"
)

func TestInit_a_Secret_deleted_while_a_reconcile_is_in_flight_rotates_the_password(t *testing.T) {
	tel := humustest.NewTelemetry()
	c := startController(t, tel.Builder())
	f := newFaults(t, c.target)

	namespaces := map[string]string{}
	t.Run("deleted while the reconcile after Ready waits to provision", func(t *testing.T) {
		ns := f.namespace(t, c.target.Prefix)
		namespaces[t.Name()] = ns
		rotate(t, f, ns, time.Second)
	})
	t.Run("deleted once the reconciles have settled", func(t *testing.T) {
		ns := f.namespace(t, c.target.Prefix)
		namespaces[t.Name()] = ns
		rotate(t, f, ns, reconcilesDone)
	})

	c.stop(t)

	spans := tel.Spans()
	for name, ns := range namespaces {
		t.Logf("%s (namespace %s):", name, ns)
		for _, r := range reconcilesOf(spans, ns, "orders") {
			t.Logf("  reconcile %s to %s, %s", r.StartTime().Format(clock), r.EndTime().Format(clock), r.Status().Code)
			for _, s := range childrenOf(spans, r) {
				if u, ok := attr(s.Attributes(), "url.full"); ok && strings.Contains(u.AsString(), "/secrets/") {
					t.Logf("    %s the Secret at %s", s.Name(), s.StartTime().Format(clock))
				}
				if v, ok := attr(s.Attributes(), "db.query.summary"); ok {
					t.Logf("    %s at %s", v.AsString(), s.StartTime().Format(clock))
				}
			}
		}
	}
}

func rotate(t *testing.T, f *faults, ns string, wait time.Duration) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	secrets := f.kube.CoreV1().Secrets(ns)
	var watched sync.WaitGroup
	var mu sync.Mutex
	var events []string
	record := func(what string, w watch.Interface) {
		watched.Go(func() {
			for e := range w.ResultChan() {
				m, err := meta.Accessor(e.Object)
				if err != nil {
					continue
				}
				line := fmt.Sprintf("%s %s %s %s uid %.8s resourceVersion %s", time.Now().Format(clock), what, e.Type, m.GetName(), m.GetUID(), m.GetResourceVersion())
				if fields := m.GetManagedFields(); len(fields) > 0 {
					last := slices.MaxFunc(fields, func(a, b metav1.ManagedFieldsEntry) int { return a.Time.Compare(b.Time.Time) })
					line += fmt.Sprintf(", last written by %s %s %s", last.Manager, last.Operation, last.Subresource)
				}
				mu.Lock()
				events = append(events, line)
				mu.Unlock()
			}
		})
	}
	sw, err := secrets.Watch(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	record("Secret", sw)
	dw, err := f.dynamic.Resource(databases).Namespace(ns).Watch(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	record("Database", dw)

	f.create(t, ns, "orders", f.scenario(t, fit.Rule{Point: "postgresql:provision", Fault: fit.Fault{Delay: provisionDelay}}))
	f.awaitReady(t, ns, "orders", metav1.ConditionTrue, "Provisioned")
	t.Logf("Ready at %s", time.Now().Format(clock))
	old, err := secrets.Get(t.Context(), "orders-postgres", metav1.GetOptions{})
	require.NoError(t, err)
	oldPassword := string(old.Data["password"])

	time.Sleep(wait)
	t.Logf("deleting the Secret uid %.8s at %s, %s after Ready", old.UID, time.Now().Format(clock), wait)
	err = secrets.Delete(t.Context(), old.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &old.UID}})
	require.NoError(t, err)

	f.await(t, "a new Secret", func(ctx context.Context) (bool, error) {
		s, err := secrets.Get(ctx, old.Name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return s.UID != old.UID, nil
	})
	rotated, err := secrets.Get(t.Context(), old.Name, metav1.GetOptions{})
	require.NoError(t, err)
	t.Logf("the new Secret uid %.8s came at %s, with the old password: %t", rotated.UID, time.Now().Format(clock), string(rotated.Data["password"]) == oldPassword)

	time.Sleep(reconcilesDone)
	settled, err := secrets.Get(t.Context(), old.Name, metav1.GetOptions{})
	require.NoError(t, err)
	t.Logf("at %s: the Secret uid %.8s, with the old password: %t", time.Now().Format(clock), settled.UID, string(settled.Data["password"]) == oldPassword)

	cancel()
	watched.Wait()
	for _, e := range events {
		t.Log("watched: " + e)
	}

	require.False(t, oldPassword == string(rotated.Data["password"]), "the new Secret holds the old password")
	require.False(t, oldPassword == string(settled.Data["password"]), "the rotation was lost: the Secret still holds the old password")
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
