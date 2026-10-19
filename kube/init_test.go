// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/fit"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/fault/jsonscenario"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/kube"
)

// The tests in this file run the code of example_test.go, which the
// Kubernetes page of the docs site quotes. None of them needs a cluster:
// the Connections reach an API server of the test's own on humustest's
// Host, and the Controllers run on client-go's fakes.

// certPEM returns the PEM encoding of a certificate's DER.
func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// caPEM returns the PEM encoding of ca's certificate, as a service account's
// ca.crt holds it.
func caPEM(t *testing.T, ca *humustest.CA) []byte {
	t.Helper()
	// The chain a certificate's TLS returns ends with the CA's own.
	chain := ca.Issue(t, "unused").TLS().Certificate
	return certPEM(chain[len(chain)-1])
}

// podSetup returns a Setup on h whose environment holds env, as the
// kubelet sets it.
func podSetup(t *testing.T, h *humustest.Host, env map[string]string) *humus.Setup {
	t.Helper()
	s := newSetup(t, h.Host(), humustest.NewTelemetry())
	s.Process.Environment = bedrock.NewStaticEnvironment(env)
	return s
}

func TestInit_without_its_variables_exits_78(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		_, status := humustest.Start(t, Init)

		st := <-status
		require.Equal(t, humus.ExitConfig, st.Code(), "cause: %v", st.Cause())
		require.EqualError(t, st.Cause(), "init: config CONTROLLER_NAME: not found\n"+
			"config POD_NAMESPACE: not found\n"+
			"config POD_NAME: not found")
	})
}

func TestInPod_reaches_the_API_server_with_the_service_accounts_token(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		first, second := newToken(), newToken()
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"serviceaccount/ca.crt": caPEM(t, ca),
			"serviceaccount/token":  []byte(first + "\n"),
		}))
		api := &apiServer{}
		defer startAPIServer(t, h, ca, server, api, "apiserver")()

		s := podSetup(t, h, map[string]string{
			"KUBERNETES_SERVICE_HOST": "apiserver",
			"KUBERNETES_SERVICE_PORT": "6443",
		})
		conn, err := inPod(t.Context(), s, h.Path("serviceaccount")+"/")
		require.NoError(t, err)
		require.Equal(t, server, conn.Server)

		c, err := kube.NewClient(s, conn)
		require.NoError(t, err)
		kc := newClientset(t, c)
		require.NoError(t, getConfigMap(t.Context(), kc, "settings"), "the CA verifies the API server")

		// The kubelet rotates the token; the Client reads it again within a minute.
		f, err := h.FS().OpenFile("serviceaccount/token", os.O_WRONLY|os.O_TRUNC, 0)
		require.NoError(t, err)
		_, err = io.WriteString(f, second+"\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		time.Sleep(time.Minute)
		require.NoError(t, getConfigMap(t.Context(), kc, "settings"))

		reqs := api.received()
		require.Len(t, reqs, 2)
		require.Equal(t, "Bearer "+first, reqs[0].auth, "the token is trimmed of its newline")
		require.Equal(t, "Bearer "+second, reqs[1].auth)
	})
}

func TestInPod_brackets_an_IPv6_host(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"serviceaccount/ca.crt": caPEM(t, humustest.NewCA(t)),
		}))
		conn, err := inPod(t.Context(), podSetup(t, h, map[string]string{
			"KUBERNETES_SERVICE_HOST": "fd00:10:96::1",
			"KUBERNETES_SERVICE_PORT": "443",
		}), h.Path("serviceaccount")+"/")
		require.NoError(t, err)
		require.Equal(t, "[fd00:10:96::1]:443", conn.Server)
		require.NoError(t, kube.CheckServer(conn.Server))
	})
}

func TestInPod_without_the_service_accounts_CA_fails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		_, err := inPod(t.Context(), podSetup(t, h, nil), h.Path("serviceaccount")+"/")
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

// kubeconfig writes the kubeconfig of a cluster at server, which ca signs
// the certificate of, with a client certificate from ca, as kind and k3s
// write theirs: everything embedded.
func kubeconfig(t *testing.T, ca *humustest.CA, server string) []byte {
	t.Helper()
	client := ca.Issue(t, "admin").TLS()
	key, err := x509.MarshalPKCS8PrivateKey(client.PrivateKey)
	require.NoError(t, err)
	b, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"dev": {Server: server, CertificateAuthorityData: caPEM(t, ca)},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"admin": {
				ClientCertificateData: certPEM(client.Certificate[0]),
				ClientKeyData:         pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
			},
		},
		Contexts:       map[string]*clientcmdapi.Context{"dev": {Cluster: "dev", AuthInfo: "admin"}},
		CurrentContext: "dev",
	})
	require.NoError(t, err)
	return b
}

