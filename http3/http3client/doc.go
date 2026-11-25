// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package http3client configures calls to HTTP dependencies over HTTP/3 for
// a humus service.
//
// It is in the nested module github.com/z5labs/humus/http3, which keeps
// quic-go out of the core module and is always built against the core
// module in the same commit. Its API is its own: no exported name of the
// package names a type of httpclient's or httpserver's, so a service that
// calls a dependency over HTTP/3 needs neither. Each of its [Settings]
// fields means what httpclient's field of the same name means, has the same
// default, and has a check of the same name with the same rules, except
// where HTTP/3 differs: it has no plain-text form, and QUIC requires TLS
// 1.3.
//
// # One client per dependency
//
// A client is for one dependency, such as another service's API, and
// [Settings].BaseURL names it. A dependency is an origin: the scheme, host
// and port of BaseURL, with the scheme's default port when the URL has
// none, so https://payments and https://payments:443 are one origin.
//
// # TLS
//
// HTTP/3 runs over QUIC, which carries nothing in plain text, so BaseURL's
// scheme is https, and [CheckBaseURL] rejects http. Settings.TLS is the
// config the client dials with. nil means the system's roots, as does a
// config with no RootCAs. A config must verify the server's certificate,
// and must allow TLS 1.3, which [CheckTLS] checks. A config that allows TLS
// 1.2 as well, such as httpclient's, passes, so one config can serve a
// client of each transport.
//
// # How an attempt's result counts
//
// Every attempt's result is classified twice: for retry, whether another
// attempt follows, and for the dependency's circuit breaker, whether the
// result counts against the dependency. [Settings].Classify returns both, in
// a [Classification]. What the dependency's answers mean differs from one
// API to another, and Classify decides it.
//
// What [DefaultSettings]' Classify decides, of every response and every
// error:
//
//	| An attempt's result                                 | Retry                    | Breaker |
//	|-----------------------------------------------------|--------------------------|---------|
//	| a status below 500, other than 408 and 429          | Success                  | Success |
//	| 408 Request Timeout                                 | Retry                    | Success |
//	| 429 Too Many Requests                               | Retry, after Retry-After | Success |
//	| 500 Internal Server Error                           | Stop                     | Failure |
//	| 502 Bad Gateway, 504 Gateway Timeout                | Retry                    | Failure |
//	| 503 Service Unavailable                             | Retry, after Retry-After | Failure |
//	| any other 5xx, such as 501 Not Implemented          | Stop                     | Success |
//	| a server certificate the client doesn't trust       | Stop                     | Failure |
//	| any other error, such as a handshake that times     | Retry                    | Failure |
//	| out, or AttemptTimeout                              |                          |         |
//
// It is httpclient's default classification. A 4xx is the dependency
// working: it answered, and the request was wrong, so another attempt won't
// fix it, and the dependency counts as healthy. 408 and 429 are retried, and
// don't count against it: a 408 says the server gave up waiting for the
// request, which RFC 9110 says a client may repeat, and a 429 is the
// server's own flow control, which says when to come back. A 500 counts
// against the dependency but isn't retried: it is usually the server's bug,
// which a second attempt meets again. 502, 503 and 504 say the dependency,
// or what is in front of it, is unavailable, which is what retries are for.
// A certificate the client doesn't trust is met again by every attempt. The
// default reads only the status and the headers, and never a body.
//
// An end user whose API means something else by an answer writes a
// Classify of their own, which usually handles what differs and hands the
// rest to the default. The ledger below answers 409 while another write to
// the account is in flight, which is over by the next attempt:
//
//	d := http3client.DefaultSettings()
//	classify := d.Classify
//
//	settings.Classify = func(resp *http.Response, err error) http3client.Classification {
//		if resp != nil && resp.StatusCode == http.StatusConflict {
//			return http3client.Classification{Retry: retry.Retry, Breaker: circuit.Success}
//		}
//		return classify(resp, err)
//	}
//
// The function is the end user's own, so one function can be set on every
// client whose dependencies share a convention, such as a company's own
// services.
//
// # Retry-After
//
// The default Classify reads the Retry-After header of the 429s and 503s it
// retries, and returns it as the Classification's RetryAfter. Both of RFC
// 9110's forms are read: a number of seconds, which is used as it is, and an
// HTTP-date, which is measured from now and clamped at 0. A malformed header
// is ignored. A RetryAfter longer than BackoffMax, from any Classify, isn't
// waited for: the call ends, with the response as it came.
//
// # Settings
//
// [DefaultSettings] returns the package's defaults, and each field has a
// check, such as [CheckBaseURL], that reports what is wrong with a value
// without naming the field. The package reads no environment variable and
// defines no variable name: the end user's Init sources each field, with
// whatever variable names the service uses, and passes the field's check to
// Validate, so an operator's bad value fails startup with exit 78, naming the
// variable. BaseURL has no default, so it is sourced with no .Default in its
// chain, and a variable that isn't set fails as not found.
//
// [CheckBaseURL] rejects user info: a password in the URL would sit in a
// setting, and in every error that names the client's origin. Credentials go
// in a header that service code sets. No check's error quotes a URL.
package http3client
