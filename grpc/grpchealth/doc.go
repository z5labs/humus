// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package grpchealth answers gRPC health v1, the protocol of
// grpc.health.v1.Health, from an App's health.Probes, so Kubernetes' grpc
// probes, grpc-health-probe and grpc-go's client-side health checking can
// all ask a gRPC service whether it is healthy.
//
// [NewServer] is the health server, registered on the end user's
// grpcserver.Service where the cluster's grpc probes can use TLS. [NewService]
// serves it in plain text on a port of its own, where they can't, which is
// most clusters today.
//
// # The names and their answers
//
// [NewServer] answers three service names, each from one of the probes'
// monitors:
//
//   - "" and "readiness" check Probes.Readiness;
//   - "liveness" checks Probes.Liveness.
//
// "" is the overall status, which a grpc probe without a service field,
// grpc-health-probe and grpc-go's client-side health checking all ask for,
// so it is readiness. "liveness" and "readiness" are the names Kubernetes
// suggests for answering both probes on one port.
//
// Check answers SERVING when the monitor is healthy, and NOT_SERVING when
// it is unhealthy or returns an error. Any other name fails with
// codes.NotFound, the name of a service registered on the same server, such
// as cart.v1.CartService, included: health has no status per gRPC service.
// Each Check checks its monitor once, with the RPC's context. The server
// adds no timeout and caches nothing: the caller's deadline bounds the
// check. List answers the three names, each with its current status.
//
// A monitor's error is never in an answer. It is logged instead, as one
// ERROR record through the Setup's LoggerProvider, under this package's
// import path, with the RPC's context. The record's body is the error, and
// its attribute health.probe is liveness or readiness. A monitor that is
// unhealthy without an error, such as a health Service's own readiness once
// the App starts shutting down, logs nothing.
//
// # Watch
//
// Monitors are checked when asked, and send no changes, so a Watch polls.
// It sends the current status at once, and then each change of it. The
// monitor is checked once a second, with a context that is done after a
// second, and a check that runs out of time is NOT_SERVING, whether or not
// the monitor honours its context. A monitor that doesn't is not checked
// again until that check returns. Every Watch of one name on one server
// shares one poller, which runs while the name has watchers and stops when
// it has none, so a hundred clients watching "" cost one check a second,
// not a hundred. A poller logs a failing monitor's error when the status
// changes, not on every poll.
//
// A Watch of any other name gets one SERVICE_UNKNOWN, and nothing else, and
// its stream stays open until the client or the server ends it. A stream
// ends when its context is done. On a grpcserver.Service, Shutdown does
// that, and the Watch ends with codes.Unavailable, as every stream does.
//
// With a one-second poll, a client using grpc-go's client-side health
// checking learns that a pod is draining within a second of shutdown
// starting, well within grpcserver.Main's 5s drain, and moves its RPCs to
// other pods before the GOAWAY.
//
// # Registering the server
//
// The server is registered on the end user's grpcserver.Service, next to
// the end user's own services. It answers from the Probes of a health
// Service, which owns the App's readiness and is returned from Init next
// to the gRPC Service:
//
//	svc, err := grpcserver.NewService(s, settings)
//	if err != nil {
//		return nil, err
//	}
//	cartpb.RegisterCartServiceServer(svc, carts)
//	hs := health.NewService()
//	healthpb.RegisterHealthServer(svc, grpchealth.NewServer(s, hs.Probes()))
//	return []humus.Service{svc, hs}, nil
//
// A grpcserver.Service serves only TLS, and the kubelet's grpc probes
// connect in plain text unless the cluster has the GRPCContainerProbeTLS
// feature gate on. So the kubelet's probes reach the server there only on a
// cluster with that gate on, and only when the Service requires no client
// certificate, since the kubelet never presents one: its settings.TLS sets
// ClientAuth to neither RequireAnyClientCert nor RequireAndVerifyClientCert.
// grpc-go's clients reach it on every cluster. Everywhere else, the
// kubelet's probes reach a probes Service on a plaintext port of its own,
// below.
//
// A probe through the server is an RPC like any other of the Service, so
// the Service's instrumentation traces and measures it, and a monitor's
// ERROR record carries the trace of the probe's span. A team that doesn't
// want the probes' spans drops them in its sampler.
//
// A Check or List recovers no panic of a monitor: the Service it is
// registered on does, and fails the RPC with codes.Internal. A Watch's
// poller runs outside any RPC, so it recovers a monitor's panic itself: the
// check is NOT_SERVING, and the panic is logged as the monitor's error.
//
// # The plaintext probes port
//
// The kubelet's grpc probes connect in plain text on most clusters, so they
// can't reach a grpcserver.Service, which serves only TLS. [NewService]
// serves the health server in plain text on a port of its own, for those
// probes. It answers gRPC health v1 and nothing else: no service of the end
// user's is registered on it, so nothing of theirs is reachable in plain
// text, and their own services stay TLS-only on their own port.
//
// The probes Service owns the App's readiness, as a health Service does. It
// builds a health Service from the health.Options it is given, and runs it
// for as long as it serves, so the end user's Init returns no health
// Service next to it:
//
//	svc, err := grpcserver.NewService(s, settings)
//	if err != nil {
//		return nil, err
//	}
//	cartpb.RegisterCartServiceServer(svc, carts)
//	probes, err := grpchealth.NewService(s, probeSettings)
//	if err != nil {
//		return nil, err
//	}
//	return []humus.Service{svc, probes}, nil
//
// Readiness is false until the Service's Serve starts, and false for good
// once Serve's context is done, which is when the App starts shutting down,
// or once Shutdown is called. Otherwise it checks the monitors
// health.Readiness adds. Liveness checks the monitors health.Liveness adds,
// and is healthy when there are none.
//
// The kubelet's grpc probes name the probe in their service field, on the
// probes port:
//
//	livenessProbe:
//	  grpc:
//	    port: 8081 # the probes port; the end user's services are served over TLS on their own
//	    service: liveness
//	readinessProbe:
//	  grpc:
//	    port: 8081
//	    service: readiness
//
// The port stays plaintext until Kubernetes' grpc probes use TLS without a
// feature gate. TLS is made required on it then, by a proposal of its own.
//
// It serves through the drain. Once the App starts shutting down, readiness
// answers NOT_SERVING, not a refused connection, and liveness still answers
// SERVING, until the App shuts the Service down. Shutdown ends every stream,
// a Watch included, with codes.Unavailable, as a grpcserver.Service does,
// and then stops the server gracefully.
//
// A probes port isn't for the public. The server answers anyone who can
// reach the port, and every RPC checks a monitor. The port is the
// container's, for the kubelet, and isn't routed by a Kubernetes Service or
// an ingress.
//
// The probes Service records no span and no metric, and extracts no trace
// context: a probe every few seconds would be most of a quiet service's
// traces. It logs under this package's import path, through the Setup's
// LoggerProvider: "listening" at INFO, with the address as server.address,
// when it starts serving, each monitor's error as above, and a monitor's
// panic, which fails its RPC with codes.Internal, as one ERROR record.
//
// # Settings
//
// The probes Service is configured with [Settings]: its address, as a plain
// Go value. The field has a check, [CheckAddr], that reports what is wrong
// with a value without naming the field. The package reads no environment
// variable and defines no variable name: the end user's Init sources the
// field, with whatever variable name the service uses, and passes the check
// to Validate, so an operator's bad value fails startup with exit 78,
// naming the variable. The field has no default, so the package has no
// DefaultSettings:
//
//	probeSettings, err := config.Struct(func(r *config.Resolver) grpchealth.Settings {
//		return grpchealth.Settings{
//			// No .Default: the field is required.
//			Addr: r.Get(env("PROBES_ADDR").Validate(grpchealth.CheckAddr)),
//		}
//	}).Read(ctx)
//	if err != nil {
//		return nil, err // exit 78, naming every bad variable
//	}
package grpchealth
