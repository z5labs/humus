//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package main

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
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
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/z5labs/humus/example/kube/kubebuilder/cases"
)

const k3sImage = "docker.io/rancher/k3s:v1.37.1-k3s1"

const (
	controllerNamespace = "memcached-system"
	serviceAccount      = "controller-manager"
	casesPrefix         = "memcached-cases-"
	waitLimit           = 2 * time.Minute
	startLimit          = 3 * time.Minute
)

func TestManager_passes_the_cases(t *testing.T) {
	ctrl.SetLogger(logr.Discard())
	admin := startK3s(t)
	createCRD(t, admin, filepath.Join("..", "config", "crd", "bases", "cache.example.com_memcacheds.yaml"))
	cfg := asServiceAccount(t, admin,
		filepath.Join("..", "config", "rbac", "role.yaml"),
		filepath.Join("..", "config", "rbac", "leader_election_role.yaml"),
	)

	mgr, err := newManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		LeaderElection:          true,
		LeaderElectionID:        "4b13e8ac.example.com",
		LeaderElectionNamespace: controllerNamespace,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	select {
	case <-mgr.Elected():
	case err := <-done:
		t.Fatalf("the manager stopped before it was elected: %v", err)
	case <-time.After(waitLimit):
		t.Fatal("the manager wasn't elected")
	}

	t.Run("in parallel", func(t *testing.T) {
		for _, c := range cases.All {
			t.Run(c.Name, func(t *testing.T) {
				t.Parallel()
				c.Run(t, cases.Target{Config: rest.CopyConfig(admin), Prefix: casesPrefix, Timeout: waitLimit})
			})
		}
	})

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("the manager didn't stop")
	}
}

func startK3s(t *testing.T) *rest.Config {
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
	return cfg
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

func asServiceAccount(t *testing.T, admin *rest.Config, clusterRolePath, rolePath string) *rest.Config {
	t.Helper()
	ctx := t.Context()
	kc, err := kubernetes.NewForConfig(admin)
	require.NoError(t, err)

	var clusterRole rbacv1.ClusterRole
	unmarshalFile(t, clusterRolePath, &clusterRole)
	var role rbacv1.Role
	unmarshalFile(t, rolePath, &role)
	subjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount, Namespace: controllerNamespace}}

	_, err = kc.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: controllerNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = kc.CoreV1().ServiceAccounts(controllerNamespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccount}}, metav1.CreateOptions{})
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
	tr, err := kc.CoreV1().ServiceAccounts(controllerNamespace).CreateToken(ctx, serviceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	return &rest.Config{
		Host:            admin.Host,
		TLSClientConfig: rest.TLSClientConfig{CAData: admin.CAData},
		BearerToken:     tr.Status.Token,
	}
}

func unmarshalFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, yaml.UnmarshalStrict(b, v), "%s", path)
}
