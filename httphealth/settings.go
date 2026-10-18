// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httphealth

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
)

// Settings configures a Service. Neither of its fields has a default, so
// the end user always sources both.
type Settings struct {
	// Addr is the TCP address the Service listens on through the Host, as
	// host:port. It is required: humus imposes no port. An empty host
	// listens on every name, and port 0 picks a free port, which
	// Service.Addr reports.
	//
	// Default: none. Valid: host:port, with a numeric port from 0 to 65535.
	// Check: CheckAddr.
	Addr string

	// TLS is the config the Service serves HTTPS with. It is required: the
	// Service serves nothing in plain text. The Service serves a clone of
	// it, offering only HTTP/1.1, so the end user's config is never changed.
	// The kubelet doesn't verify the certificate of a probe with
	// scheme: HTTPS, and presents none of its own.
	//
	// Default: none. Valid: a config with a certificate, that doesn't
	// require a client certificate.
	// Check: CheckTLS.
	TLS *tls.Config
}

// CheckAddr rejects an address that Host.Listen can't listen on, without
// naming the field: one that is not host:port, such as "" or an address
// with no port, or one whose port is not a number from 0 to 65535, a
// service name such as "https" included. An empty host and port 0 are
// valid. It checks the address's form, not that the host resolves or that
// the port is free: Host.Listen reports those when the Service listens.
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

// CheckTLS rejects a nil config, one with no certificate, where none of
// Certificates, GetCertificate and GetConfigForClient is set, and one whose
// ClientAuth requires a client certificate, which the kubelet never
// presents, without naming the field. A ClientAuth of
// VerifyClientCertIfGiven is valid: a probe that presents no certificate
// is still served. When GetConfigForClient is set, the config it returns
// governs each handshake, and CheckTLS can't check that one: it must not
// require a client certificate either.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return errors.New("must not be nil")
	}
	if len(c.Certificates) == 0 && c.GetCertificate == nil && c.GetConfigForClient == nil {
		return errors.New("has no certificate: none of Certificates, GetCertificate and GetConfigForClient is set")
	}
	switch c.ClientAuth {
	case tls.RequireAnyClientCert, tls.RequireAndVerifyClientCert:
		return fmt.Errorf("has a ClientAuth of %s, but the kubelet presents no client certificate, so every probe would fail", c.ClientAuth)
	}
	return nil
}
