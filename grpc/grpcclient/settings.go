// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package grpcclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Settings configures a Client. Every field is a plain value, and a field's
// zero value means zero, never "use the default": start from DefaultSettings
// and change the fields you source. Addr has no default, so it is always
// among them.
type Settings struct {
	// Addr is the address the Client dials through the Setup's Host, as
	// host:port. The Host resolves the host when it dials. Addr is also the
	// RPCs' :authority, and its host the name the server's certificate is
	// verified for, unless TLS sets a ServerName, or opts has a
	// grpc.WithAuthority, which then replaces both. It is required.
	//
	// Default: none. Valid: host:port, with a host and a port from 1 to
	// 65535. Check: CheckAddr.
	Addr string

	// TLS is the config the Client dials with. nil means plain text. A
	// client certificate in Certificates makes it mTLS. With RootCAs nil,
	// the server is verified against the system's roots.
	//
	// Default: nil, plain text. Valid: nil, or a config with a MaxVersion
	// of TLS 1.2 or later, or none. Check: CheckTLS.
	TLS *tls.Config

	// Timeout bounds each unary RPC, unless the caller's context has an
	// earlier deadline. It doesn't apply to streams.
	//
	// Default: 10s. Valid: positive. Check: CheckTimeout.
	Timeout time.Duration
}

// DefaultSettings returns the Client's defaults. Addr has none, so its Addr
// is "", which CheckAddr rejects: the end user always sources it.
func DefaultSettings() Settings {
	return Settings{
		Addr:    "",
		TLS:     nil,
		Timeout: 10 * time.Second,
	}
}

// CheckAddr reports why addr is not a valid Addr, or nil if it is. A valid
// Addr is host:port, with a host, and a port written as a number from 1 to
// 65535. It rejects an empty host, which grpcserver.CheckAddr accepts: a
// client dials a host, it doesn't listen on every name. It rejects a scheme,
// such as dns:///, since the Client never resolves names itself, and a
// named port, such as :grpc. It rejects ?, # and %, which grpc-go's target
// would read as a query, a fragment or an escape, so that the Host would be
// asked to dial something other than addr.
func CheckAddr(addr string) error {
	if strings.Contains(addr, "/") {
		return fmt.Errorf("%q is not host:port: it names a scheme or a path, and only the Host resolves names", addr)
	}
	if strings.ContainsAny(addr, "?#%") {
		return fmt.Errorf("%q is not host:port: it has a ?, # or %%", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if host == "" {
		return fmt.Errorf("%q has no host", addr)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		return fmt.Errorf("%q has no port from 1 to 65535", addr)
	}
	return nil
}

// CheckTLS reports why c can't dial a server, or nil if it can. nil is
// valid, and means plain text. A config must allow TLS 1.2 or later, which
// HTTP/2 requires: a MaxVersion of TLS 1.2 or later, or none. A config needs
// no certificate, since a client certificate is only for mTLS.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return nil
	}
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS12 {
		return fmt.Errorf("has a MaxVersion of %s, below the TLS 1.2 that HTTP/2 requires", tls.VersionName(c.MaxVersion))
	}
	return nil
}

// CheckTimeout rejects a duration that is not positive. A zero would mean
// no deadline, and an RPC with none can hang for as long as the server
// does.
func CheckTimeout(d time.Duration) error {
	if d <= 0 {
		return errors.New("must be positive")
	}
	return nil
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
	check("Settings.Addr", CheckAddr(settings.Addr))
	check("Settings.TLS", CheckTLS(settings.TLS))
	check("Settings.Timeout", CheckTimeout(settings.Timeout))
	return errors.Join(errs...)
}
