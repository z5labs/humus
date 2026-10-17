// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package humus_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/z5labs/bedrock"
	"github.com/z5labs/bedrock/bedrocktest"
	"github.com/z5labs/bedrock/config"
	"software.sslmate.com/src/go-pkcs12"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/humustest"
	"github.com/z5labs/humus/telemetry"
)

// The tests in this file run whole Apps that serve real HTTP, with net/http,
// over a humustest Host's in-memory network. Each runs inside a synctest
// bubble and is started with bedrocktest.Start or humustest.Start, so none
// opens a socket, sets a variable of the real process or touches a file
// outside its own temp directory.

// httpService is a test-only humus.Service: an http.Server serving on a
// listener from the Setup's Host. It keeps serving after Serve's context is
// done, as an HTTP Service does, until Shutdown stops it.
type httpService struct {
	host humus.Host
	addr string
	tls  *tls.Config
	srv  *http.Server
}

// newHTTPService returns an httpService that serves h on addr of s.Host.
func newHTTPService(s *humus.Setup, addr string, h http.Handler) *httpService {
	return &httpService{
		host: s.Host,
		addr: addr,
		srv:  &http.Server{Handler: h},
	}
}

// newHTTPSService returns an httpService that serves h with TLS on addr of
// s.Host.
func newHTTPSService(s *humus.Setup, addr string, cfg *tls.Config, h http.Handler) *httpService {
	srv := newHTTPService(s, addr, h)
	srv.tls = cfg
	return srv
}

// Serve listens on the Service's address, with TLS when the Service has a
// TLS config, and serves until Shutdown. It returns nil when Shutdown
// stopped it, including a Shutdown called before Serve.
func (s *httpService) Serve(context.Context) error {
	ln, err := s.host.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}
	if s.tls != nil {
		ln = tls.NewListener(ln, s.tls)
	}
	// Serve closes ln when it returns.
	if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http serve: %w", err)
	}
	return nil
}

// Shutdown stops accepting connections and waits for in-flight requests to
// finish. If ctx is done first, or is done already, it closes every
// connection at once, which cancels the context of each in-flight request,
// and returns an error wrapping ctx's error.
func (s *httpService) Shutdown(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		err = s.srv.Shutdown(ctx)
	}
	if err == nil {
		return nil
	}
	// Close only fails with the error of closing a listener, and
	// Shutdown's error already says why the Service did not stop
	// gracefully.
	_ = s.srv.Close()
	return fmt.Errorf("http shutdown: %w", err)
}

func TestHTTPService(t *testing.T) {
	humustest.TestService(t, func(t *testing.T, s *humus.Setup) humus.Service {
		return newHTTPService(s, ":8080", http.NotFoundHandler())
	})
}

// job is a Service that finishes on its own after d, unless Shutdown stops
// it or Serve's context is done first.
type job struct {
	d    time.Duration
	once sync.Once
	stop chan struct{}
}

func newJob(d time.Duration) *job {
	return &job{d: d, stop: make(chan struct{})}
}

func (j *job) Serve(ctx context.Context) error {
	timer := time.NewTimer(j.d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	case <-j.stop:
	}
	return nil
}

func (j *job) Shutdown(context.Context) error {
	j.once.Do(func() { close(j.stop) })
	return nil
}

// httpApp returns an App on h whose Init returns an httpService serving
// handler on :8080, followed by more.
func httpApp(h *humustest.Host, handler http.Handler, more ...humus.Service) humus.App {
	return humus.App{
		Host:                h.Host(),
		ShutdownGracePeriod: 10 * time.Second,
		Telemetry:           telemetry.NoopBuilder,
		Init: func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
			return append([]humus.Service{newHTTPService(s, ":8080", handler)}, more...), nil
		},
	}
}

// response is what a client got back from a request: the body, or the
// error.
type response struct {
	body string
	err  error
}

// get sends a GET to url with h's HTTP client in a new goroutine, and
// returns the channel its response arrives on.
func get(h *humustest.Host, url string) <-chan response {
	got := make(chan response, 1)
	go func() {
		resp, err := h.HTTPClient().Get(url)
		if err != nil {
			got <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("status %s", resp.Status)
		}
		got <- response{body: string(b), err: err}
	}()
	return got
}

// awaitStarted waits for the handler to start, failing t with the
// request's error if the response arrives first.
func awaitStarted(t *testing.T, started <-chan struct{}, got <-chan response) {
	t.Helper()
	select {
	case <-started:
	case r := <-got:
		t.Fatalf("the request ended before the handler started: %q, %v", r.body, r.err)
	}
}

// requireSignalCause fails t unless cause is the SIGTERM the App received.
func requireSignalCause(t *testing.T, cause error) {
	t.Helper()
	if !errors.Is(cause, context.Canceled) {
		t.Errorf("errors.Is(%v, context.Canceled) = false, want the signal as the cause", cause)
	}
	if cause == nil || cause.Error() != "terminated signal received" {
		t.Errorf("cause = %v, want terminated signal received", cause)
	}
}

