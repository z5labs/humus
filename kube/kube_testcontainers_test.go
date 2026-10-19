//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

// The tests in this file run a Kubernetes API server, k3s's, in a container
// started with testcontainers-go, and reach it through humus.OSHost(). No
// API server runs on humustest's Host, since humus writes no fake of
// another project's server, so what needs the API server itself, such as
// whether it accepts a reflected CRD, is tested here. They open real
// sockets, so they sit behind the testcontainers build tag and run in a CI
// job of their own, outside the coverage gate:
//
//	go -C kube test -race -tags testcontainers ./...
//
// k3s runs as server --disable-agent: the API server and the controller
// manager, without a kubelet, so no pod runs. Rootless Podman can't run
// k3s's agent, since it doesn't delegate the cpuset controller.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tck3s "github.com/testcontainers/testcontainers-go/modules/k3s"
	"github.com/testcontainers/testcontainers-go/wait"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	apiwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/kube"
	"github.com/z5labs/humus/telemetry"
)

// k3sImage is the k3s the tests run, pinned to an exact release.
const k3sImage = "docker.io/rancher/k3s:v1.37.1-k3s1"

// k3s is a Kubernetes API server in a container, and the Connection that
// reaches it with the cluster admin's client certificate.
type k3s struct {
	once      sync.Once
	conn      kube.Connection
	container testcontainers.Container
	err       error
}

// cluster is the one k3s every test in the package shares, started by the
// first test that needs it and stopped by TestMain.
var cluster k3s

func TestMain(m *testing.M) {
	m.Run()
	if cluster.container != nil {
		// The tests have finished; a container left behind is reaped by
		// testcontainers' own reaper.
		_ = cluster.container.Terminate(context.Background())
	}
}

// k3sClient returns a Client of the shared k3s, starting it on first use.
func k3sClient(t *testing.T) *kube.Client {
	t.Helper()
	cluster.once.Do(func() {
		// Overwritten when the start returns, so a start that fails t
		// rather than returning still leaves an error behind.
		cluster.err = errors.New("starting k3s did not finish")
		start := time.Now()
		cluster.err = startK3s()
		// The job's log shows how long k3s took to start, with go test
		// -v, under the test that started it.
		t.Logf("starting k3s, pulling %s included, took %s", k3sImage, time.Since(start).Round(time.Millisecond))
	})
	require.NoError(t, cluster.err, "starting k3s in a container")

	c, err := kube.NewClient(&humus.Setup{Host: humus.OSHost(), Telemetry: telemetry.Noop()}, cluster.conn)
	require.NoError(t, err)
	return c
}

// startK3s starts k3s's API server, and fills in cluster's Connection from
// the kubeconfig k3s writes, with the cluster's CA and the admin's client
// certificate.
func startK3s() error {
	ctx := context.Background()
	c, err := tck3s.Run(ctx, k3sImage,
		// The module starts a server with its agent; the tests need only
		// the API server. Its certificate is for localhost, among others.
		testcontainers.WithCmd("server", "--disable-agent", "--disable=traefik"),
		testcontainers.WithWaitStrategy(wait.ForLog("k3s is up and running").WithStartupTimeout(3*time.Minute)),
	)
	if c != nil {
		cluster.container = c
	}
	if err != nil {
		return err
	}

	b, err := c.GetKubeConfig(ctx)
	if err != nil {
		return err
	}
	kc, err := clientcmd.Load(b)
	if err != nil {
		return err
	}
	current := kc.Contexts[kc.CurrentContext]
	server, user := kc.Clusters[current.Cluster], kc.AuthInfos[current.AuthInfo]
	u, err := url.Parse(server.Server)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(server.CertificateAuthorityData) {
		return errors.New("the kubeconfig has no CA")
	}
	cert, err := tls.X509KeyPair(user.ClientCertificateData, user.ClientKeyData)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return err
	}
	cluster.conn = kube.Connection{
		Server: net.JoinHostPort(host, port),
		TLS: &tls.Config{
			RootCAs:      roots,
			Certificates: []tls.Certificate{cert},
			// The container's host may be any name the Docker daemon
			// has; the API server's certificate is always for localhost.
			ServerName: "localhost",
		},
	}
	return nil
}

