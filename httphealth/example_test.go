// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
	"github.com/z5labs/humus/httphealth"
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
	<-served

	// Output: /health/liveness: 200 OK
	// /health/readiness: 503 Service Unavailable
	// /health/readiness: 200 OK
	// /hello: 200 hello
	// /health/readiness: 503 Service Unavailable
}