// requireRefused fails t unless a Dial to address is refused, as it is once
// nothing listens on it.
func requireRefused(t *testing.T, h *humustest.Host, address string) {
	t.Helper()
	conn, err := h.Dial(t.Context(), "tcp", address)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("Dial(%q) connected, want ECONNREFUSED", address)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("Dial(%q) = %v, want ECONNREFUSED", address, err)
	}
}

func TestApp_Run_HTTP_SIGTERM_lets_an_in_flight_request_finish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		started := make(chan struct{})
		var startedAt time.Time
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			startedAt = time.Now()
			close(started)
			time.Sleep(3 * time.Second)
			_, _ = io.WriteString(w, "finished")
		})
		p, status := bedrocktest.Start(t, httpApp(h, handler))

		<-h.Listening("localhost:8080")
		got := get(h, "http://localhost:8080/")
		awaitStarted(t, started, got)
		<-p.Notified(syscall.SIGTERM)
		if inFlight := time.Since(startedAt); inFlight != 0 {
			t.Fatalf("SIGTERM is sent %s after the handler started, want while it sleeps", inFlight)
		}
		p.Signal(syscall.SIGTERM)

		r := <-got
		if r.err != nil {
			t.Fatalf("request failed: %v", r.err)
		}
		if r.body != "finished" {
			t.Errorf("body = %q, want the full body", r.body)
		}
		if took := time.Since(startedAt); took != 3*time.Second {
			t.Errorf("the response arrived %s after the handler started, want 3s", took)
		}
		st := <-status
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		requireSignalCause(t, st.Cause())
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}

func TestApp_Run_HTTP_shutdown_is_bounded_by_the_grace_period(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		started := make(chan struct{})
		handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		})
		app, tel := recorded(httpApp(h, handler))
		p, status := bedrocktest.Start(t, app, bedrocktest.WithEnv(map[string]string{
			"HUMUS_SHUTDOWN_GRACE_PERIOD": "2s",
		}))

		<-h.Listening("localhost:8080")
		got := get(h, "http://localhost:8080/")
		awaitStarted(t, started, got)
		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)

		st := <-status
		if elapsed := time.Since(sigterm); elapsed != 2*time.Second {
			t.Errorf("the App exited %s after SIGTERM, want exactly 2s", elapsed)
		}
		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		gerr, ok := errors.AsType[*humus.GracePeriodError](st.Cause())
		if !ok {
			t.Fatalf("cause = %v, want a *humus.GracePeriodError", st.Cause())
		}
		if gerr.GracePeriod != 2*time.Second {
			t.Errorf("GracePeriod = %s, want 2s", gerr.GracePeriod)
		}
		if !errors.Is(st.Cause(), context.DeadlineExceeded) {
			t.Errorf("errors.Is(%v, context.DeadlineExceeded) = false", st.Cause())
		}
		if got := strings.Join(errorLogs(tel), "\n"); !strings.Contains(got, "http shutdown: context deadline exceeded") {
			t.Errorf("logged errors = %q, want the HTTP shutdown error", got)
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}

		r := <-got
		if r.err == nil {
			t.Fatalf("request got %q, want an error", r.body)
		}
		if !errors.Is(r.err, io.EOF) {
			t.Errorf("request error = %v, want the connection closed (io.EOF)", r.err)
		}
	})
}

func TestApp_Run_HTTP_a_job_ends_the_App_and_stops_the_other_Services(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		})
		app := humus.App{
			Host:                h.Host(),
			ShutdownGracePeriod: 10 * time.Second,
			Telemetry:           telemetry.NoopBuilder,
			Init: func(_ context.Context, s *humus.Setup) ([]humus.Service, error) {
				return []humus.Service{newJob(time.Second), newHTTPService(s, ":8081", probe)}, nil
			},
		}
		begin := time.Now()
		p, status := bedrocktest.Start(t, app)

		<-h.Listening("localhost:8081")
		if r := <-get(h, "http://localhost:8081/"); r.err != nil || r.body != "ok" {
			t.Fatalf("probe = %q, %v, want ok while the job runs", r.body, r.err)
		}

		st := <-status
		if elapsed := time.Since(begin); elapsed != time.Second {
			t.Errorf("the App exited after %s, want when the 1s job finished", elapsed)
		}
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if st.Cause() != nil {
			t.Errorf("cause = %v, want none", st.Cause())
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
		requireRefused(t, h, "localhost:8081")
	})
}

