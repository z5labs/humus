// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package kube_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/z5labs/bedrock"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/job"
	"github.com/z5labs/humus/kube"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run inside synctest bubbles, on humustest Hosts.
// The API server is a plain HTTP handler that records what it receives,
// served over TLS at apiserver:6443 with a certificate from a CA generated
// for each test, which the Client trusts through RootCAs. Every token is
// generated for the test that sends it.

const server = "apiserver:6443"

// received is what the API server saw of one request.
type received struct {
	method      string
	path        string
	watch       bool
	auth        string
	hasAuth     bool
	userAgent   string
	traceparent string
	proto       int
	serverName  string
}

// apiServer records every request it receives, and answers a GET of a
// ConfigMap with it, and a watch of ConfigMaps with one ADDED event.
type apiServer struct {
	mu   sync.Mutex
	reqs []received
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, hasAuth := r.Header["Authorization"]
	rec := received{
		method:      r.Method,
		path:        r.URL.Path,
		watch:       r.URL.Query().Get("watch") == "true",
		auth:        r.Header.Get("Authorization"),
		hasAuth:     hasAuth,
		userAgent:   r.Header.Get("User-Agent"),
		traceparent: r.Header.Get("Traceparent"),
		proto:       r.ProtoMajor,
	}
	if r.TLS != nil {
		rec.serverName = r.TLS.ServerName
	}
	a.mu.Lock()
	a.reqs = append(a.reqs, rec)
	a.mu.Unlock()

	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	cm := fmt.Sprintf(`{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":%q,"namespace":"default"},"data":{"from":"apiserver"}}`, name)
	w.Header().Set("Content-Type", "application/json")
	if rec.watch {
		_, _ = fmt.Fprintf(w, `{"type":"ADDED","object":%s}`+"\n", cm)
		return
	}
	_, _ = io.WriteString(w, cm)
}

// received returns a copy of the requests the API server has received, in
// the order it received them.
func (a *apiServer) received() []received {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]received(nil), a.reqs...)
}

// startAPIServer serves api on h at addr, over TLS with a certificate from
// ca for names. It returns a function that stops it.
func startAPIServer(t *testing.T, h *humustest.Host, ca *humustest.CA, addr string, api http.Handler, names ...string) (stop func()) {
	t.Helper()
	l, err := h.Listen("tcp", addr)
	require.NoError(t, err)
	cert := ca.Issue(t, names...)
	srv := &http.Server{
		Handler:   api,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert.TLS()}},
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(l, "", "") }()
	return func() {
		require.NoError(t, srv.Close())
		require.ErrorIs(t, <-done, http.ErrServerClosed)
	}
}

// dials records the address of every dial through the Host.
type dials struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dials) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// recordDials returns h's humus.Host, whose Dial records every address it
// dials, and the record.
func recordDials(h *humustest.Host) (humus.Host, *dials) {
	d := &dials{}
	host := h.Host()
	dial := host.Dial
	host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		d.mu.Lock()
		d.addrs = append(d.addrs, address)
		d.mu.Unlock()
		return dial(ctx, network, address)
	}
	return host, d
}

// newSetup returns a Setup on host whose telemetry tel records.
func newSetup(t *testing.T, host humus.Host, tel *humustest.Telemetry) *humus.Setup {
	t.Helper()
	sdk, err := tel.Builder()(t.Context(), bedrock.Process{}, nil)
	require.NoError(t, err)
	return &humus.Setup{Host: host, Telemetry: sdk}
}

// newClientset returns a clientset from c's Config.
func newClientset(t *testing.T, c *kube.Client) kubernetes.Interface {
	t.Helper()
	kc, err := kubernetes.NewForConfig(c.Config())
	require.NoError(t, err)
	return kc
}

// getConfigMap gets the ConfigMap default/name through kc.
func getConfigMap(ctx context.Context, kc kubernetes.Interface, name string) error {
	cm, err := kc.CoreV1().ConfigMaps("default").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.Name != name || cm.Data["from"] != "apiserver" {
		return fmt.Errorf("got the ConfigMap %s/%s with %v", cm.Namespace, cm.Name, cm.Data)
	}
	return nil
}

