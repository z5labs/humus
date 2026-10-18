// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/z5labs/bedrock/config"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
	"github.com/z5labs/humus/httpserver"
	"github.com/z5labs/humus/telemetry"
)

// The handler is mounted at "/health/" of the mux an API is served on,
// answering from a health Service's Probes. In the end user's Init, the mux
// goes to httpserver.NewService, and Init returns that Service and the
// health Service. Here the App's part is played by hand, and each request
// is answered with an httptest.ResponseRecorder.
func ExampleNewHandler() {
	s := &humus.Setup{Telemetry: telemetry.Noop()} // the Setup the App hands Init

	var reachable health.Binary // a stand-in for a database ping
	hs := health.NewService(health.Readiness(&reachable))

	api := http.NewServeMux()
	api.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello\n")
	})

	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))

	get := func(path string) string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return fmt.Sprintf("%s: %d %s", path, rec.Code, rec.Body.String())
	}

	fmt.Print(get("/health/liveness"))
	fmt.Print(get("/health/readiness")) // the health Service isn't serving yet

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ctx) }()
	reachable.MarkHealthy()
	for ready, _ := hs.Probes().Readiness.Healthy(ctx); !ready; ready, _ = hs.Probes().Readiness.Healthy(ctx) {
		time.Sleep(time.Millisecond) // until Serve has started
	}
	fmt.Print(get("/health/readiness"))
	fmt.Print(get("/hello"))

	cancel() // the App starts shutting down
	fmt.Print(get("/health/readiness"))
	fmt.Println("Serve returned:", <-served)

	// Output: /health/liveness: 200 OK
	// /health/readiness: 503 Service Unavailable
	// /health/readiness: 200 OK
	// /hello: 200 hello
	// /health/readiness: 503 Service Unavailable
	// Serve returned: <nil>
}

// TestAPIInit_answers_the_probes_on_the_APIs_port runs APIInit.

// APIInit is the end user's Init for an HTTPS API: it attaches the probes
// to the mux the API is served on, and returns the health Service whose
// Probes they answer from next to the API's Service. The pod needs no
// second port: the kubelet probes the API's own, with scheme: HTTPS.
func APIInit(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	settings, err := config.Struct(func(r *config.Resolver) httpserver.Settings {
		settings := httpserver.DefaultSettings()
		settings.Addr = r.Get(env("ADDR").Validate(httpserver.CheckAddr))
		settings.TLS = r.Get(env("TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httpserver.CheckTLS))
		return settings
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	db := openStore() // the end user's database
	hs := health.NewService(health.Readiness(health.MonitorFunc(func(ctx context.Context) (bool, error) {
		if err := db.Ping(ctx); err != nil {
			return false, err // logged, never in the probe's answer
		}
		return true, nil
	})))

	api := http.NewServeMux()
	api.HandleFunc("GET /hello", hello)

	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.Handle("/health/", httphealth.NewHandler(s, hs.Probes()))
	svc, err := httpserver.NewService(s, settings, mux)
	if err != nil {
		return nil, err
	}
	return []humus.Service{svc, hs}, nil
}

// store stands in for the end user's database client.
type store struct{}

func openStore() store { return store{} }

// Ping reports whether the database answers, honouring ctx.
func (store) Ping(ctx context.Context) error { return ctx.Err() }

// hello is the end user's API.
func hello(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "hello\n")
}

// TestInit_serves_the_probes_next_to_a_worker runs Init.

// Init is the end user's Init for a pod with no server of its own: it runs
// a worker, and a probes server whose readiness waits for the worker to
// connect. It returns the two Services, and no health Service: the probes
// server owns readiness.
func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
	env := func(name string) config.Value[string] { return config.Env(s.Process.Environment, name) }

	probeSettings, err := config.Struct(func(r *config.Resolver) httphealth.Settings {
		return httphealth.Settings{
			// No .Default: both fields are required.
			Addr: r.Get(env("PROBES_ADDR").Validate(httphealth.CheckAddr)),
			TLS:  r.Get(env("PROBES_TLS_PEM_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(httphealth.CheckTLS)),
		}
	}).Read(ctx)
	if err != nil {
		return nil, err // exit 78, naming every bad variable
	}

	w := newWorker()
	probes, err := httphealth.NewService(s, probeSettings, health.Readiness(w.Connected()))
	if err != nil {
		return nil, err
	}
	return []humus.Service{w, probes}, nil
}

// worker stands in for a Kafka consumer, or any other Service with no
// server: it is ready once it has connected, as a consumer is once it has
// joined its group.
type worker struct {
	connected health.Binary
	stop      chan struct{}
	stopOnce  sync.Once
}

func newWorker() *worker {
	return &worker{stop: make(chan struct{})}
}

// Connected returns a monitor that is healthy once the worker has
// connected.
func (w *worker) Connected() health.Monitor {
	return &w.connected
}

// Serve connects, and works until ctx is done or Shutdown is called.
func (w *worker) Serve(ctx context.Context) error {
	w.connected.MarkHealthy()
	select {
	case <-ctx.Done():
	case <-w.stop:
	}
	return nil
}

// Shutdown stops Serve.
func (w *worker) Shutdown(context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	return nil
}

// readFile returns a parser for config.Value.Parse that reads the whole
// file a variable names, through fsys.
func readFile(fsys humus.FS) func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		f, err := fsys.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
}

// serverTLS returns a server's TLS config for a PEM file that holds its
// certificate chain and its private key. The probes need no certificate of
// their own: the kubelet doesn't verify it.
func serverTLS(pem []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(pem, pem)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// A pod with no server, such as a consumer's, builds the probes server in
// its Init, with the monitors its readiness checks, and returns it next to
// the Service that does the work. Its main is humus.Main, and its probes
// set scheme: HTTPS. This example has no Output comment, so it is compiled
// but never run: running it would exit the test binary.
func ExampleNewService() {
	humus.Main(Init)
}
