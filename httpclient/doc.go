// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package httpclient calls HTTP dependencies for a humus service.
//
// A client is for one dependency, such as another service's API. [NewClient]
// builds it in the end user's Init, from [Settings] the end user sources, and
// returns a plain *http.Client:
//
//	func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
//		paymentsSettings, err := readPaymentsSettings(ctx, s) // httpclient.Settings, sourced with config.Struct
//		if err != nil {
//			return nil, err
//		}
//		payments, err := httpclient.NewClient(s, paymentsSettings)
//		if err != nil {
//			return nil, err
//		}
//
//		api := newAPI(payments) // the end user's handler, which calls payments
//		svc, err := httpserver.NewService(s, serverSettings, api)
//		if err != nil {
//			return nil, err
//		}
//		return []humus.Service{svc}, nil
//	}
//
// Service code, generated clients and SDKs take the *http.Client as they take
// any other, and depend on net/http's type rather than on this package, so a
// unit test of that code passes any client. The client dials only through
// the Setup's Host, so a test runs the end user's whole App, its outbound
// calls included, on humustest's network.
//
// A client is not a humus.Service. NewClient opens no connection, and the
// client closes its idle connections when the App stops.
//
// # One client per dependency
//
// A dependency is an origin: the scheme, host and port of Settings.BaseURL,
// with the scheme's default port when the URL has none, so
// https://payments and https://payments:443 are one origin. The client sends
// requests to that origin and to no other. A request to another origin fails
// without being sent, and nothing is dialled, with an error such as:
//
//	httpclient: request to https://ledger:8443 on a client for https://payments:8443
//
// BaseURL's path, if it has one, is for service code to join request paths
// onto. The client doesn't use it.
//
// The end user's Init builds one client for each dependency, with its own
// Settings, and shares it between every Service that calls it. Two clients
// may share an origin: two APIs behind one gateway, which the end user wants
// configured separately, get two clients, each with its own BaseURL path and
// Settings.
//
// A client per dependency pools connections as one shared http.Transport
// would, since net/http keys its idle connections by scheme, host and port,
// and a shared transport never lends one dependency's connection to another.
// The two differ in two ways, and neither is a cost in practice:
//
//   - Idle limits are per client. Each client keeps up to 100 idle
//     connections for 90s, where one shared transport's MaxIdleConns would
//     cap every dependency together. A process with many dependencies can
//     hold more idle sockets, up to what each dependency's busiest burst
//     opened, until they've been idle for 90s. In return, a burst to one
//     dependency never evicts another's idle connections, which would cost
//     each a new TLS handshake.
//   - Two clients of one origin don't share connections. Each opens its own,
//     which over HTTP/2 is one more connection for each client.
//
// The cost that matters is a client built for each call. It pools nothing,
// so every call dials and handshakes, and NewClient registers one more
// function with Setup.OnStop each time. NewClient takes the *humus.Setup
// because it belongs in Init: build the client there, once, and share it.
//
// # TLS and plain text
//
// Settings.TLS is the config the client dials the dependency with: the roots
// it trusts in RootCAs, and for mTLS, a client certificate in Certificates.
// The client dials with a clone of it, so the end user's config is never
// changed. [CheckTLS] rejects a config that sets InsecureSkipVerify, or that
// allows nothing from TLS 1.2 on: a test trusts the CA it generates with
// humustest.NewCA through RootCAs, so nothing needs to skip verification.
//
// A config with no RootCAs trusts the system's roots, which crypto/x509
// loads from the real host. A service that calls only its own services sets
// RootCAs to its CA and reads nothing ambient.
//
// A nil TLS means plain text, with an http BaseURL. humus can't control what
// a service connects to, and a mesh's sidecar, a node-local agent or a cloud
// metadata endpoint is often plain HTTP. The URL names its scheme too, so
// NewClient requires the two to agree: an https BaseURL with a nil TLS, or an
// http one with a config, fails startup rather than quietly doing one of the
// two. Every humus server serves TLS, so a call to one is always https.
//
// # The transport
//
// The client's transport is an http.Transport of its own:
//
//   - It dials with the Setup's Host.Dial, so the client reaches the
//     dependency the way every humus App reaches the network, and a test's
//     in-memory Host serves it.
//   - Its Proxy is nil. The client never reads HTTP_PROXY, HTTPS_PROXY or
//     NO_PROXY, and has no proxy setting.
//   - For an https BaseURL it offers HTTP/2 and then HTTP/1.1 in ALPN, so
//     every server that offers h2 gets HTTP/2, a humus server set to
//     HTTP2Only among them. HTTP/2 puts every call on one connection, so
//     behind a Kubernetes ClusterIP Service, one client's calls reach one pod
//     until that connection closes. A humus server's Shutdown sends a GOAWAY,
//     so a rollout moves them. An http BaseURL is HTTP/1.1 alone, since humus
//     serves no unencrypted HTTP/2 (h2c).
//   - It keeps up to 100 idle connections to the origin, for 90s, as
//     http.DefaultTransport does for every host together. net/http's default
//     of 2 idle connections per host would make a busy client open and close
//     connections on every burst.
//
// The client's Jar is nil, so it keeps no cookies. The end user may set one.
//
// # Timeout
//
// Settings.Timeout bounds a whole call. It is the http.Client's Timeout,
// which net/http puts on the request's context as a deadline, unless the
// caller's context has an earlier one, which then wins. It covers reading
// the response's body, so a client that streams large bodies raises it.
// [CheckTimeout] rejects a Timeout that is not positive: a client with no
// bound on a call holds a goroutine and a connection for as long as a
// dependency hangs.
//
// # Redirects
//
// A redirect within the origin is followed, as http.Client follows one, up
// to 10 redirects, as net/http's default policy does. A redirect to another
// origin isn't: the client returns the 3xx response to service code, which
// may read its Location header.
//
// # Shutdown
//
// The client has nothing to drain. A call in flight ends with its own
// context: a handler's call ends when its request's context does.
// NewClient registers one function with Setup.OnStop, which closes the
// client's idle connections. The OnStop functions run after every Service
// has stopped, so nothing is still calling the dependency by then, and they
// run when Init fails after NewClient too.
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
// in a header that service code sets.
package httpclient
