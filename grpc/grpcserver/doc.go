// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package grpcserver serves gRPC with humus's lifecycle.
//
// It is in the nested module github.com/z5labs/humus/grpc, which is
// always built against the core module in the same commit.
//
// [NewService] builds a [Service], in the end user's Init, from [Settings]
// the end user sources. The end user registers their generated services on
// it, as they would on a *grpc.Server, and returns it:
//
//	func Init(ctx context.Context, s *humus.Setup) ([]humus.Service, error) {
//		settings, err := readSettings(ctx, s) // grpcserver.Settings, sourced with config.Struct
//		if err != nil {
//			return nil, err
//		}
//		svc, err := grpcserver.NewService(s, settings, grpc.ChainUnaryInterceptor(authenticate))
//		if err != nil {
//			return nil, err
//		}
//		cartpb.RegisterCartServiceServer(svc, carts)
//		return []humus.Service{svc}, nil
//	}
//
// NewService listens on the address through the Setup's Host, so a port
// that is already taken fails Init, and a connection that arrives before
// Serve waits in the backlog. If Init fails after NewService, the listener
// is closed by a function NewService registers with the Setup's OnStop.
//
// # TLS
//
// TLS is required. The Service serves gRPC with Settings.TLS and nothing in
// plain text. Its transport credentials are built from Settings.TLS and
// passed after the end user's options, and grpc-go keeps the last
// grpc.Creds it is given, so a grpc.Creds in opts, such as
// grpc.Creds(insecure.NewCredentials()), has no effect. A plaintext client
// fails to connect.
//
// # Options
//
// Everything about the server that isn't a setting is a grpc.ServerOption
// the end user passes to NewService: their interceptors, keepalive
// parameters, message size limits, a second stats handler. They come after
// humus's own options, and grpc-go keeps one value of some options and
// chains others:
//
//   - An option that chains, such as grpc.ChainUnaryInterceptor or
//     grpc.ChainStreamInterceptor, adds the end user's interceptors inside
//     humus's, so humus's run first.
//   - An option that grpc-go keeps one value of, such as
//     grpc.MaxRecvMsgSize, replaces humus's.
//   - grpc.WaitForHandlers(false) replaces humus's
//     grpc.WaitForHandlers(true), and lets a handler outlive Shutdown. That
//     breaks the humus.Service contract, and humustest.TestService reports
//     the handler's goroutine as a leak.
//
// # Registration
//
// [Service.RegisterService] must be called before Serve, as with a
// *grpc.Server. grpc-go exits the process on three registration mistakes:
// a registration after Serve, a service registered twice, and an
// implementation that doesn't implement the service's interface. The
// Service checks for the same three first, and panics instead, with the
// service's name in the message. All three are programming errors, so the
// panic fails the end user's Init in the first test or the first start,
// without leaving the process through os.Exit.
//
// # Reflection
//
// The Service registers no server reflection: reflection exposes the
// service's schema to anyone who can reach its port. An end user who wants
// it registers it in their Init, which [Service.GetServiceInfo] makes work:
//
//	reflection.Register(svc)
//
// # Shutdown
//
// The Service follows the two shutdown signals of a humus App:
//
//   - Serve's context being done changes nothing. The Service keeps serving
//     connections and RPCs through the App's drain, while a load balancer
//     stops sending it traffic.
//   - Shutdown calls GracefulStop. It stops accepting connections, sends
//     every client a GOAWAY, so that clients open new connections
//     elsewhere, and waits for the RPCs in flight to finish. gRPC
//     connections are long-lived, and the GOAWAY is what moves a client off
//     a terminating pod. When the grace period runs out first, Shutdown
//     calls Stop, which closes every connection and cancels every RPC's
//     context, waits for every handler to return, and the App exits with a
//     *humus.GracePeriodError.
//
// A handler should return once its context is done. With
// grpc.WaitForHandlers(true), which humus sets, Stop waits for every
// handler, so a handler that ignores its context holds the App up past the
// grace period.
//
// # Telemetry
//
// The Service takes everything from the Setup's telemetry and never reads
// or sets OpenTelemetry's globals. The first of humus's options is a stats
// handler, OpenTelemetry's gRPC instrumentation, otelgrpc, given the
// TracerProvider, the MeterProvider and the Propagator of s.Telemetry:
//
//	grpc.StatsHandler(otelgrpc.NewServerHandler(
//		otelgrpc.WithTracerProvider(s.Telemetry.TracerProvider),
//		otelgrpc.WithMeterProvider(s.Telemetry.MeterProvider),
//		otelgrpc.WithPropagators(s.Telemetry.Propagator),
//	))
//
// Each RPC gets one server span, named after the method without its
// leading slash, such as "cart.v1.CartService/AddItem". Its parent is the
// trace context the RPC's metadata brought in, extracted with the Setup's
// propagator, so an RPC joins its caller's trace. The propagator comes from
// the telemetry Builder, and it decides which metadata carries the trace
// context: traceparent and tracestate, and baggage, with the default one.
// The span carries the attributes of OpenTelemetry's RPC semantic
// conventions, among them rpc.system.name, rpc.method, server.port and
// rpc.response.status_code. Its status is Error for the codes those
// conventions count as server errors, Unknown, DeadlineExceeded,
// Unimplemented, Internal, Unavailable and DataLoss, and is left unset for
// every other code, such as NotFound, which is the caller's error rather
// than the server's.
//
// Each RPC also records the histogram rpc.server.call.duration on the
// Setup's MeterProvider, in otelgrpc's instrumentation scope.
//
// The names above are those of the stable RPC conventions, which otelgrpc
// uses unless OTEL_SEMCONV_STABILITY_OPT_IN, read from the real
// environment, not the Process's, asks for the old ones with rpc/old or
// rpc/dup.
//
// An end user who wants the metrics of gRFC A66, grpc-go's own
// OpenTelemetry metrics, passes them in opts as a second stats handler,
// which grpc-go calls after humus's:
//
//	grpcserver.NewService(s, settings, opentelemetry.ServerOption(opentelemetry.Options{
//		MetricsOptions: opentelemetry.MetricsOptions{MeterProvider: s.Telemetry.MeterProvider},
//	}))
//
// # Logging
//
// The Service writes no log record of its own but one for each panic, as
// described under Panics: the App already logs that it is serving,
// shutting down and stopped.
//
// grpc-go's own logger is an ambient leak. Its grpclog package reads
// GRPC_GO_LOG_SEVERITY_LEVEL, GRPC_GO_LOG_VERBOSITY_LEVEL and
// GRPC_GO_LOG_FORMATTER, and its internal/envconfig package reads its other
// GRPC_ variables, from the real environment, not the Process's, and it
// writes to the real standard error. Replacing it means
// grpclog.SetLoggerV2, which is global state, and humus sets none. An end
// user who wants grpc-go's logs in their telemetry sets the logger in main.
//
// # Panics
//
// A handler that panics fails only its own RPC. grpc-go recovers nothing,
// so the outermost of humus's interceptors, a unary and a stream one, do:
// they run inside the stats handler and outside the end user's
// interceptors, so the panic of an interceptor in opts is recovered too.
//
// The RPC fails with codes.Internal and the message "internal error", so
// the panic's value never reaches the client. A stream that had already
// sent messages ends with that status. The panic is recorded on the RPC's
// span as an exception event, and the span's status is Error, from the
// Internal code. One ERROR record, "RPC panicked", is emitted through the
// Setup's logger named github.com/z5labs/humus/grpc/grpcserver, in the
// RPC's context, so it carries the span's trace and span IDs. It has the
// attributes rpc.method, exception.type, exception.message and
// exception.stacktrace, the stack of the panic. Every other RPC, and the
// server, carry on.
//
// The cost is that of net/http's recovery: whatever the handler left
// half-changed stays that way. A lock it held is never released, and a
// structure it was updating is left as it was when it panicked, for the
// RPCs that come after.
//
// Only RPCs are recovered. The lifecycle itself, the end user's Init and
// the Service's Serve and Shutdown, never recovers a panic.
//
// # Settings
//
// [Settings] holds what an operator changes from one deployment to the
// next: the address to listen on, and the TLS config to serve with. Every
// field is a plain value, and each has a check, [CheckAddr] and [CheckTLS],
// that reports what is wrong with a value without naming the field. The
// package reads no environment variable and no file: the end user sources
// the settings in their Init, with whatever variable names they choose,
// and runs each field's check as they do:
//
//	settings, err := config.Struct(func(r *config.Resolver) grpcserver.Settings {
//		return grpcserver.Settings{
//			// No .Default: both fields are required, so an unset variable fails as not found.
//			Addr: r.Get(env("GRPC_ADDR").Validate(grpcserver.CheckAddr)),
//			TLS:  r.Get(env("GRPC_TLS_P12_FILE").Parse(readFile(s.Host.FS)).Parse(serverTLS).Validate(grpcserver.CheckTLS)),
//		}
//	}).Read(ctx)
//
// Here s is the Setup the end user's Init is given, and env, readFile and
// serverTLS are the end user's own helpers: env names a variable, readFile
// reads the file it names, and serverTLS builds a *tls.Config from what it
// read.
//
// Neither field has a default. humus imposes no port, and serves nothing in
// plain text, so the end user always sources both, with no .Default in
// their chains. The package has no DefaultSettings: it would only return
// the zero Settings, whose Addr of "" and nil TLS both checks reject. A
// variable that isn't set fails as not found, and a value that fails its
// check fails Init, naming the variable, so the process exits 78.
package grpcserver