func TestFromKubeconfig_reaches_the_API_server_with_the_client_certificate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"home/.kube/config": kubeconfig(t, ca, "https://"+server),
		}))
		var (
			mu    sync.Mutex
			peers []string
		)
		api := &apiServer{}
		l, err := h.Listen("tcp", server)
		require.NoError(t, err)
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				for _, c := range r.TLS.PeerCertificates {
					peers = append(peers, c.Subject.CommonName+" "+c.DNSNames[0])
				}
				mu.Unlock()
				api.ServeHTTP(w, r)
			}),
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{ca.Issue(t, "apiserver").TLS()},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    ca.Pool(),
			},
		}
		done := make(chan error, 1)
		go func() { done <- srv.ServeTLS(l, "", "") }()

		conn, err := fromKubeconfig(h.FS(), "home/.kube/config")
		require.NoError(t, err)
		require.Equal(t, server, conn.Server)
		require.Nil(t, conn.Token, "the client certificate authenticates")

		c, err := kube.NewClient(newSetup(t, h.Host(), humustest.NewTelemetry()), conn)
		require.NoError(t, err)
		require.NoError(t, getConfigMap(t.Context(), newClientset(t, c), "settings"))
		reqs := api.received()
		require.Len(t, reqs, 1)
		require.False(t, reqs[0].hasAuth)
		mu.Lock()
		require.NotEmpty(t, peers, "the API server verified the client certificate")
		mu.Unlock()

		require.NoError(t, srv.Close())
		require.ErrorIs(t, <-done, http.ErrServerClosed)
	})
}

func TestFromKubeconfig_reports_what_it_lacks(t *testing.T) {
	t.Parallel()
	ca := humustest.NewCA(t)
	noContext, err := clientcmd.Write(clientcmdapi.Config{CurrentContext: "dev"})
	require.NoError(t, err)
	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
		"no-context": noContext,
		"valid":      kubeconfig(t, ca, "https://"+server),
	}))

	_, err = fromKubeconfig(h.FS(), "missing")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = fromKubeconfig(h.FS(), "no-context")
	require.EqualError(t, err, `the kubeconfig has no context "dev"`)
	_, err = fromKubeconfig(h.FS(), "valid")
	require.NoError(t, err)
}

// deletes is the end user's backups in a test: it records what is deleted.
type deletes struct {
	mu   sync.Mutex
	objs []string
}

func (d *deletes) Delete(_ context.Context, namespace, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objs = append(d.objs, namespace+"/"+name)
	return nil
}

func (d *deletes) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.objs...)
}

func TestFinalizing_deletes_the_backups_before_the_Memcached_is_deleted(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCluster(t, memcachedObject("cache", 2))
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		b := &deletes{}
		ctrl, err := finalizing(s, testClient(t, s), testSettings(), fc.kube, b)
		require.NoError(t, err)
		kube.SetClients(ctrl, fc.dyn, fc.meta, fc.kube.CoordinationV1())
		sv := serve(t, ctrl)
		time.Sleep(settle)

		require.Equal(t, []string{"cache.example.com/finalizer"}, fc.object(t, "cache").GetFinalizers())
		d, err := fc.kube.AppsV1().Deployments("default").Get(t.Context(), "cache", metav1.GetOptions{})
		require.NoError(t, err, "the Reconciler applied the Deployment")
		require.Equal(t, int32(2), *d.Spec.Replicas)
		require.Empty(t, b.all())

		fc.deleting(t, "cache")
		time.Sleep(settle)
		require.Equal(t, []string{"default/cache"}, b.all())
		require.True(t, fc.gone("cache"), "the deletion completes")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestDefaultDeny_applies_a_policy_the_Namespace_controls(t *testing.T) {
	t.Parallel()
	kc := kubefake.NewClientset()
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: "team-a-uid"}}

	after, err := DefaultDeny{Kube: kc}.Reconcile(t.Context(), ns)
	require.NoError(t, err)
	require.Zero(t, after, "reconciled again when it changes")

	p, err := kc.NetworkingV1().NetworkPolicies("team-a").Get(t.Context(), "default-deny", metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, p.Spec.PodSelector.MatchLabels, "every pod")
	require.Empty(t, p.Spec.Ingress, "no ingress")
	owner := metav1.GetControllerOf(p)
	require.NotNil(t, owner)
	require.Equal(t, ns.UID, owner.UID)
	require.Equal(t, "Namespace", owner.Kind)
}