// newToken returns a token generated for the test, never written in the
// source.
func newToken() string {
	return rand.Text()
}

// tokens is a Connection.Token that returns the next of its tokens on each
// call, or its err once they run out, and counts its calls.
type tokens struct {
	mu     sync.Mutex
	queue  []string
	err    error
	calls  atomic.Int32
	failed atomic.Int32
}

func (tk *tokens) token(context.Context) (string, error) {
	tk.calls.Add(1)
	tk.mu.Lock()
	defer tk.mu.Unlock()
	if len(tk.queue) == 0 {
		tk.failed.Add(1)
		return "", tk.err
	}
	next := tk.queue[0]
	tk.queue = tk.queue[1:]
	return next, nil
}

func TestCheckServer(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{
		"apiserver:6443",
		"10.96.0.1:443",
		"[fd00::1]:443",
		"kubernetes.default.svc:1",
		"kubernetes.default.svc:65535",
	} {
		require.NoError(t, kube.CheckServer(addr), "CheckServer(%q)", addr)
	}

	for _, addr := range []string{
		"",
		"apiserver",
		":443",
		"apiserver:",
		"apiserver:0",
		"apiserver:65536",
		"apiserver:-1",
		"apiserver:https",
		"https://apiserver:443",
		"fd00::1:443",
	} {
		err := kube.CheckServer(addr)
		require.Error(t, err, "CheckServer(%q)", addr)
		require.NotContains(t, err.Error(), "Server", "the check names no field")
		require.NotContains(t, err.Error(), "Connection", "the check names no field")
	}
}

func TestCheckTLS(t *testing.T) {
	t.Parallel()

	require.NoError(t, kube.CheckTLS(&tls.Config{}))
	err := kube.CheckTLS(nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "TLS", "the check names no field")
}

func TestNewClient_reports_every_bad_field(t *testing.T) {
	t.Parallel()

	h := humustest.NewHost(t)
	s := &humus.Setup{Host: h.Host(), Telemetry: telemetry.Noop()}

	_, err := kube.NewClient(s, kube.Connection{Server: "apiserver"})
	require.ErrorContains(t, err, "kube: NewClient")
	require.ErrorContains(t, err, "Connection.Server")
	require.ErrorContains(t, err, "Connection.TLS")

	_, err = kube.NewClient(s, kube.Connection{Server: server, TLS: &tls.Config{}})
	require.NoError(t, err)
}

func TestNewClient_rejects_a_bad_Setup(t *testing.T) {
	t.Parallel()

	conn := kube.Connection{Server: server, TLS: &tls.Config{}}

	_, err := kube.NewClient(nil, conn)
	require.ErrorContains(t, err, "the Setup must not be nil")

	_, err = kube.NewClient(&humus.Setup{Telemetry: telemetry.Noop()}, kube.Connection{TLS: &tls.Config{}})
	require.ErrorContains(t, err, "Host.Dial must not be nil")
	require.ErrorContains(t, err, "Connection.Server", "the Setup's error is reported with the Connection's")
}

func TestClient_dials_through_the_Host(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		api := &apiServer{}
		defer startAPIServer(t, h, ca, server, api, "apiserver")()

		host, dialled := recordDials(h)
		tlsConfig := &tls.Config{RootCAs: ca.Pool()}
		c, err := kube.NewClient(newSetup(t, host, humustest.NewTelemetry()), kube.Connection{Server: server, TLS: tlsConfig})
		require.NoError(t, err)

		require.NoError(t, getConfigMap(t.Context(), newClientset(t, c), "settings"))

		require.Equal(t, []string{server}, dialled.all())
		reqs := api.received()
		require.Len(t, reqs, 1)
		require.Equal(t, "apiserver", reqs[0].serverName, "ServerName defaults to Server's host")
		require.Equal(t, 2, reqs[0].proto, "the transport speaks HTTP/2 over TLS")
		require.Empty(t, tlsConfig.ServerName, "the end user's tls.Config isn't changed")
	})
}

