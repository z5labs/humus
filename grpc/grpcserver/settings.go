// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcserver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
)

// Settings configures a Service. Every field is a plain value, and a
// field's zero value means zero, never "use the default". Neither field
// has a default, so the end user sources both.
type Settings struct {
	// Addr is the address the Service listens on through the Setup's Host,
	// as host:port. It is required: humus imposes no port. An empty host
	// listens on every name, and port 0 picks a free port, which
	// Service.Addr reports.
	//
	// Default: none. Valid: host:port, with a port from 0 to 65535.
	// Check: CheckAddr.
	Addr string

	// TLS is the config the Service serves gRPC with. It is required: the
	// Service serves nothing in plain text. A client certificate is
	// required by setting ClientAuth, as with any tls.Config.
	//
	// Default: none. Valid: a config with Certificates, GetCertificate or
	// GetConfigForClient set, and a MaxVersion of TLS 1.2 or later, or
	// none. Check: CheckTLS.
	TLS *tls.Config
}

// CheckAddr reports why addr is not a valid Addr, or nil if it is. It
// accepts what Host.Listen accepts, an empty host and port 0 included, and
// rejects an address that is not host:port, such as "" or an address with
// no port, and one whose port is not a number from 0 to 65535.
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

// CheckTLS reports why c can't serve gRPC, or nil if it can. c must not be
// nil, must have Certificates, GetCertificate or GetConfigForClient set,
// and must allow TLS 1.2 or later, which HTTP/2 requires. Its CipherSuites
// are not checked.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return errors.New("must not be nil")
	}
	if len(c.Certificates) == 0 && c.GetCertificate == nil && c.GetConfigForClient == nil {
		return errors.New("has no certificate: none of Certificates, GetCertificate and GetConfigForClient is set")
	}
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS12 {
		return fmt.Errorf("has a MaxVersion of %s, below the TLS 1.2 that HTTP/2 requires", tls.VersionName(c.MaxVersion))
	}
	return nil
}
