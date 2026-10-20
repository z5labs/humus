// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package grpcclient calls gRPC services from a humus service.
//
// It is in the nested module github.com/z5labs/humus/grpc, next to
// grpcserver: grpcserver is the server half, and this package the client
// half.
//
// A [Client] is for one dependency, another service that the end user's
// service calls over gRPC. [NewClient] builds it in the end user's Init,
// from [Settings] the end user sources, and generated code takes it, since
// it is a grpc.ClientConnInterface:
//
//	func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
//		settings, err := readCartsSettings(ctx, s) // grpcclient.Settings, sourced with config.Struct
//		if err != nil {
//			return nil, err
//		}
//		carts, err := grpcclient.NewClient(s, settings)
//		if err != nil {
//			return nil, err
//		}
//
//		api := &api{carts: cartpb.NewCartServiceClient(carts)}
//		svc, err := grpcserver.NewService(s, serverSettings)
//		if err != nil {
//			return nil, err
//		}
//		orderpb.RegisterOrderServiceServer(svc, api)
//		return []humus.Service{svc}, nil
//	}
//
// The Client dials only through the Setup's Host, so a test runs the end
// user's whole App, its outbound calls included, on humustest's network,
// inside a testing/synctest bubble.
//
// # One Client per dependency
//
// A Client is one dependency at one Addr, over one connection, with one
// retry budget and one circuit breaker. The end user's Init builds one
// Client for each dependency, with its own Settings, and shares it between
// every Service that calls it: the Client is safe for concurrent use, and
// its RPCs share its connection, its budget and its breaker. A Client built
// for each call would dial and handshake for each call, start each with a
// full budget and a closed breaker that never sees a second failure, and
// register one more function with Setup.OnStop each time. NewClient takes the
// *humus.Setup because it belongs in Init: build the Client there, once,
// and share it.
//
// A Client is not a humus.Service. It has nothing to serve, and nothing to
// drain: an RPC in flight ends with its own context.
//
// # Dialling and names
//
// NewClient targets "passthrough:///" and Settings.Addr, and dials with the
// Setup's Host.Dial. The passthrough resolver hands Addr to the dialler as
// it is, so grpc-go looks no name up: the Host resolves the name when it
// dials. grpc-go's dns resolver, the default for a target with no scheme, is
// never used. It would look the name up with net.DefaultResolver, around the
// Host, reading the real host's resolver configuration and querying the real
// network, and inside a testing/synctest bubble it crashes the test binary.
//
// In main, humus.OSHost's Dial is a net.Dialer's, which looks the name up as
// net.Dial does, each time it dials. In a test, humustest's Host compares
// names as written, so a name that exists only on the test Host works,
// inside a bubble or out.
//
// The Client uses no HTTP CONNECT proxy, so it never reads HTTPS_PROXY or
// the other proxy variables from the real environment: it dials through the
// Host, and passes grpc.WithNoProxy.
//
// # One connection, picked first
//
// The passthrough resolver hands grpc-go one address, whatever its form, a
// name or an IP, so the Client has one subchannel, and grpc-go's default
// pick_first policy. Every RPC goes over one HTTP/2 connection, to whichever
// backend that dial reached. When the connection breaks, grpc-go dials again
// through the Host, with its own connection backoff, which starts at 1s and
// grows to 120s. grpc.WithConnectParams in opts changes it.
//
// OSHost's Dial tries a name's addresses in the order the lookup returns
// them, and keeps the first connection that succeeds. So:
//
//   - Behind a Kubernetes ClusterIP Service, the Service's name resolves to
//     its one virtual IP, and kube-proxy picks a pod for each connection, as
//     for the IP itself. The Client stays on that pod.
//   - Behind a headless Service, the name resolves to every ready pod, and
//     the Client connects to the first that accepts. Clients in different
//     pods may reach different backends, but each Client keeps one.
//   - When the connection ends, grpc-go dials the same name again, and the
//     Host looks it up again, so a Client finds pods that weren't there when
//     it first dialled. A pod's IP, given as Addr, never moves.
//
// This is what an end user who calls grpc.NewClient with nothing but an
// address already has: grpc-go's dns resolver and pick_first keep one
// connection too, even to a name with several addresses. The Client changes
// who looks the name up, not how many connections there are. Client-side
// load balancing is out of scope: it wouldn't help a ClusterIP Service,
// whose name has one address whichever resolver looks it up.
//
// Two things move a Client to another pod. grpcserver's Shutdown sends every
// client a GOAWAY, so a rollout of the dependency moves them. And a server
// whose end user sets keepalive.ServerParameters{MaxConnectionAge: ...} in
// grpcserver.NewService's options sends a GOAWAY when a connection reaches
// that age: the Client then dials again, and reaches whichever pod
// kube-proxy picks, or, behind a headless Service, whichever pod accepts
// first. gRFC A9 added MaxConnectionAge for this kind of rebalancing.
//
// Either one may end the streams open on the old connection. Shutdown ends
// each with Unavailable, and so does grpc-go when the server's
// MaxConnectionAgeGrace closes the connection under them. The circuit
// breaker ignores a stream that ends with Unavailable, however many end at
// once (see Streams, below), so the end user's code opens them again, and
// they land on the next pod.
//
// # :authority and the certificate
//
// grpc-go takes the authority from the target's endpoint, so every RPC's
// :authority is Addr, and TLS verifies the server's certificate for Addr's
// host. A Client of carts:9090 sends :authority carts:9090, and requires a
// certificate for carts.
//
// Two settings replace both, as they would on any grpc-go client: a
// ServerName in Settings.TLS, and a grpc.WithAuthority in opts. A Client of
// 10.0.0.7:9090 whose TLS has the ServerName carts sends :authority carts,
// and requires a certificate for carts.
//
// # TLS and plain text
//
// Settings.TLS is the config the Client dials with: the roots it trusts in
// RootCAs, and for mTLS, a client certificate in Certificates. The Client
// dials with a clone of it, so the end user's config is never changed.
// grpc-go adds h2 to its ALPN and enforces it. [CheckTLS] rejects a config
// that allows nothing from TLS 1.2 on, which HTTP/2 requires.
//
// A nil TLS means plain text. Addr is host:port and names no scheme, so a
// nil TLS is the one way to ask for it. humus can't control what a service
// connects to: a mesh's sidecar, a vendor's agent or a local emulator may
// well be plain text. Every humus server requires TLS, so a Client of one
// always sets it, and a Client that dials one in plain text fails its RPCs.
//
// A config with RootCAs nil verifies the server against the system's roots,
// which crypto/x509 reads from the real filesystem, and from SSL_CERT_FILE
// and SSL_CERT_DIR in the real environment. That is an ambient leak of
// every TLS client. A service that calls only its own services sets RootCAs
// to its CA and reads nothing ambient, and a test never relies on the
// system's roots: it sets RootCAs to the pool of a CA from humustest.NewCA.
//
// # Timeout
//
// Settings.Timeout bounds each unary RPC: every attempt, and the waits
// between them. Invoke applies it to the call's context before the first
// attempt, so a caller's earlier deadline wins, but a later one doesn't
// extend it: an end user whose calls need longer sets the field. A Timeout
// that has passed fails the RPC with codes.DeadlineExceeded. [CheckTimeout]
// rejects a Timeout that is not positive: an RPC with no deadline can hang
// for as long as the server does.
//
// Timeout doesn't apply to streams. A stream may rightly live as long as its
// caller wants, so it ends with the context its caller opened it with.
//
// # Retries
//
// Every unary RPC is retried with bedrock's retry package. NewClient builds
// one retry.Budget, the schedule retry.Exponential(BackoffBase, BackoffMax)
// and one retry.Retrier, shared by every method and every goroutine, so a
// Client is one dependency with one budget. Invoke ranges over the
// Retrier's attempts. Each attempt is one grpc-go RPC, which reads the
// attempt's trailer, and Settings.Classify decides how its result counts,
// once, in an attempt function of its own. The call returns the last
// attempt's error.
//
// Retries are bounded three ways. Settings.MaxAttempts bounds the attempts
// of each call, 3 by default, and 1 turns retries off. Settings.RetryBudget
// bounds every call to the dependency together, as gRPC's retry throttling
// does, and as retry.Budget describes: each attempt classified Retry takes
// a token, each Success adds a tenth of one back, and retries stop while
// half the tokens or fewer are left. So while the dependency fails, every
// call makes one attempt, rather than three, and first attempts are never
// limited. And Timeout bounds the call: a wait that would end after the
// call's deadline isn't started, and the call returns the last attempt's
// error at once. The wait before each retry is drawn from
// retry.Exponential's schedule, exponential backoff with full jitter.
//
// An interceptor or other option in NewClient's opts runs inside the
// retries, once per attempt: an interceptor chained with
// grpc.WithChainUnaryInterceptor sees every attempt, as grpc-go's stats
// handlers do.
//
// # How an attempt's result counts
//
// Every attempt's result is classified twice: for retry, whether another
// attempt follows, and for the dependency's circuit breaker, whether the
// result counts against the dependency. [Settings].Classify returns both,
// in a [Classification]. Some of that is about the call, and the Client
// decides it the same way for every dependency. The rest is about what the
// dependency's codes mean, which differs from one API to another, and
// Classify decides it.
//
// What the Client decides: Classify isn't asked about these, and the call
// ends, since another attempt can't help:
//
//   - a call the circuit breaker rejected. It reached nothing, and the
//     breaker would reject the next attempt too (see The circuit breaker,
//     below);
//   - an error once the call's context was cancelled: the caller gave up.
//     The breaker ignores it;
//   - an error once the call's deadline passed: Settings.Timeout, or a
//     deadline the caller set or inherited. The breaker counts it as a
//     failure;
//   - the end of a stream that opened, with Unavailable: its server or its
//     connection went away. The breaker ignores it (see Streams, below).
//
// And whatever Classify returns:
//
//   - a RetryAfter longer than BackoffMax ends the call, with the
//     attempt's error (see The pushback, below);
//   - a stream that opened is never retried (see Streams, below).
//
// What [DefaultSettings]' Classify decides, of every other result, by its
// code, which status.Code gives, so an error with no gRPC status, such as
// one an interceptor in opts returns, is Unknown:
//
//	| An attempt's code                                    | Retry                         | Breaker |
//	|------------------------------------------------------|-------------------------------|---------|
//	| OK                                                   | Success                       | Success |
//	| Unavailable                                          | Retry, after the pushback     | Failure |
//	|                                                      | if there is one               |         |
//	| ResourceExhausted with a pushback of 0 or more       | Retry, after the pushback     | Failure |
//	| ResourceExhausted without a pushback                 | Stop                          | Failure |
//	| DeadlineExceeded, Internal, Unknown, DataLoss        | Stop                          | Failure |
//	| Canceled                                             | Stop                          | Ignored |
//	| any other code, such as NotFound, InvalidArgument    | Stop                          | Success |
//	| or Aborted                                           |                               |         |
//
// A grpc-retry-pushback-ms that is negative or not a number makes any of
// these Stop but OK, which has nothing to retry: the server asked not to be
// tried again.
//
//   - Unavailable is gRPC's code for "try again". It is what grpc-go returns
//     when it can't reach the server. It is also what grpcserver ends
//     streams with at shutdown, but Classify isn't asked about a stream
//     that opened and ended with it: the Client decides that, as above.
//   - ResourceExhausted is retried only when the server says when. Without
//     a pushback it is as likely to be a quota as an overload, and a quota
//     isn't fixed by trying again a few hundred milliseconds later. Either
//     way it counts against the dependency, which is shedding load.
//   - DeadlineExceeded while the call's own deadline is live is the
//     server's: it gave up on something of its own, such as its
//     dependency. It counts, as the call's own deadline does, and isn't
//     retried, because the server's deadline is likely to end the next
//     attempt the same way.
//   - Canceled while the call's context is live comes from the server
//     cancelling the RPC, or the connection closing at OnStop. It says
//     nothing about the dependency.
//   - Any other code is the dependency working. It answered, and the
//     request was wrong or conflicted with its state, so the breaker counts
//     the dependency healthy, and the default doesn't try again.
//   - The default reads only the code and the trailer. It never reads a
//     status's details.
//
// The default retries every method on Unavailable, whatever the method. A
// server that returns Unavailable after acting on a request breaks gRPC's
// meaning of the code, so for most APIs that is safe. But grpc-go also
// returns Unavailable when the connection breaks after the request was
// sent, and then the server may have acted on it. So a method that must
// never be sent twice, such as one that charges a card, opts out in
// Classify, which gets the method: there is no per-method table in
// Settings.
//
// An end user whose API means something else by a code writes a Classify
// of their own, which usually handles what differs and hands the rest to
// the default. The ledger below aborts a write that races another write to
// the same account, which is over by the next attempt, and its Charge must
// never be sent twice:
//
//	d := grpcclient.DefaultSettings()
//	classify := d.Classify
//
//	settings.Classify = func(method string, err error, trailer metadata.MD) grpcclient.Classification {
//		c := classify(method, err, trailer)
//		if status.Code(err) == codes.Aborted {
//			c = grpcclient.Classification{Retry: retry.Retry, Breaker: circuit.Success}
//		}
//		if method == "/ledger.v1.LedgerService/Charge" && c.Retry == retry.Retry {
//			c.Retry, c.RetryAfter = retry.Stop, 0
//		}
//		return c
//	}
//
// An API that sends google.rpc.RetryInfo in its statuses' details, rather
// than a pushback, has a Classify that reads it with status.FromError and
// returns it as RetryAfter. The function is the end user's own, so one
// function can be set on every Client whose dependencies share a
// convention, such as a company's own services. humus exports no
// classifier but the default, and no helpers to build one.
//
// Classify is called on the goroutine that made the call, once for each
// attempt, so it must be safe for concurrent use. A Classification with a
// zero Outcome, or with retry.Retry and a negative RetryAfter, panics, as
// bedrock's classifiers do. The Client recovers nothing: a panic in
// Classify passes to the goroutine that made the call.
//
// # The pushback
//
// grpc-retry-pushback-ms is gRFC A6's trailer, in which a server says how
// many milliseconds to wait before the next attempt. The default Classify
// reads it from the attempt's trailer and returns it as the
// Classification's RetryAfter, so the wait before the next attempt is the
// larger of it and the schedule's. A pushback that is negative, not a
// number or given more than once means "don't retry", as A6 says of the
// first two. An end user's Classify returns whatever its API says.
//
// A RetryAfter longer than BackoffMax, from any Classify, isn't waited for:
// the call ends, and returns the attempt's error, as httpclient does with a
// long Retry-After. A server that asks for longer than that is not one a
// call in flight should wait on, and a wait past the call's deadline would
// end it anyway.
//
// # The circuit breaker
//
// Each Client has one circuit breaker, bedrock's circuit, built by
// NewClient with circuit.New from [Settings].Breaker and
// [Settings].BreakerPolicy. It guards the dependency at Addr: every method,
// from every goroutine, goes through the same breaker. Two Clients of one
// address have a breaker each, so an end user who wants one API's outage
// kept apart from another's, served at the same address, builds two
// Clients.
//
// The breaker sits inside the retries. Each attempt is one call through
// it, in this order:
//
//  1. Allow, with the call's context, and a deferred Release;
//  2. failure injection's evaluation of the attempt's point, and then,
//     unless a fault ended the attempt, the call, grpc-go's RPC (see
//     Failure injection, below);
//  3. Classify, once, unless the result is one the Client decides;
//  4. Done, with the result carrying its Classification, whose Breaker the
//     breaker counts.
//
// What counts is the Breaker of each attempt's Classification. A success
// counts toward the dependency being healthy, a failure against it, and
// an Ignored result counts nothing. The caller giving up is ignored. Every
// deadline counts as a failure, whether it is Settings.Timeout or a
// deadline the caller set or inherited from the request it serves: a
// dependency that hangs ends every call that way, so a breaker that
// ignored deadlines would never open on the outage it exists for. A stream
// that opened and ended with Unavailable is ignored: a rollout of the
// dependency, or its connection's age, ends every stream on a connection
// that way at once, and the breaker would open on a dependency whose other
// pods are healthy (see Streams, below).
//
// The default policy opens the breaker after 5 failures in a row, or once
// half of the last 20 or more results failed. While open, the breaker
// rejects every call for Breaker.OpenFor, 10s by default, and then lets
// Breaker.Probes calls through at once, one by default: it reopens on a
// probe that fails, and closes after 3 that succeed. The ratio rule needs
// 20 results, so with a Breaker.Window below 20 only the rule of 5 failures
// in a row trips it. A BreakerPolicy whose Trip always returns false turns
// the breaker off:
//
//	settings.BreakerPolicy.Trip = func(circuit.Counts) bool { return false }
//
// A call the breaker rejects reaches neither grpc-go nor the backend, and
// isn't retried: the call ends at once. Its error matches
// circuit.ErrRejected with errors.Is, and *circuit.RejectedError with
// errors.As, which says how much longer the breaker stays open. Its gRPC
// status is Unavailable, as for a server the Client can't reach, with the
// rejection's message, such as "circuit: call rejected: open for another
// 4.2s":
//
//	_, err := carts.AddItem(ctx, req)
//	if errors.Is(err, circuit.ErrRejected) {
//		// Nothing was sent. status.Code(err) is Unavailable.
//	}
//
// Since nothing reached grpc-go, otelgrpc makes no span for a rejected
// call. The Client starts one of its own, a child of the span in the
// call's context, under the scope github.com/z5labs/humus/grpc/grpcclient.
// It has kind client, and is named and attributed as otelgrpc names an
// attempt's span, with rpc.response.status_code UNAVAILABLE, and with
// server.address and server.port taken from Addr, since no connection was
// made. It ends at once, with status Error and the rejection's message.
//
// The Client logs one record for each change of the breaker's state, and
// nothing else, through s.Telemetry's LoggerProvider, under the same
// scope, in the context of the call that made the change: circuit breaker
// open at WARN, and circuit breaker half-open and circuit breaker closed at
// INFO. Each carries server.address and server.port, from Addr,
// humus.circuit.previous_state, and the counts of the state the breaker
// left: humus.circuit.successes, humus.circuit.failures,
// humus.circuit.consecutive_successes, humus.circuit.consecutive_failures
// and humus.circuit.timed_out. The bodies and the attributes are those of
// humus's other clients. The breaker is measured too, with humus's own
// instruments, as Telemetry, below, describes.
//
// BreakerPolicy's functions run with the breaker locked, on the goroutine
// whose result they decide on. The Client recovers nothing: a panic in
// one passes to the goroutine that made the call, and the breaker stays
// consistent, so the next call goes through it as before.
//
// # Streams
//
// NewStream runs the same loop, with MaxAttempts and the backoff, but not
// Timeout. Each attempt opens the stream, through the breaker:
//
//   - Opening is retried. NewStream fails before anything reaches the
//     server: no connection, or the channel in transient failure. A failed
//     open is classified as a unary attempt is, and Classify gets it with
//     no trailer, since nothing came back.
//   - An open stream is never retried. A stream that opens ends the call
//     for retry, without asking Classify. The end user's code sends and
//     receives on it directly, and a retry would replay what it already
//     saw: the call is committed once its result is handed to its caller.
//     A stream that fails after it opened ends with that error.
//
// A stream that opens takes its attempt's ticket, and the breaker counts
// it once, when it ends: when RecvMsg returns an error, io.EOF given as
// nil, or, for a client-streaming RPC, when RecvMsg returns its one
// response, given as nil. How it counts:
//
//   - a stream whose caller cancelled its context is ignored, without
//     asking Classify;
//   - one whose context reached its deadline counts as a failure, as a
//     unary call's does;
//   - one that ends with Unavailable is ignored, without asking Classify;
//   - any other end counts with the Breaker of Classify's answer, which
//     gets the stream's Trailer().
//
// Unavailable is how a stream ends when its server or its connection goes
// away. grpcserver's Shutdown ends every stream with it, so a rollout of
// the dependency ends each stream on a pod it replaces that way, and
// grpc-go ends with it every stream on a connection that closes, such as
// one the server's MaxConnectionAgeGrace closes. A connection's streams end
// together, and a stream opened again on the next pod counts nothing until
// it ends, so counting each would open the breaker after a rollout of a
// few pods, or a connection with five streams, on a dependency whose other
// pods are healthy. Whether the dependency is down is told by the next
// open instead: it goes through the breaker, and while the dependency is
// down it fails, and counts as a failure. A dependency that ends a stream
// with Unavailable as an error of its own, before or after a message, is
// ignored the same way.
//
// A stream its caller abandons, without reading it to its end or
// cancelling its context, never settles its ticket. While the breaker is
// closed that costs nothing. While it is half-open, the abandoned stream
// holds a probe's slot, but for at most Breaker.ProbeTimeout: circuit then
// frees the slot, so that another probe can be admitted, and counts the
// probe as timed out, which the default policy reads as neither a success
// nor a failure. So one leaked stream can't keep the breaker from closing.
// A stream that is still open holds its slot the same way: one opened
// while the breaker is half-open, such as a subscription opened again,
// holds a probe's slot until it ends, or for at most Breaker.ProbeTimeout.
//
// # opts
//
// NewClient's opts are grpc.DialOptions, for what humus doesn't configure:
// per-RPC credentials with grpc.WithPerRPCCredentials, interceptors,
// grpc.WithConnectParams, and the like. They come after humus's own
// options, which trace and measure every attempt, dial through the Host,
// use no proxy, and turn grpc-go's configurable retries off. They run
// inside the Client's retries: grpc-go applies them to each attempt. A stats
// handler in opts, with grpc.WithStatsHandler, is a second one: grpc-go
// calls it as well, after humus's. The transport credentials built from
// Settings.TLS come after opts, so a grpc.WithTransportCredentials in opts
// has no effect: Settings.TLS is the one place TLS is configured, and plain
// text is a nil TLS. NewClient sets no default service config.
//
// Since opts come after humus's own options, an option in opts that
// replaces one of them wins. A grpc.WithContextDialer, or a
// grpc.WithResolvers with a resolver for the passthrough scheme, takes the
// dial away from the Host, and with it the test Host's network: the Client
// then dials only where the end user's dialler or resolver does.
//
// # grpc-go's retries
//
// grpc-go retries an RPC by itself when a service config gives its method a
// retryPolicy, with its own throttling and pushback. The Client passes
// grpc.WithDisableRetry, so no service config, from the end user's
// grpc.WithDefaultServiceConfig in opts or any other source, turns those
// configurable retries on. Whether an RPC is tried again is decided by the
// Client, never by a service config that an operator can't see in Settings.
//
// WithDisableRetry leaves grpc-go's transparent retries on. grpc-go retries
// an RPC by itself when no data reached the wire, or when the server
// reports it never processed it, such as a stream refused by a server that
// sent a GOAWAY. They never repeat anything a server saw, so they are safe
// whatever the method, and the Client keeps them.
//
// # Telemetry
//
// The Client takes everything from the Setup's telemetry and never reads
// or sets OpenTelemetry's globals. NewClient's first dial option is a stats
// handler, OpenTelemetry's gRPC instrumentation, otelgrpc, given the
// TracerProvider, the MeterProvider and the Propagator of s.Telemetry:
//
//	grpc.WithStatsHandler(otelgrpc.NewClientHandler(
//		otelgrpc.WithTracerProvider(s.Telemetry.TracerProvider),
//		otelgrpc.WithMeterProvider(s.Telemetry.MeterProvider),
//		otelgrpc.WithPropagators(s.Telemetry.Propagator),
//	))
//
// grpc-go calls a stats handler for each attempt, so each attempt is one
// span of kind client, a child of the span in the call's context. A call
// the Client retries is a span for each attempt, siblings under the
// caller's span, and so is a call that grpc-go retries transparently. A
// stream is one span, from its opening to its end. The span is named after
// the full method without its leading slash, such as
// "cart.v1.CartService/AddItem", and carries the attributes of
// OpenTelemetry's RPC semantic conventions, among them rpc.system.name,
// which is grpc, rpc.method, and rpc.response.status_code, the name of the
// call's code, such as UNAVAILABLE. Its status is Error for every code other than
// OK: otelgrpc counts every failed call as an error on the client's side,
// NotFound included, unlike on a server's.
//
// Each attempt carries the trace context of its span, and the baggage of
// the call's context, in its metadata, written by the Setup's propagator:
// with the default one, traceparent, tracestate when the span context has
// one, and baggage when the context holds any. So a
// backend served with grpcserver joins the caller's trace, its server span
// a child of the attempt's client span, and its handler sees the caller's
// baggage, less a failure scenario the Client strips (see Failure
// injection).
//
// Each attempt also records the histogram rpc.client.call.duration on the
// Setup's MeterProvider, in otelgrpc's instrumentation scope.
//
// The Client records four instruments of humus's own, for its breaker and
// its retries, with the Setup's MeterProvider, in the scope
// github.com/z5labs/humus/grpc/grpcclient. Every data point carries the
// dependency's server.address and server.port, from Addr, and nothing per
// call, such as rpc.method. Every other attribute's value is the String()
// of bedrock's own type:
//
//   - humus.circuit.state, an asynchronous UpDownCounter in {breaker}, with
//     humus.circuit.state: closed, open or half-open. At each collection it
//     reads the breaker's state and observes 1 for it and 0 for the other
//     two. It reads half-open as soon as Breaker.OpenFor has passed, with
//     no call made. Two Clients of one Addr add up, such as closed 1 and
//     open 1. It is still read by the App's last collection, after the
//     OnStop functions, so the final export has the state the App stopped
//     in.
//   - humus.circuit.transitions, a Counter in {transition}, with
//     humus.circuit.state, the state the breaker went to, and
//     humus.circuit.previous_state, the state it left: one for each change
//     of state, beside its log record.
//   - humus.circuit.calls, a Counter in {call}, with humus.circuit.outcome:
//     success, failure or ignored, as each call counted in the breaker, when
//     it is counted; or rejected, for a call the breaker didn't admit. A
//     call is one unary attempt, or one stream's open: a unary attempt
//     counts once it has returned, and a stream once it has ended, as
//     Streams, above, describes. A call whose ticket is released uncounted,
//     such as a stream whose caller cancelled its context, or an attempt
//     that panicked, counts nothing.
//   - humus.retry.decisions, a Counter in {decision}, with
//     humus.retry.decision: again, succeeded, stopped, exhausted, throttled
//     or expired, what retry decided after each unary attempt and each
//     attempt to open a stream, a rejected one's stopped included.
//
// A View in the end user's telemetry Builder drops or renames them. humus
// has no setting to turn them off.
//
// The rest of the Client's own telemetry is what the circuit breaker adds,
// under the same scope: the span of a call the breaker rejected, which
// never reached grpc-go, and one log record for each change of the
// breaker's state, as The circuit breaker, above, describes; and the span
// of an attempt a fault ended, which never reached grpc-go either, as
// Failure injection, below, describes.
//
// Nothing reaches OpenTelemetry's globals: every provider and the
// propagator are passed to otelgrpc, so it never falls back to them, and
// the Client sets none. What is still read from the real environment, not
// the Process's, is what grpcserver's documentation records: grpc-go's
// logger, which reads GRPC_GO_LOG_SEVERITY_LEVEL and the other GRPC_GO_LOG_
// variables and writes to the real standard error, grpc-go's other GRPC_
// variables, and OTEL_SEMCONV_STABILITY_OPT_IN, which otelgrpc reads, and
// which asks for the old RPC conventions with rpc/old or rpc/dup. The
// Client adds nothing to that list.
//
// # Failure injection
//
// Every Client is an injection point per method, named after it:
// grpc:<service>/<method>, the full method without its leading slash, as
// rpc.method writes it, such as grpc:payments.v1.Payments/Charge. A call
// whose context carries a failure scenario can then make that one method
// of the dependency fail or slow down, and the end user sees how the whole
// service copes: its retries, its breaker and its fallbacks. The Client
// evaluates its points with the fault.Injector in the Setup's Faults,
// which the App builds from humus.WithFaults. Without humus.WithFaults,
// nothing is evaluated or stripped, and a scenario goes to the dependency
// as any baggage does.
//
// Each attempt of a unary call, and each attempt to open a stream,
// evaluates its point once, after the circuit breaker's Allow and before
// grpc-go is called, inside Timeout. An attempt the breaker rejects
// evaluates nothing. On a call that carries a scenario, the evaluation is
// a humus.fit.evaluation event on the span in the call's context, the
// caller's, since the attempt's own span doesn't exist yet. A retried
// attempt evaluates the point again, and meets the same fault.
//
// An abort ends the attempt with status Unavailable, as for a server the
// Client can't reach, and an error wrapping a *fault.InjectedError naming
// the point, which matches fit.ErrInjected. grpc-go isn't called, so
// otelgrpc makes no span for it: the Client records one of its own, in its
// own scope, named and attributed as a rejected call's is, with
// rpc.response.status_code UNAVAILABLE and the error.type
// github.com/z5labs/humus/fault.InjectedError.
//
// A delay is waited before grpc-go is called, inside Timeout, and then the
// attempt goes on as usual, so the delay is in no span of otelgrpc's. A
// delay that the attempt's context cuts short ends the attempt as grpc-go
// would: with status DeadlineExceeded, at Timeout or a deadline of the
// caller's, or Canceled, once the caller gives up. Its error wraps a
// *fault.InjectedError whose Err is the context's cause, which matches
// context.DeadlineExceeded or context.Canceled and not fit.ErrInjected.
// grpc-go isn't called, and the attempt has a span of the Client's own, as
// an abort does, from the start of the wait to its end, with the same
// error.type.
//
// A fault is a real failure: the Client decides an attempt a fault ended
// with Classify, as it decides any other attempt, the ones it decides
// itself included. Under the default Classify:
//
//   - an abort is Retry, Failure, as Unavailable is;
//   - a delay cut short by its deadline is Stop, Failure, as any deadline
//     is;
//   - a delay the caller cancelled is Stop, Ignored.
//
// They draw on the retry budget, and the breaker counts them, so a
// scenario that aborts a method of the dependency opens the breaker for
// every caller, as a dependency that is down does. An end user's Classify
// can single out a *fault.InjectedError with errors.As, such as to answer
// Stop, Ignored, which keeps the breaker closed and makes one attempt per
// call:
//
//	settings.Classify = func(method string, err error, trailer metadata.MD) grpcclient.Classification {
//		var injected *fault.InjectedError
//		if errors.As(err, &injected) {
//			return grpcclient.Classification{Retry: retry.Stop, Breaker: circuit.Ignored}
//		}
//		return classify(method, err, trailer) // DefaultSettings().Classify
//	}
//
// The Client strips the scenario from what it sends, unless
// Settings.ForwardScenario is set: a dependency the end user hasn't decided
// about is a trust boundary. The strip happens after the point is
// evaluated, since the point reads the scenario about to be stripped, and
// before grpc-go is called, so otelgrpc injects from the stripped context:
// the dependency still gets the attempt's trace context and every other
// baggage member. Each strip adds a humus.fit.strip event, with no
// attributes, to the span in the call's context. Set ForwardScenario for a
// dependency that takes part in failure injection with the same codec,
// such as another service of the end user's, so that a scenario can reach
// the points it calls in turn.
//
// # Closing
//
// NewClient registers the connection's Close with Setup.OnStop. OnStop
// functions run after every Service has stopped, so no Service is still
// calling, and within the grace period. They run when Init fails after
// NewClient too. An RPC made after the close fails with codes.Canceled, and
// "grpc: the client connection is closing".
//
// Client has no Close of its own, so the end user's code can't close a
// connection that other Services share, under them.
//
// NewClient dials nothing, as grpc.NewClient doesn't: the first RPC dials,
// or the first check of [Client.Connected]. So a dependency that is down
// when the service starts doesn't fail Init, and the service doesn't
// crash-loop over another service's outage. Its RPCs fail with
// codes.Unavailable until the dependency is reachable. A service that must
// not turn ready without its dependency puts Connected in its readiness, as
// Readiness, below, sets out.
//
// # Readiness
//
// [Client.Connected] returns a health.Monitor for readiness. It is healthy
// once the Client has connected to Addr: unhealthy until its connection is
// first ready, its TLS handshake done, and healthy from then until the
// connection is closed. It never returns an error. It goes in the readiness
// of whatever exposes the probes:
//
//	hs := health.NewService(health.Readiness(carts.Connected()))
//	return []humus.Service{api, hs}, nil
//
// What it gates is a rollout. NewClient dials nothing, so without it a
// release with a wrong Addr, a CA that doesn't trust the server, a missing
// TLS config, or a dependency it can't reach turns ready, replaces the pods
// that worked, and fails every RPC. With Connected in its readiness, it
// never turns ready, and the rollout stops at its progress deadline with
// the old pods still serving.
//
// It latches, as kafka's (*Service).Joined does, for the same reason. After
// the first connection, an outage of the dependency hits every pod at once.
// Turning them all unready would take the service out of its load
// balancer, so its callers would get no answer instead of this service's
// own error, and the parts of it that don't need the dependency would stop
// too. It would also block a rollout that might be the fix. So once the
// connection has been ready, the monitor stays healthy whatever the
// connection's state: after a GOAWAY, from a server's MaxConnectionAge or a
// rollout of the dependency, after a lost connection, and after grpc-go
// closes a connection idle for 30 minutes. Every monitor Connected returns
// reads the same latch, kept on the Client, so an HTTP probe and a gRPC one
// agree.
//
// Connected is for readiness only. A liveness monitor would restart every
// pod in an outage of the dependency, and restarting the pod doesn't bring
// a dependency back.
//
// Its first check dials. Until the connection is first ready, a check while
// it is idle starts it, so a service with Connected in its readiness dials
// at its first probe rather than its first RPC. The check then waits for
// the connection's state to change, until it is ready, the connection is
// closed, or the check's context is done. So a reachable dependency passes
// the first probe, and an unreachable one fails it when the probe's own
// timeout ends the check. A check makes no RPC: the dependency sees no
// call, the trace has no span, and nothing is logged. A connection that
// fails shows in the RPCs that wait on it, and in grpc-go's own logger.
//
// After Setup.OnStop has closed the connection, the monitor is unhealthy.
// By then every Service has stopped, and the health Service has already
// turned readiness false.
//
// A monitor that calls the dependency's grpc.health.v1 service is the end
// user's own, a few lines over the Client:
//
//	health.MonitorFunc(func(ctx context.Context) (bool, error) {
//		resp, err := healthpb.NewHealthClient(carts).Check(ctx, &healthpb.HealthCheckRequest{Service: "cart.v1.CartService"})
//		return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING, nil
//	})
//
// Such a monitor makes an RPC through the Client, so whatever fails the
// Client's calls fails it too: an outage of the dependency, and an open
// circuit breaker, which fails calls at once while the dependency keeps
// failing. It then turns unhealthy, and with it every pod, in the outage
// Connected's latch exists to ride out. Connected makes no RPC, so neither
// turns it unhealthy.
//
// # Settings
//
// [DefaultSettings] returns the package's defaults, and each field has a
// check, such as [CheckAddr], that reports what is wrong with a value
// without naming the field. The package reads no environment variable and
// defines no variable name: the end user's Init sources each field, with
// whatever variable names the service uses, and passes the field's check to
// Validate, so an operator's bad value fails startup with exit 78, naming the
// variable. Addr has no default, so it is sourced with no .Default in its
// chain, and a variable that isn't set fails as not found. NewClient runs
// every check again, and reports every bad field in one error that names
// each one.
//
// [CheckAddr] rejects an empty host, which grpcserver.CheckAddr accepts: a
// client dials a host, it doesn't listen on every name. It rejects a scheme,
// such as dns:///, since the Client never resolves names itself.
package grpcclient
