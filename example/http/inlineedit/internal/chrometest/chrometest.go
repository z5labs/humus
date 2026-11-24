//go:build testcontainers

// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package chrometest drives one of inlineedit's UIs in two tabs of headless-shell, a real Chrome started in a container.
package chrometest

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/httpserver/httpservertest"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

type image struct {
	datasource string
	depName    string
	version    string
	digest     string
}

func (i image) String() string {
	return i.depName + ":" + i.version + "@" + i.digest
}

var headlessShell = image{datasource: "docker", depName: "docker.io/chromedp/headless-shell", version: "160.0.8171.0", digest: "sha256:e2d2e25553d187f044dae3828e0b85c6049599b8bbb432b7516540454514865c"}

const (
	devtoolsPort = "9222/tcp"
	waitLimit    = 2 * time.Minute
	stepLimit    = 15 * time.Second
)

type UI struct {
	RequestHeader   string
	FormReady       string
	BeforeEdit      string
	BeforeFirstSave string
}

func Run(t *testing.T, init func(context.Context, *humus.Setup, humus.Host) ([]humus.Service, error), ui UI) {
	ca := humustest.NewCA(t)
	cert := ca.Issue(t, testcontainers.HostInternal, "127.0.0.1")
	addr := freeAddr(t)
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	p, status := httpservertest.Start(t, humus.OSHost(), init,
		humustest.WithApp(humus.WithTelemetry(telemetry.NoopBuilder)),
		humustest.WithEnv(map[string]string{
			"ADDR":         addr,
			"TLS_PEM_FILE": writePEM(t, cert),
		}),
	)
	waitListening(t, ca, addr)

	devtools := startHeadlessShell(t, cert, port)
	alloc, cancel := remote.NewAllocator(t.Context(), devtools)
	t.Cleanup(cancel)
	a, cancelA := chromedp.NewContext(alloc)
	t.Cleanup(cancelA)
	b, cancelB := chromedp.NewContext(a)
	t.Cleanup(cancelB)

	base := "https://" + net.JoinHostPort(testcontainers.HostInternal, port)
	tabA, tabB := watch(t, a), watch(t, b)
	for _, tab := range []struct {
		ctx context.Context
		*tab
	}{{a, tabA}, {b, tabB}} {
		require.NoError(t, chromedp.Do(tab.ctx,
			chromedp.Navigate(base+"/contacts"),
			chromedp.WaitVisible(chromedp.CSS("div#contact-1 button")),
		))
		select {
		case <-tab.feedOpen:
		case <-time.After(stepLimit):
			t.Fatalf("the page's feed never opened; exceptions: %q", tab.thrown())
		}
	}

	require.NoError(t, chromedp.Do(a,
		eval(ui.BeforeEdit),
		chromedp.Click(chromedp.CSS("div#contact-1 button")),
		chromedp.WaitVisible(chromedp.CSS(`form#contact-1 input[name="name"]`)),
		fill(`form#contact-1 input[name="name"]`, ""),
		fill(`form#contact-1 input[name="email"]`, "joe"),
	))
	save(t, a, ui, ui.BeforeFirstSave)
	waitText(t, a, "form#contact-1", "Enter a name.")
	waitText(t, a, "form#contact-1", "Enter an email address.")

	require.NoError(t, chromedp.Do(a,
		fill(`form#contact-1 input[name="name"]`, "Joe"),
		fill(`form#contact-1 input[name="email"]`, "joe@example.com"),
	))
	save(t, a, ui, "")
	require.Equal(t, "Joe joe@example.com", waitText(t, a, "div#contact-1 .text", "Joe joe@example.com"), "tab A's row is back in place of the form")
	require.Equal(t, "Joe joe@example.com", waitText(t, b, "div#contact-1 .text", "Joe joe@example.com"), "tab B's row, from its feed alone")
	require.Empty(t, tabB.requests(http.MethodPut), "tab B sent no PUT")

	puts := tabA.requests(http.MethodPut)
	require.Len(t, puts, 2, "a save with problems and a good save")
	for _, r := range puts {
		require.Equal(t, base+"/contacts/1", r.URL)
		require.Equal(t, "true", header(r.Headers, ui.RequestHeader), "set by the library: %v", r.Headers)
	}
	require.Equal(t, []string{base + "/contacts"}, tabA.navigations(), "tab A stays on /contacts")
	require.Equal(t, []string{base + "/contacts"}, tabB.navigations(), "tab B stays on /contacts")
	require.Empty(t, tabA.thrown(), "tab A threw")
	require.Empty(t, tabB.thrown(), "tab B threw")
	require.True(t, tabA.feedStillOpen() && tabB.feedStillOpen(), "both tabs' feeds are open at SIGTERM")

	<-p.Notified(syscall.SIGTERM)
	sigterm := time.Now()
	p.Signal(syscall.SIGTERM)
	select {
	case st := <-status:
		took := time.Since(sigterm)
		require.Zero(t, st.Code(), "%v", st.Cause())
		require.GreaterOrEqual(t, took, 5*time.Second, "the drain runs first")
		require.Less(t, took, 7*time.Second, "the feeds end when Shutdown starts, after the 5s drain, not at the 10s grace period")
	case <-time.After(waitLimit):
		t.Fatal("the App didn't exit")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func writePEM(t *testing.T, c *humustest.Certificate) string {
	t.Helper()
	cert := c.TLS()
	var b []byte
	for _, der := range cert.Certificate {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	b = append(b, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})...)
	path := filepath.Join(t.TempDir(), "tls.pem")
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return path
}

func waitListening(t *testing.T, ca *humustest.CA, addr string) {
	t.Helper()
	client := &http.Client{
		Timeout:   stepLimit,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool()}},
	}
	defer client.CloseIdleConnections()
	require.Eventually(t, func() bool {
		resp, err := client.Get("https://" + addr + "/contacts")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, waitLimit, 100*time.Millisecond, "the App didn't serve /contacts")
}

