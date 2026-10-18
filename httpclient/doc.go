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
// # Timeout and AttemptTimeout
//
// Settings.Timeout bounds a whole call: every attempt, the waits between
// them, and reading the response's body. It is the http.Client's Timeout,
// which net/http puts on the request's context as a deadline, unless the
// caller's context has an earlier one, which then wins. So the retries see
// the deadline, and don't start a wait that would end after it. It covers
// reading the response's body, so a client that streams large bodies
// raises it. [CheckTimeout] rejects a Timeout that is not positive: a client
// with no bound on a call holds a goroutine and a connection for as long as
// a dependency hangs.
//
// Settings.AttemptTimeout bounds one attempt, from dialling until the
// response's headers arrive. A layer just outside the http.Transport cancels
// the attempt's context after it, with a cause that wraps
// context.DeadlineExceeded, and the attempt fails with an error that wraps
// it too. The call's context is still live, so the error reaches Classify,
// and the default retries it, as any failed attempt. With the defaults, an
// attempt to a dependency that hangs fails after 5s, which leaves time for a
// second before Timeout's 10s. AttemptTimeout stops counting once the
// headers arrive, so a slow body isn't cut off: that is Timeout's to bound.
//
// # Which requests are retried
//
// A call is retried only when sending it twice is both safe and possible:
//
//   - Safe: its method is GET, HEAD, OPTIONS, TRACE, PUT or DELETE, which
//     RFC 9110 defines as idempotent, or it carries an Idempotency-Key or
//     X-Idempotency-Key header, which net/http treats the same way.
//   - Possible: it has no body, or its GetBody is set, which
//     http.NewRequest does for a *bytes.Buffer, a *bytes.Reader and a
//     *strings.Reader. The client rewinds the body with GetBody before each
//     attempt after the first.
//
// Any other request makes one attempt, whatever Classify says of its
// result. Whether a request is safe to send twice is about the request, not
// about what the dependency answered, so a POST without an idempotency key
// can't be charged twice. Its result still counts in the retry budget.
//
// net/http's own list of requests it replays on a stale connection leaves
// out PUT and DELETE. RFC 9110 defines both as idempotent, and a service
// that sends them relies on that already.
//
// Retries are bounded three ways. Settings.MaxAttempts bounds the attempts
// of each request a call sends, 3 by default, and 1 turns retries off. A
// redirect is a request of its own, with attempts of its own. Settings.RetryBudget
// bounds every call to the dependency together, as retry.Budget describes:
// each attempt classified Retry takes a token, each Success adds a tenth of
// one back, and retries stop while half the tokens or fewer are left. So
// while the dependency fails, every call makes one attempt, rather than
// three. And Timeout bounds the call. The wait before each retry is drawn
// from retry.Exponential's schedule, with Settings.BackoffBase and
// Settings.BackoffMax.
//
// # How an attempt's result counts
//
// Every attempt's result is classified twice: for retry, whether another
// attempt follows, and for the dependency's circuit breaker, whether the
// result counts against the dependency. [Settings].Classify returns both, in
// a [Classification]. Some of that is about the call, and the client
// decides it the same way for every dependency. The rest is about what the
// dependency's answers mean, which differs from one API to another, and
// Classify decides it.
//
// What the client decides: Classify isn't asked about an attempt the
// circuit breaker rejects, which ends the call, nor about an error once the
// call's context is done, because the caller gave up or the call's deadline
// passed. Another attempt can't help, so the call ends: the breaker
// ignores the caller giving up, and counts the deadline against the
// dependency. Nor is it asked about a body that fails once service code
// reads it (see The circuit breaker). And whatever Classify returns:
//
//   - a request that isn't safe to send twice, or can't be replayed, makes
//     one attempt;
//   - a RetryAfter longer than BackoffMax ends the call, with the response
//     as it came;
//   - a response whose body doesn't fit in 64 KiB isn't retried.
//
// What [DefaultSettings]' Classify decides, of every response and every
// error while the call's context is live:
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
//	| any other error, such as refused, reset or          | Retry                    | Failure |
//	| AttemptTimeout                                      |                          |         |
//
// A 4xx is the dependency working: it answered, and the request was wrong,
// so another attempt won't fix it, and the dependency counts as healthy. 408
// and 429 are retried, and don't count against it: a 408 says the server
// gave up waiting for the request, which RFC 9110 says a client may repeat,
// and a 429 is the server's own flow control, which says when to come
// back. A 500 counts against the dependency but isn't retried: it is usually
// the server's bug, which a second attempt meets again. 502, 503 and 504 say
// the dependency, or what is in front of it, is unavailable, which is what
// retries are for. A certificate the client doesn't trust is met again by
// every attempt. The default reads only the status and the headers, and
// never a body.
//
// An end user whose API means something else by an answer writes a
// Classify of their own, which usually handles what differs and hands the
// rest to the default. The ledger below answers 409 while another write to
// the account is in flight, which is over by the next attempt:
//
//	d := httpclient.DefaultSettings()
//	classify := d.Classify
//
//	settings.Classify = func(resp *http.Response, err error) httpclient.Classification {
//		if resp != nil && resp.StatusCode == http.StatusConflict {
//			return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
//		}
//		return classify(resp, err)
//	}
//
// The function is the end user's own, so one function can be set on every
// client whose dependencies share a convention, such as a company's own
// services.
//
// A Classify may read the body, for an API that answers 200 with an error in
// its body. It closes the body it read, and leaves in resp.Body what service
// code is to read, such as the bytes it read:
//
//	settings.Classify = func(resp *http.Response, err error) httpclient.Classification {
//		if err != nil {
//			return classify(resp, err)
//		}
//		b, err := io.ReadAll(resp.Body)
//		_ = resp.Body.Close()
//		resp.Body = io.NopCloser(bytes.NewReader(b))
//		if err != nil {
//			return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Failure}
//		}
//		if bytes.Contains(b, []byte(`"error"`)) {
//			return httpclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
//		}
//		return classify(resp, nil)
//	}
//
// Its read counts against Timeout, but not against AttemptTimeout, which
// ends with the headers.
//
// Classify is called on the goroutine that made the call, once for each
// attempt, so it must be safe for concurrent use. A Classification with a
// zero Outcome, or with retry.Retry and a negative RetryAfter, panics, as
// bedrock's classifiers do. The client recovers nothing: a panic in
// Classify passes to the goroutine that made the call, once the attempt's
// response is closed, so its connection isn't held.
//
// # The circuit breaker
//
// Each client has one circuit breaker, bedrock's circuit.Breaker, which
// guards its dependency: while the dependency keeps failing, the client
// fails calls at once, rather than hold every caller for its timeouts and
// retries, and a probe now and then finds out when the dependency is back.
// The breaker sits inside the retries, so each attempt is a call it admits
// or rejects, and inside otelhttp, so each attempt it rejects still has a
// span.
//
// What counts is each attempt's result, as the Breaker outcome of its
// Classification: Success, Failure, or Ignored for a result that says
// nothing about the dependency. The client decides some of it itself:
//
//   - an error once the caller cancelled the call's context is Ignored;
//   - an error once the call's deadline passed, Timeout or one the caller
//     set, is a Failure: a dependency that hangs ends every call this way,
//     and a breaker that ignored deadlines would never open;
//   - a response's result is counted by its body, with its
//     Classification's Breaker, once service code reads the body to the
//     end or closes it. A read that fails is a Failure instead, whatever
//     Classify said of the status, since the dependency sent its headers
//     and then failed, or Ignored when the caller cancelled the call's
//     context. A body the client buffers before a retry is counted then.
//
// Settings.Breaker configures it, from configuration, and
// Settings.BreakerPolicy decides when it opens and closes, in code. The
// default opens after 5 failures in a row, or once half of the last 20 or
// more results failed. It then rejects every attempt for OpenFor, 10s, and
// then lets one probe through at a time: a failed probe reopens it, and 3
// successful ones close it. A policy whose Trip always returns false turns
// the breaker off:
//
//	settings.BreakerPolicy.Trip = func(circuit.Counts) bool { return false }
//
// The policy runs with the breaker locked, on the goroutine of the call
// whose result it decides on. The client recovers nothing: a panic in it
// passes to that goroutine, the one that made the call or read its body,
// and circuit leaves the breaker consistent.
//
// An attempt the breaker rejects sends nothing, and isn't retried, which
// would defeat the breaker. Classify isn't asked about it. The call
// returns at once, with an error that wraps a *circuit.RejectedError, which
// says how much longer the breaker stays open, through http.Client's
// *url.Error:
//
//	_, err := payments.Get("https://payments:8443/charges/1")
//	if errors.Is(err, circuit.ErrRejected) {
//		// Nothing was sent.
//	}
//
// A rejected attempt still has otelhttp's client span, named and
// attributed as every attempt's span is, which ends at once with status
// Error and error.type *circuit.RejectedError. It is in
// http.client.request.duration too, with an error.type that otelhttp
// writes with the package path,
// github.com/z5labs/bedrock/circuit.RejectedError.
//
// A body that service code never closes leaves its result uncounted. That
// costs nothing while the breaker is closed. While it is half-open, the
// attempt was a probe, whose slot the body holds until it is read or
// closed, or for at most Breaker.ProbeTimeout, after which another probe
// may go. net/http asks for every body to be closed anyway.
//
// # Retry-After
//
// The default Classify reads the Retry-After header of the 429s and 503s it
// retries, and returns it as the Classification's RetryAfter. Both of RFC
// 9110's forms are read: a number of seconds, which is used as it is, and an
// HTTP-date, which is measured from now and clamped at 0. A malformed header
// is ignored. The wait before the next attempt is the longer of RetryAfter
// and the backoff. An end user's Classify returns whatever its API says,
// such as a header of its own.
//
// A RetryAfter longer than BackoffMax, from any Classify, isn't waited for:
// the call ends, and service code gets the response as it came. A server
// that asks for a minute is not one a call in flight should wait on. A wait
// that would end after the call's deadline isn't started either: the call
// returns the last response at once.
//
// # The response body
//
// Before another attempt follows, the client reads the body of the response
// it replaces into memory, up to 64 KiB, and closes it, before the wait.
// Closing it ends the attempt's span, counts its result in the breaker,
// and frees its connection during the backoff. If the wait is cut short,
// such as by the caller's context, the buffered response is what the call
// returns. A body larger than 64 KiB
// isn't retried: the call returns the response with its body intact. Error
// bodies are small, so 64 KiB keeps every one the client retries, and
// bounds what each call in flight holds.
//
// A Classify that reads the body leaves what service code reads in
// resp.Body, and these rules apply to the body it leaves.
//
// # Redirects
//
// A redirect within the origin is followed, as http.Client follows one, up
// to 10 redirects, as net/http's default policy does. A redirect to another
// origin isn't: the client returns the 3xx response to service code, which
// may read its Location header.
//
// # Telemetry
//
// The client traces and measures every attempt with the Setup's telemetry,
// through otelhttp's Transport, which sits inside the origin check: a
// request to another origin is refused before it is an attempt.
//
// Each request the transport sends, every retry and redirect included, is
// one span of kind CLIENT, a child of the span in the request's context,
// such as a handler's request span or a job's run span. A slow or failing dependency
// shows up in the caller's trace. The span is named after the method, such
// as GET: the HTTP semantic conventions name a client span after its method
// and URL template, and a client has no template, only whatever URL service
// code built, which would make a span name per resource. A request with an
// empty method is a GET, as net/http sends it, and its span is named GET.
// The span carries
// otelhttp's attributes, among them url.full, with any user info removed,
// server.address, server.port, http.response.status_code and
// network.protocol.version, which is the version the attempt was answered
// with, 2.0 for HTTP/2. Its status is Error for a 4xx or 5xx response, and
// for an attempt that fails with no response, such as a refused connection.
//
// An attempt that is not a call's first carries http.request.resend_count,
// which otelhttp doesn't set. A retry and a redirect each count, as the
// semantic conventions count them, so a retry's span has 1, and so does a
// call's first redirect, unless an attempt before it was retried.
//
// The Setup's propagator puts the attempt's trace context and the request
// context's baggage in each attempt's headers, so a dependency served with
// httpserver joins the caller's trace, with the attempt's span as its
// server span's parent, and its handler sees the caller's baggage.
//
// otelhttp records http.client.request.duration and
// http.client.request.body.size for each attempt, in its own scope, with the
// status code, or error.type for an attempt that failed with no response,
// one the breaker rejected included.
//
// The client records four instruments of humus's own, for its breaker and
// its retries, with the Setup's MeterProvider, in the scope
// github.com/z5labs/humus/httpclient. Every data point carries the
// dependency's server.address and server.port, of BaseURL's origin, and
// nothing per call, such as a method or a route. Every other attribute's
// value is the String() of bedrock's own type:
//
//   - humus.circuit.state, an asynchronous UpDownCounter in {breaker}, with
//     humus.circuit.state: closed, open or half-open. At each collection it
//     reads the breaker's state and observes 1 for it and 0 for the other
//     two. It reads half-open as soon as OpenFor has passed, with no call
//     made. Two clients of one origin add up, such as closed 1 and open 1.
//     It is still read by the App's last collection, after the OnStop
//     functions, so the final export has the state the App stopped in.
//   - humus.circuit.transitions, a Counter in {transition}, with
//     humus.circuit.state, the state the breaker went to, and
//     humus.circuit.previous_state, the state it left: one for each change
//     of state, beside its log record.
//   - humus.circuit.calls, a Counter in {call}, with humus.circuit.outcome:
//     success, failure or ignored, as each attempt's result counted in the
//     breaker, when it is counted, such as once a response's body is read
//     to the end or closed; or rejected, for an attempt the breaker didn't
//     admit. An attempt whose ticket is released uncounted, such as one that
//     panicked, counts nothing.
//   - humus.retry.decisions, a Counter in {decision}, with
//     humus.retry.decision: again, succeeded, stopped, exhausted, throttled
//     or expired, what retry decided after each attempt, a rejected one's
//     stopped included. again is what retry decided, not what the client
//     then did: no attempt follows a request that isn't safe to repeat, a
//     response whose body doesn't fit the 64 KiB buffer, or a call whose
//     context ends during the wait.
//
// A View in the end user's telemetry Builder drops or renames them. humus
// has no setting to turn them off.
//
// The client logs through the Setup's LoggerProvider, with the logger
// github.com/z5labs/humus/httpclient, one record for each change of the
// breaker's state, in the context of the call that made it: "circuit
// breaker open" at WARN, and "circuit breaker half-open" and "circuit
// breaker closed" at INFO. Each has the dependency's server.address and
// server.port, the state the breaker left as humus.circuit.previous_state,
// and the counts of that state: humus.circuit.successes,
// humus.circuit.failures, humus.circuit.consecutive_successes,
// humus.circuit.consecutive_failures and humus.circuit.timed_out. The client
// logs nothing else: an error is returned to service code, not logged.
//
// The tracer, the meter and the propagator all come from the Setup's
// telemetry, so nothing reaches OpenTelemetry's globals: a process that
// sets them sees none of the client's spans or metrics.
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
