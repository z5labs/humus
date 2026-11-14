//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
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
	"github.com/z5labs/humus/example/kube/kubebuilder/cases"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

const k3sImage = "docker.io/rancher/k3s:v1.37.1-k3s1"

const (
	controllerNamespace = "memcached-system"
	controllerName      = "4b13e8ac.example.com"
	serviceAccountName  = "controller-manager"
	casesPrefix         = "memcached-cases-"
	waitLimit           = 2 * time.Minute
	startLimit          = 3 * time.Minute
)

func TestInit_passes_the_cases(t *testing.T) {
	admin, caData := startK3s(t)
	createCRD(t, admin, filepath.Join("config", "crd", "bases", "cache.example.com_memcacheds.yaml"))
	token := serviceAccountToken(t, admin,
		filepath.Join("config", "rbac", "role.yaml"),
		filepath.Join("config", "rbac", "leader_election_role.yaml"),
	)

	server, err := url.Parse(admin.Host)
	require.NoError(t, err)
	ca := humustest.NewCA(t)
	certPEM, keyPEM := pemPair(t, ca.Issue(t, "127.0.0.1"))
	dir := t.TempDir()
	probesAddr := freeAddr(t)
	p, status := humustest.Start(t, Init,
		humustest.WithApp(humus.WithHost(humus.OSHost()), humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{
			"KUBERNETES_SERVICE_HOST": server.Hostname(),
			"KUBERNETES_SERVICE_PORT": server.Port(),
			"KUBERNETES_CA_FILE":      writeFile(t, dir, "ca.crt", caData),
			"KUBERNETES_TOKEN_FILE":   writeFile(t, dir, "token", []byte(token+"\n")),
			"CONTROLLER_NAME":         controllerName,
			"POD_NAMESPACE":           controllerNamespace,
			"POD_NAME":                "controller-manager-0",
			"PROBES_ADDR":             probesAddr,
			"PROBES_TLS_CERT_FILE":    writeFile(t, dir, "tls.crt", certPEM),
			"PROBES_TLS_KEY_FILE":     writeFile(t, dir, "tls.key", keyPEM),
		}),
	)

	probes := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}},
	}
	for ready := time.Now(); ; time.Sleep(100 * time.Millisecond) {
		select {
		case st := <-status:
			require.FailNowf(t, "the controller exited", "exit %d: %v", st.Code(), st.Cause())
		default:
		}
		if resp, err := probes.Get("https://" + probesAddr + "/health/readiness"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		require.Less(t, time.Since(ready), waitLimit, "the controller never turned ready")
	}

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, cases.Target{Config: rest.CopyConfig(admin), Prefix: casesPrefix, Timeout: waitLimit})
			})
		}
	})

	kc, err := kubernetes.NewForConfig(admin)
	require.NoError(t, err)
	lease, err := kc.CoordinationV1().Leases(controllerNamespace).Get(t.Context(), controllerName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "controller-manager-0", *lease.Spec.HolderIdentity)

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

	lease, err = kc.CoordinationV1().Leases(controllerNamespace).Get(t.Context(), controllerName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, lease.Spec.HolderIdentity, "the Lease is released on SIGTERM")
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

func createCRD(t *testing.T, cfg *rest.Config, path string) {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	unmarshalFile(t, path, &crd)

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

func serviceAccountToken(t *testing.T, admin *rest.Config, clusterRolePath, rolePath string) string {
	t.Helper()
	ctx := t.Context()
	kc, err := kubernetes.NewForConfig(admin)
	require.NoError(t, err)

	var clusterRole rbacv1.ClusterRole
	unmarshalFile(t, clusterRolePath, &clusterRole)
	var role rbacv1.Role
	unmarshalFile(t, rolePath, &role)
	subjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccountName, Namespace: controllerNamespace}}

	_, err = kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kc.CoreV1().ServiceAccounts(controllerNamespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kc.RbacV1().ClusterRoles().Create(ctx, &clusterRole, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kc.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: clusterRole.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole.Name},
		Subjects:   subjects,
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kc.RbacV1().Roles(controllerNamespace).Create(ctx, &role, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kc.RbacV1().RoleBindings(controllerNamespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   subjects,
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	expiry := int64(time.Hour / time.Second)
	tr, err := kc.CoreV1().ServiceAccounts(controllerNamespace).CreateToken(ctx, serviceAccountName, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	return tr.Status.Token
}

func unmarshalFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, yaml.UnmarshalStrict(b, v), "%s", path)
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

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func writeFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	return path
}