// createCRD creates crd through client, and waits for the API server to
// serve its resource. It deletes the CRD when the test ends.
func createCRD(t *testing.T, client *kube.Client, crd *apiextensionsv1.CustomResourceDefinition) {
	t.Helper()
	ctx := t.Context()

	ext, err := apiextensions.NewForConfig(client.Config())
	require.NoError(t, err)
	crds := ext.ApiextensionsV1().CustomResourceDefinitions()
	_, err = crds.Create(ctx, crd, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = crds.Delete(context.Background(), crd.Name, metav1.DeleteOptions{})
	})

	// The resource is served once the CRD is established.
	err = apiwait.PollUntilContextTimeout(ctx, 250*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		got, err := crds.Get(ctx, crd.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, c := range got.Status.Conditions {
			if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, err, "waiting for the CRD %s to be established", crd.Name)
}

func TestResource_CRD_is_accepted_by_the_API_server(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)

	crd, err := Memcached.CRD()
	require.NoError(t, err)
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	memcacheds := dyn.Resource(Memcached.GVR()).Namespace("default")

	want := kube.Object[MemcachedSpec, MemcachedStatus]{
		TypeMeta:   metav1.TypeMeta{APIVersion: "cache.example.com/v1alpha1", Kind: "Memcached"},
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "default"},
		Spec:       MemcachedSpec{Size: 3, Image: "memcached:1.6.26-alpine3.19"},
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&want)
	require.NoError(t, err)
	_, err = memcacheds.Create(ctx, &unstructured.Unstructured{Object: u}, metav1.CreateOptions{FieldValidation: "Strict"})
	require.NoError(t, err)

	read, err := memcacheds.Get(ctx, "cache", metav1.GetOptions{})
	require.NoError(t, err)
	var got kube.Object[MemcachedSpec, MemcachedStatus]
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(read.Object, &got))
	require.Equal(t, want.Spec, got.Spec)
	require.Equal(t, "Memcached", got.Kind)
	require.Equal(t, "cache", got.Name)

	// The schema is the API server's: a Memcached without its required
	// size is refused.
	_, err = memcacheds.Create(ctx, memcached("no-size", map[string]any{"image": "memcached:1.6"}), metav1.CreateOptions{FieldValidation: "Strict"})
	require.ErrorContains(t, err, "spec.size: Required value")
}

// memcached returns a Memcached named name, in the default namespace, with
// spec as it is given.
func memcached(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cache.example.com/v1alpha1",
		"kind":       "Memcached",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec":       spec,
	}}
}

func TestResource_CRD_rules_are_enforced_by_the_API_server(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)

	crd, err := Memcached.CRD()
	require.NoError(t, err)
	// A name of its own, so the CRD the test above deletes isn't in the
	// way while it terminates.
	crd.Name = "memcacheds.rules.example.com"
	crd.Spec.Group = "rules.example.com"
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	gvr := Memcached.GVR()
	gvr.Group = "rules.example.com"
	memcacheds := dyn.Resource(gvr).Namespace("default")
	create := func(name string, spec map[string]any) (*unstructured.Unstructured, error) {
		obj := memcached(name, spec)
		obj.SetAPIVersion("rules.example.com/v1alpha1")
		return memcacheds.Create(ctx, obj, metav1.CreateOptions{FieldValidation: "Strict"})
	}

	_, err = create("too-small", map[string]any{"size": int64(0), "image": "memcached:1.6"})
	require.ErrorContains(t, err, "spec.size in body should be greater than or equal to 1")

	_, err = create("too-large", map[string]any{"size": int64(4), "image": "memcached:1.6"})
	require.ErrorContains(t, err, "spec.size in body should be less than or equal to 3")

	_, err = create("redis", map[string]any{"size": int64(1), "image": "redis:7"})
	require.ErrorContains(t, err, "spec.image in body should match '^memcached:'")

	created, err := create("defaulted", map[string]any{"size": int64(1)})
	require.NoError(t, err)
	var got kube.Object[MemcachedSpec, MemcachedStatus]
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(created.Object, &got))
	require.Equal(t, MemcachedSpec{Size: 1, Image: "memcached:1.6.26-alpine3.19"}, got.Spec)
}

func TestResource_CRD_validation_rules_are_enforced_by_the_API_server(t *testing.T) {
	ctx := t.Context()
	client := k3sClient(t)

	crd, err := Widget.CRD()
	require.NoError(t, err)
	crd.Name = "widgets.rules.humus.dev"
	crd.Spec.Group = "rules.humus.dev"
	createCRD(t, client, crd)

	dyn, err := dynamic.NewForConfig(client.Config())
	require.NoError(t, err)
	gvr := Widget.GVR()
	gvr.Group = "rules.humus.dev"
	widgets := dyn.Resource(gvr).Namespace("default")
	create := func(name string, change func(spec map[string]any)) error {
		spec := map[string]any{
			"name":     name,
			"ports":    []any{map[string]any{"name": "http", "port": int64(80)}},
			"template": map[string]any{"image": "memcached:1.6"},
			"timeout":  "1m",
			"memory":   "64Mi",
			"extra":    nil,
		}
		change(spec)
		_, err := widgets.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "rules.humus.dev/v1beta1",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       spec,
		}}, metav1.CreateOptions{FieldValidation: "Strict"})
		return err
	}

	require.NoError(t, create("ok", func(map[string]any) {}))

	// The Widget's own CEL rule, from TypeRules.Validation.
	err = create("negative", func(spec map[string]any) { spec["replicas"] = int64(-1) })
	require.ErrorContains(t, err, "replicas must not be negative")

	// The template field's CEL rule, from FieldOf.Validation.
	err = create("no-image", func(spec map[string]any) { spec["template"] = map[string]any{"image": ""} })
	require.ErrorContains(t, err, "a template names its image")

	// The version's schema, from Schema.
	err = create("bad-version", func(spec map[string]any) { spec["version"] = "one" })
	require.ErrorContains(t, err, "spec.version in body should match")
}

func TestResource_CRD_of_every_mapped_type_is_accepted_by_the_API_server(t *testing.T) {
	client := k3sClient(t)

	// The golden test's Widget holds a field of each type CRD maps, and a
	// constraint of each kind, so the API server checks each mapping is a
	// structural schema it accepts, and compiles each CEL rule.
	crd, err := Widget.CRD()
	require.NoError(t, err)
	createCRD(t, client, crd)
}