func TestClient_keeps_the_end_users_ServerName(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		api := &apiServer{}
		// The certificate names only kubernetes, so the handshake succeeds
		// only with the end user's ServerName, not Server's host.
		defer startAPIServer(t, h, ca, server, api, "kubernetes")()

		c, err := kube.NewClient(newSetup(t, h.Host(), humustest.NewTelemetry()), kube.Connection{
			Server: server,
			TLS:    &tls.Config{RootCAs: ca.Pool(), ServerName: "kubernetes"},
		})
		require.NoError(t, err)

		require.NoError(t, getConfigMap(t.Context(), newClientset(t, c), "settings"))
		require.Equal(t, "kubernetes", api.received()[0].serverName)
	})
}

// TestClient_uses_no_proxy shows a request reaching the API server directly
// with the proxy variables set. net/http reads them once per process, so
// TestNewClient_the_transport's check that the transport's Proxy is nil is
// what guards that whatever the order the tests run in.
func TestClient_uses_no_proxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("HTTP_PROXY", "http://proxy:3128")
	t.Setenv("NO_PROXY", "")

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		api := &apiServer{}
		defer startAPIServer(t, h, ca, server, api, "apiserver")()

		host, dialled := recordDials(h)
		c, err := kube.NewClient(newSetup(t, host, humustest.NewTelemetry()), kube.Connection{
			Server: server,
			TLS:    &tls.Config{RootCAs: ca.Pool()},
		})
		require.NoError(t, err)

		require.NoError(t, getConfigMap(t.Context(), newClientset(t, c), "settings"))
		require.Equal(t, []string{server}, dialled.all(), "the Client dials the API server directly")
	})
}