func TestApp_Run_HTTP_the_drain_keeps_serving(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "still serving")
		})
		app := httpApp(h, handler)
		app.Drain = humus.DrainDelay(3 * time.Second)
		p, status := bedrocktest.Start(t, app)

		<-h.Listening("localhost:8080")
		<-p.Notified(syscall.SIGTERM)
		sigterm := time.Now()
		p.Signal(syscall.SIGTERM)

		time.Sleep(time.Second)
		r := <-get(h, "http://localhost:8080/")
		if r.err != nil {
			t.Fatalf("request 1s after SIGTERM failed: %v", r.err)
		}
		if r.body != "still serving" {
			t.Errorf("body = %q, want still serving", r.body)
		}

		var st bedrock.ExitStatus
		select {
		case st = <-status:
			t.Fatalf("the App exited during the drain: %d %v", st.Code(), st.Cause())
		default:
		}
		st = <-status
		if elapsed := time.Since(sigterm); elapsed != 3*time.Second {
			t.Errorf("the App exited %s after SIGTERM, want after the 3s drain", elapsed)
		}
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		requireSignalCause(t, st.Cause())
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
		requireRefused(t, h, "localhost:8080")
	})
}

// The docs site's "Testing a humus App" page and humustest's package doc
// quote the two tests below.

func TestApp_Run_HTTP_a_whole_App_on_a_test_Host(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := humustest.NewHost(t)
		hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello")
		})
		p, status := bedrocktest.Start(t, httpApp(h, hello))

		// Wait until every goroutine in the bubble is blocked, which
		// includes the App blocked in Accept on :8080.
		synctest.Wait()

		resp, err := h.HTTPClient().Get("http://localhost:8080/")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "hello" {
			t.Errorf("body = %q, want hello", b)
		}

		p.Signal(syscall.SIGTERM)
		st := <-status
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		requireSignalCause(t, st.Cause())
		requireRefused(t, h, "localhost:8080")
	})
}

// tlsFromPKCS12 reads the PKCS#12 bundle that TLS_PKCS12_FILE names,
// through the Setup's Host, and decrypts it with TLS_PKCS12_PASSWORD. The
// setting names are only for illustration.
func tlsFromPKCS12(ctx context.Context, s *humus.Setup) (*tls.Config, error) {
	path, err := config.Env(s.Process.Environment, "TLS_PKCS12_FILE").Read(ctx)
	if err != nil {
		return nil, err
	}
	password, err := config.Env(s.Process.Environment, "TLS_PKCS12_PASSWORD").Read(ctx)
	if err != nil {
		return nil, err
	}
	f, err := s.Host.FS.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	key, leaf, chain, err := pkcs12.DecodeChain(b, password)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	cert := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	for _, c := range chain {
		cert.Certificate = append(cert.Certificate, c.Raw)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func TestApp_Run_HTTPS_with_a_PKCS12_bundle_from_NewCA(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		bundle, password := ca.Issue(t, "api.internal").PKCS12(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"etc/tls/server.p12": bundle,
		}))
		hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello over TLS")
		})
		init := func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
			cfg, err := tlsFromPKCS12(ctx, s)
			if err != nil {
				return nil, err
			}
			return []humus.Service{newHTTPSService(s, ":8443", cfg, hello)}, nil
		}
		p, status := humustest.Start(t, init,
			humustest.WithApp(humus.WithHost(h.Host()), humus.WithTelemetry(telemetry.NoopBuilder)),
			humustest.WithEnv(map[string]string{
				"TLS_PKCS12_FILE":     h.Path("etc/tls/server.p12"),
				"TLS_PKCS12_PASSWORD": password,
			}),
		)
		synctest.Wait()

		client := h.HTTPClient()
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.Pool()}
		resp, err := client.Get("https://api.internal:8443/")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "hello over TLS" {
			t.Errorf("body = %q, want hello over TLS", b)
		}

		p.Signal(syscall.SIGTERM)
		st := <-status
		if st.Code() != 0 {
			t.Errorf("code = %d, want 0: %v", st.Code(), st.Cause())
		}
		if got := p.Stderr(); got != "" {
			t.Errorf("stderr = %q, want nothing", got)
		}
	})
}

func TestApp_Run_HTTPS_a_wrong_password_exits_1_with_the_cause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ca := humustest.NewCA(t)
		bundle, _ := ca.Issue(t, "api.internal").PKCS12(t)
		_, wrong := ca.Issue(t, "api.internal").PKCS12(t)
		h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{
			"etc/tls/server.p12": bundle,
		}))
		app := humus.App{
			Host:                h.Host(),
			ShutdownGracePeriod: 10 * time.Second,
			Telemetry:           telemetry.NoopBuilder,
			Init: func(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
				if _, err := tlsFromPKCS12(ctx, s); err != nil {
					return nil, err
				}
				return nil, nil
			},
		}
		_, status := bedrocktest.Start(t, app, bedrocktest.WithEnv(map[string]string{
			"TLS_PKCS12_FILE":     h.Path("etc/tls/server.p12"),
			"TLS_PKCS12_PASSWORD": wrong,
		}))

		st := <-status
		if st.Code() != humus.ExitFailure {
			t.Errorf("code = %d, want %d", st.Code(), humus.ExitFailure)
		}
		if !errors.Is(st.Cause(), pkcs12.ErrIncorrectPassword) {
			t.Errorf("cause = %v, want pkcs12.ErrIncorrectPassword", st.Cause())
		}
	})
}
