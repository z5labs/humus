// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth

import (
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/health"
)

// loggerName is the name of the logger the handler logs through.
const loggerName = "github.com/z5labs/humus/httphealth"

// The paths the handler answers.
const (
	livenessPath  = "/health/liveness"
	readinessPath = "/health/readiness"
)

// NewHandler returns a handler that answers an orchestrator's probes from p:
//
//   - GET /health/liveness checks p.Liveness;
//   - GET /health/readiness checks p.Readiness.
//
// HEAD is answered as GET is, any other method gets 405, and any other path
// 404. A probe answers 200 with the body "OK" when its monitor is healthy,
// and 503 with the body "Service Unavailable" when it is unhealthy or
// returns an error. The monitor's error is never in the body: it is logged,
// at ERROR, through s.Telemetry's LoggerProvider.
//
// The monitor is checked with the request's context, and the handler adds
// no timeout: the probe's own timeout bounds the check.
//
// Mount it at "/health/" of a mux, without StripPrefix. It panics if
// p.Liveness or p.Readiness is nil.
func NewHandler(s *humus.Setup, p health.Probes) http.Handler {
	if p.Liveness == nil {
		panic("httphealth: NewHandler called with a nil Probes.Liveness")
	}
	if p.Readiness == nil {
		panic("httphealth: NewHandler called with a nil Probes.Readiness")
	}
	logger := s.Telemetry.LoggerProvider.Logger(loggerName)

	// A "GET" pattern matches HEAD too, and the mux answers another method
	// on either path with 405 and an Allow header, and any other path with
	// 404.
	mux := http.NewServeMux()
	mux.Handle("GET "+livenessPath, probe{name: "liveness", monitor: p.Liveness, logger: logger})
	mux.Handle("GET "+readinessPath, probe{name: "readiness", monitor: p.Readiness, logger: logger})
	return noStore{next: mux}
}

// noStore sets Cache-Control: no-store on every answer of next, the mux's
// 404 and 405 included, so no cache between the prober and the pod answers
// a probe with an old result.
type noStore struct {
	next http.Handler
}

func (h noStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.next.ServeHTTP(w, r)
}

// probe answers one probe by checking its monitor.
type probe struct {
	// name is the probe's name, liveness or readiness, which is the value of
	// a logged error's health.probe attribute.
	name    string
	monitor health.Monitor
	logger  log.Logger
}

// ServeHTTP checks the monitor once, with the request's context, and
// answers 200 or 503 with the status text as the body. It recovers no
// panic: the server the handler is attached to does.
func (p probe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	healthy, err := p.monitor.Healthy(ctx)
	if err != nil {
		var rec log.Record
		rec.SetTimestamp(time.Now())
		rec.SetSeverity(log.SeverityError)
		rec.SetSeverityText(log.SeverityError.String())
		rec.SetBody(attribute.StringValue(err.Error()))
		rec.AddAttributes(attribute.String("health.probe", p.name))
		p.logger.Emit(ctx, rec)
	}

	code := http.StatusOK
	if !healthy || err != nil {
		code = http.StatusServiceUnavailable
	}
	body := http.StatusText(code) + "\n"

	// Content-Length is set here, so a HEAD gets the same headers as a GET.
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}
