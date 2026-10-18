// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/z5labs/humus"
)

// The pool of a client's http.Transport. It keeps as many idle connections,
// for as long, as http.DefaultTransport does for every host together, all to
// the one origin. None of these is a setting.
const (
	maxIdleConns    = 100
	idleConnTimeout = 90 * time.Second
)

// maxRedirects is how many redirects a client follows, as net/http's
// default policy does.
const maxRedirects = 10

// NewClient returns an *http.Client for the dependency at settings.BaseURL.
//
// NewClient runs every check on settings, and rejects a BackoffMax below
// BackoffBase, a TLS that doesn't match BaseURL's scheme (an https BaseURL
// with a nil TLS, or an http one with a config) and a nil Classify. It
// reports every bad field in one error that names each one. It also rejects
// a nil s, and an s whose Host has no Dial, rather than let net/http fall
// back to dialling the real network. It builds the dependency's
// retry.Budget and retry.Retrier, dials nothing, and registers a function
// with s.OnStop that closes the client's idle connections.
//
// The client dials through s.Host. A request to any origin other than
// settings.BaseURL's fails without being sent. A request that is safe to
// send twice is retried, as settings.Classify decides each attempt's result
// counts. Every attempt it sends is a client span, and is measured, with
// s.Telemetry's providers and propagator.
func NewClient(s *humus.Setup, settings Settings) (*http.Client, error) {
	if s == nil {
		return nil, errors.New("httpclient: NewClient: the Setup must not be nil")
	}
	err := checkSettings(settings)
	if s.Host.Dial == nil {
		err = errors.Join(errors.New("the Setup's Host.Dial must not be nil"), err)
	}
	if err != nil {
		return nil, fmt.Errorf("httpclient: NewClient: %w", err)
	}
	retrier, err := newRetrier(settings)
	if err != nil {
		// checkSettings accepts only what retry accepts, so this is
		// humus's bug.
		return nil, fmt.Errorf("httpclient: NewClient: %w", err)
	}

	base := originOf(settings.BaseURL)

	var protocols http.Protocols
	protocols.SetHTTP1(true)
	tr := &http.Transport{
		DialContext:         s.Host.Dial,
		Proxy:               nil,
		Protocols:           &protocols,
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConns,
		IdleConnTimeout:     idleConnTimeout,
	}
	if base.scheme == "https" {
		// HTTP/2 is offered first in ALPN, and HTTP/1.1 after it. An http
		// BaseURL is HTTP/1.1 alone, since humus serves no h2c.
		tr.TLSClientConfig = settings.TLS.Clone()
		protocols.SetHTTP2(true)
	}

	// The layers, outermost first: the origin check and the retries, then
	// otelhttp, then the layer that classifies each attempt, then the
	// attempt timeout, then the http.Transport. otelhttp sits inside the
	// origin check, so a refused request is not an attempt, and outside
	// the layers that see each attempt's span, so they find it in the
	// request's context. Every provider and the propagator are passed, so
	// otelhttp never falls back to OpenTelemetry's globals.
	attempts := attemptTransport{
		classify: settings.Classify,
		next:     newAttemptTimeout(settings.AttemptTimeout, tr),
	}
	instrumented := otelhttp.NewTransport(attempts,
		otelhttp.WithTracerProvider(s.Telemetry.TracerProvider),
		otelhttp.WithMeterProvider(s.Telemetry.MeterProvider),
		otelhttp.WithPropagators(s.Telemetry.Propagator),
		otelhttp.WithSpanNameFormatter(spanName),
	)

	c := &http.Client{
		Transport: &retryTransport{origin: base, retrier: retrier, next: instrumented, conns: tr},
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
	s.OnStop(func(context.Context) error {
		c.CloseIdleConnections()
		return nil
	})
	return c, nil
}

// checkSettings runs every check on settings, and the agreement of TLS with
// BaseURL's scheme. It reports every failure in one error, each naming its
// field.
func checkSettings(settings Settings) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	baseErr := CheckBaseURL(settings.BaseURL)
	tlsErr := CheckTLS(settings.TLS)
	check("Settings.BaseURL", baseErr)
	check("Settings.TLS", tlsErr)
	check("Settings.Timeout", CheckTimeout(settings.Timeout))
	check("Settings.AttemptTimeout", CheckAttemptTimeout(settings.AttemptTimeout))
	check("Settings.MaxAttempts", CheckMaxAttempts(settings.MaxAttempts))
	backoffBaseErr := CheckBackoffBase(settings.BackoffBase)
	backoffMaxErr := CheckBackoffMax(settings.BackoffMax)
	check("Settings.BackoffBase", backoffBaseErr)
	check("Settings.BackoffMax", backoffMaxErr)
	if backoffBaseErr == nil && backoffMaxErr == nil && settings.BackoffMax < settings.BackoffBase {
		check("Settings.BackoffMax", fmt.Errorf("must be at least Settings.BackoffBase, %s, got %s", settings.BackoffBase, settings.BackoffMax))
	}
	check("Settings.RetryBudget.MaxTokens", CheckRetryBudgetMaxTokens(settings.RetryBudget.MaxTokens))
	check("Settings.RetryBudget.TokenRatio", CheckRetryBudgetTokenRatio(settings.RetryBudget.TokenRatio))
	if settings.Classify == nil {
		check("Settings.Classify", errNil)
	}
	if baseErr == nil && tlsErr == nil {
		switch https := settings.BaseURL.Scheme == "https"; {
		case https && settings.TLS == nil:
			check("Settings.TLS", errors.New("must be set for an https Settings.BaseURL"))
		case !https && settings.TLS != nil:
			check("Settings.TLS", errors.New("must be nil for an http Settings.BaseURL, which is plain text"))
		}
	}
	return errors.Join(errs...)
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
