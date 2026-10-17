// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package httpservertest tests an HTTP service on the defaults that
// httpserver.Main gives it in production, with a client that speaks HTTP/2
// to a humustest Host.
//
// It sits beside httpserver as net/http/httptest sits beside net/http. It
// has two functions:
//
//	p, status := httpservertest.Start(t, Init,
//		humustest.WithApp(humus.WithHost(h.Host())),
//		humustest.WithEnv(env),
//	)
//	<-h.Listening("localhost:8443")
//
//	resp, err := httpservertest.NewClient(h, ca.Pool()).Get("https://localhost:8443/hello/humus")
//
// # Start and the defaults
//
// [Start] is humustest.Start with httpserver.Defaults applied before the
// test's options, and does nothing else. A test of a main that calls
// httpserver.Main runs its App with the defaults production runs with, and
// can't forget them: a drain of humus.DrainDelay(5 * time.Second) and a 10s
// grace period. So a handler blocked on r.Context() sees its context
// cancelled 15s after SIGTERM, once the drain and the grace period are both
// up.
//
// The test passes its Host, its telemetry and its environment as it would to
// humustest.Start. Options given with humustest.WithApp come after the
// defaults, so they win: with
// humustest.WithApp(humus.WithShutdownGracePeriod(2*time.Second)), the same
// handler's context is cancelled 7s after SIGTERM. An operator's
// HUMUS_SHUTDOWN_GRACE_PERIOD and HUMUS_SHUTDOWN_DRAIN_DELAY, given with
// humustest.WithEnv, override them as they do in production.
//
// # The certificate
//
// Start makes no CA and no certificate. How the end user's Init sources its
// certificate, from which variable and in which encoding, is the end user's
// decision, so Start couldn't hand it one. The test makes a humustest.CA,
// issues a certificate, and gives it to the App the way production does: it
// writes the certificate into the Host's filesystem with humustest.WithFiles,
// and sets the variables that name the file, and any password, with
// humustest.WithEnv. A PKCS#12 bundle, for example:
//
//	ca := humustest.NewCA(t)
//	bundle, password := ca.Issue(t, "localhost").PKCS12(t)
//	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{"tls.p12": bundle}))
//
//	p, status := httpservertest.Start(t, Init,
//		humustest.WithApp(humus.WithHost(h.Host())),
//		humustest.WithEnv(map[string]string{
//			"ADDR":                ":8443",
//			"TLS_PKCS12_FILE":     "tls.p12",
//			"TLS_PKCS12_PASSWORD": password,
//		}),
//	)
//
// A test that builds the httpserver.Settings itself sets their TLS config
// from the certificate's TLS method instead. Either way, the test passes the
// CA's Pool to NewClient.
//
// # NewClient speaks only HTTP/2
//
// [NewClient] returns h.HTTPClient with a TLS config that trusts only the
// given roots, and a Transport whose Protocols is HTTP/2 alone. Every request
// it makes is HTTP/2, so a Service that stopped offering h2 fails the test
// rather than falling back to HTTP/1.1 unnoticed. Like h.HTTPClient, it keeps
// keep-alives off, so no connection outlives a request.
//
// The client is in the package because HTTP/2 over a test Host is easy to get
// wrong. A Go http.Transport with its own TLS config or dialer negotiates only
// HTTP/1.1 unless ForceAttemptHTTP2 or Protocols says otherwise, and
// h.HTTPClient has its own dialer. Such a client gets HTTP/1.1 from a default
// Service, and against a Service with httpserver.Settings.HTTP2Only, its
// request fails with the connection closed and no hint why.
//
// A test that needs HTTP/1.1, to check the fallback or HTTP2Only, configures
// h.HTTPClient itself: it sets the Transport's TLSClientConfig to trust the
// CA, with NextProtos of "http/1.1", and its Protocols to HTTP/1.1 alone.
//
// # NewClient without Start
//
// NewClient works without Start. It needs only the Host the App runs on and
// the roots that its certificate chains to, so it works next to an App
// started with humustest.Start, or with another runtime package's Start. A
// test of a process that serves HTTP next to another protocol uses one
// package's Start and each package's client.
//
// # Handlers that ignore their context
//
// A handler that ignores r.Context() outlives the App once the grace period
// has run out and its connection is closed. In a test that runs the App
// inside testing/synctest, synctest reports its goroutine as a leak, still
// blocked when the test ends: the handler should return once r.Context() is
// done.
package httpservertest
