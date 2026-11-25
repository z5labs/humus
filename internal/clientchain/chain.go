// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package clientchain is the chain of layers every humus HTTP client sends
// its calls through: the origin check and the retries, otelhttp's
// instrumentation, the circuit breaker, the attempt timeout and failure
// injection.
//
// httpclient calls a dependency over HTTP/1.1 and HTTP/2 through it, and
// http3client calls one over HTTP/3 through it, so a call is retried,
// broken, timed, injected and measured the same way whatever protocol it
// goes over. It knows nothing of the protocol: each client passes the
// transport at the bottom of the chain, which sends each attempt and holds
// the connections, its own name, which its errors start with, and the scope
// its instruments and records are under.
package clientchain

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/z5labs/bedrock/circuit"
	"github.com/z5labs/bedrock/retry"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/z5labs/humus"
)

// maxRedirects is how many redirects a client follows, as net/http's
// default policy does.
const maxRedirects = 10

// Classification is how one attempt's result counts. Each client's own
// Classification has the same fields, and converts to it.
type Classification struct {
	// Retry is whether another attempt follows.
	Retry retry.Outcome

	// RetryAfter is, with retry.Retry, the least time to wait before the
	// next attempt.
	RetryAfter time.Duration

	// Breaker is how the result counts in the dependency's circuit breaker.
	Breaker circuit.Outcome
}

// Settings are the fields of a client's Settings the chain is built from.
// Each means what httpclient's field of the same name means. The client
// has run every check on them before New is called.
type Settings struct {
	BaseURL         *url.URL
	Timeout         time.Duration
	AttemptTimeout  time.Duration
	MaxAttempts     int
	BackoffBase     time.Duration
	BackoffMax      time.Duration
	RetryBudget     retry.BudgetConfig
	Classify        func(resp *http.Response, err error) Classification
	Breaker         circuit.Config
	BreakerPolicy   circuit.Policy
	ForwardScenario bool
}

// Transport is the bottom of the chain: it sends each attempt, and holds the
// client's connections. *http.Transport and quic-go's *http3.Transport are
// both one.
type Transport interface {
	http.RoundTripper

	// CloseIdleConnections closes the connections no attempt is using.
	CloseIdleConnections()
}

// New returns an *http.Client for the dependency at settings.BaseURL, which
// sends every attempt through bottom. name is the client's package, such as
// httpclient, which every error and panic of the client starts with. scope
// names the meter that records humus's own instruments, and the logger the
// breaker logs through.
//
// s must not be nil, and its Telemetry must have a MeterProvider and a
// LoggerProvider. New builds the dependency's retry.Budget and
// retry.Retrier, and its circuit breaker, and dials nothing. It registers
// no OnStop function: closing bottom's connections is the client's.
//
// The client refuses a request to any origin other than settings.BaseURL's,
// and follows a redirect only within it, at most 10 times. Its
// CloseIdleConnections reaches bottom's.
func New(s *humus.Setup, name, scope string, settings Settings, bottom Transport) (*http.Client, error) {
	retrier, err := newRetrier(settings)
	if err != nil {
		// The client's checks accept only what retry accepts, so this is
		// humus's bug.
		return nil, fmt.Errorf("%s: NewClient: %w", name, err)
	}

	base := originOf(settings.BaseURL)
	meter := s.Telemetry.MeterProvider.Meter(scope)
	m, err := newInstruments(meter, base)
	if err != nil {
		// The instruments' names and options are humus's own, so this is
		// humus's bug.
		return nil, fmt.Errorf("%s: NewClient: creating its instruments: %w", name, err)
	}
	logger := s.Telemetry.LoggerProvider.Logger(scope)
	breaker, err := circuit.New(settings.Breaker, settings.BreakerPolicy, m.countCalls(countVerdict), newObserver(logger, m))
	if err != nil {
		// The client's checks accept only what circuit accepts, so this is
		// humus's bug too.
		return nil, fmt.Errorf("%s: NewClient: building the circuit breaker: %w", name, err)
	}

	// The layers, outermost first: the origin check and the retries, then
	// otelhttp, then the breaker, which classifies each attempt, then the
	// attempt timeout, then the injection layer, then bottom. otelhttp sits
	// inside the origin check, so a refused request is not an attempt, and
	// outside the breaker, so an attempt the breaker rejects has a span, and
	// the layers that see each attempt's span find it in the request's
	// context. The breaker sits inside the retries, so each attempt is a
	// call it admits or rejects. The injection layer sits inside the breaker
	// and the attempt timeout, so an attempt a fault ends is classified, and
	// a Delay is bounded, as the failure it emulates is. Every provider and
	// the propagator are passed, so otelhttp never falls back to
	// OpenTelemetry's globals.
	injected := injectFaults{
		faults:     s.Faults,
		point:      pointOf(base),
		forward:    settings.ForwardScenario,
		propagator: s.Telemetry.Propagator,
		next:       bottom,
	}
	attempts := attemptTransport{
		name:     name,
		classify: settings.Classify,
		breaker:  breaker,
		metrics:  m,
		next:     newAttemptTimeout(name, settings.AttemptTimeout, injected),
	}
	instrumented := otelhttp.NewTransport(attempts,
		otelhttp.WithTracerProvider(s.Telemetry.TracerProvider),
		otelhttp.WithMeterProvider(s.Telemetry.MeterProvider),
		otelhttp.WithPropagators(s.Telemetry.Propagator),
		otelhttp.WithSpanNameFormatter(spanName),
	)

	c := &http.Client{
		Transport: &retryTransport{name: name, origin: base, retrier: retrier, metrics: m, next: instrumented, conns: bottom},
		Timeout:   settings.Timeout,
		Jar:       nil,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if originOf(req.URL) != base {
				return http.ErrUseLastResponse
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
	// humus.circuit.state's callback is registered last, once nothing after
	// it can fail, so a New that returns an error leaves none behind.
	if err := m.observeState(meter, breaker); err != nil {
		return nil, fmt.Errorf("%s: NewClient: registering humus.circuit.state's callback: %w", name, err)
	}
	return c, nil
}

// mustBeValid panics if c is a Classification no classifier may return: a
// zero or unknown Outcome, or retry.Retry with a negative RetryAfter. Its
// panics start with name.
func mustBeValid(name string, c Classification) {
	switch c.Retry {
	case retry.Success, retry.Retry, retry.Stop:
	default:
		panic(name + ": Settings.Classify returned a Classification whose Retry is " + c.Retry.String() + ", not a retry.Outcome")
	}
	switch c.Breaker {
	case circuit.Success, circuit.Failure, circuit.Ignored:
	default:
		panic(name + ": Settings.Classify returned a Classification whose Breaker is " + c.Breaker.String() + ", not a circuit.Outcome")
	}
	if c.Retry == retry.Retry && c.RetryAfter < 0 {
		panic(name + ": Settings.Classify returned retry.Retry with a negative RetryAfter, " + c.RetryAfter.String())
	}
}

// origin is the scheme, host and port of a URL, with the scheme's default
// port when the URL has none. Two URLs of one origin are equal origins.
type origin struct {
	scheme string
	host   string
	port   string
}

// originOf returns u's origin. Its scheme and host are lowercase, as both
// compare without regard to case.
func originOf(u *url.URL) origin {
	o := origin{
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Hostname()),
		port:   u.Port(),
	}
	if o.port == "" {
		switch o.scheme {
		case "https":
			o.port = "443"
		case "http":
			o.port = "80"
		}
	}
	return o
}

// String returns the origin as scheme://host:port.
func (o origin) String() string {
	return o.scheme + "://" + net.JoinHostPort(o.host, o.port)
}
