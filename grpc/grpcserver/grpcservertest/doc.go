// Copyright (c) 2026 Z5Labs and Contributors
//
// This software is released under the MIT License.
// https://opensource.org/licenses/MIT

// Package grpcservertest tests gRPC services on a humustest Host.
//
// It sits beside grpcserver as httpservertest sits beside httpserver. Its
// [Start] runs an App the way grpcserver.Main does, and its [NewClient]
// gives a test a client of a Service on a humustest Host, over TLS:
//
//	ca := humustest.NewCA(t)
//	h := humustest.NewHost(t)
//	// ... start the App on h, serving a certificate from ca.Issue(t, "carts") on carts:9090 ...
//	<-h.Listening("carts:9090")
//
//	client := testpb.NewTestServiceClient(grpcservertest.NewClient(t, h, "carts:9090", ca.Pool()))
//
// # Start and the defaults
//
// Start is humustest.Start with grpcserver.Defaults applied first, so a test
// can't forget the defaults the App runs with in production: the 5s drain,
// humus.DrainDelay(5 * time.Second), and the 15s grace period. An RPC that
// never returns is cancelled 20s after SIGTERM. Start does nothing else.
//
// Options given with humustest.WithApp come after the defaults, so they win,
// as the end user's options to grpcserver.Main do:
// humustest.WithApp(humus.WithShutdownGracePeriod(2*time.Second)) keeps the
// 5s drain and cancels that RPC 7s after SIGTERM. HUMUS_SHUTDOWN_GRACE_PERIOD
// and HUMUS_SHUTDOWN_DRAIN_DELAY, given with humustest.WithEnv, override them
// too, as an operator's manifest does.
//
// Start makes no CA and no certificate. The test gives the App its
// certificate the way production does: it writes the certificate's file to
// the Host with humustest.WithFiles, and names it, and its password, in the
// environment the end user's Init sources, with humustest.WithEnv:
//
//	ca := humustest.NewCA(t)
//	bundle, password := ca.Issue(t, "carts").PKCS12(t)
//	h := humustest.NewHost(t, humustest.WithFiles(map[string][]byte{"tls.p12": bundle}))
//
//	p, exit := grpcservertest.Start(t, app.Init,
//		humustest.WithApp(humus.WithHost(h.Host())),
//		humustest.WithEnv(map[string]string{
//			"GRPC_ADDR":             ":9090",
//			"GRPC_TLS_P12_FILE":     "tls.p12",
//			"GRPC_TLS_P12_PASSWORD": password,
//		}),
//	)
//	<-h.Listening("carts:9090")
//
// # The dns resolver and synctest
//
// A gRPC client looks its target up with a resolver, picked by the target's
// scheme. A target with no scheme, such as "carts:9090", gets grpc-go's
// default, the dns resolver, which looks the host up with
// net.DefaultResolver whatever dialler the client has. That reaches the real
// network, where a name that exists only on the test Host is not found.
// Inside a testing/synctest bubble it is worse: the lookup crashes the test
// binary with "fatal error: send on synctest channel from outside bubble".
//
// So NewClient targets "passthrough:///" and address. The passthrough
// resolver hands address to the dialler as it is, and the dialler is the
// Host's: no lookup is made, and a name that exists nowhere but on the test
// Host works, inside a bubble or out.
//
// A test that builds a client of its own, with grpc.NewClient, must not use
// the dns resolver either. It targets "passthrough:///" and the address, and
// dials through the Host with grpc.WithContextDialer, whose function takes
// no network, so it calls h.Dial(ctx, "tcp", addr).
//
// # The certificate
//
// Every Service serves TLS, so NewClient verifies the Service's certificate,
// for address's host, against the roots it is given. It makes no CA: how the
// end user's Init sources its certificate is the end user's decision. The
// test makes a CA with humustest.NewCA, gives the App a certificate from
// ca.Issue for the name it dials, the way production does, and passes
// ca.Pool() to NewClient. A certificate from another CA, or for another name,
// fails the handshake, and the RPC fails with codes.Unavailable.
//
// # Plain text
//
// Transport credentials in opts replace NewClient's, since opts are applied
// after its own options. A test of a server that serves plain text, such as
// a health server, passes insecure.NewCredentials():
//
//	conn := grpcservertest.NewClient(t, h, "localhost:8081", nil,
//		grpc.WithTransportCredentials(insecure.NewCredentials()),
//	)
//
// # Cleanup
//
// NewClient closes the client with t.Cleanup, so a client built in a test,
// or in a subtest, is shut down when that test ends. Inside synctest.Test,
// the cleanup runs inside the bubble, before the bubble waits for its
// goroutines, so the client's goroutines are not reported as a leak.
package grpcservertest
