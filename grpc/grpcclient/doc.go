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
// A Client is one dependency at one Addr, over one connection. The end
// user's Init builds one Client for each dependency, with its own Settings,
// and shares it between every Service that calls it: the Client is safe for
// concurrent use, and its RPCs share its connection. A Client built for each
// call would dial and handshake for each call, and register one more
// function with Setup.OnStop each time. NewClient takes the *humus.Setup
// because it belongs in Init: build the Client there, once, and share it.
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
// Settings.Timeout bounds each unary RPC. Invoke applies it to the call's
// context, so a caller's earlier deadline wins, but a later one doesn't
// extend it: an end user whose calls need longer sets the field. A Timeout
// that has passed fails the RPC with codes.DeadlineExceeded. [CheckTimeout]
// rejects a Timeout that is not positive: an RPC with no deadline can hang
// for as long as the server does.
//
// Timeout doesn't apply to streams. A stream may rightly live as long as its
// caller wants, so it ends with the context its caller opened it with.
//
// # opts
//
// NewClient's opts are grpc.DialOptions, for what humus doesn't configure:
// per-RPC credentials with grpc.WithPerRPCCredentials, interceptors,
// grpc.WithConnectParams, and the like. They come after humus's own
// options, which trace and measure every attempt, dial through the Host,
// use no proxy, and turn grpc-go's configurable retries off. A stats
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
// that grpc-go retries transparently is two attempts, and two spans. A
// stream is one span, from its opening to its end. The span is named after
// the full method without its leading slash, such as
// "cart.v1.CartService/AddItem", and carries the attributes of
// OpenTelemetry's RPC semantic conventions, among them rpc.system.name,
// grpc, rpc.method, and rpc.response.status_code, the name of the call's
// code, such as UNAVAILABLE. Its status is Error for every code other than
// OK: otelgrpc counts every failed call as an error on the client's side,
// NotFound included, unlike on a server's.
//
// Each attempt carries the trace context of its span, and the baggage of
// the call's context, in its metadata, written by the Setup's propagator:
// traceparent and tracestate, and baggage, with the default one. So a
// backend served with grpcserver joins the caller's trace, its server span
// a child of the attempt's client span, and its handler sees the caller's
// baggage.
//
// Each attempt also records the histogram rpc.client.call.duration on the
// Setup's MeterProvider, in otelgrpc's instrumentation scope. humus records
// no instrument of its own: whether it should is the question of Proposal:
// humus's own metrics, in wave 5.
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
// NewClient dials nothing, as grpc.NewClient doesn't: the first RPC dials.
// So a dependency that is down when the service starts doesn't fail Init,
// and the service doesn't crash-loop over another service's outage. Its RPCs
// fail with codes.Unavailable until the dependency is reachable. A service
// that must not start without its dependency calls it in Init.
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