func startHeadlessShell(t *testing.T, cert *humustest.Certificate, port string) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.TLS().Certificate[0])
	require.NoError(t, err)
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	hostPort, err := strconv.Atoi(port)
	require.NoError(t, err)

	c, err := testcontainers.Run(t.Context(), headlessShell.String(),
		testcontainers.WithExposedPorts(devtoolsPort),
		testcontainers.WithHostPortAccess(hostPort),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Sysctls = map[string]string{"net.ipv6.conf.all.disable_ipv6": "1"}
		}),
		testcontainers.WithCmd(
			"--disable-dev-shm-usage",
			"--user-data-dir=/tmp/profile",
			"--ignore-certificate-errors-spki-list="+base64.StdEncoding.EncodeToString(spki[:]),
		),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/json/version").
			WithPort(devtoolsPort).
			WithStartupTimeout(waitLimit)),
	)
	if c != nil {
		t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	}
	require.NoError(t, err, "starting headless-shell in a container")
	endpoint, err := c.PortEndpoint(t.Context(), devtoolsPort, "ws")
	require.NoError(t, err)
	return endpoint + "/"
}

type tab struct {
	mu         sync.Mutex
	sent       []*network.Request
	navigated  []string
	exceptions []string
	feed       network.RequestID
	feedEnded  bool
	feedOpen   chan struct{}
}

func watch(t *testing.T, ctx context.Context) *tab {
	t.Helper()
	tb := &tab{feedOpen: make(chan struct{})}
	require.NoError(t, chromedp.Listen(ctx, tb.record))
	return tb
}

func (tb *tab) record(ev any) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	switch ev := ev.(type) {
	case *network.EventRequestWillBeSent:
		tb.sent = append(tb.sent, ev.Request)
	case *page.EventFrameNavigated:
		if ev.Frame.ParentID == "" {
			tb.navigated = append(tb.navigated, ev.Frame.URL)
		}
	case *network.EventResponseReceived:
		path, _, _ := strings.Cut(ev.Response.URL, "?")
		if tb.feed == "" && strings.HasSuffix(path, "/contacts/changes") && ev.Response.Status == http.StatusOK {
			tb.feed = ev.RequestID
			close(tb.feedOpen)
		}
	case *network.EventLoadingFinished:
		tb.ended(ev.RequestID)
	case *network.EventLoadingFailed:
		tb.ended(ev.RequestID)
	case *runtime.EventExceptionThrown:
		tb.exceptions = append(tb.exceptions, (&chromedp.ExceptionError{ExceptionDetails: ev.ExceptionDetails}).Error())
	}
}

func (tb *tab) ended(id network.RequestID) {
	if id == tb.feed {
		tb.feedEnded = true
	}
}

func (tb *tab) feedStillOpen() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.feed != "" && !tb.feedEnded
}

func (tb *tab) requests(method string) []*network.Request {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	var out []*network.Request
	for _, r := range tb.sent {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (tb *tab) navigations() []string {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return append([]string(nil), tb.navigated...)
}

func (tb *tab) thrown() []string {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return append([]string(nil), tb.exceptions...)
}

func header(h network.Headers, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func fill(sel, value string) chromedp.Action[chromedp.Void] {
	return chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		_, err := chromedp.Evaluate[bool](fmt.Sprintf(`(document.querySelector(%q).value = %q) !== null`, sel, value))(ctx, t)
		return err
	})
}

func save(t *testing.T, ctx context.Context, ui UI, before string) {
	t.Helper()
	if ui.FormReady != "" {
		wctx, cancel := context.WithTimeout(ctx, stepLimit)
		defer cancel()
		_, err := chromedp.Run(wctx, chromedp.Poll[bool](fmt.Sprintf(`(form => form !== null && (%s))(document.querySelector("form#contact-1"))`, ui.FormReady)))
		require.NoError(t, err, "waiting for the library to take form#contact-1's submit")
	}
	require.NoError(t, chromedp.Do(ctx,
		eval(before),
		chromedp.Click(chromedp.CSS(`form#contact-1 button[type="submit"]`)),
	))
}

func eval(script string) chromedp.Action[chromedp.Void] {
	return chromedp.Func(func(ctx context.Context, t *chromedp.Target) error {
		if script == "" {
			return nil
		}
		_, err := chromedp.Evaluate[bool](fmt.Sprintf(`(() => { %s; return true })()`, script))(ctx, t)
		return err
	})
}

func waitText(t *testing.T, ctx context.Context, sel, want string) string {
	t.Helper()
	wctx, cancel := context.WithTimeout(ctx, stepLimit)
	defer cancel()
	_, err := chromedp.Run(wctx, chromedp.Poll[bool](fmt.Sprintf(`(document.querySelector(%q)?.textContent ?? '').includes(%q)`, sel, want)))
	text, terr := chromedp.Run(ctx, chromedp.Evaluate[string](fmt.Sprintf(`document.querySelector(%q)?.textContent ?? ''`, sel)))
	require.NoError(t, terr)
	require.NoError(t, err, "waiting for %q in %s, which holds %q", want, sel, text)
	return text
}
