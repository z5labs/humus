// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

package httpclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// Settings configures a client of one dependency. Every field is a plain
// value, and a field's zero value means zero, never "use the default": start
// from DefaultSettings and change the fields you source. BaseURL has no
// default, so it is always among them.
type Settings struct {
	// BaseURL is the dependency. Its scheme, host and port are the origin
	// the client sends requests to, and the only one. Its path, if any, is
	// for service code to join request paths onto; the client doesn't use it.
	//
	// Default: none. Valid: an absolute http or https URL with a host, and
	// no user info, query or fragment. Check: CheckBaseURL.
	BaseURL *url.URL

	// TLS is the config the client dials the dependency with: the roots it
	// trusts in RootCAs, and for mTLS, a client certificate in
	// Certificates. nil means plain text. An https BaseURL requires a
	// config, and an http one requires nil. A config with no RootCAs trusts
	// the system's roots.
	//
	// Default: nil, which means plain text. Valid: nil, or a config that
	// verifies the server's certificate and allows TLS 1.2 or later.
	// Check: CheckTLS.
	TLS *tls.Config

	// Timeout bounds a whole call, reading the response's body included. A
	// deadline the caller's context has already, if it is earlier, wins. It
	// is the http.Client's Timeout.
	//
	// Default: 10s. Valid: positive. Check: CheckTimeout.
	Timeout time.Duration
}

// DefaultSettings returns the client's defaults. BaseURL has none, so its
// BaseURL is nil, which CheckBaseURL rejects: the end user always sources it.
func DefaultSettings() Settings {
	return Settings{
		BaseURL: nil,
		TLS:     nil,
		Timeout: 10 * time.Second,
	}
}

// The causes the checks report. None of them names a field: whoever calls a
// check names the field, or the variable that sourced it.
var (
	errNil         = errors.New("must not be nil")
	errNotPositive = errors.New("must be positive")
)

// CheckBaseURL reports why u can't be a client's dependency, or nil if it
// can. u must not be nil, and must be an absolute http or https URL with a
// host. It must have no user info, since credentials in a URL would sit in
// a setting and in every error that names the client's origin, and no query
// or fragment, since the client sends requests to many paths of one origin.
// No error quotes u, so a password in it is never repeated.
func CheckBaseURL(u *url.URL) error {
	switch {
	case u == nil:
		return errNil
	case u.Scheme == "":
		return errors.New("must be absolute, with the scheme http or https")
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("has the scheme %q, not http or https", u.Scheme)
	case u.User != nil:
		return errors.New("has user info: set credentials in a request's header instead")
	case u.Hostname() == "":
		return errors.New("has no host")
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("has a query")
	case u.Fragment != "":
		return errors.New("has a fragment")
	}
	return nil
}

// CheckTLS reports why c can't dial a dependency, or nil if it can. nil is
// valid, and means plain text. A config must verify the server's
// certificate, so InsecureSkipVerify is rejected: a test trusts its CA
// through RootCAs instead. And it must allow TLS 1.2 or later: a MaxVersion
// of TLS 1.2 or later, or none.
func CheckTLS(c *tls.Config) error {
	if c == nil {
		return nil
	}
	if c.InsecureSkipVerify {
		return errors.New("sets InsecureSkipVerify, so it doesn't verify the server's certificate")
	}
	if c.MaxVersion != 0 && c.MaxVersion < tls.VersionTLS12 {
		return fmt.Errorf("has a MaxVersion of %s, below TLS 1.2", tls.VersionName(c.MaxVersion))
	}
	return nil
}

// CheckTimeout rejects a duration that is not positive: a client with no
// bound on a call holds a goroutine and a connection for as long as a
// dependency hangs.
func CheckTimeout(d time.Duration) error {
	if d <= 0 {
		return errNotPositive
	}
	return nil
}
