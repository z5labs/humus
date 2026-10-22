// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal/authn"
)

// loggerName is the name of the logger NewHandler's handlers log through.
const loggerName = "github.com/z5labs/humus/auth"

// schemeKey is the record attribute naming the scheme whose verifier
// failed.
const schemeKey = attribute.Key("humus.auth.scheme")

// Answers answers the requests NewHandler's handler refuses. Each one is
// the end user's: auth writes no body of its own.
type Answers struct {
	// Unauthenticated answers a request no requirement authenticated, with
	// WWW-Authenticate already set.
	Unauthenticated http.Handler

	// Forbidden answers a request authenticated without a scope it needs,
	// with WWW-Authenticate already set for a bearer scheme.
	Forbidden http.Handler

	// Failed answers a request a verifier failed on with an error of the
	// server's, after it is recorded.
	Failed http.Handler
}

// NewHandler returns a handler that serves h only to a request that meets
// one of reqs, with its principals in the request's context, and answers
// any other with answers.
//
// It returns one error that names every mistake:
//   - a nil s, h or answer, no requirement, and Disabled;
//   - the zero Scheme or Requirement, and a nil verify function;
//   - two different schemes with one name, and a name with a character
//     other than letters, digits, '.', '-' and '_';
//   - one scheme twice in one All, an All of no requirement, and Disabled
//     with another requirement or inside an All;
//   - Format on a scheme that isn't Bearer;
//   - a Basic realm that is empty or holds a quote, and an API key read
//     from an empty name;
//   - an OAuth2 with no flow, a flow without the URLs it needs, or a scope
//     no flow declares;
//   - an OpenIDConnect whose discovery URL isn't absolute.
//
// It logs through s.Telemetry's LoggerProvider, with the logger name
// "github.com/z5labs/humus/auth".
func NewHandler(s *humus.Setup, h http.Handler, answers Answers, reqs ...Requirement) (http.Handler, error) {
	return newHandler(s, h, answers, reqs, false)
}

// newHandler is NewHandler, which takes Disabled when allowDisabled is
// true, as the engine does for openapi and grpcauth.
func newHandler(s *humus.Setup, h http.Handler, answers Answers, reqs []Requirement, allowDisabled bool) (http.Handler, error) {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if s == nil {
		fail("s is nil")
	}
	if h == nil {
		fail("h is nil")
	}
	if answers.Unauthenticated == nil {
		fail("Answers.Unauthenticated is nil")
	}
	if answers.Forbidden == nil {
		fail("Answers.Forbidden is nil")
	}
	if answers.Failed == nil {
		fail("Answers.Failed is nil")
	}
	for i, r := range reqs {
		if !allowDisabled && authn.IsDisabled(r) {
			fail("reqs[%d] is Disabled, which authenticates no one: serve h itself", i)
		}
	}
	list, mistakes := authn.Compile(reqs)
	errs = append(errs, mistakes...)
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("auth: NewHandler: %w", err)
	}
	return &handler{
		next:    h,
		answers: answers,
		list:    list,
		logger:  s.Telemetry.LoggerProvider.Logger(loggerName),
	}, nil
}

// handler is the handler NewHandler returns.
type handler struct {
	next    http.Handler
	answers Answers
	list    *authn.List
	logger  log.Logger
}

// ServeHTTP authenticates r, and serves it with next, or answers it.
//
// It recovers nothing: a verifier's panic passes to httpserver, which
// records it and aborts the response.
//
// next is served a copy of r, which carries the authenticated principal.
// The Pattern next sets on that copy, as an http.ServeMux does, is set back
// on r, so httpserver's span is still named after the route.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	res := h.list.Authenticate(r.Context(), authn.HTTPSource(r))
	switch res.Outcome {
	case authn.Served:
		authenticated := r.WithContext(res.Context)
		defer func() {
			if authenticated.Pattern != "" {
				r.Pattern = authenticated.Pattern
			}
		}()
		h.next.ServeHTTP(w, authenticated)
	case authn.Unauthenticated:
		challenge(w, res.Challenges)
		h.answers.Unauthenticated.ServeHTTP(w, r)
	case authn.Forbidden:
		challenge(w, res.Challenges)
		h.answers.Forbidden.ServeHTTP(w, r)
	default:
		h.record(r, res.Scheme, res.Err)
		h.answers.Failed.ServeHTTP(w, r)
	}
}

// challenge sets one WWW-Authenticate header for each of challenges.
func challenge(w http.ResponseWriter, challenges []string) {
	for _, c := range challenges {
		w.Header().Add("WWW-Authenticate", c)
	}
}

// record records a verifier's error as an exception event on the request's
// span, and as one ERROR record, "authentication failed", in the request's
// context, so the record carries the span's trace and span IDs. When the
// request's context is done, because the client went away, err goes on
// the span alone. No principal, subject or key is recorded.
func (h *handler) record(r *http.Request, scheme string, err error) {
	ctx := r.Context()
	trace.SpanFromContext(ctx).RecordError(err)
	if ctx.Err() != nil {
		return
	}
	emit(ctx, h.logger, "authentication failed",
		schemeKey.String(scheme),
		semconv.ErrorType(err),
		semconv.ExceptionMessage(err.Error()),
		semconv.HTTPRequestMethodKey.String(r.Method),
	)
}

// emit emits one ERROR record, with body and attrs, through logger.
func emit(ctx context.Context, logger log.Logger, body string, attrs ...attribute.KeyValue) {
	var rec log.Record
	rec.SetTimestamp(time.Now())
	rec.SetSeverity(log.SeverityError)
	rec.SetSeverityText(log.SeverityError.String())
	rec.SetBody(attribute.StringValue(body))
	rec.AddAttributes(attrs...)
	logger.Emit(ctx, rec)
}