func TestClient_sends_the_bearer_token(t *testing.T) {
	t.Parallel()

	t.Run("on every request, calling Token once a minute", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			api := &apiServer{}
			defer startAPIServer(t, h, ca, server, api, "apiserver")()

			first, second := newToken(), newToken()
			tk := &tokens{queue: []string{first, second}}
			c, err := kube.NewClient(newSetup(t, h.Host(), humustest.NewTelemetry()), kube.Connection{
				Server: server,
				TLS:    &tls.Config{RootCAs: ca.Pool()},
				Token:  tk.token,
			})
			require.NoError(t, err)
			kc := newClientset(t, c)

			// Many requests within a minute, some at once.
			var wg sync.WaitGroup
			for i := range 10 {
				wg.Go(func() {
					require.NoError(t, getConfigMap(t.Context(), kc, fmt.Sprintf("cm-%d", i)))
				})
			}
			wg.Wait()
			time.Sleep(59 * time.Second)
			require.NoError(t, getConfigMap(t.Context(), kc, "settings"))
			require.EqualValues(t, 1, tk.calls.Load(), "Token is called once within a minute")

			// A minute after the call, the next request calls it again, and
			// carries its new token.
			time.Sleep(time.Second)
			require.NoError(t, getConfigMap(t.Context(), kc, "settings"))
			require.EqualValues(t, 2, tk.calls.Load())

			reqs := api.received()
			require.Len(t, reqs, 12)
			for _, r := range reqs[:11] {
				require.Equal(t, "Bearer "+first, r.auth)
			}
			require.Equal(t, "Bearer "+second, reqs[11].auth)
		})
	})

	t.Run("keeping the last token while a call fails", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			api := &apiServer{}
			defer startAPIServer(t, h, ca, server, api, "apiserver")()

			tel := humustest.NewTelemetry()
			last := newToken()
			tk := &tokens{queue: []string{last}, err: errors.New("the token file is gone")}
			c, err := kube.NewClient(newSetup(t, h.Host(), tel), kube.Connection{
				Server: server,
				TLS:    &tls.Config{RootCAs: ca.Pool()},
				Token:  tk.token,
			})
			require.NoError(t, err)
			kc := newClientset(t, c)

			require.NoError(t, getConfigMap(t.Context(), kc, "settings"))
			time.Sleep(time.Minute)
			require.NoError(t, getConfigMap(t.Context(), kc, "settings"), "the request goes on with the last token")
			require.EqualValues(t, 1, tk.failed.Load())

			// The failed call counts toward the minute too.
			time.Sleep(30 * time.Second)
			require.NoError(t, getConfigMap(t.Context(), kc, "settings"))
			require.EqualValues(t, 2, tk.calls.Load())

			reqs := api.received()
			require.Len(t, reqs, 3)
			for _, r := range reqs {
				require.Equal(t, "Bearer "+last, r.auth)
			}

			// The failure is recorded on the request's span.
			spans := tel.Spans()
			require.Len(t, spans, 3)
			events := spans[1].Events()
			require.Len(t, events, 1)
			require.Equal(t, "exception", events[0].Name)
			require.Contains(t, fmt.Sprint(events[0].Attributes), "the token file is gone")
		})
	})

	t.Run("failing a request while no call has returned a token", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			api := &apiServer{}
			defer startAPIServer(t, h, ca, server, api, "apiserver")()

			tel := humustest.NewTelemetry()
			tk := &tokens{err: errors.New("the token file isn't mounted yet")}
			c, err := kube.NewClient(newSetup(t, h.Host(), tel), kube.Connection{
				Server: server,
				TLS:    &tls.Config{RootCAs: ca.Pool()},
				Token:  tk.token,
			})
			require.NoError(t, err)
			kc := newClientset(t, c)

			// A request with a body, which the Client closes without
			// sending it.
			_, err = kc.CoreV1().ConfigMaps("default").Create(t.Context(), newConfigMap("settings"), metav1.CreateOptions{})
			require.ErrorContains(t, err, "the token file isn't mounted yet")
			require.Empty(t, api.received(), "nothing is sent without a token")

			spans := tel.Spans()
			require.Len(t, spans, 1)
			require.Equal(t, codes.Error, spans[0].Status().Code)

			// With no token to keep, the next request calls Token again.
			tk.mu.Lock()
			tk.queue = []string{newToken()}
			tk.mu.Unlock()
			require.NoError(t, getConfigMap(t.Context(), kc, "settings"))
			require.EqualValues(t, 2, tk.calls.Load())
		})
	})

	t.Run("not at all when Token is nil", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			api := &apiServer{}
			defer startAPIServer(t, h, ca, server, api, "apiserver")()

			c, err := kube.NewClient(newSetup(t, h.Host(), humustest.NewTelemetry()), kube.Connection{
				Server: server,
				TLS:    &tls.Config{RootCAs: ca.Pool()},
			})
			require.NoError(t, err)

			require.NoError(t, getConfigMap(t.Context(), newClientset(t, c), "settings"))
			reqs := api.received()
			require.Len(t, reqs, 1)
			require.False(t, reqs[0].hasAuth, "no request carries an Authorization header")
		})
	})

	t.Run("leaving an Authorization header the request already has", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			h := humustest.NewHost(t)
			ca := humustest.NewCA(t)
			api := &apiServer{}
			defer startAPIServer(t, h, ca, server, api, "apiserver")()

			tk := &tokens{queue: []string{newToken()}}
			c, err := kube.NewClient(newSetup(t, h.Host(), humustest.NewTelemetry()), kube.Connection{
				Server: server,
				TLS:    &tls.Config{RootCAs: ca.Pool()},
				Token:  tk.token,
			})
			require.NoError(t, err)

			own := newToken()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+server+"/api/v1/namespaces/default/configmaps/settings", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+own)
			resp, err := (&http.Client{Transport: c.Config().Transport}).Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			require.Equal(t, "Bearer "+own, api.received()[0].auth)
			require.Zero(t, tk.calls.Load())
		})
	})
}

