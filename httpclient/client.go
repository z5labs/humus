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
// NewClient runs every check on settings, and rejects a TLS that doesn't
// match BaseURL's scheme: an https BaseURL with a nil TLS, or an http one
// with a config. It reports every bad field in one error that names each
// one. It also rejects a nil s, and an s whose Host has no Dial, rather than
// let net/http fall back to dialling the real network. It dials nothing, and
// registers a function with s.OnStop that closes the client's idle
// connections.
//
// The client dials through s.Host. A request to any origin other than
// settings.BaseURL's fails without being sent.
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

	c := &http.Client{
		Transport: &originTransport{origin: base, next: tr},
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

// originTransport refuses every request to an origin other than its own,
// before anything is sent, and passes the rest to next. The check is the
// client's first step.
type originTransport struct {
	origin origin
	next   *http.Transport
}

// RoundTrip implements http.RoundTripper. A refused request's body is
// closed, as RoundTrip must close it even on an error.
func (t *originTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if o := originOf(r.URL); o != t.origin {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, fmt.Errorf("httpclient: request to %s on a client for %s", o, t.origin)
	}
	return t.next.RoundTrip(r)
}

// CloseIdleConnections closes next's idle connections, so that
// http.Client.CloseIdleConnections reaches them.
func (t *originTransport) CloseIdleConnections() {
	t.next.CloseIdleConnections()
}
