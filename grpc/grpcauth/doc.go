// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package grpcauth authenticates and authorizes each RPC of a
// grpcserver.Service by its method, with the schemes and requirements the
// auth package declares.
//
// It is in the nested module github.com/z5labs/humus/grpc, which is
// always built against the core module in the same commit, so a pod
// without gRPC doesn't link grpc-go's server.
//
// [Interceptors] returns a unary and a stream interceptor, which the end
// user passes to grpcserver.NewService:
//
//	bearer := auth.Bearer("bearer", tokens.Verify)
//	unary, stream, err := grpcauth.Interceptors(s,
//		grpcauth.Default(auth.Require(bearer)),
//		grpcauth.Method(cartpb.CartService_DeleteCart_FullMethodName, auth.RequireScopes(bearer, "carts:write")),
//		grpcauth.Method(healthpb.Health_Check_FullMethodName, auth.Disabled()), // the kubelet presents no credentials
//		grpcauth.Method(healthpb.Health_Watch_FullMethodName, auth.Disabled()),
//		grpcauth.Method(healthpb.Health_List_FullMethodName, auth.Disabled()),
//	)
//	if err != nil {
//		return nil, err
//	}
//	svc, err := grpcserver.NewService(s, settings, unary, stream)
//
// A handler reads its principal back with the scheme's From, typed:
//
//	func (c carts) DeleteCart(ctx context.Context, req *cartpb.DeleteCartRequest) (*cartpb.DeleteCartResponse, error) {
//		claims, _ := c.bearer.From(ctx)
//		...
//	}
//
// # Rules
//
// A [Rule] gives methods their requirement: a list of auth.Requirement, of
// which an RPC must meet one, tried in order, as the auth package
// describes.
//
//   - [Default] is the requirement of every method no Method names. A
//     method added to the Service later, and server reflection if the end
//     user registers it, is covered by it.
//   - [Method] is one method's requirement, which replaces the Default's.
//   - auth.Disabled() makes either public: the RPC is served, no credential
//     is read, and no principal is in its context.
//
// A method is named by its full name, /package.Service/Method, which is
// the only thing an interceptor knows a call by, as gRPC's own
// authorization policies and Istio's key on it too. protoc-gen-go-grpc
// generates a constant for each, such as
// cartpb.CartService_DeleteCart_FullMethodName, so no name is typed by
// hand.
//
// Default is required, because there is no document to show which methods
// are open. Default(auth.Disabled()) makes "public unless listed" an
// explicit choice, with each secured method listed by Method:
//
//	grpcauth.Default(auth.Disabled()),
//	grpcauth.Method(cartpb.CartService_DeleteCart_FullMethodName, auth.Require(bearer)),
//
// An empty Default() or Method(m) is a mistake rather than "public", so
// that every open path says auth.Disabled() where a review sees it.
//
// # Both interceptors
//
// gRPC has no ServerOption that is a unary and a stream interceptor at
// once: a unary interceptor sees only unary methods, and a stream
// interceptor only streaming ones. Interceptors returns both, so dropping
// one is a visible _, rather than a streaming method left open.
//
// They are grpc.ChainUnaryInterceptor and grpc.ChainStreamInterceptor, so
// they run inside grpcserver's own interceptors: after otelgrpc has started
// the RPC's span, and inside its panic recovery and the stream
// cancellation of shutdown. An interceptor the end user passes after them
// runs after them, with the principals in its context.
//
// # Where each credential is read
//
//   - auth.Bearer, auth.OAuth2 and auth.OpenIDConnect read the authorization
//     metadata, "Bearer <token>", as the Authorization header is read.
//   - auth.Basic reads the authorization metadata, "Basic <base64>".
//   - auth.APIKeyHeader reads the metadata key, lowercased, as HTTP/2
//     carries every header.
//   - auth.MutualTLS reads the client certificate chains the TLS handshake
//     verified, from the peer's credentials.TLSInfo. The Service's
//     Settings.TLS verifies them against its ClientCAs when its ClientAuth is
//     tls.VerifyClientCertIfGiven or tls.RequireAndVerifyClientCert. A
//     certificate that is only requested, not verified, is no credential.
//   - auth.APIKeyQuery and auth.APIKeyCookie read what an RPC doesn't have,
//     so either is a mistake Interceptors reports.
//
// # Health and other open methods
//
// grpcauth exempts no method on its own: an exemption the end user can't
// see is a hole. gRPC health v1, registered on the same Service with
// grpchealth.NewServer, passes through the interceptors like every RPC,
// and the kubelet's probes present no credentials. So the end user opens
// Health's Check, Watch and List by name, with
// healthpb.Health_Check_FullMethodName, Health_Watch_FullMethodName and
// Health_List_FullMethodName, and auth.Disabled(), as in the example above.
// If they don't, the probes get Unauthenticated, and the pod never turns
// ready: that fails at the first deploy, never silently.
//
// A probes Service of its own, grpchealth.NewService, has no
// authentication and needs no rule.
//
// # Answers
//
// An RPC that meets no requirement ends with a fixed status, and no
// verifier's text reaches the client:
//
//   - an RPC no requirement authenticated: codes.Unauthenticated,
//     "unauthenticated";
//   - one authenticated without a scope it needs: codes.PermissionDenied,
//     "forbidden";
//   - one a verifier failed on with an error that doesn't wrap
//     auth.ErrUnauthenticated, such as a directory that can't be reached:
//     the error is the server's, and is recorded. The RPC ends with the
//     error's own gRPC status if it has one, so a verifier that returns
//     status.Error(codes.Unavailable, …) gets Unavailable, which clients
//     retry. Otherwise it ends with codes.Internal, "authentication failed",
//     never Unauthenticated, which would tell the client to drop a valid
//     credential.
//   - a verifier that panics: the panic passes to grpcserver's recovery,
//     which records it and ends the RPC with codes.Internal.
//
// # Telemetry
//
// A refusal isn't recorded: Unauthenticated and PermissionDenied are the
// client's mistakes, and otelgrpc already records the RPC's code on its
// span and in rpc.server.call.duration.
//
// A verifier's other error is recorded once: as an exception event on the
// RPC's span, and as one ERROR record, "authentication failed", in the
// RPC's context, so it carries the span's trace and span IDs. The record
// has the attributes humus.auth.scheme, the name of the scheme whose
// verifier failed, error.type, exception.message and rpc.method. When the
// client has gone away, the error goes on the span alone. No principal,
// subject or key is recorded.
//
// The interceptors log through the Setup's LoggerProvider, with the logger
// name github.com/z5labs/humus/grpc/grpcauth, and read and set no
// OpenTelemetry global.
package grpcauth