func TestDefaultDeny_retries_a_failed_apply(t *testing.T) {
	t.Parallel()
	kc := kubefake.NewClientset()
	kc.PrependReactor("patch", "networkpolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the namespace is terminating")
	})
	_, err := DefaultDeny{Kube: kc}.Reconcile(t.Context(), corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})
	require.ErrorContains(t, err, "the namespace is terminating")
}

func TestDefaultDeny_gives_every_Namespace_its_policy(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeClusterOf(t, Namespaces.GVR, "NamespaceList", "", namespaceObject("team-a"), namespaceObject("team-b"))
		s := newSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
		ctrl, err := defaultDeny(s, testClient(t, s), testSettings(), fc.kube)
		require.NoError(t, err)
		kube.SetClients(ctrl, fc.dyn, fc.meta, fc.kube.CoordinationV1())
		sv := serve(t, ctrl)
		time.Sleep(settle)

		for _, ns := range []string{"team-a", "team-b"} {
			_, err := fc.kube.NetworkingV1().NetworkPolicies(ns).Get(t.Context(), "default-deny", metav1.GetOptions{})
			require.NoError(t, err, ns)
		}

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

func TestWithProbes_the_pod_is_ready_once_the_caches_have_synced(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const readiness = "https://localhost:8081/health/readiness"
		ca := humustest.NewCA(t)
		h := humustest.NewHost(t)
		fc := newFakeCluster(t, memcachedObject("cache", 2))
		// The API server doesn't answer the first list until the test lets it.
		answer := make(chan struct{})
		fc.dyn.PrependReactor("list", Memcached.Plural, func(clienttesting.Action) (bool, runtime.Object, error) {
			<-answer
			return false, nil, nil
		})
		probeSettings := httphealth.Settings{
			Addr: ":8081",
			TLS:  &tls.Config{Certificates: []tls.Certificate{ca.Issue(t, "localhost").TLS()}},
		}

		p, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			ctrl, err := kube.NewController(s, testClient(t, s), testSettings(), Memcached, Reconciler{Kube: fc.kube})
			if err != nil {
				return nil, err
			}
			kube.SetClients(ctrl, fc.dyn, fc.meta, fc.kube.CoordinationV1())
			return withProbes(s, ctrl, probeSettings)
		}, humustest.WithApp(humus.WithHost(h.Host())))
		<-h.Listening("localhost:8081")
		synctest.Wait()
		require.Equal(t, http.StatusServiceUnavailable, probe(t, h, ca, readiness), "ready before the caches have synced")

		close(answer)
		time.Sleep(settle)
		require.Equal(t, http.StatusOK, probe(t, h, ca, readiness), "not ready once the caches have synced")

		p.Signal(syscall.SIGTERM)
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
	})
}

// probe GETs url with h's HTTP client, trusting only ca, and returns the
// status code.
func probe(t *testing.T, h *humustest.Host, ca *humustest.CA, url string) int {
	t.Helper()
	c := h.HTTPClient()
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
	resp, err := c.Get(url)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

// abortsDeployments records, for each reconcile of a Memcached, whether
// the point of an apply of a Deployment aborts with the scenario its
// context carries.
type abortsDeployments struct {
	mu      sync.Mutex
	aborted map[string][]bool
}

// reconciler returns a Reconciler that records into a with s.Faults, and
// settles.
func (a *abortsDeployments) reconciler(s *humus.Setup) kube.ReconcilerFunc[MemcachedSpec, MemcachedStatus] {
	return func(ctx context.Context, m kube.Object[MemcachedSpec, MemcachedStatus]) (kube.Result[MemcachedStatus], error) {
		f := s.Faults.Eval(ctx, "kube:deployments.apps:patch")
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.aborted == nil {
			a.aborted = map[string][]bool{}
		}
		a.aborted[m.Namespace+"/"+m.Name] = append(a.aborted[m.Namespace+"/"+m.Name], f.Abort)
		return kube.Result[MemcachedStatus]{}, nil
	}
}

// of returns whether each reconcile of the Memcached namespace/name
// aborted the point, in order.
func (a *abortsDeployments) of(object string) []bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bool(nil), a.aborted[object]...)
}

