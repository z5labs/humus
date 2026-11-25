// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package httpchain is the handler chain every humus HTTP server serves the
// end user's handler through: otelhttp's instrumentation, route naming,
// panic recording and scenario stripping.
//
// httpserver serves HTTP/1.1 and HTTP/2 through it, and http3server serves
// HTTP/3 through it, so a request is instrumented, named, recorded and
// stripped the same way whatever protocol it came over. It knows nothing
// of the protocol: each server passes the logger named after its own
// package, so the records the chain emits keep that package's scope.
package httpchain

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"

	"github.com/z5labs/humus"
)

// Wrap returns h wrapped in the chain, with s.Telemetry's providers and
// propagator, and s.Faults. A panic of h is recorded on the request's span
// and as an ERROR record emitted through logger, and the response is
// aborted with http.ErrAbortHandler. Before h runs, the failure scenario a
// request carries is stripped with s.Faults, unless trust trusts the
// caller. A nil trust trusts no caller.
func Wrap(s *humus.Setup, logger log.Logger, trust func(r *http.Request) bool, h http.Handler) http.Handler {
	// The operation name is empty, so otelhttp names each span after the
	// method and the request's route pattern, as the semantic conventions
	// do. Every provider and the propagator are passed, so otelhttp never
	// falls back to OpenTelemetry's globals. recordPanics runs inside it, so
	// a panic is recorded on the request's span before the span ends.
	// stripScenarios runs inside recordPanics, after otelhttp has extracted
	// the baggage and before the handler, so a panic of trust is recorded as
	// the handler's would be. nameRoute runs right below otelhttp, so the
	// route reaches otelhttp's request however many handlers on the way pass
	// on a copy of it.
	stripped := stripScenarios{next: h, faults: s.Faults, trust: trust}
	named := nameRoute{next: recordPanics{next: stripped, logger: logger}}
	return otelhttp.NewHandler(named, "",
		otelhttp.WithTracerProvider(s.Telemetry.TracerProvider),
		otelhttp.WithMeterProvider(s.Telemetry.MeterProvider),
		otelhttp.WithPropagators(s.Telemetry.Propagator),
	)
}

// emit emits one record at sev, with body and attrs, through logger.
func emit(ctx context.Context, logger log.Logger, sev log.Severity, body string, attrs ...attribute.KeyValue) {
	var r log.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(sev.String())
	r.SetBody(attribute.StringValue(body))
	r.AddAttributes(attrs...)
	logger.Emit(ctx, r)
}