func TestClient_traces_every_request_but_a_watch(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		api := &apiServer{}
		defer startAPIServer(t, h, ca, server, api, "apiserver")()

		tel := humustest.NewTelemetry()
		c, err := kube.NewClient(newSetup(t, h.Host(), tel), kube.Connection{
			Server: server,
			TLS:    &tls.Config{RootCAs: ca.Pool()},
		})
		require.NoError(t, err)
		kc := newClientset(t, c)

		require.NoError(t, getConfigMap(t.Context(), kc, "settings"))

		w, err := kc.CoreV1().ConfigMaps("default").Watch(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		var events int
		for range w.ResultChan() {
			events++
		}
		w.Stop()
		require.Equal(t, 1, events)

		reqs := api.received()
		require.Len(t, reqs, 2)
		require.False(t, reqs[0].watch)
		require.True(t, reqs[1].watch)

		spans := tel.Spans()
		require.Len(t, spans, 1, "the watch has no span")
		span := spans[0]
		require.Equal(t, trace.SpanKindClient, span.SpanKind())
		require.Equal(t, http.MethodGet, span.Name())
		require.Equal(t, "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp", span.InstrumentationScope().Name)

		// The span's context reached the API server, through the Setup's
		// propagator, and the watch carried none.
		require.Contains(t, reqs[0].traceparent, span.SpanContext().TraceID().String())
		require.Empty(t, reqs[1].traceparent)

		// The request is measured with the Setup's MeterProvider.
		rm, err := tel.Metrics(t.Context())
		require.NoError(t, err)
		var names []string
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				names = append(names, m.Name)
			}
		}
		require.Contains(t, names, "http.client.request.duration")
	})
}

// TestClient_Config_reaches_the_API_server_the_same_way shows a clientset
// from Config sending its request through the Host, with the token, the
// span and the Client's User-Agent, and no rate limit of client-go's.
func TestClient_Config_reaches_the_API_server_the_same_way(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		api := &apiServer{}
		defer startAPIServer(t, h, ca, server, api, "apiserver")()

		host, dialled := recordDials(h)
		tel := humustest.NewTelemetry()
		token := newToken()
		c, err := kube.NewClient(newSetup(t, host, tel), kube.Connection{
			Server: server,
			TLS:    &tls.Config{RootCAs: ca.Pool()},
			Token:  func(context.Context) (string, error) { return token, nil },
		})
		require.NoError(t, err)

		cfg := c.Config()
		require.Equal(t, "https://"+server, cfg.Host)
		require.Negative(t, cfg.QPS, "client-go limits no client's rate")
		require.Nil(t, cfg.RateLimiter)
		cfg.UserAgent = "changed"
		require.NotEqual(t, "changed", c.Config().UserAgent, "each Config is the caller's own")

		kc := newClientset(t, c)
		// client-go's default limit is 5 requests a second, bursting to 10,
		// so 50 requests at one instant would wait for its limiter.
		start := time.Now()
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() {
				require.NoError(t, getConfigMap(t.Context(), kc, fmt.Sprintf("cm-%d", i)))
			})
		}
		wg.Wait()
		require.Zero(t, time.Since(start), "no request waited for a rate limiter")

		reqs := api.received()
		require.Len(t, reqs, 50)
		for _, r := range reqs {
			require.Equal(t, http.MethodGet, r.method)
			require.True(t, strings.HasPrefix(r.path, "/api/v1/namespaces/default/configmaps/cm-"), r.path)
			require.Equal(t, "Bearer "+token, r.auth)
			require.Equal(t, "humus-kube", r.userAgent)
			require.NotEmpty(t, r.traceparent)
		}
		require.NotEmpty(t, dialled.all())
		for _, addr := range dialled.all() {
			require.Equal(t, server, addr)
		}
		require.Len(t, tel.Spans(), 50)
	})
}

