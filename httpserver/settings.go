// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpserver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"
)

// Settings configures a Service. Every field is a plain value, and a field's
// zero value means zero, never "use the default": start from DefaultSettings
// and change the fields you source. Addr and TLS have no default, so they are
// always among them.
type Settings struct {
	// Addr is the TCP address the Service listens on through the Host, as
	// host:port. It is required: humus imposes no port. An empty host listens
	// on every address. Port 0 picks a free port, which Service.Addr reports.
	//
	// Default: none. Valid: host:port, with a port from 0 to 65535.
	// Check: CheckAddr.
	Addr string

	// TLS is the config the Service serves HTTPS with. It is required: the
	// Service serves nothing in plain text.
	//
	// Default: none. Valid: a config with Certificates, GetCertificate or
	// GetConfigForClient set, that HTTP/2 can use. Check: CheckTLS.
	TLS *tls.Config

	// HTTP2Only turns HTTP/1.1 off, so the Service serves HTTP/2 alone. When
	// it is false, the Service serves HTTP/2 to every client that offers it
	// and HTTP/1.1 to the rest. When it is true, a client that offers only
	// HTTP/1.1 completes the TLS handshake and is then disconnected without
	// a response.
	//
	// Default: false. Valid: either. Check: none.
	HTTP2Only bool

	// ReadHeaderTimeout bounds reading a request's headers. Zero means
	// ReadTimeout applies, as in net/http.
	//
	// Default: 2s. Valid: not negative. Check: CheckReadHeaderTimeout.
	ReadHeaderTimeout time.Duration

	// ReadTimeout bounds reading a whole request, its body included. Zero
	// means no timeout, as in net/http.
	//
	// Default: 5s. Valid: not negative. Check: CheckReadTimeout.
	ReadTimeout time.Duration

	// WriteTimeout bounds writing a response, from the end of the request's
	// headers. Zero means no timeout, as in net/http. A handler that streams
	// extends its own deadline with http.ResponseController.
	//
	// Default: 10s. Valid: not negative. Check: CheckWriteTimeout.
	WriteTimeout time.Duration

	// IdleTimeout bounds how long a keep-alive connection waits for its next
	// request. Zero means ReadTimeout applies, as in net/http.
	//
	// Default: 120s. Valid: not negative. Check: CheckIdleTimeout.
	IdleTimeout time.Duration

	// MaxHeaderBytes bounds the size of a request's headers.
	//
	// Default: 1 MiB (1 << 20). Valid: positive. Check: CheckMaxHeaderBytes.
	MaxHeaderBytes int
}

// DefaultSettings returns the Service's defaults. Addr and TLS have none,
// so its Addr is "" and its TLS is nil, which CheckAddr and CheckTLS reject:
// the end user always sources both.
func DefaultSettings() Settings {
	return Settings{
		Addr:              "",
		TLS:               nil,
		HTTP2Only:         false,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errNil         = errors.New("must not be nil")
	errNegative    = errors.New("must not be negative")
	errNotPositive = errors.New("must be positive")
)

// CheckAddr rejects an address that Host.Listen can't listen on: one that is
// not host:port, such as "" or an address with no port, or one whose port is
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

// http2CipherSuites are the cipher suites HTTP/2 requires a TLS 1.2
// connection to support, one of which must be listed when a config sets
// CipherSuites and allows TLS 1.2.
var http2CipherSuites = []uint16{
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
}

// CheckTLS reports why c can't serve HTTPS, or nil if it can. c must not be
// nil, must have Certificates, GetCertificate or GetConfigForClient set, and
// must allow HTTP/2: a MaxVersion of TLS 1.2 or later, or none, and when it
// sets CipherSuites with a MinVersion below TLS 1.3, one of the two suites
// HTTP/2 requires.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return errNil
	}
	if len(c.Certificates) == 0 && c.GetCertificate == nil && c.GetConfigForClient == nil {
		return errors.New("has no certificate: none of Certificates, GetCertificate and GetConfigForClient is set")
	}
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS12 {
		return fmt.Errorf("has a MaxVersion of %s, below the TLS 1.2 that HTTP/2 requires", tls.VersionName(c.MaxVersion))
	}
	if len(c.CipherSuites) > 0 && c.MinVersion < tls.VersionTLS13 &&
		!slices.ContainsFunc(c.CipherSuites, func(s uint16) bool { return slices.Contains(http2CipherSuites, s) }) {
		return errors.New("lists CipherSuites with neither TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 nor " +
			"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, one of which HTTP/2 requires below TLS 1.3")
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

// CheckMaxHeaderBytes rejects a size that is not positive. Zero is
// rejected rather than meaning 1 MiB, as it would in net/http.
func CheckMaxHeaderBytes(n int) error {
	if n <= 0 {
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
