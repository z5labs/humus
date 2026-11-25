// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/z5labs/humus"
	"github.com/z5labs/humus/http3/internal/quicdial"
	"github.com/z5labs/humus/internal/clientchain"
)

// scopeName names the client's meter, which records humus's own
// instruments, and its logger, which the breaker logs through, so the
// instruments and the records share a scope.
const scopeName = "github.com/z5labs/humus/http3/http3client"

// The QUIC config of every connection the client dials. None of these is a
// setting.
const (
	// handshakeIdleTimeout ends a handshake the server is silent for this
	// long. quic-go also ends one that runs twice as long in all.
	handshakeIdleTimeout = 5 * time.Second

	// maxIdleTimeout is the idle timeout the client offers. The smaller of
	// the peers' applies.
	maxIdleTimeout = 30 * time.Second

	// keepAlivePeriod is how often the client sends a keep-alive on a
	// connection it isn't using, so the connection outlives a server's idle
	// timeout shorter than its own.
	keepAlivePeriod = 10 * time.Second
)

// Network is what a client needs of a host: a way to dial over UDP.
// humus.Host implements it, and so does any network of the end user's with
// this method.
type Network interface {
	// DialUDP returns a UDP socket connected to address, for "udp", "udp4"
	// or "udp6", like net.Dialer.DialContext.
	DialUDP(ctx context.Context, network, address string) (net.Conn, error)
}

// NewClient returns an *http.Client for the dependency at settings.BaseURL
// that speaks HTTP/3 alone, over UDP. It never falls back to TCP: a
// dependency that doesn't answer over UDP fails each attempt, and the
// breaker counts it.
//
// NewClient runs every check on settings, and rejects a BackoffMax below
// BackoffBase, a nil Classify and a BreakerPolicy with a nil Trip or Probe.
// It reports every bad field in one error that names each one. It also
// rejects a nil s, and an s whose Telemetry has no LoggerProvider, which the
// breaker logs through, or no MeterProvider, which humus's own instruments
// are recorded with. It builds the dependency's retry.Budget and
// retry.Retrier, and its circuit breaker from settings.Breaker and
// settings.BreakerPolicy, and dials nothing.
//
// The client dials each QUIC connection with n.DialUDP(ctx, "udp", address),
// so quic-go never opens a socket or resolves a name itself. It registers a
// function with s.OnStop that closes every QUIC connection and socket it
// dialled that is still open, including one quic-go stopped using after a
// stream error.
//
// Otherwise the client behaves as httpclient's does. A request to any origin
// other than settings.BaseURL's fails without being sent. A request that is
// safe to send twice is retried, as settings.Classify decides each attempt's
// result counts. Every attempt goes through the dependency's breaker, which
// counts each attempt's result as Classify decides it, and while the
// dependency keeps failing, rejects attempts at once, with an error that
// matches circuit.ErrRejected, sending nothing. Failures are injected at
// http:<host>:<port>. Every attempt it sends or rejects is a client span,
// whose network.protocol.version is 3.0, and is measured, with s.Telemetry's
// providers and propagator. The breaker's state, its changes of state, the
// attempts it counted or rejected, and what the retries decided after each
// attempt are humus's own instruments, with s.Telemetry's MeterProvider,
// and each change of the breaker's state is a log record through
// s.Telemetry's LoggerProvider, both under the scope
// github.com/z5labs/humus/http3/http3client.
func NewClient(s *humus.Setup, n Network, settings Settings) (*http.Client, error) {
	if s == nil {
		return nil, errors.New("http3client: NewClient: the Setup must not be nil")
	}
	err := checkSettings(settings)
	if s.Telemetry.MeterProvider == nil {
		err = errors.Join(errors.New("the Setup's Telemetry.MeterProvider must not be nil"), err)
	}
	if s.Telemetry.LoggerProvider == nil {
		err = errors.Join(errors.New("the Setup's Telemetry.LoggerProvider must not be nil"), err)
	}
	if err != nil {
		return nil, fmt.Errorf("http3client: NewClient: %w", err)
	}

	d := quicdial.New(n.DialUDP)
	tr := newTransport(d, settings)
	c, err := clientchain.New(s, "http3client", scopeName, chainSettings(settings), tr)
	if err != nil {
		return nil, err
	}
	c.Transport = protoTransport{next: c.Transport}
	s.OnStop(func(context.Context) error {
		// The Transport closes the connections it still uses, and the Dialer
		// every connection still open, those quic-go stopped using after a
		// stream error included, with their sockets.
		_ = tr.Close()
		d.Close()
		return nil
	})
	return c, nil
}

// newTransport returns the http3.Transport at the bottom of the client's
// layers, which dials every QUIC connection through d.
func newTransport(d *quicdial.Dialer, settings Settings) *http3.Transport {
	return &http3.Transport{
		// quic-go sets the ServerName from the request's host, and h3 as the
		// only protocol in ALPN. A nil config means the system's roots.
		TLSClientConfig: settings.TLS.Clone(),
		QUICConfig: &quic.Config{
			HandshakeIdleTimeout: handshakeIdleTimeout,
			MaxIdleTimeout:       maxIdleTimeout,
			KeepAlivePeriod:      keepAlivePeriod,
		},
		Dial: d.Dial,
	}
}

// protoTransport is the client's outermost layer. It sets each request's
// Proto to HTTP/3.0 before the retries and otelhttp see it, so the span of
// every attempt, a failed one's included, records network.protocol.version
// 3.0. net/http sets a request's Proto to HTTP/1.1, and otelhttp reads it
// when the attempt has no response.
type protoTransport struct {
	next http.RoundTripper
}

// RoundTrip implements http.RoundTripper. It doesn't modify r: it sends a
// shallow copy.
func (t protoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.WithContext(r.Context())
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/3.0", 3, 0
	return t.next.RoundTrip(r)
}

// CloseIdleConnections closes the client's idle connections, so that
// http.Client.CloseIdleConnections reaches them through this layer.
func (t protoTransport) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
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

// checkSettings runs every check on settings. It reports every failure in
// one error, each naming its field.
func checkSettings(settings Settings) error {
	var errs []error
	check := func(field string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %w", field, err))
		}
	}
	check("Settings.BaseURL", CheckBaseURL(settings.BaseURL))
	check("Settings.TLS", CheckTLS(settings.TLS))
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
	return errors.Join(errs...)
}