// TestClient_gives_up_on_a_connection_whose_ping_goes_unanswered shows the
// HTTP/2 health check: 30s without a frame sends a ping, and 15s without its
// answer closes the connection, so the next request dials again rather than
// wait on a dead one.
func TestClient_gives_up_on_a_connection_whose_ping_goes_unanswered(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		api := &apiServer{}
		defer startAPIServer(t, h, ca, server, api, "apiserver")()

		host, conns := stallableDials(h)
		c, err := kube.NewClient(newSetup(t, host, humustest.NewTelemetry()), kube.Connection{
			Server: server,
			TLS:    &tls.Config{RootCAs: ca.Pool()},
		})
		require.NoError(t, err)
		kc := newClientset(t, c)

		require.NoError(t, getConfigMap(t.Context(), kc, "settings"))
		require.Len(t, conns.all(), 1)
		first := conns.all()[0]
		// From now on, nothing the API server sends on the connection
		// reaches the Client, so its ping is never answered.
		first.stall()

		time.Sleep(44 * time.Second)
		synctest.Wait()
		require.False(t, first.isClosed(), "the connection is kept until the ping's answer is 15s late")

		time.Sleep(2 * time.Second)
		synctest.Wait()
		require.True(t, first.isClosed(), "the connection is closed 45s after its last frame")

		require.NoError(t, getConfigMap(t.Context(), kc, "settings"))
		require.Len(t, conns.all(), 2, "the next request dials a new connection")
	})
}

// TestClient_closes_its_idle_connections_at_OnStop runs the Client in a
// whole App on humustest's Host: a job whose run reads a ConfigMap. Once
// the App has stopped, the connection the Client kept idle is closed.
func TestClient_closes_its_idle_connections_at_OnStop(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		ca := humustest.NewCA(t)
		api := &apiServer{}
		defer startAPIServer(t, h, ca, server, api, "apiserver")()

		host, conns := stallableDials(h)
		var openWhileRunning bool
		_, status := humustest.Start(t, func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			c, err := kube.NewClient(s, kube.Connection{Server: server, TLS: &tls.Config{RootCAs: ca.Pool()}})
			if err != nil {
				return nil, err
			}
			kc, err := kubernetes.NewForConfig(c.Config())
			if err != nil {
				return nil, err
			}
			return []humus.Service{job.NewService(s, func(ctx context.Context) error {
				if err := getConfigMap(ctx, kc, "settings"); err != nil {
					return err
				}
				synctest.Wait()
				openWhileRunning = len(conns.all()) == 1 && !conns.all()[0].isClosed()
				return nil
			})}, nil
		}, humustest.WithApp(humus.WithHost(host), humus.WithTelemetry(telemetry.NoopBuilder)))
		st := <-status
		require.Zero(t, st.Code(), "cause: %v", st.Cause())
		synctest.Wait()

		require.True(t, openWhileRunning, "the connection was closed before the App stopped")
		require.True(t, conns.all()[0].isClosed(), "the idle connection outlived the App")
	})
}

// newConfigMap returns the ConfigMap default/name, with no data.
func newConfigMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

// conns records every connection dialled through a Host.
type conns struct {
	mu   sync.Mutex
	list []*stallableConn
}

func (c *conns) all() []*stallableConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*stallableConn(nil), c.list...)
}

// stallableDials returns h's humus.Host, whose Dial wraps every connection
// in a stallableConn, and the record of them.
func stallableDials(h *humustest.Host) (humus.Host, *conns) {
	cs := &conns{}
	host := h.Host()
	dial := host.Dial
	host.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		sc := &stallableConn{Conn: conn, closed: make(chan struct{})}
		cs.mu.Lock()
		cs.list = append(cs.list, sc)
		cs.mu.Unlock()
		return sc, nil
	}
	return host, cs
}

// stallableConn is a connection whose reads can be stalled: once stalled,
// a Read returns nothing until the connection is closed.
type stallableConn struct {
	net.Conn
	stalled   atomic.Bool
	closeOnce sync.Once
	closed    chan struct{}
}

func (c *stallableConn) stall() { c.stalled.Store(true) }

func (c *stallableConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *stallableConn) Read(p []byte) (int, error) {
	if !c.stalled.Load() {
		n, err := c.Conn.Read(p)
		if !c.stalled.Load() {
			return n, err
		}
	}
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stallableConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