func TestUnderTest_a_staging_Memcached_carries_the_scenario_until_the_experiment_ends(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		staged := memcachedObject("cache", 2)
		staged.SetNamespace("staging")
		fc := newFakeCluster(t, staged, memcachedObject("cache", 2))
		tel := humustest.NewTelemetry()
		s := faultsSetup(t, humus.Host{Dial: noDial}, tel)
		rec := &abortsDeployments{}
		ctrl, err := underTest(s, testClient(t, s), testSettings(), rec.reconciler(s))
		require.NoError(t, err)
		kube.SetClients(ctrl, fc.dyn, fc.meta, fc.kube.CoordinationV1())
		sv := serve(t, ctrl)
		time.Sleep(settle)

		codec, err := jsonscenario.New("acme.fault")
		require.NoError(t, err)
		value, err := annotationValue(codec, fit.Scenario{Rules: []fit.Rule{
			{Point: "kube:deployments.apps:patch", Fault: fit.Fault{Abort: true}},
		}})
		require.NoError(t, err)

		staging := fc.dyn.Resource(Memcached.GVR()).Namespace("staging")
		elsewhere := fc.dyn.Resource(Memcached.GVR()).Namespace("default")
		require.NoError(t, setScenario(t.Context(), staging, "cache", value))
		require.NoError(t, setScenario(t.Context(), elsewhere, "cache", value))
		time.Sleep(settle)

		require.Equal(t, []bool{false, true}, rec.of("staging/cache"), "the annotation starts the experiment at once")
		require.Equal(t, []bool{false, false}, rec.of("default/cache"), "only the staging namespace runs experiments")
		strips := allEvents(tel, stripEvent)
		require.Len(t, strips, 1, "the scenario outside staging is stripped")
		require.Empty(t, strips[0].Attributes)

		require.NoError(t, setScenario(t.Context(), staging, "cache", nil))
		time.Sleep(settle)
		require.Equal(t, []bool{false, true, false}, rec.of("staging/cache"), "removing the annotation ends the experiment at once")
		_, ok := fc.object(t, "cache").GetAnnotations()[scenarioAnnotation]
		require.True(t, ok, "default/cache keeps its annotation")

		sv.cancel()
		require.NoError(t, sv.wait(t))
	})
}

// memcachedDeployment returns the Deployment staging/cache, which the
// Memcached of controller, when it is set, controls.
func memcachedDeployment(controller *metav1.OwnerReference) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "staging", UID: "deployment-uid"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "memcached", "memcached": "cache"}},
		},
	}
	if controller != nil {
		d.OwnerReferences = []metav1.OwnerReference{*controller}
	}
	return d
}

// memcachedController is the controller owner reference of a Memcached.
func memcachedController() *metav1.OwnerReference {
	return &metav1.OwnerReference{
		APIVersion: "cache.example.com/v1alpha1", Kind: "Memcached", Name: "cache", UID: "cache-uid",
		Controller: ptr(true),
	}
}

func TestBudget_applies_a_PodDisruptionBudget_the_Deployment_controls(t *testing.T) {
	t.Parallel()
	kc := kubefake.NewClientset()
	d := memcachedDeployment(memcachedController())

	after, err := Budget{Kube: kc}.Reconcile(t.Context(), *d)
	require.NoError(t, err)
	require.Zero(t, after)

	pdb, err := kc.PolicyV1().PodDisruptionBudgets("staging").Get(t.Context(), "cache", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, pdb.Spec.MaxUnavailable.IntValue())
	require.Equal(t, d.Spec.Selector.MatchLabels, pdb.Spec.Selector.MatchLabels)
	owner := metav1.GetControllerOf(pdb)
	require.NotNil(t, owner)
	require.Equal(t, d.UID, owner.UID, "the Deployment controls the budget")
}

func TestBudget_leaves_a_Deployment_no_Memcached_controls_alone(t *testing.T) {
	t.Parallel()
	other := memcachedController()
	other.APIVersion = "cache.other.example.com/v1"
	notController := memcachedController()
	notController.Controller = nil

	for name, owner := range map[string]*metav1.OwnerReference{
		"no owner":                  nil,
		"another group's Memcached": other,
		"an owner, not controller":  notController,
	} {
		kc := kubefake.NewClientset()
		_, err := Budget{Kube: kc}.Reconcile(t.Context(), *memcachedDeployment(owner))
		require.NoError(t, err, name)
		pdbs, err := kc.PolicyV1().PodDisruptionBudgets("staging").List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err, name)
		require.Empty(t, pdbs.Items, name)
		require.False(t, memcachedChild(memcachedDeployment(owner)), name)
	}
	require.True(t, memcachedChild(memcachedDeployment(memcachedController())))
}

func TestOperator_returns_a_Controller_of_Memcacheds_and_one_of_their_Deployments(t *testing.T) {
	t.Parallel()
	s := faultsSetup(t, humus.Host{Dial: noDial}, humustest.NewTelemetry())
	budgetSettings := testSettings()
	budgetSettings.Name = "memcached-budget"

	services, err := operator(s, testClient(t, s), testSettings(), budgetSettings, kubefake.NewClientset())
	require.NoError(t, err)
	require.Len(t, services, 2)
}
