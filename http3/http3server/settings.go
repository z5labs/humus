// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package http3server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Settings configures an HTTP/3 service. Every field is a plain value, and a
// field's zero value means zero, never "use the default": start from
// DefaultSettings and change the fields you source. Addr and TLS have no
// default, so they are always among them.
type Settings struct {
	// Addr is the UDP address the Service binds through its Network, as
	// host:port. It is required: humus imposes no port. An empty host binds
	// every address. Port 0 picks a free port, which Service.Addr reports.
	//
	// Default: none. Valid: host:port, with a port from 0 to 65535.
	// Check: CheckAddr.
	Addr string

	// TLS is the config the Service serves HTTP/3 with. It is required:
	// QUIC carries nothing in plain text.
	//
	// Default: none. Valid: a config with Certificates, GetCertificate or
	// GetConfigForClient set, that allows TLS 1.3. Check: CheckTLS.
	TLS *tls.Config

	// ReadHeaderTimeout bounds reading a request's headers. Zero means
	// ReadTimeout applies.
	//
	// Default: 2s. Valid: not negative. Check: CheckReadHeaderTimeout.
	ReadHeaderTimeout time.Duration

	// ReadTimeout bounds reading a whole request stream, its body included.
	// Once it has passed, from the end of the request's headers, a read of
	// the body fails, so a slow upload ends. Zero means no timeout. A
	// handler moves its own deadline with http.ResponseController.
	//
	// Default: 5s. Valid: not negative. Check: CheckReadTimeout.
	ReadTimeout time.Duration

	// WriteTimeout bounds writing a response, from the end of the request's
	// headers. When it passes, the request's context is cancelled, every
	// read and write of its stream fails, and once the handler returns, the
	// stream is reset with H3_INTERNAL_ERROR, even for a handler that never
	// wrote. Zero means no timeout. A handler that streams extends its own
	// deadline with http.ResponseController.
	//
	// Default: 10s. Valid: not negative. Check: CheckWriteTimeout.
	WriteTimeout time.Duration

	// IdleTimeout bounds how long a connection with no request in flight
	// stays open. Zero means ReadTimeout applies. QUIC also closes a
	// connection that is silent for 30s.
	//
	// Default: 120s. Valid: not negative. Check: CheckIdleTimeout.
	IdleTimeout time.Duration

	// MaxHeaderBytes bounds the size of a request's headers.
	//
	// Default: 1 MiB (1 << 20). Valid: positive. Check: CheckMaxHeaderBytes.
	MaxHeaderBytes int

	// TrustScenario reports whether the failure scenario a request carries
	// reaches the handler. A request it doesn't trust has the scenario
	// stripped from its context first. It is asked only about a request
	// that carries a scenario, and only when the App has failure
	// injection. It is set in code, not sourced.
	//
	// Default: nil, which trusts no caller. Valid: any. Check: none.
	TrustScenario func(r *http.Request) bool
}

// DefaultSettings returns an HTTP/3 service's defaults, which are those of
// an HTTP/1.1 and HTTP/2 service. Addr and TLS have none, so its Addr is ""
// and its TLS is nil, which CheckAddr and CheckTLS reject: the end user
// always sources both.
func DefaultSettings() Settings {
	return Settings{
		Addr:              "",
		TLS:               nil,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TrustScenario:     nil,
	}
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errNil         = errors.New("must not be nil")
	errNegative    = errors.New("must not be negative")
	errNotPositive = errors.New("must be positive")
)

// CheckAddr rejects an address that a Network can't bind: one that is not
// host:port, such as "" or an address with no port, or one whose port is
// not a number from 0 to 65535. An empty host and port 0 are valid.
func CheckAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("%q has no port from 0 to 65535", addr)
	}
	return nil
}

// CheckTLS reports why c can't serve HTTP/3, or nil if it can. c must not
// be nil, must have Certificates, GetCertificate or GetConfigForClient set,
// and must allow TLS 1.3, which QUIC requires: a MaxVersion of TLS 1.3 or
// later, or none.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return errNil
	}
	if len(c.Certificates) == 0 && c.GetCertificate == nil && c.GetConfigForClient == nil {
		return errors.New("has no certificate: none of Certificates, GetCertificate and GetConfigForClient is set")
	}
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS13 {
		return fmt.Errorf("has a MaxVersion of %s, below the TLS 1.3 that QUIC requires", tls.VersionName(c.MaxVersion))
	}
	return nil
}

// CheckReadHeaderTimeout rejects a negative timeout. Zero is valid.
func CheckReadHeaderTimeout(d time.Duration) error {
	return checkTimeout(d)
}

// CheckReadTimeout rejects a negative timeout. Zero is valid.
func CheckReadTimeout(d time.Duration) error {
	return checkTimeout(d)
}

// CheckWriteTimeout rejects a negative timeout. Zero is valid.
func CheckWriteTimeout(d time.Duration) error {
	return checkTimeout(d)
}

// CheckIdleTimeout rejects a negative timeout. Zero is valid.
func CheckIdleTimeout(d time.Duration) error {
	return checkTimeout(d)
}

// CheckMaxHeaderBytes rejects a size below 1.
func CheckMaxHeaderBytes(n int) error {
	if n < 1 {
		return errNotPositive
	}
	return nil
}

func checkTimeout(d time.Duration) error {
	if d < 0 {
		return errNegative
	}
	return nil
}
