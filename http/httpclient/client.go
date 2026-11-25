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
	"strings"
	"time"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/internal/clientchain"
)

// The pool of a client's http.Transport. It keeps as many idle connections,
// for as long, as http.DefaultTransport does for every host together, all to
// the one origin. None of these is a setting.
const (
	maxIdleConns    = 100
	idleConnTimeout = 90 * time.Second
)

// Network is what a client needs of a host: a way to dial. humus.Host
// implements it, and so does any network of the end user's with this
// method.
type Network interface {
	// DialTCP connects to address over "tcp", "tcp4" or "tcp6", like
	// net.Dialer.DialContext.
	DialTCP(ctx context.Context, network, address string) (net.Conn, error)
}

// NewClient returns an *http.Client for the dependency at settings.BaseURL.
//
// NewClient runs every check on settings, and rejects a BackoffMax below
// BackoffBase, a TLS that doesn't match BaseURL's scheme (an https BaseURL
// with a nil TLS, or an http one with a config), a nil Classify and a
// BreakerPolicy with a nil Trip or Probe. It reports every bad field in one
// error that names each one. It also rejects a nil s, and an s whose
// Telemetry has no LoggerProvider, which the breaker logs through, or no
// MeterProvider, which humus's own instruments are recorded with. It builds the dependency's retry.Budget and retry.Retrier, and
// its circuit breaker from settings.Breaker and settings.BreakerPolicy,
// dials nothing, and registers a function with s.OnStop that closes the
// client's idle connections.
//
// The client dials through n. A request to any origin other than
// settings.BaseURL's fails without being sent. A request that is safe to
// send twice is retried, as settings.Classify decides each attempt's result
// counts. Every attempt goes through the dependency's breaker, which counts
// each attempt's result as Classify decides it, and while the dependency
// keeps failing, rejects attempts at once, with an error that matches
// circuit.ErrRejected, sending nothing. Every attempt it sends or rejects
// is a client span, and is measured, with s.Telemetry's providers and
// propagator. The breaker's state, its changes of state, the attempts it
// counted or rejected, and what the retries decided after each attempt are
// humus's own instruments, with s.Telemetry's MeterProvider, and each
// change of the breaker's state is a log record through s.Telemetry's
// LoggerProvider.
func NewClient(s *humus.Setup, n Network, settings Settings) (*http.Client, error) {
	if s == nil {
		return nil, errors.New("httpclient: NewClient: the Setup must not be nil")
	}
	err := checkSettings(settings)
	if s.Telemetry.MeterProvider == nil {
		err = errors.Join(errors.New("the Setup's Telemetry.MeterProvider must not be nil"), err)
	}
	if s.Telemetry.LoggerProvider == nil {
		err = errors.Join(errors.New("the Setup's Telemetry.LoggerProvider must not be nil"), err)
	}
	if err != nil {
		return nil, fmt.Errorf("httpclient: NewClient: %w", err)
	}
	c, err := clientchain.New(s, "httpclient", scopeName, chainSettings(settings), newTransport(n, settings))
	if err != nil {
		return nil, err
	}
	s.OnStop(func(context.Context) error {
		c.CloseIdleConnections()
		return nil
	})
	return c, nil
}

// scopeName names the client's meter, which records humus's own
// instruments, and its logger, which the breaker logs through, so the
// instruments and the records share a scope.
const scopeName = "github.com/z5labs/humus/http/httpclient"

// newTransport returns the http.Transport at the bottom of the client's
// layers, which dials through n.
func newTransport(n Network, settings Settings) *http.Transport {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	tr := &http.Transport{
		DialContext:         n.DialTCP,
		Proxy:               nil,
		Protocols:           &protocols,
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConns,
		IdleConnTimeout:     idleConnTimeout,
	}
	if strings.EqualFold(settings.BaseURL.Scheme, "https") {
		// HTTP/2 is offered first in ALPN, and HTTP/1.1 after it. An http
		// BaseURL is HTTP/1.1 alone, since humus serves no h2c.
		tr.TLSClientConfig = settings.TLS.Clone()
		protocols.SetHTTP2(true)
	}
	return tr
}

// chainSettings returns the fields of settings the layers are built from.
func chainSettings(settings Settings) clientchain.Settings {
	classify := settings.Classify
	return clientchain.Settings{
		BaseURL:        settings.BaseURL,
		Timeout:        settings.Timeout,
		AttemptTimeout: settings.AttemptTimeout,
		MaxAttempts:    settings.MaxAttempts,
		BackoffBase:    settings.BackoffBase,
		BackoffMax:     settings.BackoffMax,
		RetryBudget:    settings.RetryBudget,
		Classify: func(resp *http.Response, err error) clientchain.Classification {
			return clientchain.Classification(classify(resp, err))
		},
		Breaker:         settings.Breaker,
		BreakerPolicy:   settings.BreakerPolicy,
		ForwardScenario: settings.ForwardScenario,
	}
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
	check("Settings.Breaker.Window", CheckBreakerWindow(settings.Breaker.Window))
	check("Settings.Breaker.Probes", CheckBreakerProbes(settings.Breaker.Probes))
	check("Settings.Breaker.OpenFor", CheckBreakerOpenFor(settings.Breaker.OpenFor))
	check("Settings.Breaker.ProbeTimeout", CheckBreakerProbeTimeout(settings.Breaker.ProbeTimeout))
	if settings.BreakerPolicy.Trip == nil {
		check("Settings.BreakerPolicy.Trip", errNil)
	}
	if settings.BreakerPolicy.Probe == nil {
		check("Settings.BreakerPolicy.Probe", errNil)
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
